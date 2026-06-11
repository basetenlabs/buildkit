package containerimage

import (
	"github.com/distribution/reference"
	"github.com/pkg/errors"
)

// sociRefForName derives the ref the SOCI index is pushed to. If the user set
// soci-name it is used verbatim; otherwise the canonical ref's tag is suffixed with
// "-soci" (registry/repo:tag -> registry/repo:tag-soci). Digest-only / untagged refs
// are rejected: SOCI needs a stable tag to publish the side artifact under.
//
// This is pure Go (no cgo / no soci import) so it is compiled in every build,
// regardless of the `soci` build tag — see export_soci.go / export_soci_stub.go.
func sociRefForName(targetName string, opts *ImageCommitOpts) (string, error) {
	if opts.SOCIName != "" {
		return opts.SOCIName, nil
	}
	named, err := reference.ParseNormalizedNamed(targetName)
	if err != nil {
		return "", errors.Wrapf(err, "soci: parse ref %q", targetName)
	}
	tagged, ok := named.(reference.Tagged)
	if !ok {
		return "", errors.Errorf("soci: cannot derive -soci ref from untagged %q; set soci-name", targetName)
	}
	withTag, err := reference.WithTag(reference.TrimNamed(named), tagged.Tag()+"-soci")
	if err != nil {
		return "", err
	}
	return withTag.String(), nil
}
