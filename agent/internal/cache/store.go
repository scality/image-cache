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

// DefaultPath is where the cache lives unless something says otherwise. The
// CRD defaults to it, the RPM's sysconfig ships it, and both the agent and
// the command read it from here so that a change has one place to happen.
const DefaultPath = "/var/lib/image-cache"

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

// Owners a sentinel names. Garbage collection only removes what the agent
// wrote, so a directory another writer seeded stays until the agent claims
// it.
const (
	OwnerAgent   = "image-cache-agent"
	OwnerCommand = "imagecachectl"
)

// Record is what the sentinel remembers about a directory's content, beyond
// the files it lists.
type Record struct {
	// Owner is who wrote the directory. Empty means the agent: every
	// sentinel written before owners were recorded is the agent's.
	Owner string
	// Source is the reference or the archive path the content was read from,
	// kept for whoever looks at the directory. Two sources naming the same
	// image can differ, so it is not what an image is compared by.
	Source string
	// Digest is the manifest digest the source served.
	Digest string
	// Config is the digest of the image's configuration, the same however
	// the image was reached: what tells whether a directory holds the image
	// a resource asks for.
	Config string
}

type sentinel struct {
	Digest string   `json:"digest"`
	Files  []string `json:"files"`
	Owner  string   `json:"owner,omitempty"`
	Source string   `json:"source,omitempty"`
	Config string   `json:"config,omitempty"`
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

// Foreign reports whether another writer than the agent wrote the directory.
func (r Record) Foreign() bool { return r.Owner != "" && r.Owner != OwnerAgent }

// Record returns what the sentinel of the named resource's directory
// remembers. It is meant for a directory State reported Complete.
func (s Store) Record(cachePath, name string) (Record, error) {
	sn, err := s.readSentinel(cachePath, name)
	if err != nil {
		return Record{}, err
	}
	return Record{Owner: sn.Owner, Source: sn.Source, Digest: sn.Digest, Config: sn.Config}, nil
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
// A cache image is hundreds of megabytes, so the write loop honours ctx: an
// agent being drained stops between entries instead of writing out the rest of
// the payload first.
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
	tr := tar.NewReader(content)
	for {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		hdr, rerr := tr.Next()
		// Identity, not errors.Is. archive/tar ends an archive with io.EOF
		// itself, while a failure upstream can wrap one: a registry closing
		// the connection before a layer comes back as Get "...": EOF. Taken
		// for the end, it published whatever the earlier layers held as a
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
			// could be recreated from its target, but the Salt module this
			// replaces refused it too, and a boot cache image has no use for
			// one.
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

	data, err := json.Marshal(sentinel{
		Digest: rec.Digest, Files: files, Owner: rec.Owner, Source: rec.Source, Config: rec.Config,
	})
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
// Only a directory bearing the sentinel may be, which is the same rule GC
// applies: the cache path is shared, and a name is not a claim on whatever
// happens to sit under it. Without this, a resource named after a neighbour
// of the cache path, or a cache path one level too high, turns the swap into
// an rm -rf of somebody else's data.
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

// agentOwned reports whether dir bears a sentinel the agent wrote. No
// sentinel means foreign content. An owner left empty means the agent, since
// every sentinel written before owners were recorded is the agent's; reading
// it the other way would leave every existing cluster with directories
// nothing collects. A sentinel that does not parse is the agent's too, as it
// has always been treated.
func (Store) agentOwned(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, sentinelName))
	if err != nil {
		return false
	}
	var sn sentinel
	if json.Unmarshal(data, &sn) != nil {
		return true
	}
	return sn.Owner == "" || sn.Owner == OwnerAgent
}

// GC removes agent-owned directories (sentinel-bearing, plus stale hidden
// temporaries) under cachePath whose name is not in keep. Flat files and
// foreign directories survive, and so does a directory whose sentinel names
// another owner: a node seeded before the agent arrived keeps its cache until
// a resource claims it, whatever order the agent and the resources land in.
// Returns the removed names.
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
		if !stale && !s.agentOwned(filepath.Join(cachePath, e.Name())) {
			continue
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
