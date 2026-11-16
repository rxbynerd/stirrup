package executor

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseImageRef_ResolvesLikeTheEngines(t *testing.T) {
	tests := []struct {
		name, in, want string
		namesRegistry  bool
	}{
		{"bare name defaults to docker hub library", "ubuntu", "docker.io/library/ubuntu", false},
		{"tag is dropped", "ubuntu:26.04", "docker.io/library/ubuntu", false},
		{"namespaced name defaults to docker hub", "myorg/app:tag", "docker.io/myorg/app", false},
		{"explicit registry is kept", "ghcr.io/stirrup/base", "ghcr.io/stirrup/base", true},
		{"explicit registry drops tag", "ghcr.io/stirrup/base:latest", "ghcr.io/stirrup/base", true},
		{"digest is dropped", "registry:5000/team/img@sha256:" + strings.Repeat("b", 64), "registry:5000/team/img", true},
		{"localhost without port is kept", "localhost/dev", "localhost/dev", true},
		{"localhost with port drops tag", "localhost:5000/dev:tag", "localhost:5000/dev", true},
		{"docker hub library path is unchanged", "docker.io/library/alpine", "docker.io/library/alpine", true},
		{"one-segment docker hub path gains library", "docker.io/ubuntu:26.04", "docker.io/library/ubuntu", true},
		{"index.docker.io folds to docker.io", "index.docker.io/library/ubuntu", "docker.io/library/ubuntu", true},
		{"registry-1.docker.io is a separate host", "registry-1.docker.io/library/ubuntu:26.04", "registry-1.docker.io/library/ubuntu", true},
		{"registry-1.docker.io gains no library", "registry-1.docker.io/ubuntu", "registry-1.docker.io/ubuntu", true},
		{"sha384 digest is accepted", "ghcr.io/acme/x@sha384:" + strings.Repeat("a", 96), "ghcr.io/acme/x", true},
		{"repository path of 255 bytes is accepted", "ghcr.io/" + strings.Repeat("a", 255), "ghcr.io/" + strings.Repeat("a", 255), true},
		{"index.docker.io namespace folds to docker.io", "index.docker.io/myorg/app", "docker.io/myorg/app", true},
		{"upper-case first segment is a host", "Ghcr/acme/x", "Ghcr/acme/x", true},
		{"ipv6 host with port", "[fd00::1]:5000/team/img", "[fd00::1]:5000/team/img", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			ref, err := parseImageRef(tt.in)
			must.NoError(err)
			must.Equal(tt.want, ref.name())
			must.Equal(tt.namesRegistry, ref.namesRegistry)
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
		{"trailing slash leaves no repository", "ghcr.io/stirrup/", "not a valid image reference"},
		{"double slash", "ghcr.io//base", "not a valid image reference"},
		{"embedded space", "ghcr.io/acme/base image", "not a valid image reference"},
		{"embedded newline", "ghcr.io/acme/base\n", "not a valid image reference"},
		{"dot-dot segment", "ghcr.io/acme/../evil/x", "not a valid image reference"},
		{"tag before a later slash", "ghcr.io/acme/x:t/../../evil", "not a valid image reference"},
		{"at sign without a digest", "ghcr.io/acme/x@evil.com/y", "not a valid image reference"},
		{"upper-case repository", "ghcr.io/acme/X", "not a valid image reference"},
		{"two tags", "ghcr.io/acme/x:tag:tag2", "not a valid image reference"},
		{"short digest", "ghcr.io/acme/x@sha256:abc", "not a valid image reference"},
		{"sha256 digest of the wrong length", "ghcr.io/acme/x@sha256:" + strings.Repeat("a", 32), "a sha256 digest is 64"},
		{"sha256 digest in upper case", "ghcr.io/acme/x@sha256:" + strings.Repeat("A", 64), "lower-case hexadecimal"},
		{"unsupported digest algorithm", "ghcr.io/acme/x@md5:" + strings.Repeat("a", 32), `unsupported digest algorithm "md5"`},
		{"repository path over 255 bytes", "ghcr.io/" + strings.Repeat("a", 256), "longer than 255 bytes"},
		{"bare image ID", strings.Repeat("a", 64), "image ID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			err := policy.Admit(tt.image)
			must.ErrorContains(err, tt.wantErr)
			must.NotErrorIs(err, ErrImageNotAdmitted)
		})
	}
}

