//go:build !linux

package cache

import (
	"os"
	"time"
)

// sumKey is the Linux one's shape, so the code that compares keys builds
// everywhere. The agent runs on Linux only: elsewhere no digest is
// remembered, and every check reads the file.
type sumKey struct {
	ctime time.Time
}

func keyOf(os.FileInfo) (sumKey, bool) { return sumKey{}, false }
