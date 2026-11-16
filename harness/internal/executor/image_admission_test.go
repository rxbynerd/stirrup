package executor

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormaliseImageRef_ReturnsCanonicalForm(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"bare name defaults to docker hub library", "ubuntu", "docker.io/library/ubuntu"},
		{"tag is dropped", "ubuntu:26.04", "docker.io/library/ubuntu"},
		{"namespaced name defaults to docker hub", "myorg/app:tag", "docker.io/myorg/app"},
		{"explicit registry is kept", "ghcr.io/stirrup/base", "ghcr.io/stirrup/base"},
		{"explicit registry drops tag", "ghcr.io/stirrup/base:latest", "ghcr.io/stirrup/base"},
		{"digest is dropped", "registry:5000/team/img@sha256:" + strings.Repeat("b", 64), "registry:5000/team/img"},
		{"localhost without port is kept", "localhost/dev", "localhost/dev"},
		{"localhost with port drops tag", "localhost:5000/dev:tag", "localhost:5000/dev"},
		{"docker hub library path is unchanged", "docker.io/library/alpine", "docker.io/library/alpine"},
		{"index.docker.io folds to docker.io", "index.docker.io/library/ubuntu", "docker.io/library/ubuntu"},
		{"registry-1.docker.io folds to docker.io", "registry-1.docker.io/library/ubuntu:26.04", "docker.io/library/ubuntu"},
		{"index.docker.io namespace folds to docker.io", "index.docker.io/myorg/app", "docker.io/myorg/app"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tt.want, normaliseImageRef(tt.in))
		})
	}
}

func TestImageAdmissionPolicy_UnconfiguredAdmitsAnyValidReference(t *testing.T) {
	var policy ImageAdmissionPolicy
	for _, image := range []string{
		"ubuntu:26.04",
		"docker.io/someuser/app",
		"evil.example.com/malware:latest",
		"123456789012.dkr.ecr.eu-west-1.amazonaws.com/platform/sandboxes/go:1.27",
	} {
		t.Run(image, func(t *testing.T) {
			must := require.New(t)
			must.False(policy.Configured())
			must.NoError(policy.Admit(image))
		})
	}
}

func TestImageAdmissionPolicy_RejectsMalformedReferenceWithoutPolicy(t *testing.T) {
	var policy ImageAdmissionPolicy
	tests := []struct {
		name, image, wantErr string
	}{
		{"empty", "", "empty"},
		{"trailing slash leaves no repository", "ghcr.io/stirrup/", "empty repository path segment"},
		{"docker hub library with no repository", "docker.io/library/", "empty repository path segment"},
		{"double slash", "ghcr.io//base", "empty repository path segment"},
		{"embedded space", "ghcr.io/acme/base image", "whitespace"},
		{"embedded newline", "ghcr.io/acme/base\n", "control character"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.ErrorContains(policy.Admit(tt.image), tt.wantErr)
		})
	}
}

func TestImageAdmissionPolicy_PrefixPatternAdmitsEveryDepth(t *testing.T) {
	policy, err := ParseImageAdmissionPolicy([]string{
		"us-docker.pkg.dev/acme-prod/",
		"*.dkr.ecr.eu-west-1.amazonaws.com/platform/",
		"harbor.acme.internal:8443/sandboxes/",
		"ghcr.io/",
	})
	must := require.New(t)
	must.NoError(err)

	admitted := []string{
		"us-docker.pkg.dev/acme-prod/sandboxes/base:1",
		"us-docker.pkg.dev/acme-prod/sandboxes/go/toolchain/v2@sha256:" + strings.Repeat("c", 64),
		"123456789012.dkr.ecr.eu-west-1.amazonaws.com/platform/go",
		"123456789012.dkr.ecr.eu-west-1.amazonaws.com/platform/team/runtime/node:22",
		"harbor.acme.internal:8443/sandboxes/python/3.14:latest",
		"ghcr.io/anyone/anything/at/depth",
	}
	for _, image := range admitted {
		t.Run("admits "+image, func(t *testing.T) {
			must := require.New(t)
			must.NoError(policy.Admit(image))
		})
	}

	denied := []string{
		"us-docker.pkg.dev/acme-prod-evil/sandboxes/base",
		"us-docker.pkg.dev/other/acme-prod/base",
		"123456789012.dkr.ecr.us-east-1.amazonaws.com/platform/go",
		"harbor.acme.internal/sandboxes/python",
		"docker.io/library/ubuntu",
	}
	for _, image := range denied {
		t.Run("denies "+image, func(t *testing.T) {
			must := require.New(t)
			must.ErrorContains(policy.Admit(image), "not admitted by the image admission policy")
		})
	}
}

