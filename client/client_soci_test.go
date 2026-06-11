//go:build soci

// These tests exercise the soci=true image exporter (see exporter/containerimage/
// export_soci.go and SOCI.md). Like client_nydus_test.go they are gated behind the
// `soci` build tag, which is paired with a buildkitd built with BUILDKITD_TAGS=soci
// (CGO_ENABLED=1) — soci=true returns a "rebuild with soci" error otherwise. The test
// binary itself is CGO-free: it only reads the pushed artifact back from the registry
// and does not import soci-snapshotter (see the wire constants below). The tests run
// against the runc/OCI worker — SOCI's target worker — so no containerd worker is
// needed.

package client

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/util/contentutil"
	"github.com/moby/buildkit/util/testutil/integration"
	"github.com/moby/buildkit/util/testutil/workers"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

func init() {
	allTests = append(
		allTests,
		testBuildExportSOCI,
	)
}

// fixedEpoch makes canonical image digests reproducible across builds so the
// "soci is additive" guarantee can be asserted by digest equality.
const fixedEpoch = "1700000000"

// SOCI v2 wire constants, mirrored from github.com/awslabs/soci-snapshotter/soci
// (soci_index.go). They are duplicated rather than imported so this test binary
// stays CGO-free: importing the soci package pulls in its ztoc/cgo dependency.
// These strings are a stable registry wire format (see SOCI.md).
const (
	sociIndexArtifactTypeV2            = "application/vnd.amazon.soci.index.v2+json"
	imageAnnotationSociIndexDigest     = "com.amazon.soci.index-digest"
	indexAnnotationImageManifestDigest = "com.amazon.soci.image-manifest-digest"
	indexAnnotationImageLayerDigest    = "com.amazon.soci.image-layer-digest"
)

