// Package cache manages per-resource cache directories on the node.
package cache

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/scality/go-errors"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
)

// DefaultPath is the default of the agent's and the command's --cache-path.
// The preload script, its sysconfig and the sample DaemonSet's volume repeat
// it: change them together.
const DefaultPath = "/var/lib/image-cache"

// sentinelName marks a directory as fully extracted by the store, and names
// who wrote it. It is written last.
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

// lostFound is where fsck puts orphaned inodes back, at the root of an ext
// filesystem.
const lostFound = "lost+found"

// OwnerAgent is the owner the agent writes in a sentinel. Another owner, or
// none, means another writer seeded the directory: garbage collection leaves
// it until a resource adopts it.
const OwnerAgent = "image-cache-agent"

// Record is what the sentinel remembers about a directory's content, beyond
// the files it lists.
type Record struct {
	// Digest is the manifest digest the source served.
	Digest string `json:"digest"`
	// Owner is who wrote the directory.
	Owner string `json:"owner,omitempty"`
	// Source is the reference or the archive path the content was read from,
	// kept for whoever looks at the directory. Two sources naming the same
	// image can differ, so it is not what an image is compared by.
	Source string `json:"source,omitempty"`
	// Layers are the image's layer diff IDs, as puller.Image reports them.
	Layers []string `json:"layers,omitempty"`
}

