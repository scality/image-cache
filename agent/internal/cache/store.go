// Package cache manages per-resource cache directories on the node.
package cache

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/scality/go-errors"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
)

// sentinelName marks a directory as fully extracted and agent-owned.
// It is written last; garbage collection only considers directories
// bearing it, so foreign content in a shared cache path is never touched.
const sentinelName = ".image-cache-agent.json"

// Failures of this package are classified by these sentinels. Filesystem and
// archive errors are stamped with one where they enter, because a foreign
// error wrapped cause-first would come out titled "unknown error".
var (
	// ErrState covers reading the state of a resource's directory.
	ErrState = errors.New("reading the cache state failed")
	// ErrExtract covers writing a resource's directory.
	ErrExtract = errors.New("extracting the cache image failed")
	// ErrGC covers reclaiming unreferenced directories.
	ErrGC = errors.New("collecting cache garbage failed")
)

// State describes a resource's cache directory.
type State int

const (
	Absent State = iota
	// Incomplete: directory present but content untrusted (no sentinel,
	// corrupt sentinel, or a listed file is missing).
	Incomplete
	Complete
)

type sentinel struct {
	Digest string   `json:"digest"`
	Files  []string `json:"files"`
}

// Store reads and writes per-resource cache directories. Resource names are
// trusted to be Kubernetes object names (DNS-1123: no path separators); they
// are not sanitized here.
type Store struct{}

func (Store) dir(cachePath, name string) string { return filepath.Join(cachePath, name) }

// State reports the state of the named resource's directory. Callers must
// check the returned error before trusting the State: a filesystem error
// (e.g. permission denied) is reported alongside the zero value Absent.
func (s Store) State(cachePath, name string) (State, error) {
	data, err := os.ReadFile(filepath.Join(s.dir(cachePath, name), sentinelName))
	switch {
	case errors.Is(err, os.ErrNotExist):
		if _, serr := os.Stat(s.dir(cachePath, name)); errors.Is(serr, os.ErrNotExist) {
			return Absent, nil
		} else if serr != nil {
			return Absent, errors.Wrap(ErrState, errors.CausedBy(serr),
				errors.WithProperty("resource", name))
		}
		return Incomplete, nil
	case err != nil:
		return Absent, errors.Wrap(ErrState, errors.CausedBy(err),
			errors.WithProperty("resource", name))
	}
	var sn sentinel
	if json.Unmarshal(data, &sn) != nil {
		return Incomplete, nil
	}
	for _, f := range sn.Files {
		if _, err := os.Stat(filepath.Join(s.dir(cachePath, name), f)); err != nil {
			return Incomplete, nil
		}
	}
	return Complete, nil
}

// Extract writes the regular files of the tar stream into the resource
// directory, flattened to their base names, then the sentinel, then swaps
// the directory into place. The swap is remove-then-rename (a rename cannot
// replace an existing directory), so a crash mid-swap can transiently leave
// the resource Absent; the next pass redoes the extraction. Extract must not
// be called concurrently for the same name. A failed extraction leaves either
// the previous state or a hidden temporary directory that GC removes later.
//
// A cache image is hundreds of megabytes, so the write loop honours ctx: an
// agent being drained stops between entries instead of writing out the rest of
// the payload first.
func (s Store) Extract(
	ctx context.Context, cachePath, name, digest string, content io.Reader,
) (err error) {
	tmp, err := os.MkdirTemp(cachePath, "."+name+".tmp-")
	if err != nil {
		return errors.Wrap(ErrExtract, errors.CausedBy(err),
			errors.WithDetail("creating the temporary directory"),
			errors.WithProperty("resource", name))
	}
	defer func() {
		if err != nil {
			err = utilerrors.NewAggregate([]error{err, os.RemoveAll(tmp)})
		}
	}()

	var files []string
	tr := tar.NewReader(content)
	for {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		hdr, rerr := tr.Next()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return errors.Wrap(ErrExtract, errors.CausedBy(rerr),
				errors.WithDetail("reading the image filesystem"))
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// Base names only: an entry cannot escape the directory, whatever
		// path the image carries. The sentinel name is ours, and is written
		// below; an image shipping that name would just be overwritten, so
		// skip it rather than pretend it was extracted.
		base := filepath.Base(hdr.Name)
		if base == sentinelName {
			continue
		}
		out, oerr := os.OpenFile(filepath.Join(tmp, base), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
		if oerr != nil {
			if errors.Is(oerr, os.ErrExist) {
				return errors.Wrap(ErrExtract,
					errors.WithDetailf("duplicate file name %q in image", base))
			}
			return errors.Wrap(ErrExtract, errors.CausedBy(oerr),
				errors.WithDetailf("creating %q", base))
		}
		if _, cerr := io.Copy(out, tr); cerr != nil {
			return utilerrors.NewAggregate([]error{
				errors.Wrap(ErrExtract, errors.CausedBy(cerr),
					errors.WithDetailf("writing %q", base)),
				out.Close(),
			})
		}
		if cerr := out.Close(); cerr != nil {
			return errors.Wrap(ErrExtract, errors.CausedBy(cerr),
				errors.WithDetailf("closing %q", base))
		}
		files = append(files, base)
	}

	data, err := json.Marshal(sentinel{Digest: digest, Files: files})
	if err != nil {
		return errors.Wrap(ErrExtract, errors.CausedBy(err),
			errors.WithDetail("encoding the sentinel"))
	}
	if err = os.WriteFile(filepath.Join(tmp, sentinelName), data, 0o644); err != nil {
		return errors.Wrap(ErrExtract, errors.CausedBy(err),
			errors.WithDetail("writing the sentinel"))
	}
	final := s.dir(cachePath, name)
	if err = os.RemoveAll(final); err != nil {
		return errors.Wrap(ErrExtract, errors.CausedBy(err),
			errors.WithDetail("clearing the previous directory"))
	}
	if err = os.Rename(tmp, final); err != nil {
		return errors.Wrap(ErrExtract, errors.CausedBy(err),
			errors.WithDetail("swapping the directory into place"))
	}
	return nil
}

// GC removes agent-owned directories (sentinel-bearing, plus stale hidden
// temporaries) under cachePath whose name is not in keep. Flat files and
// foreign directories survive. Returns the removed names.
func (s Store) GC(cachePath string, keep map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(cachePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.Wrap(ErrGC, errors.CausedBy(err),
			errors.WithDetail("listing the cache path"))
	}
	var removed []string
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || keep[e.Name()] {
			continue
		}
		stale := strings.HasPrefix(e.Name(), ".") && strings.Contains(e.Name(), ".tmp-")
		if !stale {
			if _, err := os.Stat(filepath.Join(cachePath, e.Name(), sentinelName)); err != nil {
				continue
			}
		}
		if err := os.RemoveAll(filepath.Join(cachePath, e.Name())); err != nil {
			errs = append(errs, errors.Wrap(ErrGC, errors.CausedBy(err),
				errors.WithProperty("directory", e.Name())))
			continue
		}
		removed = append(removed, e.Name())
	}
	return removed, utilerrors.NewAggregate(errs)
}
