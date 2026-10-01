package fill

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scality/image-cache/agent/internal/cache"
	"github.com/scality/image-cache/agent/internal/puller"
)

const resource = "worker-1-0-0"

// stubPuller hands back a fixed stream, or fails, and records whether the
// stream it handed out was closed.
type stubPuller struct {
	entries  map[string]string
	pullErr  error
	closeErr error
	closed   bool
	pulls    int
}

func (s *stubPuller) Pull(context.Context, string) (io.ReadCloser, puller.Image, error) {
	s.pulls++
	if s.pullErr != nil {
		return nil, puller.Image{}, s.pullErr
	}
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	for name, body := range s.entries {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	return closer{buf, s}, puller.Image{Digest: "sha256:stub", Layers: []string{"sha256:stublayer"}}, nil
}

func (s *stubPuller) Resolve(context.Context, string) (puller.Image, error) {
	return puller.Image{Digest: "sha256:stub", Layers: []string{"sha256:stublayer"}}, s.pullErr
}

type closer struct {
	io.Reader
	s *stubPuller
}

func (c closer) Close() error { c.s.closed = true; return c.s.closeErr }

func TestCheckCachePath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckCachePath(dir); err != nil {
		t.Errorf("a directory was refused: %v", err)
	}
	for _, path := range []string{filepath.Join(dir, "absent"), file} {
		if err := CheckCachePath(path); !errors.Is(err, ErrCachePath) {
			t.Errorf("%s: err = %v, want ErrCachePath", path, err)
		}
	}
}

func TestFillExtractsAndClosesTheStream(t *testing.T) {
	dir := t.TempDir()
	p := &stubPuller{entries: map[string]string{"images/etcd.tar": "etcd"}}
	if err := Fill(t.Context(), cache.Store{}, p, dir, resource, "ref", cache.OwnerAgent, nil); err != nil {
		t.Fatal(err)
	}
	if st, _ := (cache.Store{}).State(dir, resource); st != cache.Complete {
		t.Errorf("state = %v, want Complete", st)
	}
	if !p.closed {
		t.Error("the image stream was left open")
	}
	data, err := os.ReadFile(filepath.Join(dir, resource, ".image-cache-agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"owner":"` + cache.OwnerAgent + `"`, `"source":"ref"`, `"layers":["sha256:stublayer"]`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("sentinel %s lacks %s", data, want)
		}
	}
}

func TestFillReturnsWhatThePullFailedOn(t *testing.T) {
	failure := errors.New("registry unreachable")
	err := Fill(t.Context(), cache.Store{}, &stubPuller{pullErr: failure}, t.TempDir(), resource, "ref", cache.OwnerAgent, nil)
	if !errors.Is(err, failure) {
		t.Errorf("err = %v, want the pull's error", err)
	}
}

// A stream that fails to close is worth a word only once the extraction went
// through. Before that, the error Fill returns says what happened, and a note
// about the stream in front of it would read as if the fill had succeeded.
func TestFillReportsAFailedCloseOnlyAfterASuccess(t *testing.T) {
	closeErr := errors.New("close failed")
	for _, tc := range []struct {
		name     string
		entries  map[string]string
		reported bool
	}{
		{"after an extraction", map[string]string{"images/etcd.tar": "etcd"}, true},
		{"after a refused image", map[string]string{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var heard []error
			p := &stubPuller{entries: tc.entries, closeErr: closeErr}
			_ = Fill(t.Context(), cache.Store{}, p, t.TempDir(), resource, "ref", cache.OwnerAgent, func(err error) { heard = append(heard, err) })
			if !p.closed {
				t.Error("the image stream was left open")
			}
			if got := len(heard) == 1 && errors.Is(heard[0], closeErr); got != tc.reported {
				t.Errorf("reported = %v (%v), want %v", got, heard, tc.reported)
			}
		})
	}
}

// A directory the store cannot replace is refused before the pull.
func TestFillRefusesAnUnreplaceableDirectoryBeforePulling(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, resource), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &stubPuller{entries: map[string]string{"a.tar": "a"}}

	err := Fill(t.Context(), cache.Store{}, p, dir, resource, "ref", cache.OwnerAgent, nil)
	if !errors.Is(err, cache.ErrExtract) {
		t.Fatalf("err = %v, want cache.ErrExtract", err)
	}
	if p.pulls != 0 {
		t.Errorf("pulled %d times for a directory it could not replace", p.pulls)
	}
}
