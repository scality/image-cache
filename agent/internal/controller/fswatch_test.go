package controller

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFSWatcherEmitsOnChange(t *testing.T) {
	fw, err := NewFSWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := fw.Close(); err != nil {
			t.Errorf("closing watcher: %v", err)
		}
	}()
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
	fw, err := NewFSWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := fw.Close(); err != nil {
			t.Errorf("closing watcher: %v", err)
		}
	}()
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
