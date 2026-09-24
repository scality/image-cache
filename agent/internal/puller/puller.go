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

// Image identifies what a source resolved to.
type Image struct {
	// Digest is the manifest's digest: how the source serves the image. It
	// depends on how the image is stored, so the same image reached through
	// a registry and through a docker archive carries two of them.
	Digest string
	// Config is the digest of the image's configuration: what the image is.
	// It is the same through a registry and through a docker archive, and
	// through any registry endpoint, which is what makes it the one to
	// compare two sources by.
	Config string
}

// Puller resolves an image reference and returns the entries of its layers.
type Puller interface {
	// Pull returns the entries of the image's layers, in layer order and as
	// the layers carry them, as one tar stream, and what the source resolved
	// to. Nothing is filtered out on the way, see entries. The caller closes
	// the stream.
	Pull(ctx context.Context, ref string) (io.ReadCloser, Image, error)
	// Resolve returns what the source resolves to without reading its
	// layers: for a registry, the manifest and nothing else.
	Resolve(ctx context.Context, ref string) (Image, error)
}

// Remote pulls linux/amd64 images from an OCI registry.
type Remote struct{}

var platform = v1.Platform{OS: "linux", Architecture: "amd64"}

// Pull implements Puller.
func (r Remote) Pull(ctx context.Context, ref string) (io.ReadCloser, Image, error) {
	img, id, err := r.open(ctx, ref)
	if err != nil {
		return nil, Image{}, err
	}
	return entries(img), id, nil
}

// Resolve implements Puller.
func (r Remote) Resolve(ctx context.Context, ref string) (Image, error) {
	_, id, err := r.open(ctx, ref)
	return id, err
}

// open resolves ref to its linux/amd64 image. It reads the manifest, never a
// layer: those are fetched as the stream is read.
func (Remote) open(ctx context.Context, ref string) (v1.Image, Image, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return nil, Image{}, errors.Wrap(ErrReference, errors.CausedBy(err),
			errors.WithProperty("source", ref))
	}
	img, err := remote.Image(parsed, remote.WithContext(ctx), remote.WithPlatform(platform))
	if err != nil {
		return nil, Image{}, errors.Wrap(ErrPull, errors.CausedBy(err),
			errors.WithProperty("source", ref))
	}
	id, err := identify(img, ref)
	return img, id, err
}

// Tarball reads an image out of a docker archive on the local filesystem,
// which is the form the ISO ships for a node that has no registry to reach.
type Tarball struct{}

// Pull implements Puller.
func (t Tarball) Pull(ctx context.Context, ref string) (io.ReadCloser, Image, error) {
	img, id, err := t.open(ctx, ref)
	if err != nil {
		return nil, Image{}, err
	}
	return entries(img), id, nil
}

// Resolve implements Puller.
func (t Tarball) Resolve(ctx context.Context, ref string) (Image, error) {
	_, id, err := t.open(ctx, ref)
	return id, err
}

func (Tarball) open(ctx context.Context, ref string) (v1.Image, Image, error) {
	// Nothing below reaches the network, so the context has no call to carry
	// it into. Honouring cancellation here keeps the two implementations
	// interchangeable for a caller that gave up before this one started.
	if err := ctx.Err(); err != nil {
		return nil, Image{}, errors.Wrap(ErrPull, errors.CausedBy(err),
			errors.WithProperty("source", ref))
	}
	// A nil tag demands the archive hold exactly one image, which is what a
	// boot cache image is. An archive carrying several is refused whole
	// rather than extracted down an arbitrary branch.
	img, err := tarball.ImageFromPath(ref, nil)
	if err != nil {
		return nil, Image{}, errors.Wrap(ErrPull, errors.CausedBy(err),
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
		return nil, Image{}, errors.Wrap(ErrPull, errors.CausedBy(err),
			errors.WithDetail("reading the image configuration"),
			errors.WithProperty("source", ref))
	}
	// Only a declared mismatch is refused. A carrier image holds nothing but
	// tarballs, and the tool that builds one may record no platform at all,
	// which contradicts nothing.
	if (cfg.OS != "" && cfg.OS != platform.OS) ||
		(cfg.Architecture != "" && cfg.Architecture != platform.Architecture) {
		return nil, Image{}, errors.Wrap(ErrPull,
			errors.WithDetail(fmt.Sprintf("the archive carries a %s/%s image, and the cache is filled for %s/%s",
				cfg.OS, cfg.Architecture, platform.OS, platform.Architecture)),
			errors.WithProperty("source", ref))
	}
	id, err := identify(img, ref)
	return img, id, err
}

// identify reads the two digests an image is known by.
func identify(img v1.Image, ref string) (Image, error) {
	digest, err := img.Digest()
	if err != nil {
		return Image{}, errors.Wrap(ErrPull, errors.CausedBy(err),
			errors.WithDetail("resolving the digest"), errors.WithProperty("source", ref))
	}
	config, err := img.ConfigName()
	if err != nil {
		return Image{}, errors.Wrap(ErrPull, errors.CausedBy(err),
			errors.WithDetail("resolving the configuration digest"), errors.WithProperty("source", ref))
	}
	return Image{Digest: digest.String(), Config: config.String()}, nil
}
