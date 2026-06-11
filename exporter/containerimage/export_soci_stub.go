//go:build !soci

package containerimage

import (
	"context"

	"github.com/containerd/containerd/v2/core/content"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// buildSOCIIndex is the stub used when buildkitd is built without `-tags soci`.
// SOCI index generation pulls in soci-snapshotter's cgo (zlib) dependency, so it is
// opt-in at build time to keep the default daemon CGO_ENABLED=0 / static. With this
// stub compiled in, requesting soci=true at runtime fails with a clear message.
func buildSOCIIndex(_ context.Context, _ content.InfoReaderProvider, _ content.Store, _ ocispecs.Descriptor, _ *ImageCommitOpts) (*ocispecs.Descriptor, error) {
	return nil, errors.New("this buildkitd was built without SOCI support; rebuild with BUILDKITD_TAGS=soci and CGO_ENABLED=1")
}
