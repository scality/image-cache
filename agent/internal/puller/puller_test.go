package puller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

const (
	// etcdBody is the content fixtures give the etcd archive.
	etcdBody     = "etcd"
	pauseBody    = "pause"
	etcdTarPath  = "images/etcd.tar"
	pauseTarPath = "images/pause.tar"
	amd64Arch    = "amd64"
	arm64Arch    = "arm64"
	linuxOS      = "linux"
	windowsOS    = "windows"
	// workerRefPath is the repository/tag suffix appended to every
	// in-memory registry's host:port to build a full image reference.
	workerRefPath = "/boot-cache/worker:1.0.0"
)

func TestRemotePullStreamsTheImageFiles(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	img, err := crane.Image(map[string][]byte{
		etcdTarPath:  []byte(etcdBody),
		pauseTarPath: []byte(pauseBody),
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := strings.TrimPrefix(srv.URL, "http://") + workerRefPath
	if err := crane.Push(img, ref); err != nil {
		t.Fatal(err)
	}

	rc, digest, err := Remote{}.Pull(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rc.Close(); err != nil {
			t.Errorf("closing stream: %v", err)
		}
	}()
	wantDigest, _ := img.Digest()
	if digest.Digest != wantDigest.String() {
		t.Errorf("digest = %s, want %s", digest.Digest, wantDigest)
	}
	got := map[string]string{}
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF { //nolint:errorlint // the end of an archive is the unwrapped value
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		content, _ := io.ReadAll(tr)
		got[hdr.Name] = string(content)
	}
	if got[etcdTarPath] != etcdBody || got[pauseTarPath] != pauseBody {
		t.Errorf("unexpected content: %v", got)
	}
}

func TestRemotePullResolvesMultiArchIndex(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()

	imgAmd, err := crane.Image(map[string][]byte{etcdTarPath: []byte(amd64Arch)})
	if err != nil {
		t.Fatal(err)
	}
	imgArm, err := crane.Image(map[string][]byte{etcdTarPath: []byte(arm64Arch)})
	if err != nil {
		t.Fatal(err)
	}

	imgAmd = withPlatform(t, imgAmd, amd64Arch, linuxOS)
	imgArm = withPlatform(t, imgArm, arm64Arch, linuxOS)

	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{
			Add: imgAmd,
			Descriptor: v1.Descriptor{
				Platform: &v1.Platform{OS: linuxOS, Architecture: amd64Arch},
			},
		},
		mutate.IndexAddendum{
			Add: imgArm,
			Descriptor: v1.Descriptor{
				Platform: &v1.Platform{OS: linuxOS, Architecture: arm64Arch},
			},
		},
	)

	ref := strings.TrimPrefix(srv.URL, "http://") + workerRefPath
	tag, err := name.NewTag(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(tag, idx); err != nil {
		t.Fatal(err)
	}

	rc, digest, err := Remote{}.Pull(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rc.Close(); err != nil {
			t.Errorf("closing stream: %v", err)
		}
	}()

	wantDigest, err := imgAmd.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if digest.Digest != wantDigest.String() {
		t.Errorf("digest = %s, want %s (amd64 child, not index)", digest.Digest, wantDigest)
	}

	got := map[string]string{}
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF { //nolint:errorlint // the end of an archive is the unwrapped value
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		content, _ := io.ReadAll(tr)
		got[hdr.Name] = string(content)
	}
	if got[etcdTarPath] != amd64Arch {
		t.Errorf("unexpected content: %v, want amd64 child selected", got)
	}
}

// withPlatform returns img with its config's Architecture/OS set, so its
// digest changes to reflect the platform-specific config (mirroring what a
// real multi-arch build produces for each child image).
func withPlatform(t *testing.T, img v1.Image, arch, osName string) v1.Image {
	t.Helper()
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg = cfg.DeepCopy()
	cfg.Architecture = arch
	cfg.OS = osName
	out, err := mutate.ConfigFile(img, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRemotePullBadReference(t *testing.T) {
	if _, _, err := (Remote{}).Pull(context.Background(), ":::"); err == nil {
		t.Fatal("want error on invalid reference")
	}
}

func TestRemotePullNoMatchingPlatform(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()

	imgArm, err := crane.Image(map[string][]byte{etcdTarPath: []byte(arm64Arch)})
	if err != nil {
		t.Fatal(err)
	}
	imgArm = withPlatform(t, imgArm, arm64Arch, linuxOS)

	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{
			Add: imgArm,
			Descriptor: v1.Descriptor{
				Platform: &v1.Platform{OS: linuxOS, Architecture: arm64Arch},
			},
		},
	)

	ref := strings.TrimPrefix(srv.URL, "http://") + workerRefPath
	tag, err := name.NewTag(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(tag, idx); err != nil {
		t.Fatal(err)
	}

	if _, _, err := (Remote{}).Pull(context.Background(), ref); err == nil {
		t.Fatal("want error when the index has no linux/amd64 child")
	}
}

func TestRemotePullContextCancelled(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()

	img, err := crane.Image(map[string][]byte{etcdTarPath: []byte(etcdBody)})
	if err != nil {
		t.Fatal(err)
	}
	ref := strings.TrimPrefix(srv.URL, "http://") + workerRefPath
	if err := crane.Push(img, ref); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := (Remote{}).Pull(ctx, ref); err == nil {
		t.Fatal("want error on an already-cancelled context")
	}
}

// A docker archive is what the ISO ships, so the file form of the command
// reads one rather than a registry. `crane.Save` writes the same layout.
func saveArchive(t *testing.T, img v1.Image, ref string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "boot-cache.tar")
	if err := crane.Save(img, ref, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTarballPullStreamsTheImageFiles(t *testing.T) {
	img, err := crane.Image(map[string][]byte{
		etcdTarPath:  []byte(etcdBody),
		pauseTarPath: []byte(pauseBody),
	})
	if err != nil {
		t.Fatal(err)
	}
	path := saveArchive(t, img, "boot-cache/control-plane:1.0.0")

	rc, digest, err := Tarball{}.Pull(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rc.Close(); err != nil {
			t.Errorf("closing stream: %v", err)
		}
	}()

	wantDigest, _ := img.Digest()
	if digest.Digest != wantDigest.String() {
		t.Errorf("digest = %s, want %s", digest.Digest, wantDigest)
	}
	got := map[string]string{}
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF { //nolint:errorlint // the end of an archive is the unwrapped value
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		content, _ := io.ReadAll(tr)
		got[hdr.Name] = string(content)
	}
	if got[etcdTarPath] != etcdBody || got[pauseTarPath] != pauseBody {
		t.Errorf("unexpected content: %v", got)
	}
}

func TestTarballPullMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.tar")
	if _, _, err := (Tarball{}).Pull(context.Background(), path); err == nil {
		t.Fatal("want error on an archive that is not there")
	}
}

func TestTarballPullNotAnArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boot-cache.tar")
	if err := os.WriteFile(path, []byte("not a tar"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (Tarball{}).Pull(context.Background(), path); err == nil {
		t.Fatal("want error on a file that is not a docker archive")
	}
}

// A boot cache image carries exactly one image. Refusing the rest here is
// what keeps a half extraction from being reported as a filled cache.
func TestTarballPullSeveralImages(t *testing.T) {
	first, err := crane.Image(map[string][]byte{etcdTarPath: []byte(etcdBody)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := crane.Image(map[string][]byte{pauseTarPath: []byte(pauseBody)})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "two-images.tar")
	if err := crane.MultiSave(map[string]v1.Image{
		"boot-cache/control-plane:1.0.0": first,
		"boot-cache/worker:1.0.0":        second,
	}, path); err != nil {
		t.Fatal(err)
	}

	if _, _, err := (Tarball{}).Pull(context.Background(), path); err == nil {
		t.Fatal("want error on an archive carrying more than one image")
	}
}

func TestTarballPullContextCancelled(t *testing.T) {
	img, err := crane.Image(map[string][]byte{etcdTarPath: []byte(etcdBody)})
	if err != nil {
		t.Fatal(err)
	}
	path := saveArchive(t, img, "boot-cache/control-plane:1.0.0")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := (Tarball{}).Pull(ctx, path); err == nil {
		t.Fatal("want error on an already-cancelled context")
	}
}

// Remote pins linux/amd64 when it resolves a reference. An archive carries
// whatever it was saved from, so the same check belongs here: without it, an
// image built on an arm64 machine fills the cache of an x86_64 node, the
// sentinel says the resource is complete, and nothing further down looks at
// the architecture again.
func TestTarballPullRefusesAnotherPlatform(t *testing.T) {
	img, err := crane.Image(map[string][]byte{etcdTarPath: []byte(etcdBody)})
	if err != nil {
		t.Fatal(err)
	}
	path := saveArchive(t, withPlatform(t, img, arm64Arch, linuxOS), "boot-cache/worker:1.0.0")

	_, _, err = (Tarball{}).Pull(context.Background(), path)
	if err == nil {
		t.Fatal("an arm64 archive was accepted")
	}
	if !strings.Contains(err.Error(), "arm64") || !strings.Contains(err.Error(), "amd64") {
		t.Errorf("err = %v, want both architectures named", err)
	}
}

// The operating system is half of the same pair, and an archive can disagree
// on it just as easily.
func TestTarballPullRefusesAnotherOS(t *testing.T) {
	img, err := crane.Image(map[string][]byte{etcdTarPath: []byte(etcdBody)})
	if err != nil {
		t.Fatal(err)
	}
	path := saveArchive(t, withPlatform(t, img, amd64Arch, windowsOS), "boot-cache/worker:1.0.0")

	if _, _, err := (Tarball{}).Pull(context.Background(), path); err == nil {
		t.Fatal("a windows archive was accepted")
	} else if !strings.Contains(err.Error(), windowsOS) {
		t.Errorf("err = %v, want the operating system named", err)
	}
}

// A carrier image holds nothing but tarballs, and the tool that builds one
// may record no platform at all. That contradicts nothing, so it is taken.
func TestTarballPullAcceptsAnUndeclaredPlatform(t *testing.T) {
	img, err := crane.Image(map[string][]byte{etcdTarPath: []byte(etcdBody)})
	if err != nil {
		t.Fatal(err)
	}
	path := saveArchive(t, img, "boot-cache/worker:1.0.0")

	rc, _, err := (Tarball{}).Pull(context.Background(), path)
	if err != nil {
		t.Fatalf("an image declaring no platform was refused: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Error(err)
	}
}

// layerFrom builds a layer out of raw headers, so a test can carry entries
// that crane.Image never writes: directories, links, whiteouts.
func layerFrom(t *testing.T, hdrs []tar.Header, bodies []string) v1.Layer {
	t.Helper()
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	for i := range hdrs {
		hdr := hdrs[i]
		hdr.Size = int64(len(bodies[i]))
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(bodies[i])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(raw)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return layer
}

func imageOf(t *testing.T, layers ...v1.Layer) v1.Image {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image, layers...)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// readAll drains the stream and returns its headers in order, or the error
// the stream ended on.
func readAll(t *testing.T, rc io.ReadCloser) ([]*tar.Header, error) {
	t.Helper()
	defer func() {
		if err := rc.Close(); err != nil {
			t.Error(err)
		}
	}()
	var hdrs []*tar.Header
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF { //nolint:errorlint // the end of an archive is the unwrapped value
			return hdrs, nil
		}
		if err != nil {
			return hdrs, err
		}
		hdrs = append(hdrs, hdr)
	}
}

// The case entries exists for. A context holding a link to an archive outside
// it gives an image whose link leaves the image root, and mutate.Extract drops
// such a link without a word: the store would never see it, and the node would
// be reported synced without the archive. It has to come through untouched,
// so the store can refuse it.
func TestEntriesPassesOnALinkLeavingTheImageRoot(t *testing.T) {
	img := imageOf(t, layerFrom(t,
		[]tar.Header{
			{Name: "images/", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: etcdTarPath, Typeflag: tar.TypeReg, Mode: 0o644},
			{Name: pauseTarPath, Typeflag: tar.TypeSymlink, Linkname: "../../shared/pause.tar"},
		},
		[]string{"", etcdBody, ""},
	))

	hdrs, err := readAll(t, entries(img))
	if err != nil {
		t.Fatal(err)
	}
	for _, hdr := range hdrs {
		if hdr.Name == pauseTarPath {
			if hdr.Typeflag != tar.TypeSymlink || hdr.Linkname != "../../shared/pause.tar" {
				t.Errorf("the link came through altered: %+v", hdr)
			}
			return
		}
	}
	t.Fatalf("the link leaving the image root was dropped: %v", names(hdrs))
}

// Entries come out layer by layer, in order, and nothing in a later layer
// hides anything of an earlier one: the store sees what each layer carries.
func TestEntriesKeepsLayerOrder(t *testing.T) {
	img := imageOf(t,
		layerFrom(t, []tar.Header{{Name: etcdTarPath, Typeflag: tar.TypeReg, Mode: 0o644}}, []string{etcdBody}),
		layerFrom(t, []tar.Header{{Name: pauseTarPath, Typeflag: tar.TypeReg, Mode: 0o644}}, []string{pauseBody}),
	)

	hdrs, err := readAll(t, entries(img))
	if err != nil {
		t.Fatal(err)
	}
	got := names(hdrs)
	if len(got) != 2 || got[0] != etcdTarPath || got[1] != pauseTarPath {
		t.Errorf("entries = %v, want the first layer's then the second's", got)
	}
}

// A boot cache image is built from scratch and only adds files. An entry that
// deletes one is refused rather than passed on as a file named .wh.something,
// and the error names it.
func TestEntriesRefusesAWhiteout(t *testing.T) {
	// A plain whiteout deletes one path; an opaque one empties a directory.
	for _, whiteout := range []string{"images/.wh.old.tar", "images/.wh..wh..opq"} {
		img := imageOf(t,
			layerFrom(t, []tar.Header{{Name: "images/old.tar", Typeflag: tar.TypeReg, Mode: 0o644}}, []string{"old"}),
			layerFrom(t, []tar.Header{{Name: whiteout, Typeflag: tar.TypeReg, Mode: 0o644}}, []string{""}),
		)

		_, err := readAll(t, entries(img))
		if err == nil {
			t.Fatalf("%s was passed on", whiteout)
		}
		if !errors.Is(err, ErrPull) || !strings.Contains(err.Error(), whiteout) {
			t.Errorf("err = %v, want ErrPull naming %s", err, whiteout)
		}
	}
}

// A pulled layer is checked twice once its stream has been read to the end:
// gzip's own checksum, and the digest the manifest announces. The end of the
// tar archive comes before that end, and stopping there took a layer altered
// in transit as it came, cached under the image's digest. Each case below is
// caught by one check only, so each fails if that check stops running.
func TestRemotePullRefusesALayerAlteredInTransit(t *testing.T) {
	// Stored rather than compressed, so that a flipped byte changes a file's
	// content without breaking inflate, and two payloads of the same length
	// give layers of exactly the same length: the manifest's size is checked
	// too, and a substitute of another length would be caught by that instead.
	stored := func() v1.Image {
		payload := make([]byte, 64<<10)
		if _, err := rand.Read(payload); err != nil {
			t.Fatal(err)
		}
		buf := &bytes.Buffer{}
		tw := tar.NewWriter(buf)
		if err := tw.WriteHeader(&tar.Header{Name: etcdTarPath, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(payload))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		raw := buf.Bytes()
		layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(raw)), nil
		}, tarball.WithCompressionLevel(gzip.NoCompression))
		if err != nil {
			t.Fatal(err)
		}
		return imageOf(t, layer)
	}
	img, other := stored(), stored()
	layerDigest := firstLayer(t, img).digest
	substitute := firstLayer(t, other).compressed

	for _, tc := range []struct {
		name   string
		tamper func([]byte) []byte
		cause  string
	}{
		{"one byte flipped, caught by gzip", func(b []byte) []byte {
			b[len(b)/2] ^= 0xff
			return b
		}, "checksum"},
		// Same length, valid gzip, valid tar: only the digest tells it apart.
		{"another valid layer of the same length, caught by the digest", func(b []byte) []byte {
			if len(substitute) != len(b) {
				panic("the substitute layer must be the original's length")
			}
			return append([]byte(nil), substitute...)
		}, "sha256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := servedTampered(t, img, layerDigest.String(), tc.tamper)
			rc, _, err := (Remote{}).Pull(t.Context(), ref)
			if err != nil {
				t.Fatal(err)
			}
			err = drainArchive(rc)
			if err == nil {
				t.Fatal("the altered layer came through as a clean archive")
			}
			if !errors.Is(err, ErrPull) || !strings.Contains(err.Error(), tc.cause) {
				t.Errorf("err = %v, want ErrPull caused by a %s mismatch", err, tc.cause)
			}
		})
	}
}

type layerBytes struct {
	digest     v1.Hash
	compressed []byte
}

func firstLayer(t *testing.T, img v1.Image) layerBytes {
	t.Helper()
	layers, err := img.Layers()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := layers[0].Digest()
	if err != nil {
		t.Fatal(err)
	}
	rc, err := layers[0].Compressed()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return layerBytes{digest, b}
}

// servedTampered pushes img to an in-memory registry and returns a reference
// to a proxy in front of it that serves every blob as it is, except the layer
// with the given digest, which goes through tamper first.
func servedTampered(t *testing.T, img v1.Image, digest string, tamper func([]byte) []byte) string {
	t.Helper()
	reg := registry.New()
	direct := httptest.NewServer(reg)
	t.Cleanup(direct.Close)
	if err := crane.Push(img, strings.TrimPrefix(direct.URL, "http://")+workerRefPath); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/blobs/"+digest) {
			reg.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		reg.ServeHTTP(rec, r)
		body := tamper(rec.Body.Bytes())
		maps.Copy(w.Header(), rec.Header())
		w.WriteHeader(rec.Code)
		_, _ = w.Write(body)
	}))
	t.Cleanup(proxy.Close)
	return strings.TrimPrefix(proxy.URL, "http://") + workerRefPath
}

// drainArchive reads the stream to the end of its archive, as the store does,
// and returns what it ended on: nil for a clean end.
func drainArchive(rc io.ReadCloser) error {
	defer func() { _ = rc.Close() }()
	tr := tar.NewReader(rc)
	for {
		_, err := tr.Next()
		if err == io.EOF { //nolint:errorlint // the end of an archive is the unwrapped value
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return err
		}
	}
}

// A global header describes the archive rather than any file in it. It is
// passed on as it came, for the store to skip.
func TestEntriesPassesOnAGlobalHeader(t *testing.T) {
	img := imageOf(t, layerFrom(t,
		[]tar.Header{
			{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "c0ffee"}},
			{Name: etcdTarPath, Typeflag: tar.TypeReg, Mode: 0o644},
		},
		[]string{"", etcdBody},
	))

	hdrs, err := readAll(t, entries(img))
	if err != nil {
		t.Fatal(err)
	}
	if len(hdrs) != 2 || hdrs[0].Typeflag != tar.TypeXGlobalHeader || hdrs[0].PAXRecords["comment"] != "c0ffee" {
		t.Errorf("entries = %v, want the global header first, records intact", names(hdrs))
	}
}

