package puller

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"net/http/httptest"
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
	etcdTarPath = "images/etcd.tar"
	amd64Arch   = "amd64"
	arm64Arch   = "arm64"
	linuxOS     = "linux"
	// workerRefPath is the repository/tag suffix appended to every
	// in-memory registry's host:port to build a full image reference.
	workerRefPath = "/boot-cache/worker:1.0.0"
)

func TestRemotePullExtractsFlattenedFilesystem(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	img, err := crane.Image(map[string][]byte{
		etcdTarPath:        []byte("etcd"),
		"images/pause.tar": []byte("pause"),
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
	if got[etcdTarPath] != "etcd" || got["images/pause.tar"] != "pause" {
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
func withPlatform(t *testing.T, img v1.Image, arch, os string) v1.Image {
	t.Helper()
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg = cfg.DeepCopy()
	cfg.Architecture = arch
	cfg.OS = os
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
