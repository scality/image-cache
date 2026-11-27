// Package puller pulls cache images and exposes their content.
package puller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/go-logr/logr"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
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
	// ErrPlainHTTPRegistry covers a plain HTTP registry entry that is not a
	// host[:port].
	ErrPlainHTTPRegistry = errors.New("invalid plain HTTP registry")
	// ErrUnlistedPlainHTTP covers a plain HTTP request to a registry that is
	// not listed, refused before it leaves, see httpsOnly.
	ErrUnlistedPlainHTTP = errors.New("plain HTTP to an unlisted registry refused")
	// ErrMirror covers a mirror that is not a host[:port].
	ErrMirror = errors.New("invalid registry mirror")
)

// Image identifies what a source resolved to.
type Image struct {
	// Digest is the manifest's digest: how the source serves the image. It
	// depends on how the image is stored, so the same image reached through
	// a registry and through a docker archive carries two of them.
	Digest string
	// Layers are the diff IDs of the image's layers, in order: the digests
	// of the uncompressed layers. Two sources are compared by them, see
	// "Adopting a seeded directory" in agent/DESIGN.md.
	Layers []string
}

// Puller resolves an image reference and returns the entries of its layers.
type Puller interface {
	// Pull returns the entries of the image's layers, in layer order and as
	// the layers carry them, as one tar stream, and what the source resolved
	// to. Nothing is filtered out on the way, see entries. The caller closes
	// the stream.
	Pull(ctx context.Context, ref string) (io.ReadCloser, Image, error)
	// Resolve returns what the source resolves to without reading its
	// layers: for a registry, the manifest and the configuration.
	Resolve(ctx context.Context, ref string) (Image, error)
}

// Remote pulls linux/amd64 images from an OCI registry. The zero value
// trusts the system CAs only, and reaches every registry over HTTPS only.
type Remote struct {
	// transport is nil for go-containerregistry's default.
	transport http.RoundTripper
	// plainHTTP lists the registries marked insecure, normalized as
	// name.Registry.RegistryStr returns them, see TLS.PlainHTTPRegistries.
	plainHTTP []string
	// mirror is TLS.Mirror, marked insecure when plainHTTP lists it, or nil.
	mirror *name.Registry
}

// TLS says how a Remote reaches the registry and checks its certificate.
type TLS struct {
	// CAFile is a PEM file of CAs trusted on top of the system ones.
	CAFile string
	// SkipVerify accepts any certificate. It is meant for a test cluster set
	// up by hand, never for a node.
	SkipVerify bool
	// PlainHTTPRegistries lists the registries, as host[:port], the pull may
	// reach over plain HTTP, see agent/README.md for the matching rules.
	PlainHTTPRegistries []string
	// Mirror is a registry, as host[:port], tried before the registry each
	// reference names, see "Image pulling" in agent/DESIGN.md. The settings
	// above apply to it like to any registry: it is reached over plain HTTP
	// only if it is listed in PlainHTTPRegistries.
	Mirror string
}

// NewRemote returns a Remote that reaches the registry as cfg says. The CA
// file is read once: a rotated CA needs a new Remote.
func NewRemote(cfg TLS) (Remote, error) {
	// Without a certificate setting, the library default transport does.
	if cfg.CAFile == "" && !cfg.SkipVerify {
		return withRegistries(Remote{}, cfg)
	}
	// The library declares its default as an *http.Transport.
	return newRemote(cfg, remote.DefaultTransport.(*http.Transport))
}

