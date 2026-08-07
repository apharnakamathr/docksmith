// Package store implements Docksmith's container image store: a
// Docker-compatible-in-spirit, content-addressable image store backed by
// SHA-256 digests, a JSON manifest format, and a thread-safe JSON-indexed
// disk cache.
package store

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Directory layout under the store root:
//
//	<root>/blobs/<hex>          content-addressed layer + config blobs
//	<root>/manifests/<hex>.json manifests, keyed by their own digest
//	<root>/tmp/                 scratch space for in-progress writes
//	<root>/refs.json            tag -> manifest digest
//	<root>/index.json           digest -> CacheEntry (see cache.go)
const (
	dirBlobs     = "blobs"
	dirManifests = "manifests"
	dirTmp       = "tmp"
	refsFileName = "refs.json"
)

// Store is a single Docksmith image store rooted at a directory on disk.
type Store struct {
	Root  string
	Cache *Cache

	refsMu sync.Mutex
	refs   map[string]string // tag -> manifest digest

	opMu sync.Mutex // serializes lifecycle ops (create/save/load/delete)
}

// Open initializes the structured state directory layout (creating it if
// necessary) and loads the existing cache index and tag refs, returning a
// ready-to-use Store. Calling Open on an existing root is always safe and
// non-destructive.
func Open(root string) (*Store, error) {
	s := &Store{Root: root, refs: make(map[string]string)}

	if err := s.initDirs(); err != nil {
		return nil, err
	}
	cache, err := LoadCache(root)
	if err != nil {
		return nil, err
	}
	s.Cache = cache

	if err := s.loadRefs(); err != nil {
		return nil, err
	}
	return s, nil
}

// initDirs creates the store's directory skeleton. Safe to call
// repeatedly; MkdirAll is a no-op on directories that already exist.
func (s *Store) initDirs() error {
	for _, d := range []string{dirBlobs, dirManifests, dirTmp} {
		full := filepath.Join(s.Root, d)
		if err := os.MkdirAll(full, 0o755); err != nil {
			return fmt.Errorf("initializing state directory %s: %w", full, err)
		}
	}
	return nil
}

func (s *Store) refsPath() string { return filepath.Join(s.Root, refsFileName) }

func (s *Store) loadRefs() error {
	data, err := os.ReadFile(s.refsPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading refs: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	s.refsMu.Lock()
	defer s.refsMu.Unlock()
	return json.Unmarshal(data, &s.refs)
}

func (s *Store) persistRefsLocked() error {
	data, err := json.MarshalIndent(s.refs, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling refs: %w", err)
	}
	tmp := s.refsPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("writing temp refs file: %w", err)
	}
	return os.Rename(tmp, s.refsPath())
}

func (s *Store) setRef(tag, digest string) error {
	s.refsMu.Lock()
	defer s.refsMu.Unlock()
	s.refs[tag] = digest
	return s.persistRefsLocked()
}

func (s *Store) deleteRef(tag string) (string, error) {
	s.refsMu.Lock()
	defer s.refsMu.Unlock()
	digest, ok := s.refs[tag]
	if !ok {
		return "", fmt.Errorf("no such image: %s", tag)
	}
	delete(s.refs, tag)
	if err := s.persistRefsLocked(); err != nil {
		return "", err
	}
	return digest, nil
}

// resolveTag returns the manifest digest for tag, or an error if untagged.
func (s *Store) resolveTag(tag string) (string, error) {
	s.refsMu.Lock()
	defer s.refsMu.Unlock()
	digest, ok := s.refs[tag]
	if !ok {
		return "", fmt.Errorf("no such image: %s", tag)
	}
	return digest, nil
}

// ---------------------------------------------------------------------
// Blob / manifest resolution: cache-first, with disk fallback + self-heal
// ---------------------------------------------------------------------

// blobPath returns the on-disk path for digest, trusting the cache when it
// verifiably has the blob and falling back to a direct disk-presence check
// (self-healing the cache on success) otherwise. It only fails if the blob
// genuinely cannot be found, which signals cache/disk corruption or a
// deleted blob.
func (s *Store) blobPath(digest string, kind string) (string, error) {
	if entry, ok := s.Cache.CacheLookup(digest); ok {
		return filepath.Join(s.Root, entry.Path), nil
	}

	rel := filepath.Join(dirBlobs, hexPart(digest))
	full := filepath.Join(s.Root, rel)
	info, err := os.Stat(full)
	if err != nil {
		return "", fmt.Errorf("blob %s not found (missing from disk and cache): %w", digest, err)
	}

	// Self-heal: the blob is really there, the cache just didn't know it.
	_ = s.Cache.CacheStore(CacheEntry{Digest: digest, Path: rel, Size: info.Size(), Kind: kind})
	return full, nil
}

