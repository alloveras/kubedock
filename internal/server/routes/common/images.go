package common

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"

	"github.com/distribution/reference"
	"github.com/gin-gonic/gin"
	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"k8s.io/klog"

	"github.com/joyrex2001/kubedock/internal/config"
	"github.com/joyrex2001/kubedock/internal/model/types"
	"github.com/joyrex2001/kubedock/internal/server/httputil"
)

// NormalizeImageRef returns the canonical form of an image reference, matching
// how Docker itself normalizes names before storing them locally.
//
// For bazel/ images when --registry-addr is configured, the CAS registry hostname
// is prepended (e.g. "bazel/foo/bar:tag" → "registryAddr/bazel/foo/bar:tag").
// This must be handled before Docker normalization because "bazel" has no dots or
// colons and would otherwise be misinterpreted as a Docker Hub path component.
//
// For cst.oci.local/ images, the fake registry hostname is replaced with the real
// CAS registry address and the tag is converted to a digest reference. CST uses
// "cst.oci.local/sha256-<hex>:sha256-<hex>" as a synthetic tag where both the
// path component and the tag encode the manifest digest (with "-" instead of ":").
// The manifest is pushed to registryAddr under the same path, so this transform
// produces the exact digest-pinned URL that Kubernetes can pull:
//
//	cst.oci.local/sha256-<hex>:sha256-<hex> → registryAddr/cst.oci.local/sha256-<hex>@sha256:<hex>
//
// For all other images, Docker Hub normalization is applied via the distribution
// reference package (the same library Docker itself uses):
//   - missing registry defaults to docker.io
//   - single-component names get the library/ prefix (e.g. "postgres" → "docker.io/library/postgres")
//   - missing tag defaults to :latest
//
// If the name cannot be parsed it is returned unchanged.
func NormalizeImageRef(name, registryAddr string) string {
	if strings.HasPrefix(name, "bazel/") {
		if registryAddr != "" {
			return registryAddr + "/" + name
		}
		return name
	}
	if strings.HasPrefix(name, "cst.oci.local/") && registryAddr != "" {
		rest := strings.TrimPrefix(name, "cst.oci.local/")
		if i := strings.LastIndex(rest, ":"); i >= 0 {
			repo := rest[:i]
			digest := strings.Replace(rest[i+1:], "-", ":", 1)
			return fmt.Sprintf("%s/cst.oci.local/%s@%s", registryAddr, repo, digest)
		}
	}
	ref, err := reference.ParseNormalizedNamed(name)
	if err != nil {
		return name
	}
	return reference.TagNameOnly(ref).String()
}

// ImageList - list Images. Stubbed, not relevant on k8s.
// https://docs.docker.com/engine/api/v1.41/#operation/ImageList
// https://docs.podman.io/en/latest/_static/api.html?version=v4.2#tag/images/operation/ImageListLibpod
// GET "/images/json"
// GET "/libpod/images/json"
func ImageList(cr *ContextRouter, c *gin.Context) {
	imgs, err := cr.DB.GetImages()
	if err != nil {
		httputil.Error(c, http.StatusInternalServerError, err)
		return
	}
	res := []gin.H{}
	for _, img := range imgs {
		name := img.Name
		if !strings.Contains(name, ":") {
			name = name + ":latest"
		}
		res = append(res, gin.H{"ID": img.ID, "Size": 0, "Created": img.Created.Unix(), "RepoTags": []string{name}})
	}
	c.JSON(http.StatusOK, res)
}

