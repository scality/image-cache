package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scality/go-errors"
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
