// Package puller pulls cache images and exposes their content.
package puller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"

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
	// ErrCA covers a registry CA file that cannot be read or holds no
	// certificate.
	ErrCA = errors.New("unusable registry CA")
	// ErrTLS covers TLS settings that contradict each other.
	ErrTLS = errors.New("conflicting registry TLS settings")
)

// Puller resolves an image reference and returns the entries of its layers.
type Puller interface {
	// Pull returns the entries of the image's layers, in layer order and as
	// the layers carry them, as one tar stream, and the resolved image digest.
	// Nothing is filtered out on the way, see entries. The caller closes the
	// stream.
	Pull(ctx context.Context, ref string) (io.ReadCloser, string, error)
}

// Remote pulls linux/amd64 images from an OCI registry. The zero value
// trusts the system CAs only.
type Remote struct {
	// transport is nil for go-containerregistry's default.
	transport http.RoundTripper
}

// TLS says how a Remote checks the registry certificate.
type TLS struct {
	// CAFile is a PEM file of CAs trusted on top of the system ones.
	CAFile string
	// SkipVerify accepts any certificate. It is meant for a test cluster set
	// up by hand, never for a node.
	SkipVerify bool
}

// NewRemote returns a Remote that checks certificates as cfg says. The CA
// file is read once: a rotated CA needs a new Remote.
func NewRemote(cfg TLS) (Remote, error) {
	if cfg == (TLS{}) {
		return Remote{}, nil
	}
	// The library declares its default as an *http.Transport.
	return newRemote(cfg, remote.DefaultTransport.(*http.Transport))
}

// newRemote builds on base; tests hand it a transport that dials their own
// server.
func newRemote(cfg TLS, base *http.Transport) (Remote, error) {
	t := base.Clone()
	switch {
	case cfg.CAFile != "" && cfg.SkipVerify:
		return Remote{}, errors.Wrap(ErrTLS,
			errors.WithDetail("a CA file is pointless when certificates are not verified"))
	case cfg.SkipVerify:
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
	default:
		pool, err := caPool(cfg.CAFile)
		if err != nil {
			return Remote{}, err
		}
		t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return Remote{transport: t}, nil
}

// caPool returns the system CAs plus those of the PEM file at path. The given
// CA extends the pool rather than replacing it, so a registry behind a public
// certificate stays reachable.
func caPool(path string) (*x509.CertPool, error) {
	bundle, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.Wrap(ErrCA, errors.CausedBy(err), errors.WithProperty("path", path))
	}
	// A pool that cannot be loaded is a host without system CAs, which is
	// what the distroless image would be without its bundle: start empty
	// rather than refuse, the given CA may be all the registry needs.
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(bundle) {
		return nil, errors.Wrap(ErrCA, errors.WithDetail("no PEM certificate in the file"),
			errors.WithProperty("path", path))
	}
	return pool, nil
}

var platform = v1.Platform{OS: "linux", Architecture: "amd64"}

// Pull implements Puller.
func (r Remote) Pull(ctx context.Context, ref string) (io.ReadCloser, string, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return nil, "", errors.Wrap(ErrReference, errors.CausedBy(err),
			errors.WithProperty("source", ref))
	}
	opts := []remote.Option{remote.WithContext(ctx), remote.WithPlatform(platform)}
	if r.transport != nil {
		opts = append(opts, remote.WithTransport(r.transport))
	}
	img, err := remote.Image(parsed, opts...)
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
