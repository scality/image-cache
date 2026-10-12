package cache

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// testTar is the file name used by fixtures that only need a single file.
const testTar = "a.tar"

// otherOwner is a writer that is not the agent, the command for one.
const otherOwner = "imagecachectl"

// testDigest is the manifest digest fixtures record when it does not matter.
const testDigest = "sha256:abc"

// Archive paths as a cache image carries them, under a directory, and the
// content fixtures give the first.
const (
	etcdEntry  = "images/etcd.tar"
	pauseEntry = "images/pause.tar"
	etcdBody   = "etcd"
)

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
	stream := tarStream(t, map[string]string{etcdEntry: "e", pauseEntry: "p"})
	if err := s.Extract(t.Context(), dir, "worker-134-0-0", Record{Digest: testDigest}, stream); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"etcd.tar", "pause.tar", SentinelName} {
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
	if err := s.Extract(t.Context(), dir, "c", Record{Digest: testDigest}, stream); err == nil {
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
		etcdEntry:                "e",
		"images/" + SentinelName: `{"digest":"sha256:evil","files":["etcd.tar"]}`,
	})
	if err := s.Extract(t.Context(), dir, "c", Record{Digest: testDigest}, stream); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "c", SentinelName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), testDigest) {
		t.Errorf("sentinel not written by the store: %s", data)
	}
	if st, _ := s.State(dir, "c"); st != Complete {
		t.Errorf("state = %v, want Complete", st)
	}
}

