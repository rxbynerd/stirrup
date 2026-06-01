package executor

import (
	"fmt"
	"path"
	"strings"
)

// defaultRegistryAllowlist is the set of image-reference globs accepted when
// the operator does not configure RegistryAllowlist. It admits the project's
// own images on GitHub Container Registry and the curated Docker Hub
// "library/*" official images, and nothing else — an unqualified or
// third-party reference is rejected by default so a misconfigured run cannot
// silently pull an arbitrary image.
var defaultRegistryAllowlist = []string{
	"ghcr.io/stirrup/*",
	"docker.io/library/*",
}

// checkImageAllowed reports whether image satisfies one of the allowlist
// globs. An empty allowlist falls back to defaultRegistryAllowlist. The image
// is normalised to "registry-host/repo-path" form (default registry
// docker.io, default repo namespace library/) with any tag or digest stripped
// before matching, so an operator writing "docker.io/library/*" matches a bare
// "ubuntu:26.04" reference the way Docker itself resolves it.
func checkImageAllowed(image string, allowlist []string) error {
	if len(allowlist) == 0 {
		allowlist = defaultRegistryAllowlist
	}

	ref := normaliseImageRef(image)
	// A normalised ref ending in "/" has an empty final path segment (e.g.
	// "ghcr.io/stirrup/"), which "ghcr.io/stirrup/*" would match because the
	// glob "*" accepts the empty string. Reject it here with a clear message
	// rather than handing a malformed reference to the engine to fail on.
	if strings.HasSuffix(ref, "/") {
		return fmt.Errorf("image %q (resolved to %q) has an empty repository name", image, ref)
	}
	for _, pattern := range allowlist {
		ok, err := path.Match(pattern, ref)
		if err != nil {
			return fmt.Errorf("invalid registry allowlist pattern %q: %w", pattern, err)
		}
		if ok {
			return nil
		}
	}
	return fmt.Errorf("image %q (resolved to %q) is not permitted by the registry allowlist %v", image, ref, allowlist)
}

// normaliseImageRef expands a Docker image reference to its fully-qualified
// "host/repository" form with the tag and digest removed, mirroring the
// reference-resolution rules the engine applies:
//   - "ubuntu"                 -> "docker.io/library/ubuntu"
//   - "ubuntu:26.04"           -> "docker.io/library/ubuntu"
//   - "myorg/app:tag"          -> "docker.io/myorg/app"
//   - "ghcr.io/stirrup/base"   -> "ghcr.io/stirrup/base"
//   - "host:5000/team/img@sha" -> "host:5000/team/img"
//
// A registry host is recognised by a "." or ":" in the first path segment, or
// the literal "localhost"; otherwise the reference is treated as a Docker Hub
// short name and the docker.io default registry is prepended. The Docker Hub
// pull aliases "index.docker.io" and "registry-1.docker.io" are canonicalised
// to "docker.io" so an explicit "index.docker.io/library/ubuntu" still matches
// a "docker.io/library/*" allowlist pattern.
func normaliseImageRef(image string) string {
	ref := image

	// Strip the digest first ("@sha256:...") so a ":" inside it is never
	// mistaken for a registry port or a tag separator.
	if at := strings.Index(ref, "@"); at >= 0 {
		ref = ref[:at]
	}

	// Split off an optional registry host. The host is everything before the
	// first "/" when that segment looks like a hostname (contains "." or ":")
	// or is "localhost".
	host := ""
	rest := ref
	if slash := strings.Index(ref, "/"); slash >= 0 {
		candidate := ref[:slash]
		if candidate == "localhost" || strings.ContainsAny(candidate, ".:") {
			host = candidate
			rest = ref[slash+1:]
		}
	}

	// Canonicalise the Docker Hub pull aliases to the registry's display name
	// so all three forms collapse onto the same allowlist namespace.
	if host == "index.docker.io" || host == "registry-1.docker.io" {
		host = "docker.io"
	}

	// Strip the tag from the repository path. A ":" only delimits a tag once
	// the host has been removed, so this never touches a "host:port".
	if colon := strings.LastIndex(rest, ":"); colon >= 0 {
		rest = rest[:colon]
	}

	if host == "" {
		host = "docker.io"
		// A Docker Hub short name with no namespace lives under library/.
		if !strings.Contains(rest, "/") {
			rest = "library/" + rest
		}
	}

	return host + "/" + rest
}
