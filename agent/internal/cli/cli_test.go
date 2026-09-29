package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/scality/image-cache/agent/internal/cache"
)

const (
	resourceName = "worker-1-0-0"
	importCmd    = "import"
	nameFlag     = "--name"
	etcdTarPath  = "images/etcd.tar"
	pauseTarPath = "images/pause.tar"
	sentinelName = ".image-cache-agent.json"
)

// archive writes a docker archive shaped like a boot cache image.
func archive(t *testing.T, files map[string][]byte) string {
	t.Helper()
	img, err := crane.Image(files)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "boot-cache.tar")
	if err := crane.Save(img, "boot-cache/worker:1.0.0", path); err != nil {
		t.Fatal(err)
	}
	return path
}

// served pushes the same shape to an in-memory registry and returns its reference.
func served(t *testing.T, files map[string][]byte) string {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	img, err := crane.Image(files)
	if err != nil {
		t.Fatal(err)
	}
	ref := strings.TrimPrefix(srv.URL, "http://") + "/boot-cache/worker:1.0.0"
	if err := crane.Push(img, ref); err != nil {
		t.Fatal(err)
	}
	return ref
}

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut strings.Builder
	code := Run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestImportFromAnArchive(t *testing.T) {
	cacheDir := t.TempDir()
	src := archive(t, map[string][]byte{etcdTarPath: []byte("etcd")})

	code, _, errOut := run(t, importCmd, nameFlag, resourceName, "--cache-path", cacheDir, src)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (%s)", code, errOut)
	}
	got, err := os.ReadFile(filepath.Join(cacheDir, resourceName, "etcd.tar"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "etcd" {
		t.Errorf("etcd.tar = %q, want %q", got, "etcd")
	}
	if _, err := os.Stat(filepath.Join(cacheDir, resourceName, sentinelName)); err != nil {
		t.Errorf("no sentinel written: %v", err)
	}
}

func TestImportFromARegistry(t *testing.T) {
	cacheDir := t.TempDir()
	ref := served(t, map[string][]byte{pauseTarPath: []byte("pause")})

	code, _, errOut := run(t, importCmd, nameFlag, resourceName, "--cache-path", cacheDir, ref)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (%s)", code, errOut)
	}
	got, err := os.ReadFile(filepath.Join(cacheDir, resourceName, "pause.tar"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "pause" {
		t.Errorf("pause.tar = %q, want %q", got, "pause")
	}
	if _, err := os.Stat(filepath.Join(cacheDir, resourceName, sentinelName)); err != nil {
		t.Errorf("no sentinel written: %v", err)
	}
}

// Once a resource is complete the command is a no-op, whatever the source
// says. That is what makes it safe to call on every convergence: a node that
// holds its archives never reaches the registry again.
func TestSecondRunTouchesNothing(t *testing.T) {
	cacheDir := t.TempDir()
	src := archive(t, map[string][]byte{etcdTarPath: []byte("etcd")})
	if code, _, errOut := run(t, importCmd, nameFlag, resourceName, "--cache-path", cacheDir, src); code != 0 {
		t.Fatalf("first run: exit = %d (%s)", code, errOut)
	}

	// An address nothing answers on: reaching it would fail the run.
	dead := "127.0.0.1:1/boot-cache/worker:1.0.0"
	code, out, errOut := run(t, importCmd, nameFlag, resourceName, "--cache-path", cacheDir, dead)
	if code != 0 {
		t.Fatalf("second run: exit = %d, want 0 (%s)", code, errOut)
	}
	if !strings.Contains(out, "already") {
		t.Errorf("second run said %q, want it to report the cache already holds it", out)
	}
}

func TestNameIsRequired(t *testing.T) {
	src := archive(t, map[string][]byte{etcdTarPath: []byte("etcd")})
	code, _, errOut := run(t, importCmd, "--cache-path", t.TempDir(), src)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut, "name") {
		t.Errorf("stderr = %q, want it to name the missing flag", errOut)
	}
}

func TestExactlyOneSource(t *testing.T) {
	for _, args := range [][]string{
		{importCmd, nameFlag, resourceName},
		{importCmd, nameFlag, resourceName, "one.tar", "two.tar"},
	} {
		if code, _, _ := run(t, args...); code != 2 {
			t.Errorf("%v: exit = %d, want 2", args, code)
		}
	}
}

func TestMissingCachePathIsNamed(t *testing.T) {
	src := archive(t, map[string][]byte{etcdTarPath: []byte("etcd")})
	absent := filepath.Join(t.TempDir(), "not-mounted")

	code, _, errOut := run(t, importCmd, nameFlag, resourceName, "--cache-path", absent, src)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, absent) {
		t.Errorf("stderr = %q, want it to name %q", errOut, absent)
	}
}

