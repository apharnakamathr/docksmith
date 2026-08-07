# Docksmith — Container Image Store

A lightweight, Docker-compatible-in-spirit container image store written in
Go. Docksmith manages content-addressable image manifests (SHA-256
digests), tracks layers, and keeps a JSON-indexed disk cache with
disk-presence verification so a manually deleted or corrupted blob can
never be served as if it were still valid.

This is a from-scratch, standalone implementation. It doesn't talk to the
Docker daemon or a real registry. It reproduces the *ideas* (content
addressing, manifests, layers, save/load as portable tar archives) in a
small, readable codebase.

## Features

- **Content-addressable manifests** : Every manifest, config, and layer is
  identified by the SHA-256 digest of its own bytes (`sha256:<hex>`),
  computed via streaming hashing so large layers never need to be fully
  buffered in memory just to be hashed.
- **Layer tracking** : A build context directory is packed into a single
  gzip'd tar layer; manifests reference layers (and a config blob) by
  digest + size, the same shape Docker/OCI manifests use.
- **JSON-indexed disk cache** (`internal/store/cache.go`) : A thread-safe
  `Cache` backed by `index.json`, offering:
  - `CacheLookup(digest)` returns a hit **only if** the blob it points to
    is still verifiably present on disk (right size). A stale entry is
    evicted automatically, so cache corruption can't quietly turn into
    silently serving a missing/short blob.
  - `CacheStore(entry)` records/updates an entry and persists the index
    atomically (write-temp-file-then-rename).
- **Image lifecycle** (`internal/store/store.go`):
  - `CreateImage` : build an image from a directory, write layer + config
    blobs, compute digests, write the manifest, tag it.
  - `SaveImage` : export a tagged image as a single self-contained tar
    (manifest + every referenced blob), like `docker save`.
  - `LoadImage` : import that tar, **re-hashing every blob** against the
    digest the manifest claims for it before committing anything, so a
    corrupted or tampered archive is rejected up front.
  - `ListImages` : summarize every tagged image (digest, size, layer
    count, created time).
  - `DeleteImage` : untag an image; if no other tag still points at its
    manifest, reference-counted garbage collection removes the manifest
    and any blobs no other remaining manifest still needs.
- **Structured state directory initialization** : `store.Open(root)`
  creates (or verifies) a consistent on-disk layout:

  ```
  <root>/
    blobs/         content-addressed layer + config blobs, filename = hex digest
    manifests/     manifest JSON files, filename = <hex digest>.json
    tmp/           scratch space for atomic writes
    refs.json      tag -> manifest digest
    index.json     digest -> cache entry (path, size, kind)
  ```

## Building

```bash
go build -o docksmith ./cmd/docksmith
```

Requires Go 1.22+. No third-party dependencies — the whole thing is
standard library only (`archive/tar`, `compress/gzip`, `crypto/sha256`,
`encoding/json`).

## CLI usage

```bash
# Point DOCKSMITH_ROOT wherever you want the store to live (default ./.docksmith)
export DOCKSMITH_ROOT=./.docksmith

./docksmith create --tag fraudsvc:v1 --context ./build \
    --cmd "/bin/fraudsvc --serve" --label team=fintech

./docksmith list

./docksmith inspect fraudsvc:v1

./docksmith save fraudsvc:v1 -o fraudsvc-v1.tar

./docksmith load fraudsvc-v1.tar --tag fraudsvc:restored

./docksmith delete fraudsvc:v1
```

Flags and positional arguments can be given in either order
(`save fraudsvc:v1 -o out.tar` and `save -o out.tar fraudsvc:v1` both work).

## Running the tests

```bash
go test ./... -race -v
```

The test suite (`internal/store/store_test.go`) covers:

- full create → list → inspect → delete round trip, including that
  garbage collection actually removes now-unreferenced blobs;
- save → load round trip across two independent stores, verifying
  digests are preserved exactly;
- rejection of a corrupted/tampered save archive during load;
- `CacheLookup` correctly reporting a miss (not a stale hit) after a blob
  is deleted directly from disk, outside of `DeleteImage`;
- concurrent `CacheStore`/`CacheLookup` from many goroutines, run under
  `-race`, to catch data races in the cache;
- that deleting one tag never garbage-collects blobs still shared by
  another tag (content-addressing dedup).

## Design notes

- **Why a manual reference-count scan on delete instead of a live
  refcount index?** It's the simplest thing that's obviously correct for
  a local, single-node store: `DeleteImage` re-scans `manifests/` to check
  whether any other manifest still needs a given blob before removing it.
  A production registry would maintain a live refcount instead of
  rescanning on every delete, but for a store of this size that tradeoff
  isn't worth the extra bookkeeping.
- **Why self-healing cache lookups?** `blobPath`/`manifestPath` check the
  cache first, but fall back to a direct disk-presence check if the cache
  misses — and if the blob genuinely is there, they repopulate the cache
  entry instead of erroring. This means the index can be deleted or go
  stale without corrupting the store; it's a cache, not the source of
  truth.
- **Why re-verify digests on load instead of just trusting the
  manifest?** A manifest is just a claim about what a blob's digest and
  size *should* be. `LoadImage` treats that claim as untrusted input:
  every blob is re-hashed as it's read out of the archive, and any
  mismatch aborts the whole load before anything touches the destination
  store.