// ImageLoad - load a container image tar, push its manifest to the configured
// CAS registry, and register the image in the in-memory store so subsequent
// docker run calls resolve the correct registry reference for the pod spec.
//
// Supported input formats:
//   - OCI image layout (index.json + blobs/)
//   - Docker archive (manifest.json + config blob + layer blobs)
//
// https://docs.docker.com/engine/api/v1.41/#operation/ImageLoad
// POST "/images/load"
// POST "/libpod/images/load"
func ImageLoad(cr *ContextRouter, c *gin.Context) {
	if cr.Config.RegistryAddr == "" {
		httputil.Error(c, http.StatusNotImplemented, fmt.Errorf("image load requires --registry-addr to be configured"))
		return
	}

	result, err := streamManifest(c.Request.Body)
	if err != nil {
		httputil.Error(c, http.StatusBadRequest, err)
		return
	}

	imageTag := result.desc.Annotations[ocispec.AnnotationRefName]
	if imageTag == "" {
		imageTag = result.desc.Digest.String()
	}
	// repository is derived from the original tag and used only for the registry
	// push URL — it must not include the registry hostname prefix.
	repository := imageTagToRepository(imageTag)

	mediaType := string(result.desc.MediaType)
	if mediaType == "" {
		mediaType = ocispec.MediaTypeImageManifest
	}

	manifestDigest := result.desc.Digest.String()
	if err := pushManifest(cr.Config.RegistryAddr, repository, manifestDigest, result.manifestBytes, mediaType); err != nil {
		httputil.Error(c, http.StatusInternalServerError, fmt.Errorf("pushing manifest to registry: %w", err))
		return
	}

	registryRef := fmt.Sprintf("%s/%s@%s", cr.Config.RegistryAddr, repository, manifestDigest)
	img := &types.Image{
		ID:          result.configDigest.String(),
		ShortID:     result.configDigest.Hex()[:12],
		Name:        NormalizeImageRef(imageTag, cr.Config.RegistryAddr),
		RegistryRef: registryRef,
		Config:      result.imageConfig,
	}
	if err := cr.DB.SaveImage(img); err != nil {
		httputil.Error(c, http.StatusInternalServerError, err)
		return
	}

	klog.Infof("loaded image %s (id=%s) → %s", imageTag, img.ID, registryRef)
	c.Header("Content-Type", "application/json")
	c.Status(http.StatusOK)
	_ = json.NewEncoder(c.Writer).Encode(gin.H{"stream": fmt.Sprintf("Loaded image: %s\n", imageTag)})
}

// dockerLayerInfo holds the content-addressed identity of a Docker archive layer blob,
// computed by streaming the blob through a sha256 hash writer without buffering it.
type dockerLayerInfo struct {
	digest godigest.Digest
	size   int64
}

// imageManifestResult holds the output of streamManifest: the serialised OCI manifest,
// its descriptor (digest, size, media type, annotations), and the config digest which
// is the Docker/OCI-spec image ID (sha256 of the image config blob).
type imageManifestResult struct {
	manifestBytes []byte
	desc          ocispec.Descriptor
	// configDigest is sha256(<image_config_json>) and is the canonical Docker image
	// ID returned by `docker inspect`. It is always set when streamManifest succeeds.
	configDigest godigest.Digest
	// imageConfig is the full OCI image config parsed from the config blob.
	imageConfig ocispec.ImageConfig
}