// A source that looks like a path is one: falling back to the registry would
// turn a typo into "could not resolve reference", which sends the reader
// looking at the network instead of at the filename.
func TestMissingArchiveIsNotTreatedAsAReference(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent.tar")
	code, _, errOut := run(t, importCmd, nameFlag, resourceName, "--cache-path", t.TempDir(), absent)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, absent) {
		t.Errorf("stderr = %q, want it to name the archive", errOut)
	}
}

func TestUnknownFlag(t *testing.T) {
	code, _, errOut := run(t, importCmd, "--nope", nameFlag, resourceName, "x.tar")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut, "nope") {
		t.Errorf("stderr = %q, want it to name the flag", errOut)
	}
}

// Something already sitting where the resource directory belongs is worth a
// message of its own: reporting it as an empty cache would send the reader
// back to the registry for a problem that is on the disk.
func TestUnreadableResourceState(t *testing.T) {
	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, resourceName), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := archive(t, map[string][]byte{etcdTarPath: []byte("etcd")})

	code, _, errOut := run(t, importCmd, nameFlag, resourceName, "--cache-path", cacheDir, src)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, resourceName) {
		t.Errorf("stderr = %q, want it to name the resource", errOut)
	}
}

// go-errors renders JSON under %v and a sentence under %s, and says so: the
// message form is what a CLI prints. A blob on a terminal, or in the output
// whatever the caller captures, reads as a bug in the command rather than a
// missing file.
func TestErrorsReadAsSentences(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent.tar")
	_, _, errOut := run(t, importCmd, nameFlag, resourceName, "--cache-path", t.TempDir(), absent)

	if strings.Contains(errOut, `{"`) {
		t.Errorf("stderr is structured, not a message: %q", errOut)
	}
	if !strings.Contains(errOut, "pulling the image failed") {
		t.Errorf("stderr = %q, want the error's own sentence", errOut)
	}
}