func (s *Store) manifestPath(digest string) (string, error) {
	if entry, ok := s.Cache.CacheLookup(digest); ok {
		return filepath.Join(s.Root, entry.Path), nil
	}
	rel := filepath.Join(dirManifests, hexPart(digest)+".json")
	full := filepath.Join(s.Root, rel)
	info, err := os.Stat(full)
	if err != nil {
		return "", fmt.Errorf("manifest %s not found: %w", digest, err)
	}
	_ = s.Cache.CacheStore(CacheEntry{Digest: digest, Path: rel, Size: info.Size(), Kind: KindManifest})
	return full, nil
}

func (s *Store) loadManifest(digest string) (*Manifest, error) {
	path, err := s.manifestPath(digest)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest %s: %w", digest, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing manifest %s: %w", digest, err)
	}
	return &m, nil
}

// writeBlob writes content-addressed data to blobs/<hex>, registers it in
// the cache, and returns its descriptor. If a blob with that digest
// already exists (deduplication - the same layer/config reused across
// images) it is left untouched rather than rewritten.
func (s *Store) writeBlob(data []byte, mediaType, kind string) (Descriptor, error) {
	digest := digestBytes(data)
	rel := filepath.Join(dirBlobs, hexPart(digest))
	full := filepath.Join(s.Root, rel)

	if _, err := os.Stat(full); os.IsNotExist(err) {
		if err := s.atomicWrite(full, data); err != nil {
			return Descriptor{}, err
		}
	}

	if err := s.Cache.CacheStore(CacheEntry{
		Digest: digest, Path: rel, Size: int64(len(data)), Kind: kind,
	}); err != nil {
		return Descriptor{}, err
	}

	return Descriptor{MediaType: mediaType, Digest: digest, Size: int64(len(data))}, nil
}

// writeBlobFromFile is like writeBlob but streams from an existing file
// (e.g. a freshly-built layer tarball) instead of holding it in memory.
func (s *Store) writeBlobFromFile(srcPath, mediaType, kind string) (Descriptor, error) {
	digest, size, err := digestFile(srcPath)
	if err != nil {
		return Descriptor{}, err
	}
	rel := filepath.Join(dirBlobs, hexPart(digest))
	full := filepath.Join(s.Root, rel)

	if _, statErr := os.Stat(full); os.IsNotExist(statErr) {
		if err := os.Rename(srcPath, full); err != nil {
			// Cross-device or other rename failure: fall back to copy.
			if err := copyFile(srcPath, full); err != nil {
				return Descriptor{}, fmt.Errorf("storing blob %s: %w", digest, err)
			}
			_ = os.Remove(srcPath)
		}
	} else {
		_ = os.Remove(srcPath) // dedup: identical blob already stored
	}

	if err := s.Cache.CacheStore(CacheEntry{
		Digest: digest, Path: rel, Size: size, Kind: kind,
	}); err != nil {
		return Descriptor{}, err
	}
	return Descriptor{MediaType: mediaType, Digest: digest, Size: size}, nil
}

