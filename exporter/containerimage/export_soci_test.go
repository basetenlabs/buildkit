package containerimage

import (
	"context"
	"testing"
)

func TestSOCIRefForName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		target  string
		sociOpt string
		want    string
		wantErr bool
	}{
		{
			name:   "tagged ref gets -soci tag suffix",
			target: "registry.example.com/repo:v1",
			want:   "registry.example.com/repo:v1-soci",
		},
		{
			name:   "localhost with port",
			target: "localhost:5000/app:latest",
			want:   "localhost:5000/app:latest-soci",
		},
		{
			name:   "docker hub short name normalizes and keeps tag",
			target: "busybox:1.36",
			want:   "docker.io/library/busybox:1.36-soci",
		},
		{
			name:    "explicit soci-name wins",
			target:  "registry.example.com/repo:v1",
			sociOpt: "registry.example.com/repo:custom-soci",
			want:    "registry.example.com/repo:custom-soci",
		},
		{
			name:    "untagged ref is rejected",
			target:  "registry.example.com/repo",
			wantErr: true,
		},
		{
			name:    "digest-only ref is rejected",
			target:  "registry.example.com/repo@sha256:" + "0000000000000000000000000000000000000000000000000000000000000000",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sociRefForName(tc.target, &ImageCommitOpts{SOCIName: tc.sociOpt})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", tc.target, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("sociRefForName(%q) = %q, want %q", tc.target, got, tc.want)
			}
		})
	}
}

// TestSOCIOptsLoad covers the pure-Go option parsing in ImageCommitOpts.Load for
// the soci* keys. It needs no cgo / soci build tag, so it runs in every build.
func TestSOCIOptsLoad(t *testing.T) {
	ctx := context.Background()

	t.Run("defaults: no soci keys leaves SOCI off", func(t *testing.T) {
		var c ImageCommitOpts
		if _, err := c.Load(ctx, map[string]string{}); err != nil {
			t.Fatal(err)
		}
		if c.SOCI {
			t.Fatalf("SOCI = true, want false when key absent")
		}
		if c.SOCISpanSize != 0 || c.SOCIMinLayerSize != 0 {
			t.Fatalf("span/min-layer sizes = %d/%d, want 0/0 (soci defaults applied later)", c.SOCISpanSize, c.SOCIMinLayerSize)
		}
	})

	t.Run("soci=true force-enables oci-mediatypes", func(t *testing.T) {
		var c ImageCommitOpts
		if _, err := c.Load(ctx, map[string]string{"soci": "true"}); err != nil {
			t.Fatal(err)
		}
		if !c.SOCI {
			t.Fatalf("SOCI = false, want true")
		}
		if !c.OCITypes {
			t.Fatalf("OCITypes = false, want true (soci must force OCI media types)")
		}
	})

	t.Run("soci=true overrides explicit oci-mediatypes=false", func(t *testing.T) {
		var c ImageCommitOpts
		if _, err := c.Load(ctx, map[string]string{"soci": "true", "oci-mediatypes": "false"}); err != nil {
			t.Fatal(err)
		}
		if !c.OCITypes {
			t.Fatalf("OCITypes = false, want true: soci must force OCI types even when oci-mediatypes=false")
		}
	})

	t.Run("soci=false does not touch oci-mediatypes", func(t *testing.T) {
		var c ImageCommitOpts
		if _, err := c.Load(ctx, map[string]string{"soci": "false"}); err != nil {
			t.Fatal(err)
		}
		if c.SOCI {
			t.Fatalf("SOCI = true, want false")
		}
		if c.OCITypes {
			t.Fatalf("OCITypes = true, want false when soci is off and oci-mediatypes unset")
		}
	})

	t.Run("empty value defaults soci to true", func(t *testing.T) {
		// parseBoolWithDefault(&SOCI, ..., true): a bare "soci=" enables it.
		var c ImageCommitOpts
		if _, err := c.Load(ctx, map[string]string{"soci": ""}); err != nil {
			t.Fatal(err)
		}
		if !c.SOCI {
			t.Fatalf("SOCI = false, want true for empty value (default true)")
		}
	})

	t.Run("soci-name is captured verbatim", func(t *testing.T) {
		var c ImageCommitOpts
		if _, err := c.Load(ctx, map[string]string{"soci": "true", "soci-name": "registry.example.com/repo:custom"}); err != nil {
			t.Fatal(err)
		}
		if c.SOCIName != "registry.example.com/repo:custom" {
			t.Fatalf("SOCIName = %q, want the explicit ref", c.SOCIName)
		}
	})

	t.Run("span-size and min-layer-size parse as int64", func(t *testing.T) {
		var c ImageCommitOpts
		if _, err := c.Load(ctx, map[string]string{
			"soci":                "true",
			"soci-span-size":      "1048576",
			"soci-min-layer-size": "5242880",
		}); err != nil {
			t.Fatal(err)
		}
		if c.SOCISpanSize != 1048576 {
			t.Fatalf("SOCISpanSize = %d, want 1048576", c.SOCISpanSize)
		}
		if c.SOCIMinLayerSize != 5242880 {
			t.Fatalf("SOCIMinLayerSize = %d, want 5242880", c.SOCIMinLayerSize)
		}
	})

	t.Run("non-numeric span-size is rejected", func(t *testing.T) {
		var c ImageCommitOpts
		if _, err := c.Load(ctx, map[string]string{"soci": "true", "soci-span-size": "notanumber"}); err == nil {
			t.Fatalf("expected error for non-numeric soci-span-size, got nil")
		}
	})

	t.Run("non-numeric min-layer-size is rejected", func(t *testing.T) {
		var c ImageCommitOpts
		if _, err := c.Load(ctx, map[string]string{"soci": "true", "soci-min-layer-size": "notanumber"}); err == nil {
			t.Fatalf("expected error for non-numeric soci-min-layer-size, got nil")
		}
	})

	t.Run("non-bool soci is rejected", func(t *testing.T) {
		var c ImageCommitOpts
		if _, err := c.Load(ctx, map[string]string{"soci": "notabool"}); err == nil {
			t.Fatalf("expected error for non-bool soci, got nil")
		}
	})
}