// The store takes the name on trust and joins it to the cache path, so a
// separator or a dot walks out of the directory. The command runs as root on
// a node, where `--name ..` means os.RemoveAll("/var/lib").
func TestNameCannotEscapeTheCachePath(t *testing.T) {
	// Never opened: every name below is refused before the source is read.
	// Built once all the same, so that a name slipping through fails on the
	// extraction rather than on a source that was not there either.
	src := archive(t, map[string][]byte{etcdTarPath: []byte("etcd")})

	for _, name := range []string{"..", ".", "a/b", "../elsewhere", "UPPER", strings.Repeat("x", 254)} {
		root := t.TempDir()
		cacheDir := filepath.Join(root, "cache")
		if err := os.Mkdir(cacheDir, 0o755); err != nil {
			t.Fatal(err)
		}
		witness := filepath.Join(root, "witness")
		if err := os.WriteFile(witness, []byte("keep me"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, _, errOut := run(t, importCmd, nameFlag, name, "--cache-path", cacheDir, src)
		if code != 2 {
			t.Errorf("--name %q: exit = %d, want 2 (%s)", name, code, errOut)
		}
		if _, err := os.Stat(witness); err != nil {
			t.Errorf("--name %q: destroyed %s", name, witness)
		}
		if _, err := os.Stat(cacheDir); err != nil {
			t.Errorf("--name %q: destroyed the cache path", name)
		}
	}
}

// The source is read by its shape and never by what is on disk. A file named
// exactly like a reference, in whatever directory this runs from, must not
// stand in for the image: the command runs as root during provisioning.
//
// The registry is the in-memory one, so the run reaches nothing outside the
// test and the assertion is on what landed in the cache, not on which error
// came back.
func TestASourceShapedLikeAReferenceIsNotReadFromDisk(t *testing.T) {
	cacheDir := t.TempDir()
	cwd := t.TempDir()
	ref := served(t, map[string][]byte{etcdTarPath: []byte("from the registry")})

	// A file whose path, relative to where the command runs, is exactly the
	// reference.
	decoy := filepath.Join(cwd, filepath.FromSlash(ref))
	if err := os.MkdirAll(filepath.Dir(decoy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(decoy, []byte("not the image"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)

	code, _, errOut := run(t, importCmd, nameFlag, resourceName, "--cache-path", cacheDir, ref)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (%s)", code, errOut)
	}
	got, err := os.ReadFile(filepath.Join(cacheDir, resourceName, "etcd.tar"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from the registry" {
		t.Errorf("the file in the working directory was read instead of the reference: %q", got)
	}
}

// And an archive whose name looks like neither is given as a path, which is
// what the usage tells the caller to do.
func TestAnArchiveGivenAsAPathIsReadAsOne(t *testing.T) {
	cacheDir := t.TempDir()
	src := archive(t, map[string][]byte{etcdTarPath: []byte("etcd")})
	odd := filepath.Join(filepath.Dir(src), "boot-cache")
	if err := os.Rename(src, odd); err != nil {
		t.Fatal(err)
	}

	t.Chdir(filepath.Dir(odd))
	code, _, errOut := run(t, importCmd, nameFlag, resourceName, "--cache-path", cacheDir, "./"+filepath.Base(odd))
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (%s)", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, resourceName, "etcd.tar")); err != nil {
		t.Errorf("nothing extracted: %v", err)
	}
}

// The command advertises interrupting a gigabyte-sized download, so stopping
// it has to read as a decision rather than as a defect.
func TestInterruptionIsReportedAsSuch(t *testing.T) {
	cacheDir := t.TempDir()
	src := archive(t, map[string][]byte{etcdTarPath: []byte("etcd")})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out, errOut strings.Builder
	code := Run(ctx, []string{importCmd, nameFlag, resourceName, "--cache-path", cacheDir, src},
		&out, &errOut)
	if code != 130 {
		t.Errorf("exit = %d, want 130", code)
	}
	if !strings.Contains(errOut.String(), "interrupted") {
		t.Errorf("stderr = %q, want it to say the run was interrupted", errOut.String())
	}
}

// The CRD caps a resource name at 63 characters because it becomes a node
// label, so a longer one names a resource that can never exist: the cache
// would be filled under a name nothing claims, and collected.
func TestNameLongerThanAResourceAllows(t *testing.T) {
	src := archive(t, map[string][]byte{etcdTarPath: []byte("etcd")})
	code, _, errOut := run(t, importCmd, nameFlag, strings.Repeat("a", 64),
		"--cache-path", t.TempDir(), src)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut, "63") {
		t.Errorf("stderr = %q, want it to give the limit", errOut)
	}
}

// Asking for help is not a mistake, so it goes to the standard output, alone
// and once, and it succeeds. Every form gets the flag list too: the banner
// ends on an Options: heading, so printing it without the list leaves the
// heading empty.
func TestHelpGoesToStdoutAndSucceeds(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"-h"}, {helpFlag}, {importCmd, helpFlag}} {
		var out, errOut strings.Builder
		code := Run(context.Background(), args, &out, &errOut)
		if code != 0 {
			t.Errorf("%v: exit = %d, want 0", args, code)
		}
		if n := strings.Count(out.String(), "Usage: imagecachectl"); n != 1 {
			t.Errorf("%v: usage printed %d times on stdout, want once: %q", args, n, out.String())
		}
		// The flag package prints its own usage on the way to ErrHelp unless
		// it is told not to, which put the whole thing on stderr as well.
		if errOut.Len() != 0 {
			t.Errorf("%v: help wrote to stderr: %q", args, errOut.String())
		}
		// The defaults belong there, since the banner does not repeat them.
		if !strings.Contains(out.String(), cache.DefaultPath) {
			t.Errorf("%v: help does not say where the cache goes by default: %q", args, out.String())
		}
	}
}

// A mistake gets the same block, once, and on the error output.
func TestUsageOnAMistakeIsPrintedOnceOnStderr(t *testing.T) {
	for _, args := range [][]string{{}, {"export"}, {importCmd, "-nope"}, {importCmd}} {
		var out, errOut strings.Builder
		if code := Run(context.Background(), args, &out, &errOut); code != 2 {
			t.Errorf("%v: exit = %d, want 2", args, code)
		}
		if n := strings.Count(errOut.String(), "Usage: imagecachectl"); n != 1 {
			t.Errorf("%v: usage printed %d times on stderr, want once: %q", args, n, errOut.String())
		}
		if !strings.Contains(errOut.String(), cache.DefaultPath) {
			t.Errorf("%v: usage is missing the flag list: %q", args, errOut.String())
		}
		if out.Len() != 0 {
			t.Errorf("%v: a mistake wrote to stdout: %q", args, out.String())
		}
	}
}

func TestUnknownCommandIsNamed(t *testing.T) {
	code, _, errOut := run(t, "export", nameFlag, resourceName, "x.tar")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut, "export") {
		t.Errorf("stderr = %q, want it to name the command", errOut)
	}
}

