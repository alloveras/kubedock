package image

import (
	"context"
	"fmt"

	"github.com/containers/image/v5/image"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	godigest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// InspectConfig returns the config digest and OCI image config for the named image.
// The config digest (sha256 of the raw config blob) is the Docker/OCI image ID as
// returned by docker inspect.
func InspectConfig(name string) (godigest.Digest, *v1.Image, error) {
	sys := &types.SystemContext{
		OSChoice: "linux",
	}

	ctx := context.Background()
	src, err := parseImageSource(ctx, sys, name)
	if err != nil {
		return "", nil, err
	}
	defer src.Close()

	img, err := image.FromUnparsedImage(ctx, sys, image.UnparsedInstance(src, nil))
	if err != nil {
		return "", nil, fmt.Errorf("Error parsing manifest for image: %w", err)
	}

	configDigest := img.ConfigInfo().Digest

	config, err := img.OCIConfig(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("Error reading OCI-formatted configuration data: %w", err)
	}
	return configDigest, config, nil
}

// parseImageSource converts image URL-like string to an ImageSource.
// The caller must call .Close() on the returned ImageSource.
func parseImageSource(ctx context.Context, sys *types.SystemContext, name string) (types.ImageSource, error) {
	ref, err := alltransports.ParseImageName(name)
	if err != nil {
		return nil, err
	}
	return ref.NewImageSource(ctx, sys)
}
