package cache

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testTar is the file name used by fixtures that only need a single file.
const testTar = "a.tar"

// Digests used by the two-extraction replacement test.
const (
	digestD1 = "d1"
	digestD2 = "d2"
)

// File names used by the two-extraction replacement test.
const (
	oldTarName = "old.tar"
	newTarName = "new.tar"
)

// hostileTarStream builds an in-memory tar from raw headers, so a test can
// ship entries that no honest image builder would produce.
func hostileTarStream(t *testing.T, entries []tar.Header, contents []string) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	for i := range entries {
		hdr := entries[i]
		hdr.Size = int64(len(contents[i]))
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(contents[i])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf
}

// tarStream builds an in-memory tar containing the given path->content files.
func tarStream(t *testing.T, files map[string]string) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf
}

func TestExtractFlattensAndCompletes(t *testing.T) {
	dir, s := t.TempDir(), Store{}
	stream := tarStream(t, map[string]string{"images/etcd.tar": "e", "images/pause.tar": "p"})
	if err := s.Extract(t.Context(), dir, "worker-134-0-0", "sha256:abc", stream); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"etcd.tar", "pause.tar", sentinelName} {
		if _, err := os.Stat(filepath.Join(dir, "worker-134-0-0", f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	if st, _ := s.State(dir, "worker-134-0-0"); st != Complete {
		t.Errorf("state = %v, want Complete", st)
	}
}

func TestExtractRejectsDuplicateBaseNames(t *testing.T) {
	dir, s := t.TempDir(), Store{}
	stream := tarStream(t, map[string]string{"a/x.tar": "1", "b/x.tar": "2"})
	if err := s.Extract(t.Context(), dir, "c", "sha256:abc", stream); err == nil {
		t.Fatal("want duplicate error, got nil")
	}
	if _, err := os.Stat(filepath.Join(dir, "c")); !os.IsNotExist(err) {
		t.Error("failed extraction must not leave the final directory")
	}
}

func TestExtractIgnoresAnImagesOwnSentinel(t *testing.T) {
	// The sentinel is the store's own marker: a file of that name inside the
	// image must not end up describing the extraction.
	dir, s := t.TempDir(), Store{}
	stream := tarStream(t, map[string]string{
		"images/etcd.tar":        "e",
		"images/" + sentinelName: `{"digest":"sha256:evil","files":["etcd.tar"]}`,
	})
	if err := s.Extract(t.Context(), dir, "c", "sha256:abc", stream); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "c", sentinelName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "sha256:abc") {
		t.Errorf("sentinel not written by the store: %s", data)
	}
	if st, _ := s.State(dir, "c"); st != Complete {
		t.Errorf("state = %v, want Complete", st)
	}
}

func TestExtractConfinesHostileEntries(t *testing.T) {
	// Entries come from an image, so they are attacker-controlled. Extraction
	// keeps regular files only, by base name: nothing may land outside the
	// resource directory, and no symlink, directory or device may be created.
	// These are the invariants a future rewrite must not lose.
	dir, s := t.TempDir(), Store{}
	outside := filepath.Join(dir, "escaped.tar")
	stream := hostileTarStream(t,
		[]tar.Header{
			{Name: "../../escaped.tar", Mode: 0o644, Typeflag: tar.TypeReg},
			{Name: "/etc/passwd", Mode: 0o644, Typeflag: tar.TypeReg},
			{Name: "images/link.tar", Typeflag: tar.TypeSymlink, Linkname: "/etc/shadow"},
			{Name: "images/subdir", Mode: 0o755, Typeflag: tar.TypeDir},
			{Name: "images/dev", Mode: 0o644, Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3},
			{Name: "images/honest.tar", Mode: 0o644, Typeflag: tar.TypeReg},
		},
		[]string{"escaped", "root:x:0:0", "", "", "", "h"},
	)
	if err := s.Extract(t.Context(), dir, "c", "sha256:abc", stream); err != nil {
		t.Fatal(err)
	}

	// The traversing entries are flattened into the directory, not followed.
	if _, err := os.Lstat(outside); !os.IsNotExist(err) {
		t.Errorf("%q was created outside the resource directory", outside)
	}
	for _, name := range []string{"escaped.tar", "passwd", "honest.tar"} {
		if _, err := os.Lstat(filepath.Join(dir, "c", name)); err != nil {
			t.Errorf("%q missing from the resource directory: %v", name, err)
		}
	}
	// Non-regular entries are skipped outright.
	for _, name := range []string{"link.tar", "subdir", "dev"} {
		if _, err := os.Lstat(filepath.Join(dir, "c", name)); !os.IsNotExist(err) {
			t.Errorf("non-regular entry %q was created", name)
		}
	}
	if st, _ := s.State(dir, "c"); st != Complete {
		t.Errorf("state = %v, want Complete", st)
	}
}

func TestExtractStopsOnCancelledContext(t *testing.T) {
	// A cache image is hundreds of megabytes: a drained agent must stop
	// writing instead of finishing the payload.
	dir, s := t.TempDir(), Store{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := s.Extract(ctx, dir, "c", "sha256:abc", tarStream(t, map[string]string{testTar: "1"}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if st, _ := s.State(dir, "c"); st != Absent {
		t.Errorf("state = %v, want Absent: a cancelled extraction must leave nothing", st)
	}
}

func TestState(t *testing.T) {
	dir, s := t.TempDir(), Store{}
	if st, _ := s.State(dir, "none"); st != Absent {
		t.Errorf("missing dir: state = %v, want Absent", st)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bare"), 0o755); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.State(dir, "bare"); st != Incomplete {
		t.Errorf("no sentinel: state = %v, want Incomplete", st)
	}
	if err := s.Extract(t.Context(), dir, "x", "d", tarStream(t, map[string]string{testTar: "1"})); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "x", testTar)); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.State(dir, "x"); st != Incomplete {
		t.Errorf("missing listed file: state = %v, want Incomplete", st)
	}
}

func TestGCOwnershipRules(t *testing.T) {
	dir, s := t.TempDir(), Store{}
	if err := s.Extract(t.Context(), dir, "old", "d", tarStream(t, map[string]string{testTar: "1"})); err != nil {
		t.Fatal(err)
	}
	if err := s.Extract(t.Context(), dir, "kept", "d", tarStream(t, map[string]string{testTar: "1"})); err != nil {
		t.Fatal(err)
	}
	// Foreign directory (no sentinel) and flat file: must survive.
	if err := os.MkdirAll(filepath.Join(dir, "foreign"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "boot.tar"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Stale temporary directory: must be removed.
	if err := os.MkdirAll(filepath.Join(dir, ".old.tmp-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	removed, err := s.GC(dir, map[string]bool{"kept": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 { // "old" + stale tmp
		t.Errorf("removed = %v, want [old and .old.tmp-1]", removed)
	}
	for _, still := range []string{"kept", "foreign", "boot.tar"} {
		if _, err := os.Stat(filepath.Join(dir, still)); err != nil {
			t.Errorf("%s must survive GC: %v", still, err)
		}
	}
}

func TestGCOnMissingPathIsNoop(t *testing.T) {
	if _, err := (Store{}).GC("/does/not/exist", nil); err != nil {
		t.Fatal(err)
	}
}

func TestStateCorruptSentinel(t *testing.T) {
	dir, s := t.TempDir(), Store{}
	if err := s.Extract(t.Context(), dir, "c", "d", tarStream(t, map[string]string{testTar: "1"})); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "c", sentinelName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := s.State(dir, "c")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st != Incomplete {
		t.Errorf("state = %v, want Incomplete", st)
	}
}

func TestExtractReplacesExistingDir(t *testing.T) {
	dir, s := t.TempDir(), Store{}
	if err := s.Extract(t.Context(), dir, "r", digestD1, tarStream(t, map[string]string{oldTarName: "1"})); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.State(dir, "r"); st != Complete {
		t.Fatalf("first extraction: state = %v, want Complete", st)
	}
	if err := s.Extract(t.Context(), dir, "r", digestD2, tarStream(t, map[string]string{newTarName: "2"})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "r", newTarName)); err != nil {
		t.Errorf("missing %s: %v", newTarName, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "r", oldTarName)); !os.IsNotExist(err) {
		t.Errorf("%s must be gone after replacement", oldTarName)
	}
	if st, _ := s.State(dir, "r"); st != Complete {
		t.Errorf("second extraction: state = %v, want Complete", st)
	}
	data, err := os.ReadFile(filepath.Join(dir, "r", sentinelName))
	if err != nil {
		t.Fatal(err)
	}
	var sn sentinel
	if err := json.Unmarshal(data, &sn); err != nil {
		t.Fatal(err)
	}
	if sn.Digest != digestD2 {
		t.Errorf("sentinel digest = %q, want %q", sn.Digest, digestD2)
	}
}
