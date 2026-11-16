package executor

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// ImageAdmissionPolicy is a deployment's list of admitted sandbox image
// references. The zero value is the unconfigured policy, which admits every
// syntactically valid reference. See docs/security.md#image-admission-policy
// for the pattern grammar.
type ImageAdmissionPolicy struct {
	patterns []string
}

// ParseImageAdmissionPolicy builds a policy from entries, each of which may
// hold several comma-separated patterns. Surrounding whitespace is trimmed;
// an empty pattern, or one that could never match a normalised reference, is
// an error rather than being dropped.
func ParseImageAdmissionPolicy(entries []string) (ImageAdmissionPolicy, error) {
	var patterns []string
	for _, entry := range entries {
		for raw := range strings.SplitSeq(entry, ",") {
			pattern := strings.TrimSpace(raw)
			if err := validateImagePattern(pattern); err != nil {
				return ImageAdmissionPolicy{}, err
			}
			patterns = append(patterns, foldDockerHubHost(pattern))
		}
	}
	return ImageAdmissionPolicy{patterns: patterns}, nil
}

// Configured reports whether the policy has any patterns.
func (p ImageAdmissionPolicy) Configured() bool {
	return len(p.patterns) > 0
}

// Patterns returns a copy of the policy's patterns, Docker Hub hosts folded.
func (p ImageAdmissionPolicy) Patterns() []string {
	return slices.Clone(p.patterns)
}

// Admit returns nil when image is a syntactically valid reference that the
// policy admits. An unconfigured policy admits every valid reference. A
// configured policy refuses a reference that does not name its registry:
// Podman and CRI-O resolve short names through registries.conf, which need
// not agree with the docker.io default the patterns are matched against.
func (p ImageAdmissionPolicy) Admit(image string) error {
	ref, err := imageRefSegments(image)
	if err != nil {
		return err
	}
	if !p.Configured() {
		return nil
	}
	if !imageNamesRegistry(image) {
		return fmt.Errorf("image %q %w: the reference must name its registry, as in %q",
			image, ErrImageNotAdmitted, strings.Join(ref, "/"))
	}
	for _, pattern := range p.patterns {
		if imagePatternMatches(pattern, ref) {
			return nil
		}
	}
	return fmt.Errorf("image %q (resolved to %q) %w %v",
		image, strings.Join(ref, "/"), ErrImageNotAdmitted, p.patterns)
}

// ErrImageNotAdmitted marks a valid reference that a configured policy
// refuses, as distinct from a malformed reference.
var ErrImageNotAdmitted = errors.New("is not admitted by the image admission policy")

// ociDigestSuffix is the OCI image-spec digest grammar anchored after "@".
var ociDigestSuffix = regexp.MustCompile(`@[a-z0-9]+(?:[.+_-][a-z0-9]+)*:[a-zA-Z0-9=_-]+$`)

// ImageDigestPinned reports whether image names its content by digest
// ("repo@sha256:..."), so the reference cannot move to different content.
func ImageDigestPinned(image string) bool {
	return ociDigestSuffix.MatchString(image)
}

// imagePatternMatches matches segment by segment so no glob can match "/".
// A pattern ending in "/" matches any reference with at least one further
// segment; any other pattern must match every segment.
func imagePatternMatches(pattern string, ref []string) bool {
	prefix := strings.HasSuffix(pattern, "/")
	segments := strings.Split(strings.TrimSuffix(pattern, "/"), "/")
	if prefix && len(ref) <= len(segments) {
		return false
	}
	if !prefix && len(ref) != len(segments) {
		return false
	}
	for i, seg := range segments {
		if ok, _ := path.Match(seg, ref[i]); !ok {
			return false
		}
	}
	return true
}