// streamManifest streams through a tar in a single pass and returns the OCI manifest
// bytes, descriptor, and config digest needed to push to the CAS registry.
//
// Supported formats:
//
//   - OCI image layout: contains index.json + blobs/sha256/<hash> entries.
//     Manifest blobs (JSON starting with '{' that pass isOCIManifest) are buffered;
//     all other blobs are discarded via io.Discard.
//
//   - Docker archive (docker save format): contains manifest.json + config blob +
//     layer blobs. JSON entries are buffered (small); binary entries are streamed
//     through a sha256 hash to compute their digest+size without buffering data.
//     An equivalent OCI manifest is constructed from the gathered metadata.
func streamManifest(r io.Reader) (imageManifestResult, error) {
	var index ocispec.Index
	var indexFound bool
	// OCI layout: JSON blobs from blobs/ that pass isOCIManifest.
	ociBlobs := map[string][]byte{}

	// Docker archive: manifest.json content.
	var dockerManifestJSON []byte
	// Docker archive: all other JSON blobs (config, etc.) keyed by tar entry name.
	dockerJsonBlobs := map[string][]byte{}
	// Docker archive: sha256 + size of binary blobs (layers), keyed by tar entry name.
	dockerLayerInfos := map[string]dockerLayerInfo{}
	// Docker archive: names of 0-byte layer entries — sent by incremental loaders as
	// probes to detect which layers are already present in the daemon's local cache.
	// kubedock has no local layer cache (all blobs live in CAS), so these are always
	// treated as missing and trigger a full reload from the client.
	emptyLayerNames := map[string]bool{}

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return imageManifestResult{}, fmt.Errorf("reading tar: %w", err)
		}
		name := strings.TrimPrefix(hdr.Name, "./")

		switch {
		case name == "index.json":
			if err := json.NewDecoder(tr).Decode(&index); err != nil {
				return imageManifestResult{}, fmt.Errorf("parsing index.json: %w", err)
			}
			indexFound = true

		case name == "manifest.json":
			// Docker archive manifest (array of {Config, RepoTags, Layers}).
			data, err := io.ReadAll(tr)
			if err != nil {
				return imageManifestResult{}, fmt.Errorf("reading manifest.json: %w", err)
			}
			dockerManifestJSON = data

		case strings.HasPrefix(name, "blobs/"):
			// OCI layout blob: peek at first byte. Binary layer blobs (gzip, tar)
			// never start with '{'; manifest and config blobs always do.
			var first [1]byte
			n, err := tr.Read(first[:])
			if err != nil && err != io.EOF {
				return imageManifestResult{}, fmt.Errorf("reading blob %s: %w", name, err)
			}
			if n == 1 && first[0] == '{' {
				rest, err := io.ReadAll(tr)
				if err != nil {
					return imageManifestResult{}, fmt.Errorf("reading blob %s: %w", name, err)
				}
				// Buffer all JSON blobs: both manifests and config blobs start with '{'.
				// Config blobs are small and needed for extracting the User field.
				ociBlobs[name] = append(first[:1], rest...)
			} else {
				if _, err := io.Copy(io.Discard, tr); err != nil {
					return imageManifestResult{}, fmt.Errorf("discarding %s: %w", name, err)
				}
			}

		default:
			// Could be a Docker archive entry (layer blob, config blob, oci-layout, etc.).
			// Peek at the first byte to decide: JSON → buffer (small); binary → stream+hash.
			var first [1]byte
			n, err := tr.Read(first[:])
			if err != nil && err != io.EOF {
				return imageManifestResult{}, fmt.Errorf("reading %s: %w", name, err)
			}
			if n == 0 {
				// 0-byte entry: record it so buildOCIManifestFromDockerArchive can
				// distinguish "layer omitted as an incremental probe" from "layer
				// genuinely absent from the archive".
				emptyLayerNames[name] = true
				continue
			}
			if first[0] == '{' {
				// JSON blob (Docker config, oci-layout, repositories, etc.) — buffer fully.
				rest, err := io.ReadAll(tr)
				if err != nil {
					return imageManifestResult{}, fmt.Errorf("reading %s: %w", name, err)
				}
				dockerJsonBlobs[name] = append(first[:1], rest...)
			} else {
				// Binary blob (Docker layer). Stream through sha256 without buffering.
				h := godigest.SHA256.Hash()
				h.Write(first[:1])
				written, err := io.Copy(h, tr)
				if err != nil {
					return imageManifestResult{}, fmt.Errorf("hashing %s: %w", name, err)
				}
				dockerLayerInfos[name] = dockerLayerInfo{
					digest: godigest.NewDigest(godigest.SHA256, h),
					size:   1 + written,
				}
			}
		}
	}

	switch {
	case indexFound:
		return extractOCIManifest(index, ociBlobs)
	case dockerManifestJSON != nil:
		return buildOCIManifestFromDockerArchive(dockerManifestJSON, dockerJsonBlobs, dockerLayerInfos, emptyLayerNames)
	default:
		return imageManifestResult{}, fmt.Errorf(
			"tar contains neither index.json (OCI image layout) nor manifest.json (Docker archive)")
	}
}