func names(hdrs []*tar.Header) []string {
	out := make([]string, 0, len(hdrs))
	for _, hdr := range hdrs {
		out = append(out, hdr.Name)
	}
	return out
}

// brokenImage and brokenLayer stand in for an image or a layer whose reads
// fail, for the error paths of entries that a well-formed fixture never takes.
type brokenImage struct {
	v1.Image
	layers []v1.Layer
	err    error
}

func (b brokenImage) Layers() ([]v1.Layer, error) { return b.layers, b.err }

type brokenLayer struct {
	v1.Layer
	open  func() (io.ReadCloser, error)
	close error
}

func (b brokenLayer) Uncompressed() (io.ReadCloser, error) {
	rc, err := b.open()
	if err != nil {
		return nil, err
	}
	return closeErr{rc, b.close}, nil
}

type closeErr struct {
	io.ReadCloser
	err error
}

func (c closeErr) Close() error {
	if c.err != nil {
		return c.err
	}
	return c.ReadCloser.Close()
}

// failingWriter accepts limit bytes, then fails every write.
type failingWriter struct{ limit int }

func (f *failingWriter) Write(p []byte) (int, error) {
	if len(p) > f.limit {
		n := f.limit
		f.limit = 0
		return n, io.ErrShortWrite
	}
	f.limit -= len(p)
	return len(p), nil
}

