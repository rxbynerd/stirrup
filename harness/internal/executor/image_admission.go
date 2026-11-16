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
			patterns = append(patterns, normalisePattern(pattern))
		}
	}
	return ImageAdmissionPolicy{patterns: patterns}, nil
}

// Configured reports whether the policy has any patterns.
func (p ImageAdmissionPolicy) Configured() bool {
	return len(p.patterns) > 0
}

// Patterns returns a copy of the policy's patterns as normalisePattern
// rewrote them.
func (p ImageAdmissionPolicy) Patterns() []string {
	return slices.Clone(p.patterns)
}

// Admit returns nil when image is a syntactically valid reference that the
// policy admits. An unconfigured policy admits every valid reference. A
// configured policy refuses a reference that does not name its registry:
// Podman and CRI-O resolve short names through registries.conf, which need
// not agree with the docker.io default the patterns are matched against.
func (p ImageAdmissionPolicy) Admit(image string) error {
	ref, err := parseImageRef(image)
	if err != nil {
		return err
	}
	if !p.Configured() {
		return nil
	}
	if !ref.namesRegistry {
		return fmt.Errorf("image %q %w: the reference must name its registry, as in %q",
			image, ErrImageNotAdmitted, ref.name())
	}
	for _, pattern := range p.patterns {
		if imagePatternMatches(pattern, ref.segments) {
			return nil
		}
	}
	return fmt.Errorf("image %q (resolved to %q) %w", image, ref.name(), ErrImageNotAdmitted)
}

// ErrImageNotAdmitted marks a valid reference that a configured policy
// refuses, as distinct from a malformed reference.
var ErrImageNotAdmitted = errors.New("is not admitted by the image admission policy")

// ImageDigestPinned reports whether image is a valid reference that names
// its content by digest ("repo@sha256:..."), so it cannot move to different
// content.
func ImageDigestPinned(image string) bool {
	ref, err := parseImageRef(image)
	return err == nil && ref.digest != ""
}

// imagePatternMatches matches segment by segment so no glob can match "/".
// A pattern ending in "/" matches any reference with at least one further
// segment; any other pattern must match every segment. A bracketed IPv6 host
// is compared literally, since "[" would otherwise open a glob class.
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
		if i == 0 && strings.HasPrefix(seg, "[") {
			if seg != ref[0] {
				return false
			}
			continue
		}
		if ok, _ := path.Match(seg, ref[i]); !ok {
			return false
		}
	}
	return true
}

var ipv6HostPattern = regexp.MustCompile(`^` + refIPv6Host + `(?::[0-9]+)?$`)

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
	host := segments[0]
	if !looksLikeRegistryHost(host) {
		return fmt.Errorf("image admission pattern %q must start with a registry host, as in %q", pattern, "docker.io/"+body)
	}
	if strings.Contains(pattern, "@") || slices.ContainsFunc(segments[1:], func(s string) bool { return strings.Contains(s, ":") }) {
		return fmt.Errorf("image admission pattern %q names a tag or digest; patterns match the repository path, with tags and digests removed", pattern)
	}
	globs := segments
	switch {
	case strings.HasPrefix(host, "["):
		if !ipv6HostPattern.MatchString(host) {
			return fmt.Errorf("image admission pattern %q has a malformed IPv6 registry host; an IPv6 host is matched literally and cannot hold a glob", pattern)
		}
		globs = segments[1:]
	case pattern != "*/" && !hostHasLiteralLabel(host):
		return fmt.Errorf("image admission pattern %q has a registry host made only of glob characters; name the registry (for example %q), or write \"*/\" to admit every registry", pattern, "*.corp.example/")
	}
	for _, seg := range globs {
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

// hostHasLiteralLabel reports whether a pattern host, its port removed, has
// at least one dot-separated label free of glob characters.
func hostHasLiteralLabel(host string) bool {
	if colon := strings.LastIndex(host, ":"); colon >= 0 {
		host = host[:colon]
	}
	return slices.ContainsFunc(strings.Split(host, "."), func(label string) bool {
		return label != "" && !strings.ContainsAny(label, "*?[")
	})
}

// normalisePattern folds the Docker Hub pull aliases onto docker.io and
// applies Docker Hub's library/ rule to a whole-reference pattern with one
// repository segment, so a pattern names references the way parseImageRef
// resolves them.
func normalisePattern(pattern string) string {
	for _, alias := range []string{"index.docker.io/", "registry-1.docker.io/"} {
		if rest, ok := strings.CutPrefix(pattern, alias); ok {
			pattern = "docker.io/" + rest
			break
		}
	}
	if rest, ok := strings.CutPrefix(pattern, "docker.io/"); ok && rest != "" && !strings.Contains(rest, "/") {
		return "docker.io/library/" + rest
	}
	return pattern
}

func invalidImageRune(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsControl(r)
}

// The reference grammar of github.com/distribution/reference, which Docker,
// containerd, Podman and CRI-O parse image references with.
const (
	refPathComponent   = `[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*`
	refDomainComponent = `(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9])`
	refIPv6Host        = `\[(?:[a-fA-F0-9:]+)\]`
	refDomain          = `(?:` + refDomainComponent + `(?:\.` + refDomainComponent + `)*|` + refIPv6Host + `)(?::[0-9]+)?`
	refName            = `(?:` + refDomain + `/)?` + refPathComponent + `(?:/` + refPathComponent + `)*`
	refTag             = `[\w][\w.-]{0,127}`
	refDigest          = `[A-Za-z][A-Za-z0-9]*(?:[-_+.][A-Za-z][A-Za-z0-9]*)*:[0-9a-fA-F]{32,}`
	refNameMaxBytes    = 255
)

var imageReferencePattern = regexp.MustCompile(`^(` + refName + `)(?::(` + refTag + `))?(?:@(` + refDigest + `))?$`)

// parsedImageRef is an image reference resolved the way the engines resolve
// it: the Docker Hub pull aliases folded onto docker.io, and library/ added
// to a one-segment Docker Hub repository.
type parsedImageRef struct {
	segments      []string
	namesRegistry bool
	digest        string
}

func (r parsedImageRef) name() string {
	return strings.Join(r.segments, "/")
}

// parseImageRef validates image against the reference grammar and splits
// off its registry host with the engines' rule: the first segment is a host
// when it holds "." or ":", is "localhost", or has an upper-case letter.
func parseImageRef(image string) (parsedImageRef, error) {
	if image == "" {
		return parsedImageRef{}, fmt.Errorf("image reference is empty")
	}
	m := imageReferencePattern.FindStringSubmatch(image)
	if m == nil {
		return parsedImageRef{}, fmt.Errorf("image %q is not a valid image reference (an optional registry host, a lowercase repository path, an optional :tag and an optional @digest)", image)
	}
	name := m[1]
	if len(name) > refNameMaxBytes {
		return parsedImageRef{}, fmt.Errorf("image %q has a repository name longer than %d bytes", image, refNameMaxBytes)
	}
	host, remainder, namesRegistry := "docker.io", name, false
	if first, rest, found := strings.Cut(name, "/"); found &&
		(first == "localhost" || strings.ContainsAny(first, ".:") || strings.ToLower(first) != first) {
		host, remainder, namesRegistry = first, rest, true
	}
	if host == "index.docker.io" || host == "registry-1.docker.io" {
		host = "docker.io"
	}
	if host == "docker.io" && !strings.Contains(remainder, "/") {
		remainder = "library/" + remainder
	}
	return parsedImageRef{
		segments:      append([]string{host}, strings.Split(remainder, "/")...),
		namesRegistry: namesRegistry,
		digest:        m[3],
	}, nil
}