func testBuildExportSOCI(t *testing.T, sb integration.Sandbox) {
	workers.CheckFeatureCompat(t, sb, workers.FeatureDirectPush)
	requiresLinux(t)

	registry, err := sb.NewRegistry()
	if errors.Is(err, integration.ErrRequirements) {
		t.Skip(err.Error())
	}
	require.NoError(t, err)

	c, err := New(sb.Context(), sb.Address())
	require.NoError(t, err)
	defer c.Close()

	ctx := sb.Context()

	// buildAndPush solves def, exports type=image with the given extra attrs (name +
	// push are always set), and returns the exporter response.
	buildAndPush := func(t *testing.T, def *llb.State, attrs map[string]string) (string, map[string]string) {
		marshaled, err := def.Marshal(ctx)
		require.NoError(t, err)

		target := registry + "/buildkit/soci:" + identity.NewID()
		exportAttrs := map[string]string{
			"name":              target,
			"push":              "true",
			"source-date-epoch": fixedEpoch,
		}
		for k, v := range attrs {
			exportAttrs[k] = v
		}
		resp, err := c.Solve(ctx, marshaled, SolveOpt{
			Exports: []ExportEntry{{Type: ExporterImage, Attrs: exportAttrs}},
		}, nil)
		require.NoError(t, err)
		return target, resp.ExporterResponse
	}

	readBlob := func(t *testing.T, provider content.Provider, desc ocispecs.Descriptor, v any) {
		dt, err := content.ReadBlob(ctx, provider, desc)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(dt, v))
	}

	// sociArtifact reads the pushed -soci index from the registry and splits it into
	// its two entries: the re-serialized image manifest and the SOCI v2 index manifest.
	// It returns the top-level index descriptor and the provider so callers can read
	// the entries' contents (e.g. the image manifest layers).
	type sociParts struct {
		topDesc      ocispecs.Descriptor
		provider     content.Provider
		index        ocispecs.Index
		imageEntry   ocispecs.Descriptor
		sociEntry    ocispecs.Descriptor
		sociManifest ocispecs.Manifest
	}
	sociArtifact := func(t *testing.T, sociRef string) sociParts {
		desc, provider, err := contentutil.ProviderFromRef(sociRef)
		require.NoError(t, err)
		require.Equal(t, ocispecs.MediaTypeImageIndex, desc.MediaType, "-soci ref must be an OCI image index")

		p := sociParts{topDesc: desc, provider: provider}
		readBlob(t, provider, desc, &p.index)
		require.Len(t, p.index.Manifests, 2, "-soci index must hold the image manifest + the SOCI index")

		for _, m := range p.index.Manifests {
			if m.ArtifactType == sociIndexArtifactTypeV2 {
				p.sociEntry = m
			} else {
				p.imageEntry = m
			}
		}
		require.NotEmpty(t, p.sociEntry.Digest, "no SOCI v2 index entry found in -soci index")
		require.NotEmpty(t, p.imageEntry.Digest, "no image manifest entry found in -soci index")

		readBlob(t, provider, p.sociEntry, &p.sociManifest)
		return p
	}

	// touchState is a tiny deterministic image (busybox base + a touch layer). Its
	// content is stable across builds, so canonical digests are reproducible.
	touchState := func() *llb.State {
		st := llb.Image("busybox:latest").Run(llb.Args([]string{"/bin/touch", "/hello"})).Root()
		return &st
	}

	// randomLayerState adds an incompressible layer of approximately sizeMiB so its
	// *compressed* descriptor size exceeds soci-min-layer-size and a ztoc is built.
	randomLayerState := func(name string, sizeMiB int) *llb.State {
		st := llb.Image("busybox:latest").
			Run(llb.Args([]string{"/bin/dd", "if=/dev/urandom", "of=/" + name, "bs=1048576", "count=" + strconv.Itoa(sizeMiB)})).
			Root()
		return &st
	}

	t.Run("artifact shape and annotations", func(t *testing.T) {
		// A 12MiB incompressible layer clears the 10MiB default soci-min-layer-size.
		_, resp := buildAndPush(t, randomLayerState("big", 12), map[string]string{"soci": "true"})

		sociRef := resp[exptypes.ExporterSOCIImageNameKey]
		require.NotEmpty(t, sociRef, "response missing %s", exptypes.ExporterSOCIImageNameKey)
		sociDigest := resp[exptypes.ExporterSOCIDigestKey]
		require.NotEmpty(t, sociDigest, "response missing %s", exptypes.ExporterSOCIDigestKey)

		p := sociArtifact(t, sociRef)

		// Response metadata points at this exact index.
		require.Equal(t, sociDigest, p.topDesc.Digest.String(),
			"containerimage.soci.digest must match the pushed index digest")

		// Bidirectional linkage between the two entries.
		require.Equal(t, p.sociEntry.Digest.String(), p.imageEntry.Annotations[imageAnnotationSociIndexDigest],
			"image manifest must point at the SOCI index via %s", imageAnnotationSociIndexDigest)
		require.Equal(t, p.imageEntry.Digest.String(), p.sociEntry.Annotations[indexAnnotationImageManifestDigest],
			"SOCI index must back-reference the image manifest via %s", indexAnnotationImageManifestDigest)

		// The SOCI index manifest is a v2 artifact whose layers are ztocs, each
		// annotated with the source image-layer digest. soci serializes the index as
		// an OCI 1.0 image manifest (MarshalIndex), so the registry-visible artifact
		// type lives on the index *entry descriptor* and on the manifest's config
		// media type — not on the manifest body's top-level artifactType field, which
		// MarshalIndex leaves unset.
		require.Equal(t, sociIndexArtifactTypeV2, p.sociEntry.ArtifactType,
			"-soci index entry descriptor must carry the v2 artifact type")
		require.Equal(t, sociIndexArtifactTypeV2, p.sociManifest.Config.MediaType,
			"SOCI v2 index manifest config must carry the v2 artifact type")
		require.NotEmpty(t, p.sociManifest.Layers, "expected at least one ztoc for the 12MiB layer")
		for _, ztoc := range p.sociManifest.Layers {
			require.NotEmpty(t, ztoc.Annotations[indexAnnotationImageLayerDigest],
				"ztoc layer missing %s annotation", indexAnnotationImageLayerDigest)
		}

		// Layer sharing: the re-serialized image manifest's layers are the canonical
		// image's layers, and every ztoc references one of them.
		var imageManifest ocispecs.Manifest
		readBlob(t, p.provider, p.imageEntry, &imageManifest)

		layerDigests := map[digest.Digest]bool{}
		for _, l := range imageManifest.Layers {
			layerDigests[l.Digest] = true
		}
		require.NotEmpty(t, layerDigests)
		for _, ztoc := range p.sociManifest.Layers {
			src := digest.Digest(ztoc.Annotations[indexAnnotationImageLayerDigest])
			require.True(t, layerDigests[src], "ztoc references layer %s not present in the image manifest", src)
		}
	})

	t.Run("canonical image digest is byte-stable", func(t *testing.T) {
		// Same definition + epoch with and without soci must yield an identical
		// canonical image digest: the SOCI index is purely additive (it is pushed to a
		// separate -soci ref and leaves the canonical manifest untouched).
		//
		// soci=true force-enables oci-mediatypes (Convert needs OCI input), so the
		// baseline build must also set oci-mediatypes=true; otherwise it would emit a
		// docker schema2 manifest and the digests would differ purely on media type,
		// not on anything soci did.
		_, sociResp := buildAndPush(t, touchState(), map[string]string{"soci": "true", "soci-min-layer-size": "1"})
		_, plainResp := buildAndPush(t, touchState(), map[string]string{"oci-mediatypes": "true"})

		require.NotEmpty(t, sociResp[exptypes.ExporterImageDigestKey])
		require.Equal(t, plainResp[exptypes.ExporterImageDigestKey], sociResp[exptypes.ExporterImageDigestKey],
			"soci=true must not change the canonical image digest")
	})

	t.Run("soci-min-layer-size controls ztoc count", func(t *testing.T) {
		def := randomLayerState("big", 12) // busybox base (~MiB) + a 12MiB layer

		_, defaultResp := buildAndPush(t, def, map[string]string{"soci": "true"}) // default 10MiB
		_, lowResp := buildAndPush(t, def, map[string]string{"soci": "true", "soci-min-layer-size": "1"})

		defaultParts := sociArtifact(t, defaultResp[exptypes.ExporterSOCIImageNameKey])
		lowParts := sociArtifact(t, lowResp[exptypes.ExporterSOCIImageNameKey])

		require.Greater(t, len(lowParts.sociManifest.Layers), len(defaultParts.sociManifest.Layers),
			"lowering soci-min-layer-size should produce ztocs for more (smaller) layers")
	})

	t.Run("soci-name overrides the target ref", func(t *testing.T) {
		explicit := registry + "/buildkit/soci-explicit:" + identity.NewID()
		_, resp := buildAndPush(t, randomLayerState("big", 12), map[string]string{
			"soci":      "true",
			"soci-name": explicit,
		})
		require.Equal(t, explicit, resp[exptypes.ExporterSOCIImageNameKey])

		// The index must actually be resolvable at the explicit ref.
		desc, _, err := contentutil.ProviderFromRef(explicit)
		require.NoError(t, err)
		require.Equal(t, resp[exptypes.ExporterSOCIDigestKey], desc.Digest.String())
	})

	t.Run("soci=true without push is rejected", func(t *testing.T) {
		def := touchState()
		marshaled, err := def.Marshal(ctx)
		require.NoError(t, err)
		_, err = c.Solve(ctx, marshaled, SolveOpt{
			Exports: []ExportEntry{{
				Type:  ExporterImage,
				Attrs: map[string]string{"name": registry + "/buildkit/soci-nopush:latest", "soci": "true"},
			}},
		}, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "requires")
	})
}
