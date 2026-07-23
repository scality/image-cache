// Package puller pulls cache images and exposes their content.
package puller

import (
	"context"
	"fmt"
	"io"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Puller resolves an image reference and returns its flattened filesystem.
type Puller interface {
	// Pull returns the image's flattened filesystem as a tar stream and the
	// resolved image digest. The caller closes the stream.
	Pull(ctx context.Context, ref string) (io.ReadCloser, string, error)
}

// Remote pulls linux/amd64 images from an OCI registry.
type Remote struct{}

var platform = v1.Platform{OS: "linux", Architecture: "amd64"}

// Pull implements Puller.
func (Remote) Pull(ctx context.Context, ref string) (io.ReadCloser, string, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return nil, "", fmt.Errorf("parsing image reference %q: %w", ref, err)
	}
	img, err := remote.Image(parsed, remote.WithContext(ctx), remote.WithPlatform(platform))
	if err != nil {
		return nil, "", fmt.Errorf("pulling %q: %w", ref, err)
	}
	digest, err := img.Digest()
	if err != nil {
		return nil, "", fmt.Errorf("resolving digest of %q: %w", ref, err)
	}
	return mutate.Extract(img), digest.String(), nil
}