func TestExtractConfinesHostileEntries(t *testing.T) {
	// Entries come from an image, so they are attacker-controlled. Extraction
	// keeps regular files only, by base name: nothing may land outside the
	// resource directory, and a directory entry creates nothing. Links and
	// devices are refused outright, see TestExtractRefusesNonRegularEntries.
	// These are the invariants a future rewrite must not lose.
	dir, s := t.TempDir(), Store{}
	outside := filepath.Join(dir, "escaped.tar")
	stream := hostileTarStream(t,
		[]tar.Header{
			{Name: "../../escaped.tar", Mode: 0o644, Typeflag: tar.TypeReg},
			{Name: "/etc/passwd", Mode: 0o644, Typeflag: tar.TypeReg},
			{Name: "images/subdir", Mode: 0o755, Typeflag: tar.TypeDir},
			{Name: "images/honest.tar", Mode: 0o644, Typeflag: tar.TypeReg},
		},
		[]string{"escaped", "root:x:0:0", "", "h"},
	)
	if err := s.Extract(t.Context(), dir, "c", Record{Digest: testDigest}, stream); err != nil {
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
	// A directory entry is only a container: nothing is created for it.
	if _, err := os.Lstat(filepath.Join(dir, "c", "subdir")); !os.IsNotExist(err) {
		t.Error("the directory entry was created")
	}
	if st, _ := s.State(dir, "c"); st != Complete {
		t.Errorf("state = %v, want Complete", st)
	}
}

// An entry whose name has no file name to land under is refused whole, and
// said to be what it is. Opening it would hit the temporary directory or its
// parent: nothing escapes, but the failure has to name it, not read as a
// duplicate.
func TestExtractRefusesAnEntryWithNoFileName(t *testing.T) {
	for _, name := range []string{"", ".", "..", "images/.", "images/.."} {
		dir, s := t.TempDir(), Store{}
		stream := hostileTarStream(t,
			[]tar.Header{
				{Name: etcdEntry, Mode: 0o644, Typeflag: tar.TypeReg},
				{Name: name, Mode: 0o644, Typeflag: tar.TypeReg},
			},
			[]string{etcdBody, "x"},
		)
		err := s.Extract(t.Context(), dir, "c", Record{Digest: "d"}, stream)
		if err == nil || !strings.Contains(err.Error(), "has no file name") {
			t.Errorf("%q: err = %v, want it refused for having no file name", name, err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%q: the refusal left %v behind", name, entries)
		}
	}
}

// Two entry types look unusual and are not a reason to refuse an image. A
// global header describes the archive rather than any file in it, and the
// reader returns it as a header of its own. A contiguous file is a regular
// file with a legacy type byte. Refusing either would turn away an image that
// carries everything it was built to.
func TestExtractAcceptsGlobalHeadersAndContiguousFiles(t *testing.T) {
	dir, s := t.TempDir(), Store{}
	stream := hostileTarStream(t,
		[]tar.Header{
			{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "c0ffee"}},
			{Name: etcdEntry, Mode: 0o644, Typeflag: tar.TypeCont},
			{Name: pauseEntry, Mode: 0o644, Typeflag: tar.TypeReg},
		},
		[]string{"", etcdBody, "pause"},
	)
	if err := s.Extract(t.Context(), dir, "c", Record{Digest: "d"}, stream); err != nil {
		t.Fatalf("a legitimate image was refused: %v", err)
	}
	for name, want := range map[string]string{"etcd.tar": etcdBody, "pause.tar": "pause"} {
		got, err := os.ReadFile(filepath.Join(dir, "c", name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "c", "pax_global_header")); !os.IsNotExist(err) {
		t.Error("the global header was written out as a file")
	}
}

// A link or a device in the image cannot be taken as a file, and skipping it
// would publish a resource short of an archive while the sentinel calls it
// complete. docker build copies a symbolic link as it finds it, dangling when
// it pointed outside the build context, so the node would be labelled synced
// without the image. The whole image is refused, naming the entry, and nothing
// is left behind: no link, no partial directory, no temporary.
func TestExtractRefusesNonRegularEntries(t *testing.T) {
	for _, tc := range []struct {
		entry tar.Header
		kind  string
	}{
		{tar.Header{Name: pauseEntry, Typeflag: tar.TypeSymlink, Linkname: "../elsewhere/pause.tar"}, "a symbolic link"},
		{tar.Header{Name: "images/etcd-copy.tar", Typeflag: tar.TypeLink, Linkname: etcdEntry}, "a hard link"},
		{tar.Header{Name: "images/dev", Mode: 0o644, Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}, "a device"},
		{tar.Header{Name: "images/pipe", Mode: 0o644, Typeflag: tar.TypeFifo}, "a named pipe"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			dir, s := t.TempDir(), Store{}
			// A regular file first, so the refusal comes with something
			// already written that has to be taken back.
			stream := hostileTarStream(t,
				[]tar.Header{
					{Name: etcdEntry, Mode: 0o644, Typeflag: tar.TypeReg},
					tc.entry,
				},
				[]string{etcdBody, ""},
			)

			err := s.Extract(t.Context(), dir, "c", Record{Digest: "d"}, stream)
			if err == nil {
				t.Fatal("an image carrying " + tc.kind + " was accepted")
			}
			if !errors.Is(err, ErrExtract) {
				t.Errorf("err = %v, want ErrExtract", err)
			}
			if !strings.Contains(err.Error(), tc.entry.Name) || !strings.Contains(err.Error(), tc.kind) {
				t.Errorf("err = %v, want it to name %q as %s", err, tc.entry.Name, tc.kind)
			}
			if st, _ := s.State(dir, "c"); st != Absent {
				t.Errorf("state = %v, want Absent", st)
			}
			entries, rerr := os.ReadDir(dir)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if len(entries) != 0 {
				t.Errorf("the refusal left %v behind", entries)
			}
		})
	}
}

func TestExtractStopsOnCancelledContext(t *testing.T) {
	// A cache image is hundreds of megabytes: a drained agent must stop
	// writing instead of finishing the payload.
	dir, s := t.TempDir(), Store{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := s.Extract(ctx, dir, "c", Record{Digest: testDigest}, tarStream(t, map[string]string{testTar: "1"}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if st, _ := s.State(dir, "c"); st != Absent {
		t.Errorf("state = %v, want Absent: a cancelled extraction must leave nothing", st)
	}
}

// cancelAfter cancels a context once n bytes have been read through it, and
// counts what was read in total.
type cancelAfter struct {
	r      io.Reader
	n      int64
	read   int64
	cancel context.CancelFunc
}

func (c *cancelAfter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.read >= c.n {
		c.cancel()
	}
	return n, err
}

// Cancellation stops the copy of an entry, not only the loop between entries.
func TestExtractStopsInTheMiddleOfAnEntry(t *testing.T) {
	const size = 64 << 20
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		if err := tw.WriteHeader(&tar.Header{Name: testTar, Mode: 0o644, Size: size}); err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.CopyN(tw, zeros{}, size); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.CloseWithError(tw.Close())
	}()
	t.Cleanup(func() { _ = pr.Close() })

	dir, s := t.TempDir(), Store{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	src := &cancelAfter{r: pr, n: 1 << 20, cancel: cancel}

	err := s.Extract(ctx, dir, "c", Record{Digest: testDigest}, src)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if src.read >= size {
		t.Errorf("read %d bytes, the whole entry: cancellation waited for its end", src.read)
	}
	if st, _ := s.State(dir, "c"); st != Absent {
		t.Errorf("state = %v, want Absent", st)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
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
	if err := s.Extract(t.Context(), dir, "x", Record{Digest: "d"}, tarStream(t, map[string]string{testTar: "1"})); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "x", testTar)); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.State(dir, "x"); st != Incomplete {
		t.Errorf("missing listed file: state = %v, want Incomplete", st)
	}
	// What is not a directory holds no resource, and is no error either.
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop", filepath.Join(dir, "loop")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"file", "loop"} {
		if st, err := s.State(dir, name); err != nil || st != Incomplete {
			t.Errorf("%s: state = %v, err = %v, want Incomplete", name, st, err)
		}
	}
}

// Garbage collection removes the agent's directories and the interrupted
// extractions no resource keeps. Flat files stay, the preload service restores
// from them, and so does lost+found when the cache path is a filesystem of its
// own.
func TestGCRemovesWhatNoResourceKeeps(t *testing.T) {
	dir, s := t.TempDir(), Store{}
	if err := s.Extract(t.Context(), dir, "old", Record{Digest: "d", Owner: OwnerAgent}, tarStream(t, map[string]string{testTar: "1"})); err != nil {
		t.Fatal(err)
	}
	if err := s.Extract(t.Context(), dir, "kept", Record{Digest: "d", Owner: OwnerAgent}, tarStream(t, map[string]string{testTar: "1"})); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{".old.tmp-1", lostFound} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The command was killed after it wrote the sentinel in its temporary:
	// still an interrupted extraction, not a seeded directory.
	seededTmp := filepath.Join(dir, ".seeded.tmp-1")
	if err := os.MkdirAll(seededTmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seededTmp, SentinelName), []byte(`{"owner":"`+otherOwner+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "boot.tar"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := s.GC(dir, map[string]bool{"kept": true})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(removed)
	if want := []string{".old.tmp-1", ".seeded.tmp-1", "old"}; !slices.Equal(removed, want) {
		t.Errorf("removed = %v, want %v", removed, want)
	}
	for _, name := range []string{"kept", "boot.tar", lostFound} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s must survive GC: %v", name, err)
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
	if err := s.Extract(t.Context(), dir, "c", Record{Digest: "d"}, tarStream(t, map[string]string{testTar: "1"})); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "c", SentinelName), []byte("{not json"), 0o644); err != nil {
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
	if err := s.Extract(t.Context(), dir, "r", Record{Digest: digestD1}, tarStream(t, map[string]string{oldTarName: "1"})); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.State(dir, "r"); st != Complete {
		t.Fatalf("first extraction: state = %v, want Complete", st)
	}
	if err := s.Extract(t.Context(), dir, "r", Record{Digest: digestD2}, tarStream(t, map[string]string{newTarName: "2"})); err != nil {
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
	data, err := os.ReadFile(filepath.Join(dir, "r", SentinelName))
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

// The command runs as root, and a name is not a claim on whatever happens to
// sit under it: a resource named after a neighbour of the cache path would
// otherwise make the swap an rm -rf of somebody else's data.
func TestExtractRefusesADirectoryItDidNotWrite(t *testing.T) {
	dir, s := t.TempDir(), Store{}
	foreign := filepath.Join(dir, "containers")
	if err := os.MkdirAll(filepath.Join(foreign, "storage"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(foreign, "storage", "data.db")
	if err := os.WriteFile(data, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := s.Extract(t.Context(), dir, "containers", Record{Digest: "d"},
		tarStream(t, map[string]string{testTar: "1"}))
	if err == nil {
		t.Fatal("extraction was allowed over a directory without the sentinel")
	}
	if !errors.Is(err, ErrExtract) {
		t.Errorf("err = %v, want ErrExtract", err)
	}
	got, rerr := os.ReadFile(data)
	if rerr != nil {
		t.Fatalf("the foreign data was destroyed: %v", rerr)
	}
	if string(got) != "not ours" {
		t.Errorf("the foreign data was rewritten: %q", got)
	}
}

// A file where the directory would go is refused the same way: os.RemoveAll
// would have taken it without a word.
func TestExtractRefusesAFileWhereTheDirectoryGoes(t *testing.T) {
	dir, s := t.TempDir(), Store{}
	occupied := filepath.Join(dir, "taken")
	if err := os.WriteFile(occupied, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.Extract(t.Context(), dir, "taken", Record{Digest: "d"},
		tarStream(t, map[string]string{testTar: "1"})); err == nil {
		t.Fatal("extraction was allowed over a regular file")
	}
	if got, err := os.ReadFile(occupied); err != nil || string(got) != "not ours" {
		t.Errorf("the file was replaced: %q, %v", got, err)
	}
}

// An extraction killed outright leaves a temporary directory the size of the
// image. On a node with no agent, nothing else would ever remove it.
func TestSweepTemporariesRemovesOnlyThisResourcesLeftovers(t *testing.T) {
	dir, s := t.TempDir(), Store{}
	mine, err := os.MkdirTemp(dir, ".worker.tmp-")
	if err != nil {
		t.Fatal(err)
	}
	other, err := os.MkdirTemp(dir, ".control-plane.tmp-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "worker"), 0o755); err != nil {
		t.Fatal(err)
	}

	removed, err := s.SweepTemporaries(dir, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != filepath.Base(mine) {
		t.Errorf("removed = %v, want only %s", removed, filepath.Base(mine))
	}
	if _, err := os.Stat(mine); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("its own leftover survived: %v", err)
	}
	// A run for another resource may be in flight, and the finished directory
	// is not a leftover.
	for _, keep := range []string{other, filepath.Join(dir, "worker")} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s was swept: %v", keep, err)
		}
	}
}

// A carrier with nothing in it is refused rather than published. The sentinel
// would say the resource is complete, the node would be labelled synced, and
// it would hold none of the archives the resource was meant to give it.
func TestExtractRefusesAnImageWithNothingToCache(t *testing.T) {
	dir, s := t.TempDir(), Store{}

	err := s.Extract(t.Context(), dir, "empty", Record{Digest: "d"}, tarStream(t, map[string]string{}))
	if err == nil {
		t.Fatal("an image carrying no file was accepted")
	}
	if !strings.Contains(err.Error(), "no file to cache") {
		t.Errorf("err = %v, want it to say what was found", err)
	}
	if st, _ := s.State(dir, "empty"); st != Absent {
		t.Errorf("state = %v, want Absent: the refusal left something behind", st)
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Errorf("the cache path is not empty after a refusal: %v", entries)
	}
}

// eofAfter returns r's content, then fails with an error that wraps io.EOF,
// the way a registry closing the connection before a layer comes back:
// Get "...": EOF.
type eofAfter struct{ r io.Reader }

func (e eofAfter) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if errors.Is(err, io.EOF) {
		return n, fmt.Errorf(`Get "https://registry.example/v2/boot-cache/blobs/sha256:0": %w`, io.EOF)
	}
	return n, err
}

// A failure that wraps io.EOF is not the end of the archive. Taken for it,
// the extraction published what had arrived so far as a complete resource:
// the first layer's archives, and nothing of the layers after it.
func TestExtractRefusesAStreamEndingInAWrappedEOF(t *testing.T) {
	dir, s := t.TempDir(), Store{}
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	if err := tw.WriteHeader(&tar.Header{Name: etcdEntry, Mode: 0o644, Typeflag: tar.TypeReg, Size: int64(len(etcdBody))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(etcdBody)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	// No trailer: the stream fails right after the first entry.

	err := s.Extract(t.Context(), dir, "c", Record{Digest: "d"}, eofAfter{buf})
	if err == nil {
		t.Fatal("a stream that failed was published as complete")
	}
	if st, _ := s.State(dir, "c"); st != Absent {
		t.Errorf("state = %v, want Absent", st)
	}
}

// The sentinel remembers who wrote the directory, from what, and which image
// it holds, next to the files it lists.
func TestExtractRecordsWhoWroteTheDirectoryAndFromWhat(t *testing.T) {
	dir, s := t.TempDir(), Store{}
	rec := Record{Owner: otherOwner, Source: "/mnt/iso/boot-cache.tar", Digest: "sha256:manifest", Layers: []string{"sha256:layer"}}
	if err := s.Extract(t.Context(), dir, "c", rec, tarStream(t, map[string]string{testTar: "1"})); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "c", SentinelName))
	if err != nil {
		t.Fatal(err)
	}
	var sn sentinel
	if err := json.Unmarshal(data, &sn); err != nil {
		t.Fatal(err)
	}
	got := sn.Record
	if !reflect.DeepEqual(got, rec) {
		t.Errorf("sentinel = %+v, want %+v", got, rec)
	}
}

// A seeded directory has to survive garbage collection until a resource
// claims it: at install the agent can land before the resources.
func TestGCLeavesADirectoryAnotherWriterOwns(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sentinel string
		kept     bool
	}{
		{"seeded by the command", `{"digest":"d","files":[],"owner":"` + otherOwner + `"}`, true},
		{"written by the agent", `{"digest":"d","files":[],"owner":"` + OwnerAgent + `"}`, false},
		{"naming no owner", `{"digest":"d","files":[]}`, true},
		{"a sentinel that does not parse", `{not json`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, s := t.TempDir(), Store{}
			res := filepath.Join(dir, "worker-134-0-0")
			if err := os.MkdirAll(res, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(res, SentinelName), []byte(tc.sentinel), 0o644); err != nil {
				t.Fatal(err)
			}

			removed, err := s.GC(dir, map[string]bool{})
			if err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(res)
			if kept := statErr == nil; kept != tc.kept {
				t.Errorf("kept = %v, want %v (removed %v)", kept, tc.kept, removed)
			}
		})
	}
}

// Adopting a seeded directory changes its owner and nothing else: the files,
// what the sentinel lists, and where the content came from stay as the
// command wrote them, and the directory still reads complete.
func TestAdoptOnlyChangesTheOwner(t *testing.T) {
	dir, s := t.TempDir(), Store{}
	seeded := Record{Owner: otherOwner, Source: "/mnt/iso/boot-cache.tar", Digest: "sha256:manifest", Layers: []string{"sha256:layer"}}
	if err := s.Extract(t.Context(), dir, "c", seeded, tarStream(t, map[string]string{testTar: "1"})); err != nil {
		t.Fatal(err)
	}
	if err := s.Adopt(dir, "c"); err != nil {
		t.Fatal(err)
	}

	got, err := s.ReadRecord(dir, "c")
	if err != nil {
		t.Fatal(err)
	}
	want := seeded
	want.Owner = OwnerAgent
	if !reflect.DeepEqual(got, want) {
		t.Errorf("record = %+v, want %+v", got, want)
	}
	if st, err := s.State(dir, "c"); err != nil || st != Complete {
		t.Errorf("state = %v, %v; want Complete", st, err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "c", testTar)); err != nil || string(data) != "1" {
		t.Errorf("content = %q, %v; want it untouched", data, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "c", SentinelName+".tmp")); !os.IsNotExist(err) {
		t.Errorf("the temporary sentinel is left behind: %v", err)
	}
}

// What sits at a resource's path with no sentinel, the sentinel deleted by
// hand for instance, is replaced by the agent and left alone by any other
// writer. A link goes, not what it points to.
func TestExtractReplacesWhatHasNoSentinelOnlyForTheAgent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		owner    string
		place    func(t *testing.T, res string)
		replaced bool
	}{
		{"agent, a directory", OwnerAgent, placeDir, true},
		{"agent, a link", OwnerAgent, placeLink, true},
		{"agent, a file", OwnerAgent, placeFile, true},
		{"another writer, a file", otherOwner, placeFile, false},
		{"another writer, a directory", otherOwner, placeDir, false},
		{"another writer, a link", otherOwner, placeLink, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, s := t.TempDir(), Store{}
			res := filepath.Join(dir, "worker-1-0-0")
			tc.place(t, res)

			err := s.Extract(t.Context(), dir, "worker-1-0-0", Record{Digest: "d", Owner: tc.owner},
				tarStream(t, map[string]string{testTar: "new"}))
			got, _ := os.ReadFile(filepath.Join(res, testTar))
			if tc.replaced {
				if err != nil || string(got) != "new" {
					t.Errorf("err = %v, content = %q; want the path replaced", err, got)
				}
			} else if !errors.Is(err, ErrExtract) || !strings.Contains(err.Error(), "left alone") {
				t.Errorf("err = %v, want ErrExtract leaving the path alone", err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "outside-"+filepath.Base(dir), "data")); err != nil {
				t.Errorf("what the link points to was touched: %v", err)
			}
		})
	}
}