func TestImageAdmissionPolicy_PrefixPatternAdmitsEveryDepth(t *testing.T) {
	policy, err := ParseImageAdmissionPolicy([]string{
		"us-docker.pkg.dev/acme-prod/",
		"123456789012.dkr.ecr.eu-west-1.amazonaws.com/platform/",
		"harbor.acme.internal:8443/sandboxes/",
		"registry-*.acme.internal/",
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
		"registry-eu.acme.internal/team/base",
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
		"666666666666.dkr.ecr.eu-west-1.amazonaws.com/platform/evil",
		"harbor.acme.internal/sandboxes/python",
		"registry.acme.internal/team/base",
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
	must.ErrorIs(policy.Admit("localhost/acme"), ErrImageNotAdmitted)
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
				must.ErrorIs(err, ErrImageNotAdmitted)
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

func TestImageAdmissionPolicy_DeniedErrorOmitsThePatterns(t *testing.T) {
	must := require.New(t)
	policy, err := ParseImageAdmissionPolicy([]string{"registry.internal.acme/secret-team/"})
	must.NoError(err)
	err = policy.Admit("ghcr.io/acme/base")
	must.ErrorIs(err, ErrImageNotAdmitted)
	must.NotContains(err.Error(), "secret-team")
}

func TestImageAdmissionPolicy_IPv6HostIsMatchedLiterally(t *testing.T) {
	must := require.New(t)
	policy, err := ParseImageAdmissionPolicy([]string{"[fd00::1]:5000/"})
	must.NoError(err)
	must.NoError(policy.Admit("[fd00::1]:5000/acme/base"))
	must.ErrorIs(policy.Admit("f:5000/acme/evil"), ErrImageNotAdmitted)
	must.ErrorIs(policy.Admit("d:5000/acme/evil"), ErrImageNotAdmitted)
}

func TestImageAdmissionPolicy_HostStartingWithACharacterClassIsAGlob(t *testing.T) {
	must := require.New(t)
	policy, err := ParseImageAdmissionPolicy([]string{"[ab]-registry.acme.internal/"})
	must.NoError(err)
	must.NoError(policy.Admit("a-registry.acme.internal/team/base"))
	must.ErrorIs(policy.Admit("c-registry.acme.internal/team/base"), ErrImageNotAdmitted)
}

func TestImageAdmissionPolicy_RegistryOneDockerIoIsNotDockerHub(t *testing.T) {
	must := require.New(t)
	policy, err := ParseImageAdmissionPolicy([]string{"docker.io/library/ubuntu"})
	must.NoError(err)
	must.ErrorIs(policy.Admit("registry-1.docker.io/ubuntu"), ErrImageNotAdmitted)
	must.ErrorIs(policy.Admit("registry-1.docker.io/library/ubuntu"), ErrImageNotAdmitted)

	aliased, err := ParseImageAdmissionPolicy([]string{"registry-1.docker.io/library/"})
	must.NoError(err)
	must.Equal([]string{"registry-1.docker.io/library/"}, aliased.Patterns())
}

func TestImageAdmissionPolicy_DockerHubPatternUsesTheLibraryRule(t *testing.T) {
	must := require.New(t)
	policy, err := ParseImageAdmissionPolicy([]string{"docker.io/ubuntu"})
	must.NoError(err)
	must.Equal([]string{"docker.io/library/ubuntu"}, policy.Patterns())
	must.NoError(policy.Admit("docker.io/ubuntu:26.04"))
	must.NoError(policy.Admit("docker.io/library/ubuntu"))
	must.ErrorIs(policy.Admit("docker.io/ubuntu/ubuntu"), ErrImageNotAdmitted)
}

func TestImageAdmissionPolicy_StarSlashAdmitsEveryNamedRegistry(t *testing.T) {
	must := require.New(t)
	policy, err := ParseImageAdmissionPolicy([]string{"*/"})
	must.NoError(err)
	must.NoError(policy.Admit("evil.example.com/anything"))
	must.ErrorContains(policy.Admit("acme/base"), "must name its registry")
}

func TestImageAdmissionPolicy_CharacterClassNeverMatchesSlash(t *testing.T) {
	must := require.New(t)
	policy, err := ParseImageAdmissionPolicy([]string{"ghcr.io/acme[^x]base"})
	must.NoError(err)
	must.ErrorIs(policy.Admit("ghcr.io/acme/base"), ErrImageNotAdmitted)
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
		{"glob-only host with a namespace", "*/acme/", "only of glob characters"},
		{"glob-only host with a port", "*:5000/", "only of glob characters"},
		{"glob-only labels", "*.*/acme/", "only of glob characters"},
		{"malformed ipv6 host", "[fd00::1/", "malformed IPv6"},
		{"ipv6 host holding a glob", "[fd00::*]:5000/", "malformed IPv6"},
		{"upper-case namespace", "ghcr.io/Acme/", "upper-case letter"},
		{"upper-case letter after a glob", "ghcr.io/acme-*/Base", "upper-case letter"},
		{"dot-dot segment", "ghcr.io/acme/../x/", "not a valid repository path component"},
		{"trailing underscore", "ghcr.io/acme_/", "not a valid repository path component"},
		{"leading dash", "ghcr.io/-acme/", "not a valid repository path component"},
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
		{"ghcr.io/acme/base@sha256:abc", false},
		{"ghcr.io/acme/base@sha256:" + strings.Repeat("a", 32), false},
		{"ghcr.io/acme/base@md5:" + strings.Repeat("a", 32), false},
		{"ghcr.io/acme/base@foo@sha256:" + strings.Repeat("a", 64), false},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tt.want, ImageDigestPinned(tt.image))
		})
	}
}