// --cache-path is the other half of the join the store makes, and the CRD
// validates it for the same reason. A relative or climbing path lands the
// extraction, and the removal that precedes it, somewhere else entirely.
func TestCachePathCannotBeRelativeOrClimb(t *testing.T) {
	src := archive(t, map[string][]byte{etcdTarPath: []byte("etcd")})

	for _, path := range []string{"cache", "./cache", "/var/lib/../lib/image-cache", ""} {
		code, _, errOut := run(t, importCmd, nameFlag, resourceName, "--cache-path", path, src)
		if code != 2 {
			t.Errorf("--cache-path %q: exit = %d, want 2 (%s)", path, code, errOut)
		}
		if !strings.Contains(errOut, "--cache-path") {
			t.Errorf("--cache-path %q: stderr does not name the flag: %q", path, errOut)
		}
	}
}

// A cache path that is there but unusable is named as a cache path problem.
// Asking the store first reported it as a failure to read the cache state,
// which sends the reader looking at the wrong thing.
func TestACachePathThatIsNotADirectoryIsNamed(t *testing.T) {
	root := t.TempDir()
	notADir := filepath.Join(root, "notadir")
	if err := os.WriteFile(notADir, []byte("a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := archive(t, map[string][]byte{etcdTarPath: []byte("etcd")})

	code, _, errOut := run(t, importCmd, nameFlag, resourceName, "--cache-path", notADir, src)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (%s)", code, errOut)
	}
	if !strings.Contains(errOut, "the cache path is not usable") {
		t.Errorf("stderr = %q, want the cache path named as the problem", errOut)
	}
	if strings.Contains(errOut, "reading the cache state") {
		t.Errorf("stderr blames the cache state: %q", errOut)
	}
}

// The store refuses to replace what it did not write, and the command says so
// rather than reporting success over content it left alone.
func TestImportRefusesADirectoryItDidNotWrite(t *testing.T) {
	cacheDir := t.TempDir()
	foreign := filepath.Join(cacheDir, resourceName, "storage")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(foreign, "data.db")
	if err := os.WriteFile(data, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := archive(t, map[string][]byte{etcdTarPath: []byte("etcd")})

	code, out, errOut := run(t, importCmd, nameFlag, resourceName, "--cache-path", cacheDir, src)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (%s)", code, errOut)
	}
	if strings.Contains(out, "extracted") {
		t.Errorf("stdout claims the import went through: %q", out)
	}
	if got, err := os.ReadFile(data); err != nil || string(got) != "not ours" {
		t.Errorf("the foreign data did not survive: %q, %v", got, err)
	}
}

// An import killed outright leaves a temporary directory the size of the
// image, and this command runs where no agent will ever collect it.
func TestImportSweepsItsOwnLeftovers(t *testing.T) {
	cacheDir := t.TempDir()
	leftover, err := os.MkdirTemp(cacheDir, "."+resourceName+".tmp-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leftover, "half.tar"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := archive(t, map[string][]byte{etcdTarPath: []byte("etcd")})

	code, out, errOut := run(t, importCmd, nameFlag, resourceName, "--cache-path", cacheDir, src)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (%s)", code, errOut)
	}
	if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the leftover survived the import: %v", err)
	}
	if !strings.Contains(out, "cleared 1 leftover directory") {
		t.Errorf("stdout = %q, want it to report the leftover it cleared", out)
	}
}

