package types

import (
	"fmt"
	"strings"
)

const (
	// DefaultRepositoryGuidanceMaxBytes is the guidance byte cap applied
	// when RepositoryGuidanceConfig.MaxBytes is zero.
	DefaultRepositoryGuidanceMaxBytes = 32 * 1024

	// MaxRepositoryGuidanceMaxBytes is the ceiling on
	// RepositoryGuidanceConfig.MaxBytes. It equals the harness's
	// operator-text sanitisation cap, so sanitising advisory guidance
	// never truncates below the configured budget.
	MaxRepositoryGuidanceMaxBytes = 50_000

	// MaxRepositoryGuidanceFiles bounds RepositoryGuidanceConfig.Files.
	MaxRepositoryGuidanceFiles = 16

	// RepositoryGuidanceTierPinned marks a guidance file whose SHA-256 the
	// operator supplied in RepositoryGuidanceConfig.PinnedSHA256.
	RepositoryGuidanceTierPinned = "pinned"

	// RepositoryGuidanceTierAdvisory marks repository-supplied guidance the
	// operator listed without pinning its content.
	RepositoryGuidanceTierAdvisory = "advisory"
)

// RepositoryGuidanceConfig opts a run into loading operator-listed
// guidance files (such as AGENTS.md) from the workspace root into the
// system prompt. Guidance never changes enforcement: permission policy,
// tool list, executor, egress and the Rule of Two all derive from the
// RunConfig alone.
type RepositoryGuidanceConfig struct {
	// Files lists workspace-root file names to load, in render order. No
	// globbing, nested paths or directory walk.
	Files []string `json:"files"`

	// MaxBytes caps the rendered guidance content, per file and in total
	// across all files. Zero resolves to DefaultRepositoryGuidanceMaxBytes.
	MaxBytes int `json:"maxBytes,omitempty"`

	// PinnedSHA256 maps an entry of Files to the hex SHA-256 of the exact
	// file content the operator approved. A pinned file that is missing or
	// whose digest differs ends the run as setup_failed before any
	// provider call. Unpinned files load as advisory guidance.
	PinnedSHA256 map[string]string `json:"pinnedSha256,omitempty"`
}

// EffectiveMaxBytes returns the guidance byte cap the loader applies.
func (c *RepositoryGuidanceConfig) EffectiveMaxBytes() int {
	if c == nil || c.MaxBytes == 0 {
		return DefaultRepositoryGuidanceMaxBytes
	}
	return c.MaxBytes
}

// RepositoryGuidanceRecord describes one configured guidance file as the
// run loaded it. It carries metadata only; the content reaches traces
// only through the system prompt, under the emitter's content-capture
// setting.
type RepositoryGuidanceRecord struct {
	Path string `json:"path"`
	Tier string `json:"tier"`
	// Missing reports that an advisory file was absent from the workspace;
	// the remaining fields are then zero.
	Missing bool `json:"missing,omitempty"`
	// Bytes is the file's size and SHA256 the hex digest of its full
	// content, both before sanitisation or truncation.
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256,omitempty"`
	// RenderedBytes is the length of the content placed in the prompt.
	RenderedBytes int  `json:"renderedBytes"`
	Truncated     bool `json:"truncated,omitempty"`
}

func validateRepositoryGuidanceConfig(config *RunConfig, errs *[]string) {
	cfg := config.RepositoryGuidance
	if cfg == nil {
		return
	}

	if config.Executor.Type == "none" {
		*errs = append(*errs, `repositoryGuidance requires an executor that can read the workspace; executor.type "none" cannot`)
	}

	if len(cfg.Files) == 0 {
		*errs = append(*errs, "repositoryGuidance.files must list at least one file")
	}
	if len(cfg.Files) > MaxRepositoryGuidanceFiles {
		*errs = append(*errs, fmt.Sprintf("repositoryGuidance.files must have <= %d entries, got %d", MaxRepositoryGuidanceFiles, len(cfg.Files)))
	}

	seen := make(map[string]bool, len(cfg.Files))
	for i, f := range cfg.Files {
		if reason := repositoryGuidancePathProblem(f); reason != "" {
			*errs = append(*errs, fmt.Sprintf("repositoryGuidance.files[%d] %q %s", i, f, reason))
			continue
		}
		if seen[f] {
			*errs = append(*errs, fmt.Sprintf("repositoryGuidance.files[%d] %q is listed more than once", i, f))
		}
		seen[f] = true
	}

	if cfg.MaxBytes < 0 || cfg.MaxBytes > MaxRepositoryGuidanceMaxBytes {
		*errs = append(*errs, fmt.Sprintf("repositoryGuidance.maxBytes must be 0 (use default %d) or between 1 and %d", DefaultRepositoryGuidanceMaxBytes, MaxRepositoryGuidanceMaxBytes))
	}

	for path, digest := range cfg.PinnedSHA256 {
		if !seen[path] {
			*errs = append(*errs, fmt.Sprintf("repositoryGuidance.pinnedSha256 key %q must also appear in repositoryGuidance.files", path))
		}
		if !isHexSHA256(digest) {
			*errs = append(*errs, fmt.Sprintf("repositoryGuidance.pinnedSha256[%q] must be a 64-character hex SHA-256 digest", path))
		}
	}
}

// repositoryGuidancePathProblem returns why p is not a valid guidance file
// name, or "" when it is. Version one reads the workspace root only, so a
// path separator is rejected outright, which also rules out absolute
// paths and ".." traversal below the root.
func repositoryGuidancePathProblem(p string) string {
	switch {
	case p == "":
		return "must not be empty"
	case strings.TrimSpace(p) != p:
		return "must not have leading or trailing whitespace"
	case p == "." || p == "..":
		return "must name a file, not a directory"
	case strings.ContainsAny(p, `/\`):
		return "must name a file in the workspace root; nested and absolute paths are not supported"
	case strings.ContainsAny(p, "*?["):
		return "must be a literal file name; globbing is not supported"
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "must not contain control characters"
		}
	}
	return ""
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