// sentinel is what the sentinel file holds: the record, and the files the
// directory has to hold to be complete.
type sentinel struct {
	Record
	Files []string `json:"files"`
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

// ErrAdopt covers taking over a directory another writer seeded.
var ErrAdopt = errors.New("adopting a seeded cache directory failed")

// ReadRecord reads the record from the sentinel of the named resource's
// directory. It is meant for a directory State reported Complete.
func (s Store) ReadRecord(cachePath, name string) (Record, error) {
	sn, err := s.readSentinel(cachePath, name)
	if err != nil {
		return Record{}, err
	}
	return sn.Record, nil
}

// Adopt makes the agent the owner of a directory another writer seeded,
// leaving its content and the rest of the sentinel as they are. From then on
// the directory is the agent's to refresh and to collect.
//
// The sentinel is rewritten through a temporary file renamed over it, so a
// crash leaves either the old owner or the new one, never a sentinel half
// written, which State would read as an incomplete resource to pull again.
func (s Store) Adopt(cachePath, name string) error {
	sn, err := s.readSentinel(cachePath, name)
	if err != nil {
		return errors.Wrap(ErrAdopt, errors.CausedBy(err), errors.WithProperty("resource", name))
	}
	sn.Owner = OwnerAgent
	data, err := json.Marshal(sn)
	if err != nil {
		return errors.Wrap(ErrAdopt, errors.CausedBy(err), errors.WithProperty("resource", name))
	}
	path := filepath.Join(s.dir(cachePath, name), sentinelName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return errors.Wrap(ErrAdopt, errors.CausedBy(err),
			errors.WithDetail("writing the new sentinel"), errors.WithProperty("resource", name))
	}
	if err := os.Rename(tmp, path); err != nil {
		return utilerrors.NewAggregate([]error{
			errors.Wrap(ErrAdopt, errors.CausedBy(err),
				errors.WithDetail("replacing the sentinel"), errors.WithProperty("resource", name)),
			os.Remove(tmp),
		})
	}
	return nil
}

func (s Store) readSentinel(cachePath, name string) (sentinel, error) {
	data, err := os.ReadFile(filepath.Join(s.dir(cachePath, name), sentinelName))
	if err != nil {
		return sentinel{}, errors.Wrap(ErrState, errors.CausedBy(err), errors.WithProperty("resource", name))
	}
	var sn sentinel
	if err := json.Unmarshal(data, &sn); err != nil {
		return sentinel{}, errors.Wrap(ErrState, errors.CausedBy(err),
			errors.WithDetail("the sentinel does not parse"), errors.WithProperty("resource", name))
	}
	return sn, nil
}

// Extract writes the regular files of the tar stream into the resource
// directory, flattened to their base names, skipping directories and refusing
// the whole stream on any other kind of entry, then the sentinel, then swaps
// the directory into place. The swap is remove-then-rename (a rename cannot
// replace an existing directory), so a crash mid-swap can transiently leave
// the resource Absent; the next pass redoes the extraction. Extract must not
// be called concurrently for the same name. A failed extraction leaves either
// the previous state or a hidden temporary directory that GC removes later.
//
// Every read honours ctx, inside an entry too: an entry can be hundreds of
// megabytes, and nothing else watches ctx when the source is a local archive.
func (s Store) Extract(
	ctx context.Context, cachePath, name string, rec Record, content io.Reader,
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
	tr := tar.NewReader(ctxReader{ctx: ctx, r: content})
	for {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		hdr, rerr := tr.Next()
		// Identity, not errors.Is. archive/tar ends an archive with io.EOF
		// itself, while a failure upstream can wrap one: a registry closing
		// the connection before a layer comes back as Get "...": EOF. Taken
		// for the end, it would publish whatever the earlier layers held as a
		// complete resource, the rest silently missing.
		if rerr == io.EOF { //nolint:errorlint // see above: only the unwrapped value means the end
			break
		}
		if rerr != nil {
			return errors.Wrap(ErrExtract, errors.CausedBy(rerr),
				errors.WithDetail("reading the image filesystem"))
		}
		switch hdr.Typeflag {
		// A contiguous file is a regular file to every reader that does not
		// special-case it, content included.
		case tar.TypeReg, tar.TypeCont:
		// A directory is only a container for the files below it, and those
		// land flat. A global header describes the archive, not an entry in
		// it: git archive writes one, and the reader hands it back as its own
		// header.
		case tar.TypeDir, tar.TypeXGlobalHeader:
			continue
		default:
			// A link or a device is not written out, and dropping it without a
			// word would publish a resource short of an archive the image was
			// built to carry: the sentinel would call it complete, the node would be
			// labelled synced, and the gap would show the day the kubelet needs
			// that image with no registry to fall back on. docker build copies
			// a symbolic link as it finds it, dangling if it pointed outside the
			// build context, so the image is refused whole instead. A hard link
			// could be recreated from its target, but a boot cache image has
			// no use for one.
			return errors.Wrap(ErrExtract,
				errors.WithDetailf("%q is %s, not a regular file", hdr.Name, entryKind(hdr.Typeflag)))
		}
		// Base names only: an entry cannot escape the directory, whatever
		// path the image carries. The sentinel name is ours, and is written
		// below; an image shipping that name would just be overwritten, so
		// skip it rather than pretend it was extracted.
		base := filepath.Base(hdr.Name)
		// A name that is empty or all dots has no file name to land under:
		// opening it would hit the temporary directory or its parent, and the
		// failure would read as a duplicate rather than as what it is.
		if base == "." || base == ".." || base == string(filepath.Separator) {
			return errors.Wrap(ErrExtract,
				errors.WithDetailf("%q has no file name", hdr.Name))
		}
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

	// An image with nothing in it is a mistake upstream, not an empty cache
	// to publish: the sentinel would report the resource complete, the node
	// would be labelled synced, and it would hold none of the archives the
	// resource was meant to give it.
	if len(files) == 0 {
		return errors.Wrap(ErrExtract,
			errors.WithDetail("the image carries no file to cache"))
	}

	data, err := json.Marshal(sentinel{Record: rec, Files: files})
	if err != nil {
		return errors.Wrap(ErrExtract, errors.CausedBy(err),
			errors.WithDetail("encoding the sentinel"))
	}
	if err = os.WriteFile(filepath.Join(tmp, sentinelName), data, 0o644); err != nil {
		return errors.Wrap(ErrExtract, errors.CausedBy(err),
			errors.WithDetail("writing the sentinel"))
	}
	final := s.dir(cachePath, name)
	if err = s.replaceable(final, name); err != nil {
		return err
	}
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

// entryKind names a tar entry type the way an operator would read it.
func entryKind(flag byte) string {
	switch flag {
	case tar.TypeSymlink:
		return "a symbolic link"
	case tar.TypeLink:
		return "a hard link"
	case tar.TypeChar, tar.TypeBlock:
		return "a device"
	case tar.TypeFifo:
		return "a named pipe"
	default:
		return fmt.Sprintf("an entry of tar type %q", flag)
	}
}

// replaceable reports whether the swap may remove what is already at final.
// Only a directory bearing the sentinel may be: a name is not a claim on
// whatever happens to sit under it. Without this, a resource named after a
// neighbour of the cache path turns the swap into an rm -rf of somebody
// else's data.
func (s Store) replaceable(final, name string) error {
	if _, err := os.Stat(final); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return errors.Wrap(ErrExtract, errors.CausedBy(err),
			errors.WithDetail("looking at the directory to replace"),
			errors.WithProperty("resource", name))
	}
	if _, err := os.Stat(filepath.Join(final, sentinelName)); err != nil {
		return errors.Wrap(ErrExtract,
			errors.WithDetail("what is already there was not written by this, "+
				"so it is left alone: remove it by hand if it should go"),
			errors.WithProperty("resource", name),
			errors.WithProperty("directory", final))
	}
	return nil
}

// SweepTemporaries removes the hidden temporary directories a previous
// extraction of this resource left behind, and returns their names. Extract
// cleans up after itself when it returns an error, but not when the process
// is killed outright, and a leftover is the size of the image being written.
//
// Scoped to one name because a run for another resource may be in flight:
// GC, which the agent calls, is the one that sweeps them all.
func (s Store) SweepTemporaries(cachePath, name string) ([]string, error) {
	entries, err := os.ReadDir(cachePath)
	if err != nil {
		return nil, errors.Wrap(ErrGC, errors.CausedBy(err),
			errors.WithDetail("listing the cache path"))
	}
	prefix := "." + name + ".tmp-"
	var removed []string
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if rerr := os.RemoveAll(filepath.Join(cachePath, e.Name())); rerr != nil {
			errs = append(errs, errors.Wrap(ErrGC, errors.CausedBy(rerr),
				errors.WithProperty("directory", e.Name())))
			continue
		}
		removed = append(removed, e.Name())
	}
	return removed, utilerrors.NewAggregate(errs)
}

// seeded reports whether a writer other than the agent wrote dir: content
// waiting for the resource that adopts it. A missing sentinel or one that does
// not parse is not seeded. A sentinel that cannot be read is an error: the
// directory may be seeded, and removing it would lose content an air-gapped
// node cannot fetch again.
func (Store) seeded(dir string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, sentinelName))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var sn sentinel
	if json.Unmarshal(data, &sn) != nil {
		return false, nil
	}
	return sn.Owner != OwnerAgent, nil
}

