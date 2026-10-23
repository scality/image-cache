package cache

import (
	"os"
	"syscall"
	"time"
)

// sumKey identifies a version of a file on disk. Why ctime is in it:
// "Content check" in agent/DESIGN.md.
type sumKey struct {
	dev, ino uint64
	size     int64
	mtime    time.Time
	ctime    time.Time
}

func keyOf(info os.FileInfo) (sumKey, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return sumKey{}, false
	}
	return sumKey{
		dev: st.Dev, ino: st.Ino, size: st.Size,
		mtime: time.Unix(st.Mtim.Unix()), ctime: time.Unix(st.Ctim.Unix()),
	}, true
}
