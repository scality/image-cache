package puller

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

const (
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

func TestRemotePullExtractsFlattenedFilesystem(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	img, err := crane.Image(map[string][]byte{
		etcdTarPath:  []byte("etcd"),
		pauseTarPath: []byte("pause"),
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
	if digest != wantDigest.String() {
		t.Errorf("digest = %s, want %s", digest, wantDigest)
	}
	got := map[string]string{}
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
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
	if got[etcdTarPath] != "etcd" || got[pauseTarPath] != "pause" {
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
	if digest != wantDigest.String() {
		t.Errorf("digest = %s, want %s (amd64 child, not index)", digest, wantDigest)
	}

	got := map[string]string{}
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
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

	img, err := crane.Image(map[string][]byte{etcdTarPath: []byte("etcd")})
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

func TestTarballPullExtractsFlattenedFilesystem(t *testing.T) {
	img, err := crane.Image(map[string][]byte{
		etcdTarPath:  []byte("etcd"),
		pauseTarPath: []byte("pause"),
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
	if digest != wantDigest.String() {
		t.Errorf("digest = %s, want %s", digest, wantDigest)
	}
	got := map[string]string{}
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
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
	if got[etcdTarPath] != "etcd" || got[pauseTarPath] != "pause" {
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
	first, err := crane.Image(map[string][]byte{etcdTarPath: []byte("etcd")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := crane.Image(map[string][]byte{pauseTarPath: []byte("pause")})
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
	img, err := crane.Image(map[string][]byte{etcdTarPath: []byte("etcd")})
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
	img, err := crane.Image(map[string][]byte{etcdTarPath: []byte("etcd")})
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
	img, err := crane.Image(map[string][]byte{etcdTarPath: []byte("etcd")})
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
	img, err := crane.Image(map[string][]byte{etcdTarPath: []byte("etcd")})
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
