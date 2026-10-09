package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scality/go-errors"

	"github.com/scality/image-cache/agent/internal/cache"
)

// startedWatcher returns a watcher whose forwarding loop runs for the test.
func startedWatcher(t *testing.T) *FSWatcher {
	t.Helper()
	fw, err := NewFSWatcher("test-node")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := fw.Start(ctx); err != nil {
			t.Errorf("watcher stopped with an error: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		if err := fw.Close(); err != nil {
			t.Errorf("closing watcher: %v", err)
		}
	})
	return fw
}

func TestFSWatcherEmitsOnChange(t *testing.T) {
	fw := startedWatcher(t)
	dir := t.TempDir()
	fw.SetPaths([]string{dir, "/does/not/exist"}) // missing paths are skipped
	if err := os.WriteFile(filepath.Join(dir, "x.tar"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fw.Events:
	case <-time.After(5 * time.Second):
		t.Fatal("no event received after file creation")
	}
}

func TestFSWatcherSetPathsRemovesStaleWatches(t *testing.T) {
	fw := startedWatcher(t)
	dir := t.TempDir()
	fw.SetPaths([]string{dir})
	fw.SetPaths(nil) // dir no longer watched
	if err := os.WriteFile(filepath.Join(dir, "y.tar"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fw.Events:
		t.Fatal("received event for a path that was unwatched")
	case <-time.After(500 * time.Millisecond):
	}
}

// A tarball lives one level below the cache path, in the resource's own
// directory: watching the roots alone would miss it being deleted.
func TestFSWatcherEmitsOnChangeInsideAResourceDirectory(t *testing.T) {
	fw := startedWatcher(t)
	root := t.TempDir()
	resource := filepath.Join(root, "worker-134-0-0")
	if err := os.Mkdir(resource, 0o755); err != nil {
		t.Fatal(err)
	}
	tarball := filepath.Join(resource, "pause.tar")
	if err := os.WriteFile(tarball, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	fw.SetPaths([]string{root})
	if err := os.Remove(tarball); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fw.Events:
	case <-time.After(5 * time.Second):
		t.Fatal("no event received after a tarball was deleted inside a resource directory")
	}
}

// A resource directory created after the last pass is picked up by the next
// call to SetPaths, without the caller having to enumerate subdirectories.
func TestFSWatcherWatchesResourceDirectoriesCreatedLater(t *testing.T) {
	fw := startedWatcher(t)
	root := t.TempDir()
	fw.SetPaths([]string{root}) // nothing below the root yet

	resource := filepath.Join(root, "control-plane-134-0-0")
	if err := os.Mkdir(resource, 0o755); err != nil {
		t.Fatal(err)
	}
	fw.SetPaths([]string{root}) // the pass that follows the creation event
	drain(fw)

	if err := os.WriteFile(filepath.Join(resource, "etcd.tar"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fw.Events:
	case <-time.After(5 * time.Second):
		t.Fatal("no event received for a resource directory created after the first pass")
	}
}

// An extraction by another process writes its temporary directory thousands
// of times. None of it may trigger a pass: only the rename, seen on the root.
func TestFSWatcherIgnoresTemporaryDirectories(t *testing.T) {
	fw := startedWatcher(t)
	root := t.TempDir()
	tmp := filepath.Join(root, ".worker-134-0-0.tmp-123")
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	fw.SetPaths([]string{root})
	drain(fw)

	if err := os.WriteFile(filepath.Join(tmp, "pause.tar"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fw.Events:
		t.Fatal("received an event from inside a temporary directory")
	case <-time.After(500 * time.Millisecond):
	}

	if err := os.Rename(tmp, filepath.Join(root, "worker-134-0-0")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fw.Events:
	case <-time.After(5 * time.Second):
		t.Fatal("no event received after the temporary directory was renamed")
	}
}

// A write to a tarball does not change what a pass checks, so it triggers
// none. Removing the file does.
func TestFSWatcherIgnoresWrites(t *testing.T) {
	fw := startedWatcher(t)
	root := t.TempDir()
	resource := filepath.Join(root, "worker-134-0-0")
	if err := os.Mkdir(resource, 0o755); err != nil {
		t.Fatal(err)
	}
	tarball := filepath.Join(resource, "pause.tar")
	if err := os.WriteFile(tarball, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fw.SetPaths([]string{root})
	drain(fw)

	f, err := os.OpenFile(tarball, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if _, err := f.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fw.Events:
		t.Fatal("received an event for a write")
	case <-time.After(500 * time.Millisecond):
	}

	if err := os.Remove(tarball); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fw.Events:
	case <-time.After(5 * time.Second):
		t.Fatal("no event received after the tarball was removed")
	}
}

// A pass reads the sentinel, so a sentinel damaged in place must trigger one.
func TestFSWatcherEmitsOnASentinelWrite(t *testing.T) {
	fw := startedWatcher(t)
	root := t.TempDir()
	resource := filepath.Join(root, "worker-134-0-0")
	if err := os.Mkdir(resource, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(resource, cache.SentinelName)
	if err := os.WriteFile(sentinel, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	fw.SetPaths([]string{root})
	drain(fw)

	f, err := os.OpenFile(sentinel, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fw.Events:
	case <-time.After(5 * time.Second):
		t.Fatal("no event received after the sentinel was written")
	}
}

// A watcher that dies while the agent keeps running would silently downgrade
// repairs to the periodic resync, so Start has to report it.
func TestFSWatcherStartReportsAnUnexpectedClose(t *testing.T) {
	fw, err := NewFSWatcher("test-node")
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- fw.Start(context.Background()) }()

	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-stopped:
		if !errors.Is(err, ErrWatcher) {
			t.Fatalf("got %v, want an error matching ErrWatcher", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the watcher was closed")
	}
}

// drain empties the one-slot event channel so that the next receive proves a
// new event, not a leftover one.
func drain(fw *FSWatcher) {
	select {
	case <-fw.Events:
	case <-time.After(100 * time.Millisecond):
	}
}