func (s *Store) atomicWrite(dst string, data []byte) error {
	tmp := filepath.Join(s.Root, dirTmp, filepath.Base(dst)+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("writing temp blob: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("committing blob %s: %w", dst, err)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// ---------------------------------------------------------------------
// Lifecycle: create
// ---------------------------------------------------------------------

// CreateImage builds a new image from the contents of contextDir (packed
// as a single gzip'd tar layer), attaches an ImageConfig, computes all
// SHA-256 digests, writes everything into the store, and points tag at
// the resulting manifest.
func (s *Store) CreateImage(tag, contextDir string, command []string, labels map[string]string) (*Manifest, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	info, err := os.Stat(contextDir)
	if err != nil {
		return nil, fmt.Errorf("build context %s: %w", contextDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("build context %s is not a directory", contextDir)
	}

	tmpLayer := filepath.Join(s.Root, dirTmp, fmt.Sprintf("layer-%d.tar.gz", time.Now().UnixNano()))
	if err := tarGzDir(contextDir, tmpLayer); err != nil {
		return nil, fmt.Errorf("packing layer: %w", err)
	}
	layerDesc, err := s.writeBlobFromFile(tmpLayer, MediaTypeLayer, KindLayer)
	if err != nil {
		return nil, err
	}

	cfg := ImageConfig{Command: command, Labels: labels, Created: time.Now().UTC()}
	cfgBytes, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshaling image config: %w", err)
	}
	configDesc, err := s.writeBlob(cfgBytes, MediaTypeConfig, KindConfig)
	if err != nil {
		return nil, err
	}

	manifest := Manifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeManifest,
		Config:        configDesc,
		Layers:        []Descriptor{layerDesc},
		Created:       time.Now().UTC(),
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling manifest: %w", err)
	}
	manifestDigest := digestBytes(manifestBytes)
	manifestRel := filepath.Join(dirManifests, hexPart(manifestDigest)+".json")
	if err := s.atomicWrite(filepath.Join(s.Root, manifestRel), manifestBytes); err != nil {
		return nil, err
	}
	if err := s.Cache.CacheStore(CacheEntry{
		Digest: manifestDigest, Path: manifestRel, Size: int64(len(manifestBytes)), Kind: KindManifest,
	}); err != nil {
		return nil, err
	}

	if err := s.setRef(tag, manifestDigest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

// ---------------------------------------------------------------------
// Lifecycle: list
// ---------------------------------------------------------------------

// ListImages returns a summary of every tagged image in the store, sorted
// by tag for deterministic output.
func (s *Store) ListImages() ([]ImageInfo, error) {
	s.refsMu.Lock()
	tags := make([]string, 0, len(s.refs))
	digestByTag := make(map[string]string, len(s.refs))
	for tag, digest := range s.refs {
		tags = append(tags, tag)
		digestByTag[tag] = digest
	}
	s.refsMu.Unlock()

	sort.Strings(tags)

	out := make([]ImageInfo, 0, len(tags))
	for _, tag := range tags {
		digest := digestByTag[tag]
		m, err := s.loadManifest(digest)
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", tag, err)
		}
		var size int64 = m.Config.Size
		for _, l := range m.Layers {
			size += l.Size
		}
		out = append(out, ImageInfo{
			Tag:            tag,
			ManifestDigest: digest,
			Created:        m.Created,
			SizeBytes:      size,
			LayerCount:     len(m.Layers),
		})
	}
	return out, nil
}

// InspectImage returns the full manifest for a tag.
func (s *Store) InspectImage(tag string) (*Manifest, error) {
	digest, err := s.resolveTag(tag)
	if err != nil {
		return nil, err
	}
	return s.loadManifest(digest)
}

// ---------------------------------------------------------------------
// Lifecycle: save / load
// ---------------------------------------------------------------------

// blobEntryName is how a blob is named inside a `save` export tarball.
func blobEntryName(digest string) string { return "blobs/" + hexPart(digest) }

// SaveImage exports tag as a single self-contained tar file containing its
// manifest plus every blob (config + layers) it references — analogous to
// `docker save`.
func (s *Store) SaveImage(tag, outPath string) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	digest, err := s.resolveTag(tag)
	if err != nil {
		return err
	}
	manifestPath, err := s.manifestPath(digest)
	if err != nil {
		return err
	}
	manifest, err := s.loadManifest(digest)
	if err != nil {
		return err
	}

	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("creating export file: %w", err)
	}
	defer out.Close()

	tw := tar.NewWriter(out)
	defer tw.Close()

	if err := addFileToTar(tw, manifestPath, "manifest.json"); err != nil {
		return err
	}

	blobDigests := append([]string{manifest.Config.Digest}, descriptorDigests(manifest.Layers)...)
	for _, d := range blobDigests {
		path, err := s.blobPath(d, KindLayer)
		if err != nil {
			return fmt.Errorf("save: %w", err)
		}
		if err := addFileToTar(tw, path, blobEntryName(d)); err != nil {
			return err
		}
	}
	return nil
}

func descriptorDigests(ds []Descriptor) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Digest
	}
	return out
}

func addFileToTar(tw *tar.Writer, srcPath, entryName string) error {
	info, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", srcPath, err)
	}
	hdr := &tar.Header{
		Name:    entryName,
		Mode:    0o644,
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("writing tar header for %s: %w", entryName, err)
	}
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("writing %s into archive: %w", entryName, err)
	}
	return nil
}

