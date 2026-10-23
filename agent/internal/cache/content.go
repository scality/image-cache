package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// File is one file a resource's directory holds, and what its content must be.
type File struct {
	Name string `json:"name"`
	// Digest is "sha256:" followed by the hex digest of the content.
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// digestPrefix is the only algorithm the store computes.
const digestPrefix = "sha256:"

// sumOf returns the digest of r in the form File.Digest uses, and its size.
func sumOf(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", n, err
	}
	return digestPrefix + hex.EncodeToString(h.Sum(nil)), n, nil
}

type sumEntry struct {
	key    sumKey
	digest string
	// at is when the file was hashed.
	at time.Time
	// written is set when the store wrote the file and hashed it on the way,
	// so its digest holds even though the file just changed.
	written bool
}

// racyWindow is how long after a change a digest is not trusted, see
// "Content check" in agent/DESIGN.md.
const racyWindow = 2 * time.Second

// sums remembers the digest of the files it has hashed, so that a pass hashes
// again only what changed since. See "Content check" in agent/DESIGN.md.
type sums struct {
	mu      sync.Mutex
	entries map[string]sumEntry
	// racy is racyWindow, shorter in tests that cannot wait for it.
	racy time.Duration
	// hashed counts the files actually read, for tests.
	hashed int
}

func newSums() *sums { return &sums{entries: map[string]sumEntry{}, racy: racyWindow} }

// trusted reports whether e still gives the digest of a file with key k: the
// key did not change, and either the store wrote the file or it had not
// changed for racy when it was hashed.
func (c *sums) trusted(e sumEntry, k sumKey) bool {
	return e.key == k && (e.written || k.ctime.Before(e.at.Add(-c.racy)))
}

// digest returns the digest of the file at path, which info describes.
func (c *sums) digest(path string, info os.FileInfo) (string, error) {
	key, keyed := keyOf(info)
	if keyed {
		c.mu.Lock()
		e, ok := c.entries[path]
		c.mu.Unlock()
		if ok && c.trusted(e, key) {
			return e.digest, nil
		}
	}
	at := time.Now()
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	d, _, err := sumOf(f)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hashed++
	if keyed {
		c.entries[path] = sumEntry{key: key, digest: d, at: at}
	}
	return d, nil
}

// remember records the digests an extraction computed for the files it moved
// under dir, so the passes that follow do not read a gigabyte again. A file
// it cannot stat is left out: the next pass hashes it.
func (c *sums) remember(dir string, content []File) {
	if c == nil {
		return
	}
	for _, f := range content {
		path := filepath.Join(dir, f.Name)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if key, ok := keyOf(info); ok {
			c.mu.Lock()
			c.entries[path] = sumEntry{key: key, digest: f.Digest, written: true}
			c.mu.Unlock()
		}
	}
}

// forget drops what is remembered under dir.
func (c *sums) forget(dir string) {
	if c == nil {
		return
	}
	prefix := dir + string(filepath.Separator)
	c.mu.Lock()
	defer c.mu.Unlock()
	for p := range c.entries {
		if strings.HasPrefix(p, prefix) {
			delete(c.entries, p)
		}
	}
}

// mismatch returns the first file under dir that does not hold the content
// listed, and why, or two empty strings when all do. A missing file, or one
// of another size, is told without reading it. A file that cannot be read
// does not match either: extracting it again is the repair, for a bad sector
// as for a changed file.
func (c *sums) mismatch(dir string, content []File) (file, why string) {
	// Every file is checked by name and size before any is read, so a missing
	// file is told without hashing a gigabyte first.
	infos := make([]os.FileInfo, len(content))
	for i, f := range content {
		// The store writes base names only. Any other name in a sentinel was
		// not written by it, and is not looked up outside the directory.
		if f.Name == "" || f.Name == "." || f.Name == ".." || strings.ContainsRune(f.Name, filepath.Separator) {
			return f.Name, "not a file name the store writes"
		}
		info, err := os.Stat(filepath.Join(dir, f.Name))
		switch {
		case err != nil:
			return f.Name, err.Error()
		case !info.Mode().IsRegular():
			return f.Name, "not a regular file"
		case info.Size() != f.Size:
			return f.Name, fmt.Sprintf("size %d, recorded %d", info.Size(), f.Size)
		}
		infos[i] = info
	}
	for i, f := range content {
		d, err := c.digest(filepath.Join(dir, f.Name), infos[i])
		switch {
		case err != nil:
			return f.Name, err.Error()
		case d != f.Digest:
			return f.Name, "digest " + d + ", recorded " + f.Digest
		}
	}
	return "", ""
}
