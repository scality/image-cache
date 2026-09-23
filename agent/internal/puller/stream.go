package puller

import (
	"archive/tar"
	"io"
	"path"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/scality/go-errors"
)

// whiteoutPrefix marks an entry that deletes a path of an earlier layer, in
// both the OCI and the Docker image formats.
const whiteoutPrefix = ".wh."

// entries streams every entry of the image's layers, in layer order, as one
// tar stream, each header passed on as the layer carries it.
//
// This is not mutate.Extract, and the difference is the point. mutate.Extract
// flattens the layers into a filesystem, and on the way it drops, without a
// word, every relative link whose target leaves the image root. A boot cache
// image built from a context holding a link to an archive outside it carries
// exactly such a link, so the store would never see it: the import would
// succeed and the node would be reported synced without that archive. Passing
// the entries on untouched lets the store refuse what it cannot take.
//
// The flattening mutate.Extract does is not needed here. A boot cache image is
// built from scratch and only adds files: an entry that deletes one is refused
// below, and a path written by two layers reaches the store twice, which
// refuses duplicate names anyway.
//
// The caller closes the stream. Closing it early, as a refusing store does,
// ends the goroutine writing it.
func entries(img v1.Image) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(writeEntries(img, pw))
	}()
	return pr
}

func writeEntries(img v1.Image, w io.Writer) error {
	layers, err := img.Layers()
	if err != nil {
		return errors.Wrap(ErrPull, errors.CausedBy(err), errors.WithDetail("listing the layers"))
	}
	tw := tar.NewWriter(w)
	for _, layer := range layers {
		if err := copyEntries(layer, tw); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return errors.Wrap(ErrPull, errors.CausedBy(err), errors.WithDetail("closing the stream"))
	}
	return nil
}

func copyEntries(layer v1.Layer, tw *tar.Writer) (err error) {
	rc, err := layer.Uncompressed()
	if err != nil {
		return errors.Wrap(ErrPull, errors.CausedBy(err), errors.WithDetail("opening a layer"))
	}
	defer func() {
		if cerr := rc.Close(); cerr != nil && err == nil {
			err = errors.Wrap(ErrPull, errors.CausedBy(cerr), errors.WithDetail("closing a layer"))
		}
	}()

	tr := tar.NewReader(rc)
	for {
		hdr, rerr := tr.Next()
		if errors.Is(rerr, io.EOF) {
			// The end of the tar archive is not the end of the layer: padding
			// and, for a pulled layer, the rest of the compressed stream follow.
			// The layer's digest is checked only once that has been read, so
			// stopping here would take a layer altered in transit as it came.
			// Draining it before the stream ends means a mismatch reaches the
			// store as an error, ahead of the end of its archive.
			if _, derr := io.Copy(io.Discard, rc); derr != nil {
				return errors.Wrap(ErrPull, errors.CausedBy(derr), errors.WithDetail("verifying a layer"))
			}
			return nil
		}
		if rerr != nil {
			return errors.Wrap(ErrPull, errors.CausedBy(rerr), errors.WithDetail("reading a layer"))
		}
		if strings.HasPrefix(path.Base(hdr.Name), whiteoutPrefix) {
			return errors.Wrap(ErrPull, errors.WithDetailf(
				"%q deletes a path of an earlier layer, which a boot cache image built from scratch has no reason to do",
				hdr.Name))
		}
		// PAX lifts the length limits USTAR puts on names and link targets,
		// so a header the layer could carry, this stream can carry too.
		if hdr.Typeflag != tar.TypeXGlobalHeader {
			hdr.Format = tar.FormatPAX
		}
		if werr := tw.WriteHeader(hdr); werr != nil {
			return errors.Wrap(ErrPull, errors.CausedBy(werr),
				errors.WithDetailf("passing on the header of %q", hdr.Name))
		}
		if _, cerr := io.Copy(tw, tr); cerr != nil {
			return errors.Wrap(ErrPull, errors.CausedBy(cerr),
				errors.WithDetailf("passing on the content of %q", hdr.Name))
		}
	}
}
