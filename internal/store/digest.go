package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// digestPrefix is prepended to every digest string, mirroring the
// "sha256:<hex>" convention used by Docker and the OCI image spec.
const digestPrefix = "sha256:"

// digestBytes computes the content digest of an in-memory blob.
func digestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return digestPrefix + hex.EncodeToString(sum[:])
}

// digestReader streams r through SHA-256, returning the digest and the
// number of bytes read. Used so large layer archives never need to be
// held fully in memory just to be hashed.
func digestReader(r io.Reader) (digest string, size int64, err error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, fmt.Errorf("hashing content: %w", err)
	}
	return digestPrefix + hex.EncodeToString(h.Sum(nil)), n, nil
}

// digestFile hashes the file at path on disk.
func digestFile(path string) (digest string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("opening %s for digest: %w", path, err)
	}
	defer f.Close()
	return digestReader(f)
}

// hexPart strips the "sha256:" prefix so a digest is safe to use as a
// filename (colons are awkward across filesystems even on platforms where
// they're technically legal).
func hexPart(digest string) string {
	return strings.TrimPrefix(digest, digestPrefix)
}

// validDigest reports whether s looks like a well-formed sha256 digest.
func validDigest(s string) bool {
	if !strings.HasPrefix(s, digestPrefix) {
		return false
	}
	h := strings.TrimPrefix(s, digestPrefix)
	if len(h) != 64 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}
