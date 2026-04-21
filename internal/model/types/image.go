package types

import (
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Image describes the details of an image.
//
// Name is the normalised client-facing tag used as the DB lookup key.
// RegistryRef, when non-empty, is the digest-pinned CAS URL that Kubernetes
// should use in the pod spec. It is set by ImageLoad (docker load) where the
// client tag does not encode the digest and therefore cannot be pulled by tag.
// ImageCreate (docker pull) leaves it empty because the registry already serves
// the image under the normalised tag name.
//
// Config holds the OCI image config (user, env, cmd, exposed ports, etc.)
// parsed from the image config blob. It is populated by ImageLoad and by
// ImageCreate/ImagePull when inspector mode is enabled. It is returned
// verbatim from the ImageJSON endpoint so callers always see accurate metadata.
type Image struct {
	ID          string // sha256 of the image config blob (Docker/OCI image ID)
	ShortID     string // first 12 hex chars of ID, without algorithm prefix
	Name        string
	RegistryRef string
	Config      ocispec.ImageConfig
	Created     time.Time
}