// newRemote builds on base; tests hand it a transport that dials their own
// server.
func newRemote(cfg TLS, base *http.Transport) (Remote, error) {
	switch {
	case cfg.CAFile != "" && cfg.SkipVerify:
		return Remote{}, errors.Wrap(ErrTLS,
			errors.WithDetail("a CA file is pointless when certificates are not verified"))
	case len(cfg.PlainHTTPRegistries) > 0 && cfg.SkipVerify:
		return Remote{}, errors.Wrap(ErrTLS,
			errors.WithDetail("plain HTTP registries are not meant for a test cluster that verifies no certificate"))
	}
	t := base.Clone()
	switch {
	case cfg.SkipVerify:
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
	default:
		pool, err := caPool(cfg.CAFile)
		if err != nil {
			return Remote{}, err
		}
		t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return withRegistries(Remote{transport: t}, cfg)
}

// withRegistries checks and normalizes the registries cfg names, and sets
// them on r.
func withRegistries(r Remote, cfg TLS) (Remote, error) {
	// The mirror first: imagecachectl copies it into the plain HTTP list,
	// and a bad one is better named as the mirror.
	var mirror name.Registry
	if cfg.Mirror != "" {
		var err error
		mirror, err = name.NewRegistry(cfg.Mirror, name.StrictValidation)
		if err != nil {
			return Remote{}, errors.Wrap(ErrMirror, errors.CausedBy(err),
				errors.WithDetail("want host[:port], with no scheme and no path"),
				errors.WithProperty("mirror", cfg.Mirror))
		}
	}
	plain, err := plainHTTPRegistries(cfg.PlainHTTPRegistries)
	if err != nil {
		return Remote{}, err
	}
	r.plainHTTP = plain
	if cfg.Mirror != "" {
		// Insecure is an option of the parse, so a listed mirror is parsed
		// again with it.
		if slices.Contains(plain, mirror.RegistryStr()) {
			if mirror, err = name.NewRegistry(cfg.Mirror, name.StrictValidation, name.Insecure); err != nil {
				return Remote{}, errors.Wrap(ErrMirror, errors.CausedBy(err),
					errors.WithProperty("mirror", cfg.Mirror))
			}
		}
		r.mirror = &mirror
	}
	return r, nil
}

// plainHTTPRegistries checks each entry is a host[:port] and normalizes it the
// way a parsed reference names its registry, so that docker.io matches what
// the library calls index.docker.io.
func plainHTTPRegistries(entries []string) ([]string, error) {
	var out []string
	for _, e := range entries {
		// An empty name would read as the default registry, Docker Hub.
		if e == "" {
			return nil, errors.Wrap(ErrPlainHTTPRegistry,
				errors.WithDetail("an entry is empty, want host[:port]"))
		}
		reg, err := name.NewRegistry(e, name.StrictValidation)
		if err != nil {
			return nil, errors.Wrap(ErrPlainHTTPRegistry, errors.CausedBy(err),
				errors.WithDetail("want host[:port], with no scheme and no path"),
				errors.WithProperty("registry", e))
		}
		if s := reg.RegistryStr(); !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out, nil
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
func (r Remote) Pull(ctx context.Context, ref string) (io.ReadCloser, Image, error) {
	img, id, err := r.open(ctx, ref, true)
	if err != nil {
		return nil, Image{}, err
	}
	return entries(img), id, nil
}

// Resolve implements Puller.
func (r Remote) Resolve(ctx context.Context, ref string) (Image, error) {
	_, id, err := r.open(ctx, ref, false)
	return id, err
}

// open resolves ref to its linux/amd64 image. It reads the manifest and the
// configuration, never a layer: those are fetched as the stream is read.
//
// With a mirror, the mirror is tried first and the registry ref names next.
// For a pull, the mirror must also answer a HEAD for each layer: a resolve
// reads no layer, so it does not ask. See "Image pulling" in agent/DESIGN.md.
func (r Remote) open(ctx context.Context, ref string, layers bool) (v1.Image, Image, error) {
	parsed, err := r.parse(ref)
	if err != nil {
		return nil, Image{}, err
	}
	upstream := parsed.Context().RegistryStr()
	if r.mirror == nil || r.mirror.RegistryStr() == upstream {
		return r.openFrom(ctx, parsed, ref, "")
	}
	onMirror := r.onMirror(parsed)
	img, id, mirrorErr := r.openFrom(ctx, onMirror, onMirror.String(), namespace(upstream))
	if mirrorErr == nil && layers {
		mirrorErr = holdsLayers(img, onMirror.String())
	}
	// A cancelled pull would fail the same way on the registry.
	if mirrorErr == nil || ctx.Err() != nil {
		return img, id, mirrorErr
	}
	img, id, err = r.openFrom(ctx, parsed, ref, "")
	if err != nil {
		return nil, Image{}, errors.Wrap(ErrPull, errors.CausedBy(stderrors.Join(mirrorErr, err)),
			errors.WithDetail("neither the mirror nor the registry served the image"),
			errors.WithProperty("source", ref), errors.WithProperty("mirror", r.mirror.RegistryStr()))
	}
	// Logged, not returned: the registry answered. Without it, a mirror that
	// always fails goes unnoticed until the node loses the registry too.
	logr.FromContextOrDiscard(ctx).Info("Mirror failed, resolved the image from the registry",
		"source", ref, "mirror", r.mirror.RegistryStr(), "reason", mirrorErr.Error())
	return img, id, nil
}

// holdsLayers checks that the mirror has every layer of img, with one HEAD
// request per layer.
func holdsLayers(img v1.Image, ref string) error {
	layers, err := img.Layers()
	if err != nil {
		return errors.Wrap(ErrPull, errors.CausedBy(err),
			errors.WithDetail("listing the layers"), errors.WithProperty("source", ref))
	}
	for _, l := range layers {
		ok, err := partial.Exists(l)
		if err == nil && ok {
			continue
		}
		opts := []errors.Option{errors.WithDetail("the mirror misses a layer"), errors.WithProperty("source", ref)}
		if d, derr := l.Digest(); derr == nil {
			opts = append(opts, errors.WithProperty("layer", d.String()))
		}
		// A 404 comes back as false with no error, and an empty cause
		// would make the message panic.
		if err != nil {
			opts = append(opts, errors.CausedBy(err))
		}
		return errors.Wrap(ErrPull, opts...)
	}
	return nil
}

// onMirror returns parsed with its registry replaced by the mirror. It is
// built from the registry rather than parsed from a string: a mirror named
// with no dot and no port, such as a short host name, would read as the
// first path element of a Docker Hub repository.
func (r Remote) onMirror(parsed name.Reference) name.Reference {
	repo := r.mirror.Repo(parsed.Context().RepositoryStr())
	if _, ok := parsed.(name.Digest); ok {
		return repo.Digest(parsed.Identifier())
	}
	return repo.Tag(parsed.Identifier())
}

// parse parses ref, marked insecure when its registry is listed for plain
// HTTP.
func (r Remote) parse(ref string) (name.Reference, error) {
	parsed, err := name.ParseReference(ref)
	if err == nil && slices.Contains(r.plainHTTP, parsed.Context().RegistryStr()) {
		// The library then races HTTPS and HTTP for this registry alone,
		// see "Pulling and extraction" in agent/DESIGN.md.
		parsed, err = name.ParseReference(ref, name.Insecure)
	}
	if err != nil {
		return nil, errors.Wrap(ErrReference, errors.CausedBy(err),
			errors.WithProperty("source", ref))
	}
	return parsed, nil
}

// openFrom resolves parsed. A non-empty ns is sent to the mirror, see
// withNamespace.
func (r Remote) openFrom(ctx context.Context, parsed name.Reference, ref, ns string) (v1.Image, Image, error) {
	// Read at each pull rather than once, so the library default stays the
	// one in force, as it would be without a transport of ours.
	next := r.transport
	if next == nil {
		next = remote.DefaultTransport
	}
	var t http.RoundTripper = httpsOnly{next: next, plain: r.plainHTTP, registry: parsed.Context().RegistryStr()}
	if ns != "" {
		t = withNamespace{next: t, host: r.mirror.RegistryStr(), ns: ns}
	}
	opts := []remote.Option{
		remote.WithContext(ctx),
		remote.WithPlatform(platform),
		remote.WithTransport(t),
	}
	img, err := remote.Image(parsed, opts...)
	if err != nil {
		return nil, Image{}, errors.Wrap(ErrPull, errors.CausedBy(err),
			errors.WithProperty("source", ref))
	}
	id, err := identify(img, ref)
	return img, id, err
}

// namespace is the ns a mirror receives for a registry. Docker Hub goes by
// docker.io there, as containerd sends it, not by the host the library uses.
func namespace(registry string) string {
	if registry == name.DefaultRegistry {
		return "docker.io"
	}
	return registry
}

// withNamespace adds the ns query parameter to the registry API requests sent
// to the mirror, under /v2/. It names the registry the image comes from, the
// way containerd passes it to a mirror. Any other request, such as one to a
// token server or a signed URL the mirror redirects to, goes out unchanged.
type withNamespace struct {
	next     http.RoundTripper
	host, ns string
}

// RoundTrip implements http.RoundTripper.
func (t withNamespace) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != t.host || !strings.HasPrefix(req.URL.Path, "/v2/") {
		return t.next.RoundTrip(req)
	}
	// A RoundTripper must not change the request it is given. The query is
	// appended to rather than encoded again, which would reorder it.
	req = req.Clone(req.Context())
	ns := "ns=" + url.QueryEscape(t.ns)
	if req.URL.RawQuery == "" {
		req.URL.RawQuery = ns
	} else {
		req.URL.RawQuery += "&" + ns
	}
	return t.next.RoundTrip(req)
}

// httpsOnly refuses a plain HTTP request to the pulled registry unless it is
// listed. The library also races HTTP for a private or loopback IP and for a
// localhost name, whatever the list says, see "Pulling and extraction" in
// agent/DESIGN.md, so the request is stopped here. A redirect to another host,
// such as blob storage, is left alone: each blob is checked against its digest.
type httpsOnly struct {
	next http.RoundTripper
	// plain is Remote.plainHTTP: the library builds its URLs on the same
	// RegistryStr, so a listed registry's requests carry it as their host.
	plain []string
	// registry is the RegistryStr of the image being pulled.
	registry string
}

// RoundTrip implements http.RoundTripper.
func (t httpsOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" && req.URL.Host == t.registry && !slices.Contains(t.plain, req.URL.Host) {
		return nil, errors.Wrap(ErrUnlistedPlainHTTP, errors.WithProperty("registry", req.URL.Host))
	}
	return t.next.RoundTrip(req)
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
	id, err := identify(img, ref)
	return img, id, err
}

// identify reads the manifest digest and the layers' diff IDs, and refuses an
// image built for another platform. remote.WithPlatform only picks a child out
// of an index, and an archive carries whatever it was saved from. Without the
// check, an arm64 image fills the cache of an x86_64 node, the sentinel says
// the resource is complete, and nothing further down looks at the
// architecture again.
func identify(img v1.Image, ref string) (Image, error) {
	digest, err := img.Digest()
	if err != nil {
		return Image{}, errors.Wrap(ErrPull, errors.CausedBy(err),
			errors.WithDetail("resolving the digest"), errors.WithProperty("source", ref))
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return Image{}, errors.Wrap(ErrPull, errors.CausedBy(err),
			errors.WithDetail("reading the image configuration"), errors.WithProperty("source", ref))
	}
	// Only a declared mismatch is refused. A carrier image holds nothing but
	// tarballs, and the tool that builds one may record no platform at all,
	// which contradicts nothing.
	if (cfg.OS != "" && cfg.OS != platform.OS) ||
		(cfg.Architecture != "" && cfg.Architecture != platform.Architecture) {
		return Image{}, errors.Wrap(ErrPull,
			errors.WithDetail(fmt.Sprintf("the image is %s/%s, and the cache is filled for %s/%s",
				cfg.OS, cfg.Architecture, platform.OS, platform.Architecture)),
			errors.WithProperty("source", ref))
	}
	layers := make([]string, 0, len(cfg.RootFS.DiffIDs))
	for _, d := range cfg.RootFS.DiffIDs {
		layers = append(layers, d.String())
	}
	return Image{Digest: digest.String(), Layers: layers}, nil
}
