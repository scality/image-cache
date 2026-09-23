// Package puller pulls cache images and exposes their content.
package puller

import (
	"context"
	"fmt"
	"io"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
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

// Puller resolves an image reference and returns the entries of its layers.
type Puller interface {
	// Pull returns the entries of the image's layers, in layer order and as
	// the layers carry them, as one tar stream, and the resolved image digest.
	// Nothing is filtered out on the way, see entries. The caller closes the
	// stream.
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
	return entries(img), digest.String(), nil
}

// Tarball reads an image out of a docker archive on the local filesystem,
// which is the form the ISO ships for a node that has no registry to reach.
type Tarball struct{}

// Pull implements Puller.
func (Tarball) Pull(ctx context.Context, ref string) (io.ReadCloser, string, error) {
	// Nothing below reaches the network, so the context has no call to carry
	// it into. Honouring cancellation here keeps the two implementations
	// interchangeable for a caller that gave up before this one started.
	if err := ctx.Err(); err != nil {
		return nil, "", errors.Wrap(ErrPull, errors.CausedBy(err),
			errors.WithProperty("source", ref))
	}
	// A nil tag demands the archive hold exactly one image, which is what a
	// boot cache image is. An archive carrying several is refused whole
	// rather than extracted down an arbitrary branch.
	img, err := tarball.ImageFromPath(ref, nil)
	if err != nil {
		return nil, "", errors.Wrap(ErrPull, errors.CausedBy(err),
			errors.WithProperty("source", ref))
	}
	// Remote pins the platform when it resolves the reference; an archive
	// carries whatever it was saved from. Checking here keeps the two
	// implementations of this interface interchangeable, which is what the
	// caller relies on when it picks one by the shape of the source. Without
	// it, an image saved on an arm64 machine fills the cache of an x86_64
	// node, the sentinel says the resource is complete, and nothing further
	// down looks at the architecture again.
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, "", errors.Wrap(ErrPull, errors.CausedBy(err),
			errors.WithDetail("reading the image configuration"),
			errors.WithProperty("source", ref))
	}
	// Only a declared mismatch is refused. A carrier image holds nothing but
	// tarballs, and the tool that builds one may record no platform at all,
	// which contradicts nothing.
	if (cfg.OS != "" && cfg.OS != platform.OS) ||
		(cfg.Architecture != "" && cfg.Architecture != platform.Architecture) {
		return nil, "", errors.Wrap(ErrPull,
			errors.WithDetail(fmt.Sprintf("the archive carries a %s/%s image, and the cache is filled for %s/%s",
				cfg.OS, cfg.Architecture, platform.OS, platform.Architecture)),
			errors.WithProperty("source", ref))
	}

	digest, err := img.Digest()
	if err != nil {
		return nil, "", errors.Wrap(ErrPull, errors.CausedBy(err),
			errors.WithDetail("resolving the digest"), errors.WithProperty("source", ref))
	}
	return entries(img), digest.String(), nil
}