// Every way reading an image can fail ends the stream with ErrPull, rather
// than with a clean archive the store would publish as complete.
func TestEntriesEndsInErrPullWhenReadingFails(t *testing.T) {
	good := layerFrom(t, []tar.Header{{Name: etcdTarPath, Typeflag: tar.TypeReg, Mode: 0o644}}, []string{etcdBody})
	goodRaw := func() []byte {
		rc, err := good.Uncompressed()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rc.Close() }()
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}()
	fails := errors.New("injected")

	for failure, img := range map[string]v1.Image{
		"the layers cannot be listed": brokenImage{err: fails},
		"a layer cannot be opened": brokenImage{layers: []v1.Layer{brokenLayer{
			open: func() (io.ReadCloser, error) { return nil, fails },
		}}},
		"a layer is not a tar archive": brokenImage{layers: []v1.Layer{brokenLayer{
			open: func() (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(strings.Repeat("x", 1024))), nil
			},
		}}},
		"a layer ends inside an entry": brokenImage{layers: []v1.Layer{brokenLayer{
			open: func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(goodRaw[:512+2])), nil
			},
		}}},
		"a layer cannot be closed": brokenImage{layers: []v1.Layer{brokenLayer{
			open:  func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(goodRaw)), nil },
			close: fails,
		}}},
	} {
		t.Run(failure, func(t *testing.T) {
			_, err := readAll(t, entries(img))
			if !errors.Is(err, ErrPull) {
				t.Errorf("err = %v, want ErrPull", err)
			}
		})
	}
}