// extractOCIManifest resolves the per-platform image manifest from an OCI index
// and returns its bytes, descriptor, and config digest. Multi-platform image indexes
// are drilled into to find the matching single-platform manifest; pushing only the
// index would fail at pull time because containerd requests per-platform manifests
// via the /manifests/ endpoint, not the /blobs/ endpoint.
func extractOCIManifest(index ocispec.Index, ociBlobs map[string][]byte) (imageManifestResult, error) {
	desc, err := pickManifest(index)
	if err != nil {
		return imageManifestResult{}, err
	}

	blobPath := "blobs/" + strings.ReplaceAll(desc.Digest.String(), ":", "/")
	manifestBytes, ok := ociBlobs[blobPath]
	if !ok {
		return imageManifestResult{}, fmt.Errorf("manifest blob %s not found in tar", desc.Digest)
	}

	if desc.MediaType == ocispec.MediaTypeImageIndex {
		var imageIndex ocispec.Index
		if err := json.Unmarshal(manifestBytes, &imageIndex); err != nil {
			return imageManifestResult{}, fmt.Errorf("parsing image index %s: %w", desc.Digest, err)
		}
		platformDesc, err := pickManifest(imageIndex)
		if err != nil {
			return imageManifestResult{}, fmt.Errorf("selecting platform manifest from image index: %w", err)
		}
		platformBlobPath := "blobs/" + strings.ReplaceAll(platformDesc.Digest.String(), ":", "/")
		manifestBytes, ok = ociBlobs[platformBlobPath]
		if !ok {
			return imageManifestResult{}, fmt.Errorf("platform manifest blob %s not found in tar", platformDesc.Digest)
		}
		desc = platformDesc
	}

	// Extract the config digest directly from the manifest rather than re-parsing
	// later. config.digest IS the Docker image ID per the OCI image spec.
	var mfst struct {
		Config struct {
			Digest godigest.Digest `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(manifestBytes, &mfst); err != nil {
		return imageManifestResult{}, fmt.Errorf("extracting config digest from manifest: %w", err)
	}

	// Extract the full image config from the config blob (buffered alongside manifests).
	var imageConfig ocispec.ImageConfig
	configBlobPath := "blobs/" + strings.ReplaceAll(mfst.Config.Digest.String(), ":", "/")
	if configData, ok := ociBlobs[configBlobPath]; ok {
		var imgCfgOuter struct {
			Config ocispec.ImageConfig `json:"config"`
		}
		if err := json.Unmarshal(configData, &imgCfgOuter); err == nil {
			imageConfig = imgCfgOuter.Config
		}
	}

	return imageManifestResult{
		manifestBytes: manifestBytes,
		desc:          desc,
		configDigest:  mfst.Config.Digest,
		imageConfig:   imageConfig,
	}, nil
}

// buildOCIManifestFromDockerArchive constructs an OCI image manifest from a
// Docker archive (docker save format). It uses the gathered JSON blobs (config)
// and layer hashes that were computed by streaming through the tar without
// buffering any blob content. The resulting manifest can be pushed directly to
// the CAS registry; the registry verifies that the referenced blobs are present
// in CAS via FindMissingBlobs.
func buildOCIManifestFromDockerArchive(
	manifestJSON []byte,
	jsonBlobs map[string][]byte,
	layerInfos map[string]dockerLayerInfo,
	emptyLayerNames map[string]bool,
) (imageManifestResult, error) {
	// Docker manifest.json is an array; only a single image per load is supported.
	var dockerMfsts []struct {
		Config   string   `json:"Config"`
		RepoTags []string `json:"RepoTags"`
		Layers   []string `json:"Layers"`
	}
	if err := json.Unmarshal(manifestJSON, &dockerMfsts); err != nil {
		return imageManifestResult{}, fmt.Errorf("parsing manifest.json: %w", err)
	}
	if len(dockerMfsts) != 1 {
		return imageManifestResult{}, fmt.Errorf(
			"manifest.json: expected 1 image entry, got %d", len(dockerMfsts))
	}
	dm := dockerMfsts[0]

	imageTag := ""
	if len(dm.RepoTags) > 0 {
		imageTag = dm.RepoTags[0]
	}

	// Config blob: must have been buffered as a JSON entry.
	configName := strings.TrimPrefix(dm.Config, "./")
	configData, ok := jsonBlobs[configName]
	if !ok {
		return imageManifestResult{}, fmt.Errorf(
			"docker archive: config blob %q not found in tar", configName)
	}
	// configDigest is sha256(image_config_json) — the Docker/OCI-spec image ID.
	configDigest := godigest.FromBytes(configData)

	// Parse config once for incremental-probe handling and full image config extraction.
	// rootfs.diff_ids[i] is the sha256 of the uncompressed content of layer i —
	// the exact digest format that StreamedOCIImage's DIGEST_PATTERN expects.
	var imgCfg struct {
		Config ocispec.ImageConfig `json:"config"`
		RootFS struct {
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	_ = json.Unmarshal(configData, &imgCfg)

	// Layer descriptors: digest+size were computed by streaming through each blob.
	layers := make([]ocispec.Descriptor, 0, len(dm.Layers))
	for i, rawPath := range dm.Layers {
		layerName := strings.TrimPrefix(rawPath, "./")

		var layerDigest godigest.Digest
		var layerSize int64

		if info, ok := layerInfos[layerName]; ok {
			layerDigest = info.digest
			layerSize = info.size
		} else if data, ok := jsonBlobs[layerName]; ok {
			// Unusual: a layer whose content is JSON (e.g. a single-file JSON layer).
			layerDigest = godigest.FromBytes(data)
			layerSize = int64(len(data))
		} else if emptyLayerNames[layerName] {
			// 0-byte probe entry from an incremental loader (e.g. StreamedOCIImage
			// with useEmptyLayers=true). kubedock has no local layer cache — all
			// blobs live in CAS — so we always treat these as missing.
			//
			// Return an error in the format DIGEST_PATTERN expects:
			//   expected sha256:<diff_id>
			// This causes MissingLayerException on the Java side, which resolves to
			// layer index i and triggers a full reload starting from layer 0.
			if i < len(imgCfg.RootFS.DiffIDs) {
				return imageManifestResult{}, fmt.Errorf("expected %s", imgCfg.RootFS.DiffIDs[i])
			}
			return imageManifestResult{}, fmt.Errorf(
				"docker archive: layer %q was an empty probe but config has no diff_id at index %d", layerName, i)
		} else {
			return imageManifestResult{}, fmt.Errorf(
				"docker archive: layer blob %q not found in tar", layerName)
		}

		// Infer OCI media type from the layer file extension.
		mediaType := ocispec.MediaTypeImageLayerGzip // default: .tar.gz
		switch {
		case strings.HasSuffix(layerName, ".tar.zst"), strings.HasSuffix(layerName, ".zst"):
			mediaType = ocispec.MediaTypeImageLayerZstd
		case strings.HasSuffix(layerName, ".tar"):
			mediaType = ocispec.MediaTypeImageLayer
		}

		layers = append(layers, ocispec.Descriptor{
			MediaType: mediaType,
			Digest:    layerDigest,
			Size:      layerSize,
		})
	}

	// Construct a minimal OCI image manifest referencing the config and layers.
	type ociManifest struct {
		SchemaVersion int                  `json:"schemaVersion"`
		MediaType     string               `json:"mediaType"`
		Config        ocispec.Descriptor   `json:"config"`
		Layers        []ocispec.Descriptor `json:"layers"`
	}
	m := ociManifest{
		SchemaVersion: 2,
		MediaType:     ocispec.MediaTypeImageManifest,
		Config: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageConfig,
			Digest:    configDigest,
			Size:      int64(len(configData)),
		},
		Layers: layers,
	}
	manifestBytes, err := json.Marshal(m)
	if err != nil {
		return imageManifestResult{}, fmt.Errorf("marshaling OCI manifest: %w", err)
	}

	manifestDigest := godigest.FromBytes(manifestBytes)

	annotations := map[string]string{}
	if imageTag != "" {
		annotations[ocispec.AnnotationRefName] = imageTag
	}
	desc := ocispec.Descriptor{
		MediaType:   ocispec.MediaTypeImageManifest,
		Digest:      manifestDigest,
		Size:        int64(len(manifestBytes)),
		Annotations: annotations,
	}

	return imageManifestResult{
		manifestBytes: manifestBytes,
		desc:          desc,
		configDigest:  configDigest,
		imageConfig:   imgCfg.Config,
	}, nil
}

// isOCIManifest reports whether data is an OCI image manifest or image index.
// It requires schemaVersion: 2 and checks mediaType against known manifest
// media types. When mediaType is absent (permitted by the spec), it falls back
// to checking for the presence of layers (image manifest) or manifests (image
// index). Config blobs are plain JSON objects with neither field and no
// recognized mediaType, so they are correctly excluded.
func isOCIManifest(data []byte) bool {
	var m struct {
		SchemaVersion int               `json:"schemaVersion"`
		MediaType     string            `json:"mediaType"`
		Layers        []json.RawMessage `json:"layers"`
		Manifests     []json.RawMessage `json:"manifests"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return false
	}
	if m.SchemaVersion != 2 {
		return false
	}
	switch m.MediaType {
	case ocispec.MediaTypeImageManifest, ocispec.MediaTypeImageIndex:
		return true
	default:
		// mediaType is optional in the OCI spec; fall back to field presence.
		return m.Layers != nil || m.Manifests != nil
	}
}

// pickManifest selects the manifest descriptor from an OCI index that matches
// the current runtime platform, falling back to the first entry if none matches.
func pickManifest(index ocispec.Index) (ocispec.Descriptor, error) {
	if len(index.Manifests) == 0 {
		return ocispec.Descriptor{}, fmt.Errorf("index.json contains no manifests")
	}
	for _, m := range index.Manifests {
		if m.Platform != nil && m.Platform.OS == runtime.GOOS && m.Platform.Architecture == runtime.GOARCH {
			return m, nil
		}
	}
	return index.Manifests[0], nil
}

// imageTagToRepository strips the tag suffix from an image reference to get
// the repository name (e.g. "bazel/foo/bar:target" → "bazel/foo/bar").
func imageTagToRepository(tag string) string {
	if i := strings.Index(tag, "@"); i >= 0 {
		tag = tag[:i]
	}
	if i := strings.LastIndex(tag, ":"); i >= 0 {
		tag = tag[:i]
	}
	return tag
}

// pushManifest PUTs a manifest to the OCI distribution-spec endpoint on the
// configured CAS registry. Returns an error if the registry responds with
// anything other than 201 Created.
func pushManifest(registryAddr, repository, digest string, body []byte, mediaType string) error {
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", registryAddr, repository, digest)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mediaType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("registry returned HTTP %d: %s", resp.StatusCode, string(msg))
	}
	return nil
}

