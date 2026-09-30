// Package cli fills a node's image cache once, from a registry or from a
// docker archive. It is the one-shot half of what the agent does on a loop:
// a node being installed has no Kubernetes to run the agent in, so the same
// pull and the same extraction are driven from a command instead.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/scality/go-errors"
	"github.com/spf13/cobra"

	"github.com/scality/image-cache/agent/api/v1alpha1"
	"github.com/scality/image-cache/agent/internal/cache"
	"github.com/scality/image-cache/agent/internal/puller"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ErrCachePath covers a cache directory the command cannot write into. The
// other failures already carry the sentinel of the package they come from.
var ErrCachePath = errors.New("the cache path is not usable")

// printf writes a message out. A failure to write one is not something the
// command can act on, and returning it would hide the error it was about to
// report behind the failure to report it.
func printf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// ExitInterrupted is what Run returns when the context was cancelled. The
// caller turns it into 128 plus the signal it caught, which is what a shell
// reports: 130 for SIGINT, 143 for SIGTERM.
const ExitInterrupted = 130

const importLong = `Fills the image cache with the archives a boot cache image carries.

<source> is either the path of a docker archive or the reference of an image
in a registry. It is a path when it starts with a separator or a dot, or ends
in .tar, and a reference otherwise: the shape of what you pass decides, not
what is on disk. Give an archive named something else as ./that-name.

--name is the name of the ImageCache resource this content belongs to. It is
the directory the archives land in, and the agent recognises the resource by
it, so it must be the name the resource will carry.`

// Run executes the command line and returns the process exit code: 0 on
// success, ExitInterrupted when the context was cancelled, 1 otherwise.
func Run(ctx context.Context, args []string, out, errOut io.Writer) int {
	var name, cachePath string
	importCmd := &cobra.Command{
		Use:   "import --name <resource> [--cache-path <dir>] <source>",
		Short: "Fill the image cache from a registry or a docker archive",
		Long:  importLong,
		Args:  cobra.ExactArgs(1),
		// Validated here and not in PreRunE: cobra checks the required
		// flags after PreRunE, so a missing --name would read as an invalid
		// one.
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validate(name, cachePath); err != nil {
				return err
			}
			return do(cmd.Context(), cachePath, name, args[0], out, errOut)
		},
	}
	importCmd.Flags().StringVar(&name, "name", "", "name of the ImageCache resource this content belongs to")
	importCmd.Flags().StringVar(&cachePath, "cache-path", cache.DefaultPath, "directory the archives are extracted under")
	_ = importCmd.MarkFlagRequired("name")

	root := &cobra.Command{
		Use:   "imagecachectl",
		Short: "Fill a node's image cache once",
		// Errors are printed below, without the usage: after an error, cobra
		// prints the usage on the standard output.
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(importCmd)
	root.SetArgs(args)
	root.SetOut(out)
	root.SetErr(errOut)

	err := root.ExecuteContext(ctx)
	switch {
	case err == nil:
		return 0
	// An interrupted run is not a failure to diagnose: the extraction
	// publishes by rename, so nothing half written is left behind.
	case errors.Is(err, context.Canceled):
		printf(errOut, "imagecachectl: interrupted, %s was not filled\n", name)
		return ExitInterrupted
	}
	// %s and not %v: go-errors renders JSON under %v.
	printf(errOut, "imagecachectl: %s\n", err)
	return 1
}

// validate checks what the store takes on trust. The store joins the name to
// the cache path, and the command runs as root on a node: `..` in either
// walks out of the cache. Both follow the rules the CRD applies, so what this
// accepts is what an ImageCache can carry. The name also becomes a node label,
// hence the 63 characters.
func validate(name, cachePath string) error {
	problems := validation.IsDNS1123Subdomain(name)
	if len(name) > v1alpha1.ResourceNameMax {
		problems = append(problems,
			fmt.Sprintf("must be %d characters or fewer, it becomes a node label", v1alpha1.ResourceNameMax))
	}
	if len(problems) > 0 {
		return fmt.Errorf("--name %q is not a resource name: %s", name, strings.Join(problems, "; "))
	}
	if !filepath.IsAbs(cachePath) || strings.Contains(cachePath, v1alpha1.CachePathParent) {
		return fmt.Errorf("--cache-path %q is not a cache path: must be absolute and must not contain %q",
			cachePath, v1alpha1.CachePathParent)
	}
	return nil
}

func do(ctx context.Context, cachePath, name, source string, out, errOut io.Writer) error {
	store := cache.Store{}

	// Checked before anything reads through it, so that an unusable path is
	// named as one. Asking the store first would report a path that is a
	// regular file, or one that cannot be listed, as a failure to read the
	// cache state, which sends the reader looking at the wrong thing.
	//
	// Refused rather than created. On a node the directory is usually a mount,
	// and creating it where the mount failed would fill the root filesystem
	// with something nothing reads. Making it is the caller's call, and the
	// message says so rather than assuming which of the two went wrong.
	switch info, serr := os.Stat(cachePath); {
	case serr != nil:
		return errors.Wrap(ErrCachePath, errors.CausedBy(serr),
			errors.WithDetail("create it, or check the mount that should provide it"),
			errors.WithProperty("cachePath", cachePath))
	case !info.IsDir():
		return errors.Wrap(ErrCachePath,
			errors.WithDetail("it is not a directory"),
			errors.WithProperty("cachePath", cachePath))
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
	swept, err := store.SweepTemporaries(cachePath, name)
	if err != nil {
		return err
	}
	if len(swept) > 0 {
		printf(out, "cleared %d leftover directory from an interrupted run\n", len(swept))
	}

	content, digest, err := pullerFor(source).Pull(ctx, source)
	if err != nil {
		return err
	}
	extracted := false
	defer func() {
		cerr := content.Close()
		// Only worth a word when the extraction went through: otherwise the
		// error below says what happened, and a note about the stream just
		// before it would claim the run did what it was asked.
		if cerr != nil && extracted {
			printf(errOut, "imagecachectl: closing the image stream: %s\n", cerr)
		}
	}()

	if err := store.Extract(ctx, cachePath, name, digest, content); err != nil {
		return err
	}
	extracted = true
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
