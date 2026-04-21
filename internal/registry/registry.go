package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/gorilla/mux"
	"k8s.io/klog"
)

// Registry implements a minimal OCI Distribution Spec v1.1 registry backed by
// Buildbarn's CAS for blob storage.
//
// The registry is optimistic: it claims to already have every blob, so clients
// skip all layer uploads. Blob sizes are learned from manifest pushes (the
// manifest always carries sizes in its descriptors). Blobs are served from CAS
// at pull time using those sizes to construct the ByteStream resource name.
//
// Push flow:
//  1. Client HEADs each blob → 200 (optimistic). Client skips all uploads.
//  2. Client PUTs the manifest → registry parses descriptors, registers sizes.
//
// Pull flow:
//  1. kubelet GETs the manifest → served from memory.
//  2. kubelet GETs each blob → looked up by size, streamed from CAS.
type Registry struct {
	store  BlobStore
	cas    *CASClient
	server *http.Server
}

func New(addr string, store BlobStore, cas *CASClient) *Registry {
	r := &Registry{store: store, cas: cas}
	r.server = &http.Server{
		Addr:    addr,
		Handler: r.routes(),
	}
	return r
}

// Serve starts the registry HTTP server. If certFile and keyFile are both
// non-empty it serves HTTPS; otherwise it falls back to plain HTTP.
func (r *Registry) Serve(certFile, keyFile string) error {
	if certFile != "" && keyFile != "" {
		klog.Infof("registry: listening on %s (TLS)", r.server.Addr)
		return r.server.ListenAndServeTLS(certFile, keyFile)
	}
	klog.Infof("registry: listening on %s (plain HTTP)", r.server.Addr)
	return r.server.ListenAndServe()
}

func (r *Registry) Shutdown(ctx context.Context) error {
	return r.server.Shutdown(ctx)
}

func (r *Registry) routes() http.Handler {
	m := mux.NewRouter()

	m.HandleFunc("/v2/", r.handleVersion).Methods(http.MethodGet)
	m.HandleFunc("/v2", r.handleVersion).Methods(http.MethodGet)

	// Upload routes must be registered before the blob route so that
	// gorilla/mux matches "/blobs/uploads/..." before "/blobs/{digest}".
	m.HandleFunc("/v2/{name:.+}/blobs/uploads/", r.handleUploadStart).Methods(http.MethodPost)
	m.HandleFunc("/v2/{name:.+}/blobs/uploads/{uuid}", r.handleUploadChunk).Methods(http.MethodPatch)
	m.HandleFunc("/v2/{name:.+}/blobs/uploads/{uuid}", r.handleUploadComplete).Methods(http.MethodPut)
	m.HandleFunc("/v2/{name:.+}/blobs/uploads/{uuid}", r.handleUploadDelete).Methods(http.MethodDelete)

	m.HandleFunc("/v2/{name:.+}/blobs/{digest}", r.handleBlobHead).Methods(http.MethodHead)
	m.HandleFunc("/v2/{name:.+}/blobs/{digest}", r.handleBlobGet).Methods(http.MethodGet)

	m.HandleFunc("/v2/{name:.+}/manifests/{reference}", r.handleManifestHead).Methods(http.MethodHead)
	m.HandleFunc("/v2/{name:.+}/manifests/{reference}", r.handleManifestGet).Methods(http.MethodGet)
	m.HandleFunc("/v2/{name:.+}/manifests/{reference}", r.handleManifestPut).Methods(http.MethodPut)

	return m
}

// --- Version check ---

func (r *Registry) handleVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("{}"))
}

// --- Blobs ---

// handleBlobHead always returns 200, claiming the blob exists in CAS. The
// client therefore skips the upload. If the blob is not actually in CAS, the
// error surfaces at pull time (GET), not at push time.
func (r *Registry) handleBlobHead(w http.ResponseWriter, req *http.Request) {
	dgst := mux.Vars(req)["digest"]
	w.Header().Set("Docker-Content-Digest", dgst)
	w.Header().Set("Content-Type", "application/octet-stream")
	// Include Content-Length if we already know the size (e.g. from a prior
	// manifest push). Omitting it is valid; clients only use it for pre-checks.
	if meta, ok := r.store.GetBlob(dgst); ok {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", meta.Size))
	}
	w.WriteHeader(http.StatusOK)
}

// handleBlobGet streams the blob from CAS. This requires the size, which must
// have been registered via a preceding manifest push.
func (r *Registry) handleBlobGet(w http.ResponseWriter, req *http.Request) {
	dgst := mux.Vars(req)["digest"]
	meta, ok := r.store.GetBlob(dgst)
	if !ok {
		// The client pushed a blob we never saw in a manifest — we have no size
		// and therefore cannot construct the ByteStream resource name.
		klog.Warningf("registry: blob GET %s: size unknown (was the manifest pushed first?)", dgst)
		ociError(w, "BLOB_UNKNOWN", "blob size unknown; push the manifest before pushing blobs", http.StatusNotFound)
		return
	}
	hash := hashFromDigest(dgst)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", meta.Size))
	w.Header().Set("Docker-Content-Digest", dgst)
	w.Header().Set("Content-Type", "application/octet-stream")
	if err := r.cas.ReadBlob(req.Context(), hash, meta.Size, w); err != nil {
		// Headers are already written; log and let the client detect truncation.
		klog.Errorf("registry: blob GET %s: CAS read error: %v", dgst, err)
	}
}

