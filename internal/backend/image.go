package backend

import (
	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/joyrex2001/kubedock/internal/util/image"
)

// InspectImage fetches the image config from the registry and returns the
// config digest (Docker/OCI image ID) and the full OCI image config.
func (in *instance) InspectImage(img string) (godigest.Digest, ocispec.ImageConfig, error) {
	digest, cfg, err := image.InspectConfig("docker://" + img)
	if err != nil {
		return "", ocispec.ImageConfig{}, err
	}
	return digest, cfg.Config, nil
}
