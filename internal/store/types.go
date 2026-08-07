package store

import "time"

// MediaType constants identify the kind of JSON payload a blob represents.
// Modeled loosely after the OCI image-spec media types.
const (
	MediaTypeManifest = "application/vnd.docksmith.manifest.v1+json"
	MediaTypeConfig   = "application/vnd.docksmith.container.config.v1+json"
	MediaTypeLayer     = "application/vnd.docksmith.image.layer.v1.tar+gzip"
)

// Descriptor points at a content-addressable blob: its digest, size, and
// media type. This is the same shape Docker/OCI manifests use to reference
// config and layer blobs without embedding them inline.
type Descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"` // "sha256:<hex>"
	Size      int64  `json:"size"`
}

// Manifest is the top-level, content-addressed description of an image:
// a pointer to its config blob plus an ordered list of layer blobs.
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
	Created       time.Time    `json:"created"`
}

// ImageConfig is the small JSON blob referenced by Manifest.Config. It holds
// the metadata needed to actually run the image (entrypoint/command) plus
// free-form labels, similar in spirit to a Docker image config.
type ImageConfig struct {
	Command []string          `json:"command,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
	Created time.Time         `json:"created"`
}

// CacheEntry is one row of the on-disk JSON index: a digest mapped to where
// its blob lives, how big it should be, and what kind of object it is.
// Size + Kind let CacheLookup cheaply verify disk-presence (existence AND
// expected size) without re-hashing the blob on every lookup.
type CacheEntry struct {
	Digest   string    `json:"digest"`
	Path     string    `json:"path"` // path relative to the store root
	Size     int64     `json:"size"`
	Kind     string    `json:"kind"` // "manifest" | "config" | "layer"
	CachedAt time.Time `json:"cachedAt"`
}

// Kinds of cacheable objects.
const (
	KindManifest = "manifest"
	KindConfig   = "config"
	KindLayer    = "layer"
)

// ImageInfo is a flattened, human-friendly summary of a tagged image, used
// by ListImages / `docksmith list`.
type ImageInfo struct {
	Tag             string    `json:"tag"`
	ManifestDigest  string    `json:"manifestDigest"`
	Created         time.Time `json:"created"`
	SizeBytes       int64     `json:"sizeBytes"`
	LayerCount      int       `json:"layerCount"`
}