// A failed write ends the stream with ErrPull wherever it happens, and says
// where: on an entry's header, on its content, or on the end of the archive.
func TestEntriesReportsWhereAWriteFailed(t *testing.T) {
	two := imageOf(t, layerFrom(t,
		[]tar.Header{
			{Name: etcdTarPath, Typeflag: tar.TypeReg, Mode: 0o644},
			{Name: pauseTarPath, Typeflag: tar.TypeReg, Mode: 0o644},
		},
		[]string{etcdBody, pauseBody},
	))
	for _, tc := range []struct {
		name  string
		img   v1.Image
		limit int
		want  string
	}{
		{"the first header", two, 0, "the header of \"" + etcdTarPath + "\""},
		{"the first content", two, 512, "the content of \"" + etcdTarPath + "\""},
		// The first entry's header and content go through, and the padding
		// that ends it is written with the next header.
		{"between two entries", two, 512 + len(etcdBody), "the header of \"" + pauseTarPath + "\""},
		{"the end of the archive", empty.Image, 0, "closing the stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := writeEntries(tc.img, &failingWriter{limit: tc.limit})
			if !errors.Is(err, ErrPull) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want ErrPull on %s", err, tc.want)
			}
		})
	}
}

// notifyClose closes done when the layer it wraps is closed, which is the
// last thing the goroutine producing the stream does before it returns.
type notifyClose struct {
	io.ReadCloser
	once *sync.Once
	done chan struct{}
}