// placeDir puts a directory with no sentinel at res, holding another file and
// a subdirectory.
func placeDir(t *testing.T, res string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(res, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(res, "Packages"), []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}
	placeOutside(t, res)
}

// placeLink puts at res a link to a directory outside the cache path.
func placeLink(t *testing.T, res string) {
	t.Helper()
	if err := os.Symlink(placeOutside(t, res), res); err != nil {
		t.Fatal(err)
	}
}

// placeFile puts a regular file at res.
func placeFile(t *testing.T, res string) {
	t.Helper()
	if err := os.WriteFile(res, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	placeOutside(t, res)
}

// placeOutside writes data in a directory next to the cache path of res, and
// returns that directory.
func placeOutside(t *testing.T, res string) string {
	t.Helper()
	dir := filepath.Dir(res)
	outside := filepath.Join(filepath.Dir(dir), "outside-"+filepath.Base(dir))
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "data"), []byte("d"), 0o644); err != nil {
		t.Fatal(err)
	}
	return outside
}

// There is nothing to adopt, or to read, where no sentinel is.
func TestAdoptAndReadRecordNeedASentinel(t *testing.T) {
	dir, s := t.TempDir(), Store{}
	if err := s.Adopt(dir, "missing"); !errors.Is(err, ErrAdopt) {
		t.Errorf("Adopt = %v, want ErrAdopt", err)
	}
	if _, err := s.ReadRecord(dir, "missing"); !errors.Is(err, ErrState) {
		t.Errorf("ReadRecord = %v, want ErrState", err)
	}
}

