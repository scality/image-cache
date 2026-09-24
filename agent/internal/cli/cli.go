// Package cli fills a node's image cache once, from a registry or from a
// docker archive. It is the one-shot half of what the agent does on a loop:
// a node being installed has no Kubernetes to run the agent in, so the same
// pull and the same extraction are driven from a command instead.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/scality/go-errors"

	"github.com/scality/image-cache/agent/api/v1alpha1"
	"github.com/scality/image-cache/agent/internal/cache"
	"github.com/scality/image-cache/agent/internal/fill"
	"github.com/scality/image-cache/agent/internal/puller"
	"k8s.io/apimachinery/pkg/util/validation"
)

// printf writes a message out. A failure to write one is not something the
// command can act on, and returning it would hide the error it was about to
// report behind the failure to report it.
func printf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// printUsage writes the banner and the flag list as one block: the banner ends
// on the Options: heading the list fills in, so neither is useful alone.
func printUsage(w io.Writer, fs *flag.FlagSet) {
	printf(w, "%s", usage)
	previous := fs.Output()
	fs.SetOutput(w)
	defer fs.SetOutput(previous)
	fs.PrintDefaults()
}

// ExitInterrupted is what Run returns when the context was cancelled. The
// caller turns it into 128 plus the signal it caught, which is what a shell
// reports: 130 for SIGINT, 143 for SIGTERM.
const ExitInterrupted = 130

// helpFlag is the long form of a help request, kept as a name so the switch
// and the usage cannot drift apart.
const helpFlag = "--help"

// importCommand is the only verb, kept as a name so the parser and the flag
// set cannot drift apart.
const importCommand = "import"

const usage = `Usage: imagecachectl import --name <resource> [--cache-path <dir>] <source>

Fills the image cache with the archives a boot cache image carries.

<source> is either the path of a docker archive or the reference of an image
in a registry, and the two are told apart by the shape of what you pass, not
by what is on disk. It is a path when it starts with a separator or a dot, or
ends in .tar, and a reference otherwise. An archive named something else has
to be given as ./that-name.

--name is the name of the ImageCache resource this content belongs to. It is
the directory the archives land in, and the agent later recognises the
resource by it, so it must be the name the resource will carry.

Options:
`

// Run executes one import and returns the process exit code: 0 on success,
// 2 when the command line is wrong, 1 when the work failed.
func Run(ctx context.Context, args []string, out, errOut io.Writer) int {
	// The flag set comes before the dispatch because the usage needs it: the
	// banner ends on an Options: heading only PrintDefaults can fill.
	fs := flag.NewFlagSet(importCommand, flag.ContinueOnError)
	fs.SetOutput(errOut)
	// Parse writes its own complaint about a bad flag, then calls this. The
	// usage that goes with it is printed at each return below instead, so a
	// help request can have it on the standard output and a mistake on the
	// error output, each exactly once.
	fs.Usage = func() {}
	name := fs.String("name", "", "name of the ImageCache resource this content belongs to")
	cachePath := fs.String("cache-path", cache.DefaultPath, "directory the archives are extracted under")

	switch {
	case len(args) == 0:
		printUsage(errOut, fs)
		return 2
	// A request for help is not a mistake: it belongs on the standard output,
	// so it can be piped, and it succeeds.
	case args[0] == "help", args[0] == "-h", args[0] == helpFlag:
		printUsage(out, fs)
		return 0
	case args[0] != importCommand:
		printf(errOut, "imagecachectl: unknown command %q\n\n", args[0])
		printUsage(errOut, fs)
		return 2
	}

	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(out, fs)
			return 0
		}
		printUsage(errOut, fs)
		return 2
	}
	if *name == "" {
		printf(errOut, "imagecachectl: --name is required\n\n")
		printUsage(errOut, fs)
		return 2
	}
	// The store joins this name to the cache path and takes it on trust, and
	// says so: `..` walks out of the directory, and the command runs as root
	// on a node. Both halves of what the resource must satisfy are checked
	// here, so a name this accepts is one an ImageCache can carry: the API
	// server demands a DNS-1123 subdomain, and the CRD caps it at 63 because
	// the name becomes a node label.
	problems := validation.IsDNS1123Subdomain(*name)
	if len(*name) > v1alpha1.ResourceNameMax {
		problems = append(problems,
			fmt.Sprintf("must be %d characters or fewer, it becomes a node label", v1alpha1.ResourceNameMax))
	}
	if len(problems) > 0 {
		printf(errOut, "imagecachectl: --name %q is not a resource name: %s\n\n",
			*name, strings.Join(problems, "; "))
		printUsage(errOut, fs)
		return 2
	}
	// The other half of the join the store makes, and the CRD validates it
	// too: a relative or climbing path turns the swap into a removal
	// somewhere else entirely. Same plain substring test as the CEL rule, so
	// a path this accepts is one a resource could carry.
	if !filepath.IsAbs(*cachePath) || strings.Contains(*cachePath, v1alpha1.CachePathParent) {
		printf(errOut, "imagecachectl: --cache-path %q is not a cache path: "+
			"must be absolute and must not contain %q\n\n", *cachePath, v1alpha1.CachePathParent)
		printUsage(errOut, fs)
		return 2
	}
	if fs.NArg() != 1 {
		printf(errOut, "imagecachectl: expected one source, got %d\n\n", fs.NArg())
		printUsage(errOut, fs)
		return 2
	}

	if err := do(ctx, *cachePath, *name, fs.Arg(0), out, errOut); err != nil {
		// An interrupted run is not a failure to diagnose. The extraction
		// publishes by rename, so nothing half written is left behind.
		if errors.Is(err, context.Canceled) {
			printf(errOut, "imagecachectl: interrupted, %s was not filled\n", *name)
			return ExitInterrupted
		}
		// %s and not %v: go-errors renders JSON under %v and its own sentence
		// under %s, which is the form meant for a command line.
		printf(errOut, "imagecachectl: %s\n", err)
		return 1
	}
	return 0
}