func TestImageAdmissionPolicy_PrefixPatternNeedsARepositoryBelowIt(t *testing.T) {
	must := require.New(t)
	policy, err := ParseImageAdmissionPolicy([]string{"localhost/acme/"})
	must.NoError(err)
	must.Error(policy.Admit("localhost/acme"))
	must.NoError(policy.Admit("localhost/acme/base"))
}

func TestImageAdmissionPolicy_GlobPatternMatchesOneSegment(t *testing.T) {
	policy, err := ParseImageAdmissionPolicy([]string{"ghcr.io/rxbynerd/*", "docker.io/library/ubuntu"})
	must := require.New(t)
	must.NoError(err)

	tests := []struct {
		image string
		want  bool
	}{
		{"ghcr.io/rxbynerd/stirrup-sandbox:latest", true},
		{"ghcr.io/rxbynerd/team/base", false},
		{"docker.io/library/ubuntu:26.04", true},
		{"index.docker.io/library/ubuntu", true},
		{"docker.io/library/alpine", false},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			must := require.New(t)
			err := policy.Admit(tt.image)
			if tt.want {
				must.NoError(err)
			} else {
				must.Error(err)
			}
		})
	}
}

func TestImageAdmissionPolicy_ConfiguredPolicyRefusesShortNames(t *testing.T) {
	policy, err := ParseImageAdmissionPolicy([]string{"docker.io/", "localhost/"})
	must := require.New(t)
	must.NoError(err)
	for _, image := range []string{"ubuntu", "ubuntu:26.04", "acme/base", "acme/base@sha256:" + strings.Repeat("a", 64)} {
		t.Run(image, func(t *testing.T) {
			must := require.New(t)
			err := policy.Admit(image)
			must.ErrorIs(err, ErrImageNotAdmitted)
			must.ErrorContains(err, "must name its registry")
		})
	}
	must.NoError(policy.Admit("localhost/dev"), "localhost is a registry host")
}

func TestImageAdmissionPolicy_CharacterClassNeverMatchesSlash(t *testing.T) {
	must := require.New(t)
	policy, err := ParseImageAdmissionPolicy([]string{"ghcr.io/acme[^x]base"})
	must.NoError(err)
	must.Error(policy.Admit("ghcr.io/acme/base"))
	must.NoError(policy.Admit("ghcr.io/acme-base"))
}

func TestParseImageAdmissionPolicy_SplitsCommasAndTrims(t *testing.T) {
	must := require.New(t)
	policy, err := ParseImageAdmissionPolicy([]string{" ghcr.io/acme/ , us-docker.pkg.dev/acme/", "index.docker.io/library/*"})
	must.NoError(err)
	must.True(policy.Configured())
	must.Equal([]string{"ghcr.io/acme/", "us-docker.pkg.dev/acme/", "docker.io/library/*"}, policy.Patterns())
}

func TestParseImageAdmissionPolicy_NoEntriesIsUnconfigured(t *testing.T) {
	must := require.New(t)
	policy, err := ParseImageAdmissionPolicy(nil)
	must.NoError(err)
	must.False(policy.Configured())
}

func TestParseImageAdmissionPolicy_RejectsUnmatchablePatterns(t *testing.T) {
	tests := []struct {
		name, entry, wantErr string
	}{
		{"empty entry", "", "empty"},
		{"trailing comma", "ghcr.io/acme/,", "empty"},
		{"bare slash", "/", "empty path segment"},
		{"leading slash", "/acme/base", "empty path segment"},
		{"double slash", "ghcr.io//base", "empty path segment"},
		{"host without slash", "ghcr.io", `write "ghcr.io/"`},
		{"no registry host", "myorg/app", "must start with a registry host"},
		{"tag in pattern", "ghcr.io/acme/base:latest", "tag or digest"},
		{"digest in pattern", "ghcr.io/acme/base@sha256:abc", "tag or digest"},
		{"double star", "ghcr.io/acme/**", `"**"`},
		{"bad glob", "ghcr.io/acme/[", "not a valid glob"},
		{"whitespace inside", "ghcr.io/acme /base", "whitespace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			_, err := ParseImageAdmissionPolicy([]string{tt.entry})
			must.ErrorContains(err, tt.wantErr)
		})
	}
}

func TestImageDigestPinned_RecognisesOCIDigests(t *testing.T) {
	tests := []struct {
		image string
		want  bool
	}{
		{"ghcr.io/acme/base@sha256:" + strings.Repeat("a", 64), true},
		{"ghcr.io/acme/base:1.2@sha256:" + strings.Repeat("a", 64), true},
		{"localhost:5000/base@sha512:" + strings.Repeat("f", 128), true},
		{"ghcr.io/acme/base:latest", false},
		{"ubuntu", false},
		{"ghcr.io/acme/base@", false},
		{"ghcr.io/acme/base@sha256:", false},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tt.want, ImageDigestPinned(tt.image))
		})
	}
}