func validateImagePattern(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("image admission pattern is empty")
	}
	if strings.ContainsFunc(pattern, invalidImageRune) {
		return fmt.Errorf("image admission pattern %q contains whitespace or a control character", pattern)
	}
	body := strings.TrimSuffix(pattern, "/")
	segments := strings.Split(body, "/")
	if body == "" || slices.Contains(segments, "") {
		return fmt.Errorf("image admission pattern %q has an empty path segment", pattern)
	}
	if !strings.HasSuffix(pattern, "/") && len(segments) == 1 {
		return fmt.Errorf("image admission pattern %q names a registry but no repository; write %q to admit every repository on it", pattern, pattern+"/")
	}
	if !looksLikeRegistryHost(segments[0]) {
		return fmt.Errorf("image admission pattern %q must start with a registry host, as in %q", pattern, "docker.io/"+body)
	}
	if strings.Contains(pattern, "@") || slices.ContainsFunc(segments[1:], func(s string) bool { return strings.Contains(s, ":") }) {
		return fmt.Errorf("image admission pattern %q names a tag or digest; patterns match the repository path, with tags and digests removed", pattern)
	}
	for _, seg := range segments {
		if strings.Contains(seg, "**") {
			return fmt.Errorf("image admission pattern %q uses \"**\"; \"*\" matches within one path segment, and a trailing \"/\" admits every path below a prefix", pattern)
		}
		if _, err := path.Match(seg, ""); err != nil {
			return fmt.Errorf("image admission pattern %q is not a valid glob: %w", pattern, err)
		}
	}
	return nil
}

// looksLikeRegistryHost applies the reference grammar's host rule (a "." or
// ":" or the literal "localhost"), and also accepts a segment holding a glob.
func looksLikeRegistryHost(segment string) bool {
	return segment == "localhost" || strings.ContainsAny(segment, ".:*?[")
}

// imageNamesRegistry reports whether image's first path segment is a
// registry host by the rule normaliseImageRef applies.
func imageNamesRegistry(image string) bool {
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	first, _, found := strings.Cut(image, "/")
	return found && (first == "localhost" || strings.ContainsAny(first, ".:"))
}

func foldDockerHubHost(pattern string) string {
	for _, alias := range []string{"index.docker.io/", "registry-1.docker.io/"} {
		if rest, ok := strings.CutPrefix(pattern, alias); ok {
			return "docker.io/" + rest
		}
	}
	return pattern
}

func invalidImageRune(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsControl(r)
}

// imageRefSegments normalises image and splits it into path segments,
// rejecting a reference the container engine or kubelet could not resolve.
func imageRefSegments(image string) ([]string, error) {
	if image == "" {
		return nil, fmt.Errorf("image reference is empty")
	}
	if strings.ContainsFunc(image, invalidImageRune) {
		return nil, fmt.Errorf("image %q contains whitespace or a control character", image)
	}
	ref := normaliseImageRef(image)
	segments := strings.Split(ref, "/")
	if slices.Contains(segments, "") {
		return nil, fmt.Errorf("image %q (resolved to %q) has an empty repository path segment", image, ref)
	}
	return segments, nil
}

// normaliseImageRef expands a Docker image reference to its fully-qualified
// "host/repository" form with the tag and digest removed, mirroring the
// reference-resolution rules the engine applies:
//   - "ubuntu"                  -> "docker.io/library/ubuntu"
//   - "ubuntu:26.04"            -> "docker.io/library/ubuntu"
//   - "myorg/app:tag"           -> "docker.io/myorg/app"
//   - "ghcr.io/rxbynerd/base"   -> "ghcr.io/rxbynerd/base"
//   - "host:5000/team/img@sha"  -> "host:5000/team/img"
//
// A registry host is recognised by a "." or ":" in the first path segment, or
// the literal "localhost"; otherwise the reference is treated as a Docker Hub
// short name and the docker.io default registry is prepended. The Docker Hub
// pull aliases "index.docker.io" and "registry-1.docker.io" are canonicalised
// to "docker.io" so all three forms match the same patterns.
func normaliseImageRef(image string) string {
	ref := image

	// Strip the digest first ("@sha256:...") so a ":" inside it is never
	// mistaken for a registry port or a tag separator.
	if at := strings.Index(ref, "@"); at >= 0 {
		ref = ref[:at]
	}

	host := ""
	rest := ref
	if slash := strings.Index(ref, "/"); slash >= 0 {
		candidate := ref[:slash]
		if candidate == "localhost" || strings.ContainsAny(candidate, ".:") {
			host = candidate
			rest = ref[slash+1:]
		}
	}

	if host == "index.docker.io" || host == "registry-1.docker.io" {
		host = "docker.io"
	}

	// A ":" only delimits a tag once the host has been removed, so this
	// never touches a "host:port".
	if colon := strings.LastIndex(rest, ":"); colon >= 0 {
		rest = rest[:colon]
	}

	if host == "" {
		host = "docker.io"
		if !strings.Contains(rest, "/") {
			rest = "library/" + rest
		}
	}

	return host + "/" + rest
}
