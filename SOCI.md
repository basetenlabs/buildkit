# Native SOCI v2 index generation in BuildKit

BuildKit can optionally produce a **SOCI (Seekable OCI) v2** index when it builds and pushes
an image, so [soci-snapshotter](https://github.com/awslabs/soci-snapshotter) can lazily pull
layers instead of downloading them up front. It is enabled per build with the image-exporter
option `soci=true`, and is opt-in at compile time via the `soci` build tag (see [Building](#building)).

**Status:** implemented and verified end-to-end (build → push → soci-snapshotter consumes the
index; see [Verification](#verification)). The code lives in `exporter/containerimage/` plus a few
`go.mod` pins. One operational step — a full zero-download FUSE lazy pull — has only been verified
partially, blocked by an environment policy rather than the artifact; see [Remaining work](#remaining-work).

## How it works

A normal `buildctl ... --output type=image,name=<ref>,push=true` build pushes the image to its
canonical ref. With `soci=true`, BuildKit *additionally*:

1. Runs soci-snapshotter's `IndexBuilder.Convert()` over the just-committed image.
2. Pushes the resulting OCI index to a **sibling ref** — `<ref>` with `-soci` appended to the tag
   (e.g. `repo:v1` → `repo:v1-soci`), or an explicit `soci-name=<ref>`.

The canonical image is pushed **unchanged** (its digest is byte-identical to a non-SOCI build); the
SOCI index is purely additive. This "two refs" model means consumers that don't understand SOCI keep
using the canonical ref, while soci-snapshotter pulls the `-soci` ref.

### The `-soci` artifact

The `-soci` ref is an **OCI image index** with two entries:

| entry | what it is |
|---|---|
| image manifest | the original image manifest **re-serialized** with an added annotation `com.amazon.soci.index-digest` pointing at the SOCI index (re-serialization gives it a new digest — the canonical image is untouched) |
| SOCI index | an OCI manifest, `artifactType=application/vnd.amazon.soci.index.v2+json`, whose **`.layers` are the ztoc blobs** (one per indexed layer, each annotated with its source `com.amazon.soci.image-layer-digest`); carries a back-reference `com.amazon.soci.image-manifest-digest` |

Layer blobs are **shared** with the canonical image (content-addressed — the registry stores each
layer once and the `-soci` push reports the layers as "already exists"). soci-snapshotter, pulling
the `-soci` ref, reads the `index-digest` annotation, loads the SOCI index, and fetches layer spans
on demand via the ztocs.

Layers smaller than `soci-min-layer-size` (default 10 MiB) get no ztoc and are pulled normally.

## Usage

### Building a SOCI-enabled buildkitd

SOCI generation links soci-snapshotter's `ztoc` package, which uses cgo (zlib). To keep the default
daemon cgo-free and static, it is gated behind the `soci` build tag. Build with:

```sh
docker buildx bake --set buildkitd.args.BUILDKITD_TAGS=soci
# or: go build -tags soci, with CGO_ENABLED=1 (see Dockerfile buildkitd stage)
```

A default buildkitd (no `soci` tag) builds exactly as before (`CGO_ENABLED=0`, static); requesting
`soci=true` against it fails with a clear "rebuild with BUILDKITD_TAGS=soci" error.

### Requesting a SOCI index

```sh
buildctl build ... \
  --output type=image,name=registry.example/app:v1,push=true,soci=true
```

Exporter options:

| option | meaning |
|---|---|
| `soci=true` | generate & push the SOCI index. Requires `push=true`; force-enables `oci-mediatypes` (see [why](#oci-media-types-are-required)). |
| `soci-name=<ref>` | explicit ref for the index (default: canonical tag + `-soci`). Required for untagged/digest-only pushes. |
| `soci-span-size=<bytes>` | ztoc span size (default 4 MiB). |
| `soci-min-layer-size=<bytes>` | skip ztoc generation below this size (default 10 MiB). |

Response metadata keys: `image.soci.name` (the `-soci` ref) and `containerimage.soci.digest`
(the index digest).

## Implementation

All in `exporter/containerimage/`:

- **`export_soci.go`** (`//go:build soci`) — the only file importing soci-snapshotter. `buildSOCIIndex()`
  constructs a `soci.IndexBuilder` and calls `Convert()`. It bridges BuildKit to soci's two stores:
  - read side (`readerContentStore`) wraps BuildKit's content provider as a containerd `content.Store`;
    `Convert` only ever calls `ReaderAt`/`Info`, so the rest is stubbed.
  - write side (`sociBlobStore`) implements soci's `store.Store` over the worker `content.Store`
    (where the new ztoc/index/manifest blobs land); `BatchOpen` is a no-op since the exporter already
    holds a lease.
  - an **ephemeral** bbolt artifacts DB (temp dir) so the snapshotter's real DB is never touched.
- **`export_soci_stub.go`** (`//go:build !soci`) — a `buildSOCIIndex` that returns the "build without
  soci support" error, keeping the default daemon free of the soci/cgo dependency.
- **`export_soci_common.go`** — `sociRefForName` (the `-soci` ref derivation; pure Go, always compiled).
- **`opts.go` / `exptypes/keys.go` / `exptypes/types.go`** — the option fields/keys and response keys.
- **`export.go`** — in `Export()`, after `Commit()`, generate the index once and push it to each
  target's `-soci` ref. The same content provider (`collectRemotes()`) feeds both the canonical push
  and the SOCI read/push — the worker store covers manifests/config/ztocs while image layers (possibly
  lazy) stream from the per-ref remote providers, so no separate "unlazy" step is needed.

### Build wiring (Dockerfile)

The `buildkitd` stage installs a C toolchain + per-arch `zlib-static` (`xx-apk`) and sets
`CGO_ENABLED=1` only when `soci` is in `BUILDKITD_TAGS`, mirroring the existing `runc` cgo-cross
stage. `CGO_LDFLAGS` points the linker at the target sysroot's zlib (soci's `ztoc` cgo hardcodes
`-l:libz.a`).

## Design decisions

| decision | choice | why |
|---|---|---|
| SOCI index version | **v2 only** | v2 links the index to the image via manifest annotations (not the OCI `subject`/referrers API), so a normal `push` walk covers it. `Convert()` is the only public way to get a v2 index. |
| distribution | **two refs** (canonical + `-soci`) | keeps the canonical image digest byte-stable; SOCI is additive. `Convert()` produces a *new* top-level descriptor, so making it the single canonical output would be intrusive (re-threads digests through provenance/naming). |
| where Convert runs | **in-tree, behind the `soci` build tag** | one binary, no extra process, no content round-trip — `Convert` reads layers straight from the in-process provider. The build tag keeps the cost (cgo/zlib) out of the default daemon. A **sidecar** (separate module running `Convert` and pushing `-soci`) is the fallback if the dependency pins below become a maintenance burden — the artifact and the two-ref model are identical either way. |
| target worker | **runc / OCI worker** | it exposes a containerd `content.Store`, which is all `Convert` needs; no containerd image-metadata service required. |

### OCI media types are required

`Convert()` walks the image assuming OCI media types; a Docker schema2 manifest makes it fail
mid-convert. So `soci=true` force-enables `oci-mediatypes` on the export (like `oci-artifact` does).

## Dependencies

SOCI generation depends on **soci-snapshotter** as a normal Go module (its `soci` package; the `ztoc`
transitive dependency is what pulls in cgo/zlib). The exact version and the accompanying `replace`
directives live in [`go.mod`](go.mod) — that file is the source of truth. The notes here explain *why*
the pins exist, not their current values, so they don't drift as the deps are bumped:

- **soci-snapshotter is pinned to an untagged `main` commit**, not a release tag, because its
  containerd-v2 support currently exists only on `main`. Switch to a release tag once one ships with
  containerd v2 support.
- **A cluster of `replace` directives pins containerd and the opencontainers spec set back to the
  versions BuildKit builds against.** soci-snapshotter `main` builds against a newer containerd and
  runtime-spec than BuildKit does, and the newer spec set is not API-compatible (e.g. `LinuxPids.Limit`
  changed `int64` → `*int64`), so it won't compile against BuildKit's containerd without the pins.
  They drop out once BuildKit moves to a containerd release that realigns with soci-snapshotter.

> The cost of this approach is dep-graph maintenance: a future soci bump can widen the set of pins.
> That is the main argument for the sidecar fallback noted above.

### Background

The investigation that led here (why soci `main` + a containerd-v2.0.5 pin rather than an older soci
release on containerd v1, the cgo/static-binary analysis, and the in-tree-vs-sidecar comparison) is
preserved in this branch's git history. The short version: depending on an **older soci release**
(containerd v1.7) would shrink the dependency footprint but requires a containerd v1→v2 `content.Store`
adapter and mixes containerd versions; pinning **soci `main` back to containerd v2.0.5** (the current
approach) keeps a single containerd version and leaves BuildKit's own containerd path untouched, at the
cost of the spec-island pins above.

## Verification

End-to-end, against a local `registry:2` with a self-built `-tags soci` buildkitd (runc worker):

- A `soci=true,push=true` build pushes the canonical image and the `-soci` index. The registry serves
  the index as `[re-serialized image manifest, soci-index-v2]` with the bidirectional annotations; the
  large layer is stored once and shared by both refs; the canonical image digest matches a non-SOCI
  build.
- **soci-snapshotter consumes the index**: pulling the `-soci` ref, it reads the annotation, loads the
  SOCI index, matches the ztoc to the layer, and serves file content via span reads — a checksum read
  through the mount equals the original file byte-for-byte.

The artifact-shape half of this is guarded in CI by `TestBuildExportSOCI` (`client/client_soci_test.go`,
`//go:build soci`). It runs inside the regular **Integration Tests** job — which builds buildkitd with
`BUILDKITD_TAGS=soci` and runs the suite with `--tags=soci` — rather than a dedicated SOCI job, since the
tag is purely additive.

## Remaining work

- **CI arch-matrix build.** Add a job that builds `BUILDKITD_TAGS=soci` across the release arches to
  catch any cross-compile / dependency-pin drift early. (Native build is verified; the cross matrix
  is not yet exercised.)
- **Full FUSE lazy pull.** The span-read path is confirmed, but a complete zero-download FUSE mount
  was blocked by the test host's AppArmor policy denying `fusermount` (`fuse.rawBridge`) — an
  environment restriction, not an artifact problem. Re-run on a host where FUSE mounts are permitted.
- **Drop the temporary pins.** Replace the soci commit pseudo-version with a release tag, and remove
  the spec-island `replace`s, once the containerd versions align (see [Dependencies](#dependencies)).