// --- Uploads (rejected) ---
//
// This is a read-through registry: blobs are served from CAS and are never
// stored locally. Blob HEAD always returns 200 so clients should skip uploads
// entirely. If an upload is attempted anyway, we reject it explicitly rather
// than silently discarding data.

func (r *Registry) handleUploadStart(w http.ResponseWriter, req *http.Request) {
	klog.Warningf("registry: rejected blob upload attempt for %s (this is a read-through registry)", mux.Vars(req)["name"])
	ociError(w, "UNSUPPORTED", "this is a read-through registry backed by CAS; blob uploads are not accepted", http.StatusMethodNotAllowed)
}

func (r *Registry) handleUploadChunk(w http.ResponseWriter, _ *http.Request) {
	ociError(w, "UNSUPPORTED", "this is a read-through registry backed by CAS; blob uploads are not accepted", http.StatusMethodNotAllowed)
}

func (r *Registry) handleUploadComplete(w http.ResponseWriter, _ *http.Request) {
	ociError(w, "UNSUPPORTED", "this is a read-through registry backed by CAS; blob uploads are not accepted", http.StatusMethodNotAllowed)
}

func (r *Registry) handleUploadDelete(w http.ResponseWriter, _ *http.Request) {
	ociError(w, "UNSUPPORTED", "this is a read-through registry backed by CAS; blob uploads are not accepted", http.StatusMethodNotAllowed)
}

// --- Manifests ---

func (r *Registry) handleManifestHead(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	m, ok := r.store.GetManifest(vars["name"], vars["reference"])
	if !ok {
		ociError(w, "MANIFEST_UNKNOWN", "manifest unknown", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", m.ContentType)
	w.Header().Set("Docker-Content-Digest", m.Digest)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(m.Content)))
	w.WriteHeader(http.StatusOK)
}

func (r *Registry) handleManifestGet(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	m, ok := r.store.GetManifest(vars["name"], vars["reference"])
	if !ok {
		ociError(w, "MANIFEST_UNKNOWN", "manifest unknown", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", m.ContentType)
	w.Header().Set("Docker-Content-Digest", m.Digest)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(m.Content)))
	w.Write(m.Content)
}

// handleManifestPut stores the manifest and registers all blob sizes from its
// descriptors. Before accepting the manifest it verifies — via a single
// FindMissingBlobs RPC — that every referenced blob is actually present in CAS.
// This turns a silent pull-time failure (truncated blob stream) into an explicit
// push-time error, at the cost of one batched RPC per manifest push.
func (r *Registry) handleManifestPut(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	name, ref := vars["name"], vars["reference"]

	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusInternalServerError)
		return
	}

	blobs := parseManifestBlobs(body)
	if len(blobs) > 0 {
		missing, err := r.cas.FindMissingBlobs(req.Context(), blobs)
		if err != nil {
			klog.Errorf("registry: manifest %s:%s FindMissingBlobs error: %v", name, ref, err)
			ociError(w, "BLOB_UNKNOWN", fmt.Sprintf("CAS verification failed: %v", err), http.StatusInternalServerError)
			return
		}
		if len(missing) > 0 {
			digests := make([]string, len(missing))
			for i, b := range missing {
				digests[i] = fmt.Sprintf("sha256:%s (%d bytes)", b.Hash, b.Size)
			}
			klog.Errorf("registry: manifest %s:%s rejected — %d blob(s) missing from CAS: %v", name, ref, len(missing), digests)
			ociError(w, "BLOB_UNKNOWN",
				fmt.Sprintf("%d blob(s) referenced by this manifest are not present in CAS: %v", len(missing), digests),
				http.StatusUnprocessableEntity)
			return
		}
	}

	r.store.RegisterManifestBlobs(body)

	contentType := req.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/vnd.oci.image.manifest.v1+json"
	}
	m := NewManifest(body, contentType)
	r.store.PutManifest(name, ref, m)
	klog.Infof("registry: stored manifest %s:%s (%s, %d blobs verified in CAS)", name, ref, m.Digest, len(blobs))

	w.Header().Set("Location", fmt.Sprintf("/v2/%s/manifests/%s", name, m.Digest))
	w.Header().Set("Docker-Content-Digest", m.Digest)
	w.WriteHeader(http.StatusCreated)
}

// --- Helpers ---

type ociErrorBody struct {
	Errors []ociErrorEntry `json:"errors"`
}

type ociErrorEntry struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail"`
}

func ociError(w http.ResponseWriter, code, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(ociErrorBody{
		Errors: []ociErrorEntry{{Code: code, Message: message}},
	})
}
