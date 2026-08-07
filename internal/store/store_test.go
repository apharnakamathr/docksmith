package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// newTestContext creates a temp directory with a couple of files, standing
// in for a build context / layer contents.
func newTestContext(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.bin"), []byte("pretend-binary-contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "conf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "conf", "app.yaml"), []byte("key: value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCreateListInspectDelete(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	ctxDir := newTestContext(t)
	manifest, err := s.CreateImage("app:v1", ctxDir, []string{"/bin/app", "--serve"}, map[string]string{"team": "fintech"})
	if err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	if !validDigest(manifest.Config.Digest) {
		t.Fatalf("config digest looks malformed: %s", manifest.Config.Digest)
	}
	if len(manifest.Layers) != 1 || !validDigest(manifest.Layers[0].Digest) {
		t.Fatalf("expected exactly one well-formed layer digest, got %+v", manifest.Layers)
	}

	images, err := s.ListImages()
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(images) != 1 || images[0].Tag != "app:v1" {
		t.Fatalf("unexpected list result: %+v", images)
	}

	got, err := s.InspectImage("app:v1")
	if err != nil {
		t.Fatalf("InspectImage: %v", err)
	}
	if got.Config.Digest != manifest.Config.Digest {
		t.Fatalf("inspect digest mismatch: got %s want %s", got.Config.Digest, manifest.Config.Digest)
	}

	if err := s.DeleteImage("app:v1"); err != nil {
		t.Fatalf("DeleteImage: %v", err)
	}
	images, err = s.ListImages()
	if err != nil {
		t.Fatalf("ListImages after delete: %v", err)
	}
	if len(images) != 0 {
		t.Fatalf("expected no images after delete, got %+v", images)
	}

	// GC should have removed the underlying blobs since nothing references them.
	blobDir := filepath.Join(root, dirBlobs)
	entries, err := os.ReadDir(blobDir)
	if err != nil {
		t.Fatalf("reading blobs dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected blobs to be garbage collected, found %d leftover", len(entries))
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	srcRoot := t.TempDir()
	src, err := Open(srcRoot)
	if err != nil {
		t.Fatalf("Open src: %v", err)
	}
	ctxDir := newTestContext(t)
	original, err := src.CreateImage("app:v1", ctxDir, []string{"/bin/app"}, nil)
	if err != nil {
		t.Fatalf("CreateImage: %v", err)
	}

	tarPath := filepath.Join(t.TempDir(), "app-v1.tar")
	if err := src.SaveImage("app:v1", tarPath); err != nil {
		t.Fatalf("SaveImage: %v", err)
	}
	if info, err := os.Stat(tarPath); err != nil || info.Size() == 0 {
		t.Fatalf("expected non-empty export tar, err=%v", err)
	}

	dstRoot := t.TempDir()
	dst, err := Open(dstRoot)
	if err != nil {
		t.Fatalf("Open dst: %v", err)
	}
	loaded, err := dst.LoadImage(tarPath, "app:imported")
	if err != nil {
		t.Fatalf("LoadImage: %v", err)
	}
	if loaded.Config.Digest != original.Config.Digest {
		t.Fatalf("config digest changed across save/load: %s vs %s", loaded.Config.Digest, original.Config.Digest)
	}
	if len(loaded.Layers) != len(original.Layers) || loaded.Layers[0].Digest != original.Layers[0].Digest {
		t.Fatalf("layer digests changed across save/load")
	}

	images, err := dst.ListImages()
	if err != nil || len(images) != 1 || images[0].Tag != "app:imported" {
		t.Fatalf("unexpected images in destination store: %+v, err=%v", images, err)
	}
}

func TestLoadRejectsCorruptArchive(t *testing.T) {
	srcRoot := t.TempDir()
	src, err := Open(srcRoot)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctxDir := newTestContext(t)
	if _, err := src.CreateImage("app:v1", ctxDir, nil, nil); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	tarPath := filepath.Join(t.TempDir(), "app.tar")
	if err := src.SaveImage("app:v1", tarPath); err != nil {
		t.Fatalf("SaveImage: %v", err)
	}

	// Corrupt the archive by flipping some bytes near the end (likely
	// inside a blob's content), so its digest will no longer match what
	// the manifest claims.
	data, err := os.ReadFile(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(data) - 100; i < len(data)-50; i++ {
		data[i] ^= 0xFF
	}
	if err := os.WriteFile(tarPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	dstRoot := t.TempDir()
	dst, err := Open(dstRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dst.LoadImage(tarPath, "app:bad"); err == nil {
		t.Fatalf("expected LoadImage to reject a corrupted archive, but it succeeded")
	}
}

// TestCacheLookupDetectsManualDeletion verifies the "disk-presence
// verification" behavior: if a blob is deleted out from under the store
// without going through DeleteImage, CacheLookup must not report it as
// present.
func TestCacheLookupDetectsManualDeletion(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	ctxDir := newTestContext(t)
	manifest, err := s.CreateImage("app:v1", ctxDir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	entry, ok := s.Cache.CacheLookup(manifest.Layers[0].Digest)
	if !ok {
		t.Fatalf("expected cache hit right after create")
	}

	// Simulate someone deleting the blob directly on disk.
	if err := os.Remove(filepath.Join(root, entry.Path)); err != nil {
		t.Fatal(err)
	}

	if _, ok := s.Cache.CacheLookup(manifest.Layers[0].Digest); ok {
		t.Fatalf("CacheLookup reported a hit for a blob that was deleted from disk")
	}
}

// TestCacheConcurrentAccess exercises CacheStore/CacheLookup from many
// goroutines at once to catch data races (run with -race).
func TestCacheConcurrentAccess(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			data := []byte{byte(i), byte(i >> 8), 1, 2, 3}
			if _, err := s.writeBlob(data, MediaTypeLayer, KindLayer); err != nil {
				t.Errorf("writeBlob: %v", err)
			}
		}(i)
	}
	wg.Wait()

	entries := s.Cache.List()
	if len(entries) != n {
		t.Fatalf("expected %d distinct cache entries, got %d", n, len(entries))
	}
}

func TestDeleteKeepsSharedBlobs(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	ctxDir := newTestContext(t)

	// Two tags built from the same context will end up sharing the layer
	// blob (content-addressing dedups it) but get distinct manifests
	// because CreateImage stamps a fresh Created timestamp each time.
	if _, err := s.CreateImage("app:v1", ctxDir, []string{"/bin/app"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateImage("app:v2", ctxDir, []string{"/bin/app"}, nil); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteImage("app:v1"); err != nil {
		t.Fatalf("DeleteImage app:v1: %v", err)
	}

	// app:v2 must still resolve and its blobs must still be present.
	m, err := s.InspectImage("app:v2")
	if err != nil {
		t.Fatalf("app:v2 should survive deleting app:v1: %v", err)
	}
	if _, err := s.blobPath(m.Config.Digest, KindConfig); err != nil {
		t.Fatalf("app:v2 config blob was incorrectly garbage collected: %v", err)
	}
	for _, l := range m.Layers {
		if _, err := s.blobPath(l.Digest, KindLayer); err != nil {
			t.Fatalf("app:v2 layer blob was incorrectly garbage collected: %v", err)
		}
	}
}