func do(ctx context.Context, cachePath, name, source string, out, errOut io.Writer) error {
	store := cache.Store{}

	// Checked before anything reads through it, so that an unusable path is
	// named as one. Asking the store first would report a path that is a
	// regular file, or one that cannot be listed, as a failure to read the
	// cache state, which sends the reader looking at the wrong thing.
	if err := fill.CheckCachePath(cachePath); err != nil {
		return err
	}

	// Answered from the disk alone. A node that already holds the archives
	// reaches no registry, which is what lets this run on every convergence
	// rather than only at install.
	state, err := store.State(cachePath, name)
	if err != nil {
		return err
	}
	if state == cache.Complete {
		printf(out, "%s already holds %s\n", cachePath, name)
		return nil
	}

	// An extraction killed outright leaves its temporary directory behind,
	// the size of a whole boot cache image. The agent's garbage collection
	// clears those, but this command exists for a node that has no agent, so
	// every interrupted attempt would otherwise stay on the disk for good.
	// Only this resource's own leftovers, since a run for another name may be
	// in flight.
	if swept, serr := store.SweepTemporaries(cachePath, name); serr != nil {
		return serr
	} else if len(swept) > 0 {
		printf(errOut, "imagecachectl: cleared %d leftover directory from an interrupted run\n",
			len(swept))
	}

	if err := fill.Fill(ctx, store, pullerFor(source), cachePath, name, source, func(cerr error) {
		printf(errOut, "imagecachectl: closing the image stream: %s\n", cerr)
	}); err != nil {
		return err
	}
	printf(out, "extracted %s into %s\n", name, filepath.Join(cachePath, name))
	return nil
}

// pullerFor reads the source as a path when it looks like one, and as an
// image reference otherwise.
//
// The decision is the shape of the string and never what is on disk. Probing
// the filesystem would let a file named exactly like a reference, sitting in
// whatever directory this happens to run from, stand in for the image: the
// command runs as root during provisioning, often from a directory it does
// not own. Shape alone also means a path that is not there fails naming the
// file rather than coming back with a complaint about a reference.
func pullerFor(source string) puller.Puller {
	switch {
	case strings.HasPrefix(source, string(os.PathSeparator)),
		strings.HasPrefix(source, "."),
		strings.HasSuffix(source, ".tar"):
		return puller.Tarball{}
	default:
		return puller.Remote{}
	}
}