// An unreadable sentinel is reported with its cause, not as a missing one.
func TestReplaceableTellsAnUnreadableSentinelFromAMissingOne(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads the sentinel whatever the mode")
	}
	dir, s := t.TempDir(), Store{}
	res := filepath.Join(dir, "c")
	if err := os.MkdirAll(res, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(res, SentinelName), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(res, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(res, 0o755) })

	err := s.Replaceable(dir, "c", OwnerAgent)
	if !errors.Is(err, ErrExtract) || !errors.Is(err, os.ErrPermission) {
		t.Errorf("err = %v, want ErrExtract caused by a permission error", err)
	}
}

// A sentinel that cannot be read may still name another writer: the directory
// is kept, and the failure reported, rather than removed on a doubt.
func TestGCKeepsADirectoryWhoseSentinelCannotBeRead(t *testing.T) {
	dir, s := t.TempDir(), Store{}
	res := filepath.Join(dir, "worker-134-0-0")
	// A sentinel that is a directory cannot be read, even by root.
	if err := os.MkdirAll(filepath.Join(res, SentinelName), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := s.GC(dir, map[string]bool{})
	if !errors.Is(err, ErrGC) {
		t.Errorf("err = %v, want ErrGC", err)
	}
	if _, err := os.Stat(res); err != nil {
		t.Errorf("the directory must survive: %v", err)
	}
}

// Anything image-cache did not write goes: a directory with no sentinel, one
// holding a subdirectory, a link. Removing a link leaves what it points to.
func TestGCRemovesWhatImageCacheDidNotWrite(t *testing.T) {
	dir, s := t.TempDir(), Store{}
	if err := os.MkdirAll(filepath.Join(dir, "toto", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "toto", "sub", "data"), []byte("d"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "data"), []byte("d"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	removed, err := s.GC(dir, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"link", "toto"}; !slices.Equal(removed, want) {
		t.Errorf("removed = %v, want %v", removed, want)
	}
	for _, p := range []string{"toto", "link"} {
		if _, err := os.Lstat(filepath.Join(dir, p)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived GC: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "data")); err != nil {
		t.Errorf("the link target was touched: %v", err)
	}
}

// An entry GC cannot remove is reported, and the others still go.
func TestGCReportsWhatItCannotRemoveAndGoesOn(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes whatever the mode")
	}
	dir, s := t.TempDir(), Store{}
	stuck := filepath.Join(dir, "stuck")
	if err := os.MkdirAll(stuck, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "data"), []byte("d"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stuck, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o755) })
	if err := os.Mkdir(filepath.Join(dir, "stray"), 0o755); err != nil {
		t.Fatal(err)
	}

	removed, err := s.GC(dir, map[string]bool{})
	if !errors.Is(err, ErrGC) || !errors.Is(err, os.ErrPermission) {
		t.Errorf("err = %v, want ErrGC caused by a permission error", err)
	}
	if want := []string{"stray"}; !slices.Equal(removed, want) {
		t.Errorf("removed = %v, want %v", removed, want)
	}
}

