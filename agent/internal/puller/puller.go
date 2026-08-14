// Package puller pulls cache images and exposes their content.
package puller

import (
	"context"
	"io"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/scality/go-errors"
)

// Failures of this package are classified by these sentinels: a malformed
// reference is a spec mistake, a failed pull is an environment problem.
// Registry errors are stamped where they enter, because a foreign error
// wrapped cause-first would come out titled "unknown error".
var (
	// ErrReference covers a source that is not a usable image reference.
	ErrReference = errors.New("invalid image reference")
	// ErrPull covers reaching the registry and reading the image.
	ErrPull = errors.New("pulling the image failed")
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
		return nil, "", errors.Wrap(ErrReference, errors.CausedBy(err),
			errors.WithProperty("source", ref))
	}
	img, err := remote.Image(parsed, remote.WithContext(ctx), remote.WithPlatform(platform))
	if err != nil {
		return nil, "", errors.Wrap(ErrPull, errors.CausedBy(err),
			errors.WithProperty("source", ref))
	}
	digest, err := img.Digest()
	if err != nil {
		return nil, "", errors.Wrap(ErrPull, errors.CausedBy(err),
			errors.WithDetail("resolving the digest"), errors.WithProperty("source", ref))
	}
	return mutate.Extract(img), digest.String(), nil
}