// ImageJSON - return low-level information about an image.
// https://docs.docker.com/engine/api/v1.41/#operation/ImageInspect
// GET "/images/:image/json"
func ImageJSON(cr *ContextRouter, c *gin.Context) {
	id := strings.TrimSuffix(c.Param("image")+c.Param("json"), "/json")

	// Normalize before lookup so names are always compared in canonical form,
	// matching what ImageLoad and ImageCreate store.
	normalizedID := NormalizeImageRef(id, cr.Config.RegistryAddr)

	img, err := cr.DB.GetImageByNameOrID(normalizedID)
	if err != nil {
		if cr.Config.Inspector {
			// Inspector mode: fetch real metadata from the upstream registry and
			// cache it so containers/create can populate exposed ports without a
			// second round-trip.
			img = &types.Image{Name: normalizedID}
			digest, cfg, err := cr.Backend.InspectImage(id)
			if err != nil {
				httputil.Error(c, http.StatusInternalServerError, err)
				return
			}
			img.ID = digest.String()
			img.ShortID = digest.Hex()[:12]
			img.Config = cfg
			if err := cr.DB.SaveImage(img); err != nil {
				httputil.Error(c, http.StatusInternalServerError, err)
				return
			}
		} else {
			// Image not present. Return 404 like a real Docker daemon would for an
			// image that has not been pulled yet. Clients are expected to call
			// POST /images/load (for bazel images) or POST /images/create (for
			// registry images) before creating containers.
			httputil.Error(c, http.StatusNotFound, fmt.Errorf("no such image: %s", id))
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"Id":           img.ID,
		"Architecture": config.GOARCH,
		"Created":      img.Created.Format("2006-01-02T15:04:05Z"),
		"Size":         0,
		"ContainerConfig": gin.H{
			"Image": img.Name,
		},
		"Config": img.Config,
	})
}
