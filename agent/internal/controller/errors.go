package controller

import "github.com/scality/go-errors"

// Failures are classified by these sentinels, so that a caller can tell a
// Kubernetes problem from a caching one with errors.Is. Errors coming from
// outside the agent (API client, filesystem) are stamped with a sentinel at
// the boundary where they enter: wrapping a foreign error cause-first would
// retitle it "unknown error" and lose that classification.
var (
	// ErrNode covers reading and patching the node object.
	ErrNode = errors.New("node access failed")
	// ErrResources covers listing the ImageCache resources.
	ErrResources = errors.New("listing image caches failed")
	// ErrSync covers bringing one resource's cache directory in line.
	ErrSync = errors.New("syncing an image cache failed")
	// ErrWatcher covers the filesystem watcher itself.
	ErrWatcher = errors.New("filesystem watcher failed")
)
