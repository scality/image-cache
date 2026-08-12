package controller

import (
	"sync"

	"github.com/fsnotify/fsnotify"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

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

// NewFSWatcher starts the forwarding goroutine; Close stops it.
func NewFSWatcher(nodeName string) (*FSWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	fw := &FSWatcher{
		watcher: w,
		paths:   map[string]bool{},
		trigger: event.GenericEvent{
			Object: &imagecachev1alpha1.ImageCache{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName},
			},
		},
		Events: make(chan event.GenericEvent, 1),
	}
	go fw.run()
	return fw, nil
}

func (f *FSWatcher) run() {
	for {
		select {
		case _, ok := <-f.watcher.Events:
			if !ok {
				return
			}
			select {
			case f.Events <- f.trigger:
			default:
			}
		case _, ok := <-f.watcher.Errors:
			if !ok {
				return
			}
		}
	}
}

// SetPaths adjusts the watched directories to exactly paths. Directories
// that do not exist yet are skipped; the next reconcile pass retries.
func (f *FSWatcher) SetPaths(paths []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := map[string]bool{}
	for _, p := range paths {
		want[p] = true
	}
	for p := range f.paths {
		if !want[p] {
			// Removal of an already-deleted directory fails harmlessly;
			// the path is dropped from our set regardless.
			_ = f.watcher.Remove(p)
			delete(f.paths, p)
		}
	}
	for p := range want {
		if !f.paths[p] {
			if err := f.watcher.Add(p); err == nil {
				f.paths[p] = true
			}
		}
	}
}

// Close stops the underlying watcher and the forwarding goroutine.
func (f *FSWatcher) Close() error { return f.watcher.Close() }
