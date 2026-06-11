//go:build soci

// Package note: this file is only compiled with `-tags soci`, which also requires
// CGO_ENABLED=1 (soci-snapshotter's ztoc package links zlib via cgo). The default
// buildkitd build omits the tag and uses export_soci_stub.go, so it stays
// CGO_ENABLED=0 / statically linked with no soci dependency.

package containerimage

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/awslabs/soci-snapshotter/soci"
	socistore "github.com/awslabs/soci-snapshotter/soci/store"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/errdefs"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// buildSOCIIndex runs soci-snapshotter's v2 Convert over the just-committed image
// and returns a new OCI index descriptor. That index wraps the image manifest
// re-serialized with a com.amazon.soci.index-digest annotation plus a SOCI v2 index
// entry; layer blobs are shared with the original image (content-addressed).
//
// reader supplies read access to the image's manifest/config/layers (the same
// provider the push path builds from ref.GetRemotes — layers may be lazy). writeStore
// is the worker content store where the new ztoc/index/manifest blobs are written.
func buildSOCIIndex(ctx context.Context, reader content.InfoReaderProvider, writeStore content.Store, target ocispecs.Descriptor, opts *ImageCommitOpts) (*ocispecs.Descriptor, error) {
	// Ephemeral artifacts DB so we never touch the snapshotter's real bbolt.
	tmp, err := os.MkdirTemp("", "buildkit-soci-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	db, err := soci.NewDB(filepath.Join(tmp, "artifacts.db"))
	if err != nil {
		return nil, errors.Wrap(err, "soci: create artifacts db")
	}

	spanSize := opts.SOCISpanSize
	if spanSize <= 0 {
		spanSize = 1 << 22 // 4 MiB (soci default)
	}
	minLayerSize := opts.SOCIMinLayerSize
	if minLayerSize <= 0 {
		minLayerSize = 10 << 20 // 10 MiB (soci default)
	}

	builder, err := soci.NewIndexBuilder(
		&readerContentStore{reader},
		&sociBlobStore{writeStore},
		soci.WithSpanSize(spanSize),
		soci.WithMinLayerSize(minLayerSize),
		soci.WithBuildToolIdentifier("buildkit"),
		soci.WithArtifactsDb(db),
	)
	if err != nil {
		return nil, errors.Wrap(err, "soci: new index builder")
	}

	// Convert only uses Target to walk the manifest from the read store; no image
	// metadata service is needed (good for the runc worker, which has none).
	indexDesc, err := builder.Convert(ctx, images.Image{Target: target})
	if err != nil {
		return nil, errors.Wrap(err, "soci: convert")
	}
	return indexDesc, nil
}

// readerContentStore adapts a content.InfoReaderProvider (e.g. buildkit's
// MultiProvider) to the containerd content.Store that soci's IndexBuilder reads
// through. soci only ever reads (ReaderAt/Info), so the write and
// ingest-management methods are stubbed.
type readerContentStore struct {
	content.InfoReaderProvider
}

func (s *readerContentStore) Writer(context.Context, ...content.WriterOpt) (content.Writer, error) {
	return nil, errdefs.ErrNotImplemented
}
func (s *readerContentStore) Status(context.Context, string) (content.Status, error) {
	return content.Status{}, errdefs.ErrNotImplemented
}
func (s *readerContentStore) ListStatuses(context.Context, ...string) ([]content.Status, error) {
	return nil, errdefs.ErrNotImplemented
}
func (s *readerContentStore) Abort(context.Context, string) error { return errdefs.ErrNotImplemented }
func (s *readerContentStore) Update(context.Context, content.Info, ...string) (content.Info, error) {
	return content.Info{}, errdefs.ErrNotImplemented
}
func (s *readerContentStore) Walk(context.Context, content.WalkFunc, ...string) error {
	return errdefs.ErrNotImplemented
}
func (s *readerContentStore) Delete(context.Context, digest.Digest) error {
	return errdefs.ErrNotImplemented
}

// sociBlobStore implements soci's store.Store directly over a containerd
// content.Store (the worker store). This is the runc-worker analog of soci's own
// ContainerdStore, which is gRPC-client based. BatchOpen is a no-op because the
// exporter already runs under a lease (Export -> leaseutil.WithLease).
type sociBlobStore struct {
	cs content.Store
}

var _ socistore.Store = (*sociBlobStore)(nil)

func (s *sociBlobStore) Exists(ctx context.Context, target ocispecs.Descriptor) (bool, error) {
	_, err := s.cs.Info(ctx, target.Digest)
	if errors.Is(err, errdefs.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

type sectionReaderAt struct {
	content.ReaderAt
	*io.SectionReader
}

func (s *sociBlobStore) Fetch(ctx context.Context, target ocispecs.Descriptor) (io.ReadCloser, error) {
	ra, err := s.cs.ReaderAt(ctx, target)
	if err != nil {
		return nil, err
	}
	return sectionReaderAt{ra, io.NewSectionReader(ra, 0, ra.Size())}, nil
}

func (s *sociBlobStore) Push(ctx context.Context, expected ocispecs.Descriptor, reader io.Reader) error {
	if exists, err := s.Exists(ctx, expected); err != nil {
		return err
	} else if exists {
		return errdefs.ErrAlreadyExists
	}
	w, err := content.OpenWriter(ctx, s.cs, content.WithRef(expected.Digest.String()), content.WithDescriptor(expected))
	if err != nil {
		return err
	}
	defer w.Close()
	if _, err := content.CopyReader(w, reader); err != nil {
		return err
	}
	return w.Commit(ctx, expected.Size, expected.Digest)
}

func (s *sociBlobStore) Label(ctx context.Context, target ocispecs.Descriptor, name, value string) error {
	_, err := s.cs.Update(ctx, content.Info{
		Digest: target.Digest,
		Labels: map[string]string{name: value},
	}, "labels."+name)
	return err
}

func (s *sociBlobStore) Delete(ctx context.Context, dgst digest.Digest) error {
	return s.cs.Delete(ctx, dgst)
}

func (s *sociBlobStore) BatchOpen(ctx context.Context) (context.Context, socistore.CleanupFunc, error) {
	return ctx, socistore.NopCleanup, nil
}
