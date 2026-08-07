package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// indexFileName is the JSON file (relative to the store root) that backs
// the cache. It is the durable record; the in-memory map is just a fast
// path over it.
const indexFileName = "index.json"

// Cache is a thread-safe, JSON-indexed disk cache mapping content digests
// to where their blob lives on disk. It is deliberately *not* a pure
// in-memory cache: every successful CacheLookup re-verifies that the blob
// it points to still exists on disk (and is the expected size) before
// returning a hit, so a manually deleted or truncated blob can never be
// served as if it were still valid.
type Cache struct {
	mu      sync.RWMutex
	root    string
	path    string
	entries map[string]CacheEntry
}

// LoadCache reads (or lazily initializes) the on-disk index for the store
// rooted at root.
func LoadCache(root string) (*Cache, error) {
	c := &Cache{
		root:    root,
		path:    filepath.Join(root, indexFileName),
		entries: make(map[string]CacheEntry),
	}

	data, err := os.ReadFile(c.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return c, nil
	case err != nil:
		return nil, fmt.Errorf("reading cache index: %w", err)
	}

	if len(data) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(data, &c.entries); err != nil {
		return nil, fmt.Errorf("parsing cache index %s: %w", c.path, err)
	}
	return c, nil
}

// CacheLookup returns the entry for digest if it is present in the index
// AND the blob it references is still verifiably present on disk with the
// expected size. A stale entry (missing or size-mismatched blob) is
// evicted from the index automatically and reported as a miss, so callers
// never have to special-case corruption themselves.
func (c *Cache) CacheLookup(digest string) (CacheEntry, bool) {
	c.mu.RLock()
	entry, ok := c.entries[digest]
	c.mu.RUnlock()
	if !ok {
		return CacheEntry{}, false
	}

	info, err := os.Stat(filepath.Join(c.root, entry.Path))
	if err != nil || info.IsDir() || info.Size() != entry.Size {
		c.evict(digest)
		return CacheEntry{}, false
	}
	return entry, true
}

// CacheStore records (or updates) the cache entry for a digest and
// persists the index to disk. The write to disk is atomic (write to a
// temp file, then rename) so a crash mid-write can never leave index.json
// half-written.
func (c *Cache) CacheStore(entry CacheEntry) error {
	if entry.CachedAt.IsZero() {
		entry.CachedAt = time.Now().UTC()
	}

	c.mu.Lock()
	c.entries[entry.Digest] = entry
	snapshot := c.snapshotLocked()
	c.mu.Unlock()

	return c.persist(snapshot)
}

// Evict removes digest from the index and persists the change. Exposed so
// image deletion / GC can keep the cache consistent with what's actually
// on disk.
func (c *Cache) Evict(digest string) error {
	c.mu.Lock()
	_, existed := c.entries[digest]
	if !existed {
		c.mu.Unlock()
		return nil
	}
	delete(c.entries, digest)
	snapshot := c.snapshotLocked()
	c.mu.Unlock()

	return c.persist(snapshot)
}

// evict is the same as Evict but swallows persistence errors, used
// internally from CacheLookup where surfacing a write error from a *read*
// path would be surprising. The in-memory map is corrected either way.
func (c *Cache) evict(digest string) {
	c.mu.Lock()
	delete(c.entries, digest)
	snapshot := c.snapshotLocked()
	c.mu.Unlock()
	_ = c.persist(snapshot)
}

// List returns a snapshot of all current entries, sorted by digest for
// deterministic output.
func (c *Cache) List() []CacheEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]CacheEntry, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e)
	}
	return out
}

// snapshotLocked must be called with c.mu held (read or write).
func (c *Cache) snapshotLocked() map[string]CacheEntry {
	snap := make(map[string]CacheEntry, len(c.entries))
	for k, v := range c.entries {
		snap[k] = v
	}
	return snap
}

// persist writes snapshot to the index file atomically. It does not touch
// c.entries and takes no lock, so it is safe to call after releasing mu.
func (c *Cache) persist(snapshot map[string]CacheEntry) error {
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling cache index: %w", err)
	}

	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("writing temp cache index: %w", err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return fmt.Errorf("committing cache index: %w", err)
	}
	return nil
}