// The whole chain, archive to cache directory, on the case the store's refusal
// exists for. A boot cache image built from a context holding a link to an
// archive outside it carries a link that leaves the image root. Flattening the
// image with mutate.Extract dropped that link before the store could see it,
// so the import succeeded and the resource read complete without the archive.
// It has to fail instead, naming the entry, and leave nothing behind.
func TestImportRefusesAnArchiveLinkedFromOutsideTheImage(t *testing.T) {
	cacheDir := t.TempDir()
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	for _, e := range []struct {
		hdr  tar.Header
		body string
	}{
		{tar.Header{Name: "images/", Typeflag: tar.TypeDir, Mode: 0o755}, ""},
		{tar.Header{Name: etcdTarPath, Typeflag: tar.TypeReg, Mode: 0o644}, "etcd"},
		{tar.Header{Name: pauseTarPath, Typeflag: tar.TypeSymlink, Linkname: "../../shared/pause.tar"}, ""},
	} {
		e.hdr.Size = int64(len(e.body))
		if err := tw.WriteHeader(&e.hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
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
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "boot-cache.tar")
	if err := crane.Save(img, "boot-cache/worker:1.0.0", src); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := run(t, importCmd, nameFlag, resourceName, "--cache-path", cacheDir, src)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stdout %q)", code, out)
	}
	if !strings.Contains(errOut, pauseTarPath) || !strings.Contains(errOut, "symbolic link") {
		t.Errorf("stderr = %q, want the link named", errOut)
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the refusal left %v behind", entries)
	}
}

// The case the identity check in the store exists for, end to end. The image
// has two layers, and the registry closes the connection when the second is
// asked for. The error that comes back wraps io.EOF, and read as the end of
// the archive it let the import report success with the first layer's
// archives only: exit 0, a sentinel calling the resource complete, pause.tar
// missing. It has to fail and leave nothing behind.
func TestImportFailsWhenALayerCannotBeFetched(t *testing.T) {
	base, err := crane.Image(map[string][]byte{etcdTarPath: []byte("etcd")})
	if err != nil {
		t.Fatal(err)
	}
	top, err := crane.Layer(map[string][]byte{pauseTarPath: []byte("pause")})
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(base, top)
	if err != nil {
		t.Fatal(err)
	}
	topDigest, err := top.Digest()
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	direct := httptest.NewServer(reg)
	t.Cleanup(direct.Close)
	if err := crane.Push(img, strings.TrimPrefix(direct.URL, "http://")+"/boot-cache/worker:1.0.0"); err != nil {
		t.Fatal(err)
	}
	cutting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/blobs/"+topDigest.String()) {
			panic(http.ErrAbortHandler)
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(cutting.Close)
	cacheDir := t.TempDir()

	code, out, _ := run(t, importCmd, nameFlag, resourceName, "--cache-path", cacheDir,
		strings.TrimPrefix(cutting.URL, "http://")+"/boot-cache/worker:1.0.0")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stdout %q)", code, out)
	}
	if entries, err := os.ReadDir(cacheDir); err != nil || len(entries) != 0 {
		t.Errorf("the failed import left %v behind (%v)", entries, err)
	}
}

type recorded struct {
	Owner  string   `json:"owner"`
	Source string   `json:"source"`
	Layers []string `json:"layers"`
}

func readSentinel(t *testing.T, cacheDir string) recorded {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cacheDir, resourceName, sentinelName))
	if err != nil {
		t.Fatal(err)
	}
	var r recorded
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// The sentinel names the command, the source and the layers. The archive and
// the registry record the same layers for the same image.
func TestImportRecordsTheSameImageWhateverTheSource(t *testing.T) {
	files := map[string][]byte{etcdTarPath: []byte("etcd")}
	archivePath := archive(t, files)
	registryRef := served(t, files)

	sources := []string{archivePath, registryRef}
	layers := make([][]string, 0, len(sources))
	for _, src := range sources {
		cacheDir := t.TempDir()
		if code, _, errOut := run(t, importCmd, nameFlag, resourceName, "--cache-path", cacheDir, src); code != 0 {
			t.Fatalf("%s: exit = %d (%s)", src, code, errOut)
		}
		r := readSentinel(t, cacheDir)
		if r.Owner != Owner {
			t.Errorf("%s: owner = %q, want %q", src, r.Owner, Owner)
		}
		if r.Source != src {
			t.Errorf("source = %q, want %q", r.Source, src)
		}
		if len(r.Layers) != 1 || !strings.HasPrefix(r.Layers[0], "sha256:") {
			t.Errorf("%s: layers = %q, want the image's one diff ID", src, r.Layers)
		}
		layers = append(layers, r.Layers)
	}
	if !slices.Equal(layers[0], layers[1]) {
		t.Errorf("the archive recorded %v and the registry %v for the same image", layers[0], layers[1])
	}
}
