package controller

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/scality/go-errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	imagecachev1alpha1 "github.com/scality/image-cache/agent/api/v1alpha1"
)

// FSWatcher turns filesystem changes under the cache paths into reconcile
// triggers, so manual tampering with the cache is repaired quickly.
type FSWatcher struct {
	mu      sync.Mutex
	watcher *fsnotify.Watcher
	paths   map[string]bool
	// trigger is the synthetic event pushed on every change. It is named
	// after the node so that the reconcile key in the manager's log lines
	// identifies the node this agent converges.
	trigger event.GenericEvent
	// Events feeds the controller's source.Channel. Buffer of one: the
	// workqueue collapses duplicate keys and a periodic resync covers
	// missed events, so dropping bursts is harmless.
	Events chan event.GenericEvent
}

var (
	_ manager.Runnable               = &FSWatcher{}
	_ manager.LeaderElectionRunnable = &FSWatcher{}
)

// NewFSWatcher opens the watcher. Nothing is watched until SetPaths runs, and
// no event is forwarded until Start does.
func NewFSWatcher(nodeName string) (*FSWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, errors.Wrap(ErrWatcher, errors.CausedBy(err),
			errors.WithDetail("opening the filesystem watcher"))
	}
	return &FSWatcher{
		watcher: w,
		paths:   map[string]bool{},
		trigger: event.GenericEvent{
			Object: &imagecachev1alpha1.ImageCache{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName},
			},
		},
		Events: make(chan event.GenericEvent, 1),
	}, nil
}

// Start forwards filesystem events until ctx is cancelled or the watcher is
// closed. It implements manager.Runnable so that the manager owns the
// goroutine, rather than the constructor starting one behind the caller's
// back.
func (f *FSWatcher) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("fswatch")
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-f.watcher.Events:
			if !ok {
				return f.closed(ctx)
			}
			select {
			case f.Events <- f.trigger:
			default:
			}
		case err, ok := <-f.watcher.Errors:
			if !ok {
				return f.closed(ctx)
			}
			// Losing an event is not fatal: the periodic resync still
			// converges the node. Report it rather than dropping it, since
			// it is the only sign that repairs went from prompt to delayed.
			log.Error(err, "filesystem watch error, falling back on the periodic resync")
		}
	}
}

// closed reports how the forwarding loop ended. The watcher's channels close
// on Close, which the agent only calls on its way out; if they close while
// the agent is meant to keep running, saying so is what makes the manager
// stop. Returning nil there would leave an agent that looks healthy while
// silently falling back on the periodic resync.
func (f *FSWatcher) closed(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
	return errors.Wrap(ErrWatcher, errors.WithDetail("the watcher closed on its own"))
}

// NeedLeaderElection implements manager.LeaderElectionRunnable. Leader
// election is not enabled at all (see cmd/main.go), so this only states the
// intent for anyone who would turn it on: every agent watches the node it
// runs on, and there is no leader among them.
func (f *FSWatcher) NeedLeaderElection() bool { return false }

// SetPaths adjusts the watched directories to exactly roots and their
// immediate subdirectories. Watching the roots alone would report a whole
// resource directory disappearing but not a single tarball deleted inside
// one, because the cache layout is <cachePath>/<resource>/<files> and
// fsnotify does not watch recursively.
//
// Directories that do not exist yet are skipped. A resource directory created
// later is not missed: its creation is itself an event on the root, and the
// pass that follows watches it.
func (f *FSWatcher) SetPaths(roots []string) {
	log := logf.Log.WithName("fswatch")
	want := map[string]bool{}
	for _, root := range roots {
		want[root] = true
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				want[filepath.Join(root, e.Name())] = true
			}
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for p := range f.paths {
		if !want[p] {
			// Removal of an already-deleted directory fails harmlessly;
			// the path is dropped from our set regardless.
			_ = f.watcher.Remove(p)
			delete(f.paths, p)
		}
	}
	for p := range want {
		if f.paths[p] {
			continue
		}
		err := f.watcher.Add(p)
		switch {
		case err == nil:
			f.paths[p] = true
		case errors.Is(err, os.ErrNotExist):
			// The documented case: a cache path no resource has created yet,
			// or one whose host mount is absent from this node. The next
			// pass retries.
		default:
			// Typically the inotify watch limit, which a busy node can
			// exhaust. The node still converges on the periodic resync, but
			// tampering stops being repaired promptly, and that is worth
			// saying out loud rather than degrading in silence.
			log.Error(err, "cannot watch a cache directory, tampering will only be repaired by the periodic resync",
				"path", p)
		}
	}
}

// Close stops the underlying watcher, which ends Start.
func (f *FSWatcher) Close() error { return f.watcher.Close() }