// LoadImage imports an image previously produced by SaveImage. Every blob
// is re-hashed as it is imported and checked against the digest the
// manifest claims for it — a mismatch means the export is corrupt or was
// tampered with, and the load is aborted before anything is committed. If
// tag is non-empty, it is pointed at the imported manifest.
func (s *Store) LoadImage(tarPath, tag string) (*Manifest, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	f, err := os.Open(tarPath)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", tarPath, err)
	}
	defer f.Close()

	files := make(map[string][]byte)
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("reading %s from archive: %w", hdr.Name, err)
		}
		files[hdr.Name] = data
	}

	manifestBytes, ok := files["manifest.json"]
	if !ok {
		return nil, fmt.Errorf("archive missing manifest.json")
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("parsing manifest.json: %w", err)
	}

	verify := func(desc Descriptor, kind string) error {
		data, ok := files[blobEntryName(desc.Digest)]
		if !ok {
			return fmt.Errorf("archive missing blob for %s", desc.Digest)
		}
		if int64(len(data)) != desc.Size {
			return fmt.Errorf("blob %s: size mismatch (manifest says %d, archive has %d)", desc.Digest, desc.Size, len(data))
		}
		actual := digestBytes(data)
		if actual != desc.Digest {
			return fmt.Errorf("blob integrity check failed: manifest claims %s but content hashes to %s", desc.Digest, actual)
		}
		if _, err := s.writeBlob(data, desc.MediaType, kind); err != nil {
			return err
		}
		return nil
	}

	if err := verify(manifest.Config, KindConfig); err != nil {
		return nil, fmt.Errorf("importing config: %w", err)
	}
	for _, l := range manifest.Layers {
		if err := verify(l, KindLayer); err != nil {
			return nil, fmt.Errorf("importing layer: %w", err)
		}
	}

	manifestDigest := digestBytes(manifestBytes)
	manifestRel := filepath.Join(dirManifests, hexPart(manifestDigest)+".json")
	if err := s.atomicWrite(filepath.Join(s.Root, manifestRel), manifestBytes); err != nil {
		return nil, err
	}
	if err := s.Cache.CacheStore(CacheEntry{
		Digest: manifestDigest, Path: manifestRel, Size: int64(len(manifestBytes)), Kind: KindManifest,
	}); err != nil {
		return nil, err
	}

	if tag != "" {
		if err := s.setRef(tag, manifestDigest); err != nil {
			return nil, err
		}
	}
	return &manifest, nil
}

// ---------------------------------------------------------------------
// Lifecycle: delete (with reference-counted blob/manifest GC)
// ---------------------------------------------------------------------

// DeleteImage removes tag. If no other tag references the same manifest,
// the manifest and any of its blobs that are not referenced by any other
// remaining manifest are also removed (garbage collected).
func (s *Store) DeleteImage(tag string) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	digest, err := s.deleteRef(tag)
	if err != nil {
		return err
	}

	stillReferenced, err := s.manifestReferenced(digest)
	if err != nil {
		return err
	}
	if stillReferenced {
		return nil
	}

	manifest, err := s.loadManifest(digest)
	if err != nil {
		// Manifest already gone; nothing left to clean up.
		return nil
	}

	candidateBlobs := append([]string{manifest.Config.Digest}, descriptorDigests(manifest.Layers)...)
	for _, blobDigest := range candidateBlobs {
		usedElsewhere, err := s.blobReferencedExcluding(blobDigest, digest)
		if err != nil {
			return err
		}
		if usedElsewhere {
			continue
		}
		path, statErr := s.blobPath(blobDigest, KindLayer)
		if statErr == nil {
			_ = os.Remove(path)
		}
		_ = s.Cache.Evict(blobDigest)
	}

	manifestPath, err := s.manifestPath(digest)
	if err == nil {
		_ = os.Remove(manifestPath)
	}
	_ = s.Cache.Evict(digest)
	return nil
}

// manifestReferenced reports whether any remaining tag still points at
// digest.
func (s *Store) manifestReferenced(digest string) (bool, error) {
	s.refsMu.Lock()
	defer s.refsMu.Unlock()
	for _, d := range s.refs {
		if d == digest {
			return true, nil
		}
	}
	return false, nil
}

// blobReferencedExcluding scans every manifest still on disk (other than
// excludeManifest) to see whether blobDigest is still needed by anything.
// This is a simple, correctness-first reference count: fine for a local,
// single-node store; a production registry would maintain a live refcount
// index instead of rescanning on every delete.
func (s *Store) blobReferencedExcluding(blobDigest, excludeManifest string) (bool, error) {
	dir := filepath.Join(s.Root, dirManifests)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("scanning manifests for GC: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		digest := digestPrefix + hexPart(filepathTrimExt(e.Name()))
		if digest == excludeManifest {
			continue
		}
		m, err := s.loadManifest(digest)
		if err != nil {
			continue // unreadable/partial manifest: ignore for GC purposes
		}
		if m.Config.Digest == blobDigest {
			return true, nil
		}
		for _, l := range m.Layers {
			if l.Digest == blobDigest {
				return true, nil
			}
		}
	}
	return false, nil
}

func filepathTrimExt(name string) string {
	return name[:len(name)-len(filepath.Ext(name))]
}

// ---------------------------------------------------------------------
// Layer packing helpers
// ---------------------------------------------------------------------

// tarGzDir packs the contents of srcDir into a gzip-compressed tar file at
// dstPath, using paths relative to srcDir so the archive is portable.
func tarGzDir(srcDir, dstPath string) error {
	out, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer out.Close()

	gw := gzip.NewWriter(out)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}
