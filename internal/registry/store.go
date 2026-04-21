package registry

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"k8s.io/klog"
)

// BlobMeta holds the CAS coordinates of a known blob: its hex hash and size.
// Both fields are required to construct a ByteStream resource name or a REAPI Digest.
type BlobMeta struct {
	Hash string // hex SHA-256, no "sha256:" prefix
	Size int64
}

// StoredManifest holds an in-memory manifest with its media type and digest.
type StoredManifest struct {
	Content     []byte
	ContentType string
	Digest      string // "sha256:..."
}

type descriptor struct {
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	MediaType string `json:"mediaType"`
}

// BlobStore is the interface required by Registry. It allows the in-memory
// Store and the ConfigMap-backed ConfigMapStore to be used interchangeably.
type BlobStore interface {
	RegisterManifestBlobs(body []byte)
	GetBlob(dgst string) (BlobMeta, bool)
	PutManifest(name, ref string, m *StoredManifest)
	GetManifest(name, ref string) (*StoredManifest, bool)
}

// Store is a thread-safe in-memory store for manifest content and the blob
// sizes extracted from those manifests. Blob sizes are the only state we need:
// they are required to construct the ByteStream resource name at pull time.
// Blobs are never stored locally — all content is served from CAS.
type Store struct {
	mu        sync.RWMutex
	blobs     map[string]BlobMeta                   // hex hash → size
	manifests map[string]map[string]*StoredManifest // repo name → (tag|digest) → manifest
}

func NewStore() *Store {
	return &Store{
		blobs:     make(map[string]BlobMeta),
		manifests: make(map[string]map[string]*StoredManifest),
	}
}

// RegisterBlob records the hash and size of a blob so it can be retrieved from CAS.
func (s *Store) RegisterBlob(dgst string, size int64) {
	hash := hashFromDigest(dgst)
	if hash == "" || size <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.blobs[hash]; !ok {
		s.blobs[hash] = BlobMeta{Hash: hash, Size: size}
		klog.V(3).Infof("registry: registered blob %s (%d bytes)", dgst, size)
	}
}

// GetBlob returns the metadata for a known blob, or false if unknown.
// Unknown blobs still exist optimistically in CAS — the caller handles that.
func (s *Store) GetBlob(dgst string) (BlobMeta, bool) {
	hash := hashFromDigest(dgst)
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.blobs[hash]
	return m, ok
}

// PutManifest stores a manifest under both its tag and its content digest.
func (s *Store) PutManifest(name, ref string, m *StoredManifest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manifests[name] == nil {
		s.manifests[name] = make(map[string]*StoredManifest)
	}
	s.manifests[name][ref] = m
	s.manifests[name][m.Digest] = m
}

// GetManifest returns a stored manifest by name and tag or digest.
func (s *Store) GetManifest(name, ref string) (*StoredManifest, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.manifests[name] == nil {
		return nil, false
	}
	m, ok := s.manifests[name][ref]
	return m, ok
}

// RegisterManifestBlobs parses an OCI or Docker manifest and registers the
// size of every blob descriptor it references. After this call, blob GETs for
// any of those digests can be served from CAS.
func (s *Store) RegisterManifestBlobs(body []byte) {
	parseAndRegisterManifestBlobs(s.RegisterBlob, body)
}

// parseManifestBlobs parses an OCI or Docker manifest (image manifest or image
// index) and returns all blob descriptors it references.
func parseManifestBlobs(body []byte) []BlobMeta {
	var m struct {
		Config    descriptor   `json:"config"`
		Layers    []descriptor `json:"layers"`
		Manifests []descriptor `json:"manifests"` // OCI image index
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil
	}
	var blobs []BlobMeta
	add := func(d descriptor) {
		if hash := hashFromDigest(d.Digest); hash != "" && d.Size > 0 {
			blobs = append(blobs, BlobMeta{Hash: hash, Size: d.Size})
		}
	}
	add(m.Config)
	for _, d := range m.Layers {
		add(d)
	}
	for _, d := range m.Manifests {
		add(d)
	}
	return blobs
}

// parseAndRegisterManifestBlobs parses an OCI or Docker manifest (image
// manifest or image index) and calls register for each descriptor found.
// Callers pass their own RegisterBlob implementation so that the correct
// persistence path is used (in-memory vs ConfigMap-backed).
func parseAndRegisterManifestBlobs(register func(dgst string, size int64), body []byte) {
	for _, b := range parseManifestBlobs(body) {
		register("sha256:"+b.Hash, b.Size)
	}
}

// NewManifest computes the digest of body, wraps it in a StoredManifest, and
// returns it. It does not register blobs — call RegisterManifestBlobs first.
func NewManifest(body []byte, contentType string) *StoredManifest {
	sum := sha256.Sum256(body)
	return &StoredManifest{
		Content:     body,
		ContentType: contentType,
		Digest:      fmt.Sprintf("sha256:%x", sum),
	}
}

// hashFromDigest strips the "sha256:" prefix from a digest string.
func hashFromDigest(dgst string) string {
	if strings.HasPrefix(dgst, "sha256:") {
		return dgst[7:]
	}
	return ""
}
