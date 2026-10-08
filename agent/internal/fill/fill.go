// Package fill holds the one sequence that fills a resource's cache
// directory: check the cache path, pull the image, extract what it carries.
// The agent runs it on a loop and the command runs it once; keeping a single
// copy is what stops the two from drifting apart on what a filled directory
// is.
package fill

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/scality/go-errors"

	"github.com/scality/image-cache/agent/internal/cache"
	"github.com/scality/image-cache/agent/internal/puller"
)

// ErrCachePath covers a cache path nothing can be extracted into.
var ErrCachePath = errors.New("the cache path is not usable")

// ValidCachePath reports whether p can be a cache path: absolute, and with no
// ".." anywhere, as a plain substring. The command runs as root and the agent
// writes past file permissions, so a path that could climb out is refused.
func ValidCachePath(p string) bool {
	return filepath.IsAbs(p) && !strings.Contains(p, "..")
}

// CheckCachePath refuses a cache path that is missing or is not a directory.
//
// Refused rather than created. On a node the directory is usually a mount,
// and creating it where the mount failed would fill the root filesystem, or
// the container's, with something nothing reads. Making it is the caller's
// call, and the message says so rather than guessing which of the two went
// wrong.
func CheckCachePath(cachePath string) error {
	info, err := os.Stat(cachePath)
	switch {
	case err != nil:
		return errors.Wrap(ErrCachePath, errors.CausedBy(err),
			errors.WithDetail("create it, or check the mount that should provide it"),
			errors.WithProperty("cachePath", cachePath))
	case !info.IsDir():
		return errors.Wrap(ErrCachePath,
			errors.WithDetail("it is not a directory"),
			errors.WithProperty("cachePath", cachePath))
	}
	return nil
}

// Fill pulls source with p and extracts it into the directory of the named
// resource under cachePath, recording owner as its writer along with what the
// source resolved to.
//
// onClose hears about a failure to close the image stream, and only once the
// extraction went through: before that, the error Fill returns already says
// what happened, and a note about the stream in front of it would read as if
// the fill had succeeded. It may be nil.
func Fill(
	ctx context.Context, store cache.Store, p puller.Puller,
	cachePath, name, source, owner string, onClose func(error),
) error {
	if err := store.Replaceable(cachePath, name, owner); err != nil {
		return err
	}
	content, img, err := p.Pull(ctx, source)
	if err != nil {
		return err
	}
	extracted := false
	defer func() {
		if cerr := content.Close(); cerr != nil && extracted && onClose != nil {
			onClose(cerr)
		}
	}()
	rec := cache.Record{Owner: owner, Source: source, Digest: img.Digest, Layers: img.Layers}
	if err := store.Extract(ctx, cachePath, name, rec, content); err != nil {
		return err
	}
	extracted = true
	return nil
}
