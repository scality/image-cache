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

// ExitInterrupted is what Run returns when the context was cancelled. The
// caller turns it into 128 plus the signal it caught, which is what a shell
// reports: 130 for SIGINT, 143 for SIGTERM.
const ExitInterrupted = 130

// Owner is the owner the command writes in a sentinel.
const Owner = "imagecachectl"

const importLong = `Fills the image cache with the archives a boot cache image carries.

<source> is either the path of a docker archive or the reference of an image
in a registry. It is a path when it starts with a separator or a dot, or ends
in .tar, and a reference otherwise: the shape of what you pass decides, not
what is on disk.`

const importExample = `  imagecachectl import --name worker-1-0-0 registry.example.com/boot-cache-worker:1.0.0
  imagecachectl import --name worker-1-0-0 /mnt/iso/images/boot-cache-worker.tar
  imagecachectl import --name worker-1-0-0 ./boot-cache-worker.archive`

// Run executes the command line and returns the process exit code: 0 on
// success, ExitInterrupted when the context was cancelled, 1 otherwise.
func Run(ctx context.Context, args []string, out, errOut io.Writer) int {
	var name, cachePath, caFile string
	var skipVerify bool
	importCmd := &cobra.Command{
		Use:     "import --name <resource> [--cache-path <dir>] [--ca-file <file> | --insecure-skip-tls-verify] <source>",
		Short:   "Fill the image cache from a registry or a docker archive",
		Long:    importLong,
		Example: importExample,
		Args:    cobra.ExactArgs(1),
		// Use already lists the flags.
		DisableFlagsInUseLine: true,
		// Validated here and not in PreRunE: cobra checks the required
		// flags after PreRunE, so a missing --name would read as an invalid
		// one.
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validate(name, cachePath); err != nil {
				return err
			}
			// On the values and not with MarkFlagsMutuallyExclusive, which
			// would also refuse --insecure-skip-tls-verify=false.
			if caFile != "" && skipVerify {
				return errors.New("--ca-file and --insecure-skip-tls-verify exclude each other")
			}
			err := do(cmd.Context(), cachePath, name, args[0],
				puller.TLS{CAFile: caFile, SkipVerify: skipVerify}, out, errOut)
			// An interrupted run is not a failure to diagnose: the extraction
			// publishes by rename, so nothing half written is left behind.
			if errors.Is(err, context.Canceled) {
				return fmt.Errorf("interrupted, %s was not filled: %w", name, context.Canceled)
			}
			return err
		},
	}
	importCmd.Flags().StringVar(&name, "name", "",
		"name of the ImageCache `resource` this content belongs to: the agent finds the content by it")
	importCmd.Flags().StringVar(&cachePath, "cache-path", cache.DefaultPath, "`directory` the archives are extracted under")
	importCmd.Flags().StringVar(&caFile, "ca-file", "",
		"PEM `file` of CA certificates to trust for the registry, on top of the system ones")
	importCmd.Flags().BoolVar(&skipVerify, "insecure-skip-tls-verify", false,
		"accept any registry certificate, for a test cluster set up by hand only")
	_ = importCmd.MarkFlagRequired("name")

	root := &cobra.Command{
		Use:   "imagecachectl",
		Short: "Fill a node's image cache once",
		// After an error, cobra prints the usage on the standard output.
		SilenceUsage: true,
	}
	root.SetErrPrefix("imagecachectl:")
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(importCmd)
	root.SetArgs(args)
	root.SetOut(out)
	root.SetErr(errOut)

	switch err := root.ExecuteContext(ctx); {
	case err == nil:
		return 0
	case errors.Is(err, context.Canceled):
		return ExitInterrupted
	default:
		return 1
	}
}

// validate checks what the store takes on trust. The store joins the name to
// the cache path, and the command runs as root on a node: `..` in either
// walks out of the cache. The name follows the rules the CRD applies, so what
// this accepts is what an ImageCache can carry. It also becomes a node label,
// hence the 63 characters. The cache path follows fill.ValidCachePath, like
// the agent's.
func validate(name, cachePath string) error {
	problems := validation.IsDNS1123Subdomain(name)
	if len(name) > v1alpha1.ResourceNameMax {
		problems = append(problems,
			fmt.Sprintf("must be %d characters or fewer, it becomes a node label", v1alpha1.ResourceNameMax))
	}
	if len(problems) > 0 {
		return fmt.Errorf("--name %q is not a resource name: %s", name, strings.Join(problems, "; "))
	}
	if !fill.ValidCachePath(cachePath) {
		return fmt.Errorf("--cache-path %q is not a cache path: must be absolute and must not contain %q",
			cachePath, "..")
	}
	return nil
}

func do(ctx context.Context, cachePath, name, source string, tls puller.TLS, out, errOut io.Writer) error {
	store := cache.Store{}

	// Before the cache state, so that a CA that cannot be used is reported
	// on every run, including one that would have found the resource
	// complete, rather than only on the run that finally needs the registry.
	src, err := pullerFor(source, tls)
	if err != nil {
		return err
	}
	if _, remote := src.(puller.Remote); remote && tls.SkipVerify {
		printf(errOut, "imagecachectl: warning: the registry certificate is not verified\n")
	}

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
	swept, err := store.SweepTemporaries(cachePath, name)
	if err != nil {
		return err
	}
	if len(swept) > 0 {
		printf(out, "cleared %d leftover directory from an interrupted run\n", len(swept))
	}

	if err := fill.Fill(ctx, store, src, cachePath, name, source, Owner, func(cerr error) {
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
//
// The TLS settings only matter to a registry, so an archive never reads them.
func pullerFor(source string, tls puller.TLS) (puller.Puller, error) {
	switch {
	case strings.HasPrefix(source, string(os.PathSeparator)),
		strings.HasPrefix(source, "."),
		strings.HasSuffix(source, ".tar"):
		return puller.Tarball{}, nil
	default:
		return puller.NewRemote(tls)
	}
}