func (n notifyClose) Close() error {
	n.once.Do(func() { close(n.done) })
	return n.ReadCloser.Close()
}

// The consumer going away, as a refusing store does, has to end the goroutine
// producing the stream rather than leave it blocked on a write nobody reads.
// This goes through the real pipe: the stream is closed after its first header,
// with the rest of the layer still to send, and the producer has to close the
// layer on its way out.
func TestEntriesEndsWhenTheConsumerCloses(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	raw := func() []byte {
		rc, err := layerFrom(t,
			[]tar.Header{
				{Name: etcdTarPath, Typeflag: tar.TypeReg, Mode: 0o644},
				{Name: pauseTarPath, Typeflag: tar.TypeReg, Mode: 0o644},
			},
			[]string{big, big},
		).Uncompressed()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rc.Close() }()
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}()
	done := make(chan struct{})
	layer := brokenLayer{open: func() (io.ReadCloser, error) {
		return notifyClose{io.NopCloser(bytes.NewReader(raw)), &sync.Once{}, done}, nil
	}}

	rc := entries(brokenImage{layers: []v1.Layer{layer}})
	if _, err := tar.NewReader(rc).Next(); err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the producer is still running five seconds after the consumer closed the stream")
	}
}

// The configuration digest is what the image is, whatever serves it. Read
// through a registry and through a docker archive of the same image, the two
// pullers have to agree on it, or a cache seeded from the ISO would never be
// recognised as holding the image a resource names in the registry.
func TestBothPullersAgreeOnTheConfigDigest(t *testing.T) {
	img, err := crane.Image(map[string][]byte{etcdTarPath: []byte(etcdBody)})
	if err != nil {
		t.Fatal(err)
	}
	want, err := img.ConfigName()
	if err != nil {
		t.Fatal(err)
	}
	reg := httptest.NewServer(registry.New())
	t.Cleanup(reg.Close)
	ref := strings.TrimPrefix(reg.URL, "http://") + workerRefPath
	if err := crane.Push(img, ref); err != nil {
		t.Fatal(err)
	}
	path := saveArchive(t, img, "boot-cache/worker:1.0.0")

	for via, resolve := range map[string]func() (Image, error){
		"registry": func() (Image, error) { return Remote{}.Resolve(t.Context(), ref) },
		"archive":  func() (Image, error) { return Tarball{}.Resolve(t.Context(), path) },
	} {
		id, err := resolve()
		if err != nil {
			t.Fatalf("%s: %v", via, err)
		}
		if id.Config != want.String() {
			t.Errorf("%s: config = %s, want %s", via, id.Config, want)
		}
		if id.Digest == "" {
			t.Errorf("%s: no manifest digest", via)
		}
	}
}

// Resolve answers from the manifest alone. The registry here refuses every
// layer blob, and resolving has to succeed anyway: an agent checking whether
// a seeded directory holds the right image must not download the image to
// find out.
func TestRemoteResolveReadsNoLayer(t *testing.T) {
	img, err := crane.Image(map[string][]byte{etcdTarPath: []byte(etcdBody)})
	if err != nil {
		t.Fatal(err)
	}
	layer := firstLayer(t, img).digest.String()
	ref := servedTampered(t, img, layer, func([]byte) []byte {
		panic("a layer was fetched")
	})

	id, err := Remote{}.Resolve(t.Context(), ref)
	if err != nil {
		t.Fatalf("resolving touched a layer: %v", err)
	}
	want, _ := img.ConfigName()
	if id.Config != want.String() {
		t.Errorf("config = %s, want %s", id.Config, want)
	}
}