// GC removes the entries under cachePath that keep does not name, and
// returns their names. A few stay: "Cache layout" in agent/DESIGN.md lists
// them and why.
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
		if keep[e.Name()] || e.Type().IsRegular() || e.Name() == lostFound {
			continue
		}
		// An interrupted extraction goes whoever started it. Its sentinel,
		// written before the rename, does not make it seeded.
		stale := strings.HasPrefix(e.Name(), ".") && strings.Contains(e.Name(), ".tmp-")
		if e.IsDir() && !stale {
			seeded, err := s.seeded(filepath.Join(cachePath, e.Name()))
			if err != nil {
				errs = append(errs, errors.Wrap(ErrGC, errors.CausedBy(err),
					errors.WithDetail("reading the sentinel, so the directory is kept"),
					errors.WithProperty("entry", e.Name())))
				continue
			}
			if seeded {
				continue
			}
		}
		// A link goes, not what it points to.
		if err := os.RemoveAll(filepath.Join(cachePath, e.Name())); err != nil {
			errs = append(errs, errors.Wrap(ErrGC, errors.CausedBy(err),
				errors.WithProperty("entry", e.Name())))
			continue
		}
		removed = append(removed, e.Name())
	}
	return removed, utilerrors.NewAggregate(errs)
}

// ctxReader fails every read once ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