// onFirstRead runs fn once, on the first read, while Extract is writing its
// temporary directory.
type onFirstRead struct {
	r    io.Reader
	fn   func()
	done bool
}

func (o *onFirstRead) Read(p []byte) (int, error) {
	if !o.done {
		o.done = true
		o.fn()
	}
	return o.r.Read(p)
}

// An import run by hand on a node where the agent runs: the agent's pass, or
// another run for the same resource, must not remove the directory being
// written.
func TestGCAndSweepLeaveATemporaryBeingWritten(t *testing.T) {
	dir, s := t.TempDir(), Store{}
	var removed []string
	stream := &onFirstRead{r: tarStream(t, map[string]string{etcdEntry: etcdBody}), fn: func() {
		gced, err := s.GC(dir, nil)
		if err != nil {
			t.Error(err)
		}
		swept, err := s.SweepTemporaries(dir, "worker")
		if err != nil {
			t.Error(err)
		}
		removed = append(gced, swept...)
	}}
	if err := s.Extract(t.Context(), dir, "worker", Record{Owner: otherOwner}, stream); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Errorf("removed %v while it was written", removed)
	}
	if st, _ := s.State(dir, "worker"); st != Complete {
		t.Errorf("state = %v, want Complete", st)
	}
}

func TestGCRemovesATemporaryOnceItsWriterIsGone(t *testing.T) {
	dir, s := t.TempDir(), Store{}
	tmp, err := os.MkdirTemp(dir, ".worker.tmp-")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := lockTemporary(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := s.GC(dir, nil); err != nil || len(removed) != 0 {
		t.Fatalf("locked: removed = %v, err = %v, want nothing", removed, err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	removed, err := s.GC(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != filepath.Base(tmp) {
		t.Errorf("unlocked: removed = %v, want %s", removed, filepath.Base(tmp))
	}
}
