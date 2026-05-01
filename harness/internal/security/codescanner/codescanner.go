// Package codescanner runs static-analysis passes over file content the
// agent is about to write. It is invoked from the edit pipeline after the
// agent constructs the new content but before it is committed to disk, so
// "block" findings can prevent the write entirely.
//
// Three implementations are provided:
//
//   - NoneScanner   — a no-op (used when scanning is disabled).
//   - PatternScanner — pure-Go regex pack covering hardcoded secrets and
//     a small set of obviously-dangerous eval/exec sinks.
//   - SemgrepScanner — shells out to a local `semgrep` binary if present.
//   - CompositeScanner — unions findings from a set of child scanners.
//
// The constructor New dispatches on CodeScannerConfig.Type. A nil or
// empty config is treated as "none" (no scanning), so callers that have
// not yet wired the new field continue to behave as before.
package codescanner

import (
	"context"
	"fmt"

	"github.com/rxbynerd/stirrup/types"
)

// Severity values returned in Finding.Severity.
const (
	SeverityBlock = "block"
	SeverityWarn  = "warn"
)

// Finding describes a single static-analysis result. Line is 1-indexed;
// 0 means the scanner could not localise the finding to a specific line.
type Finding struct {
	Severity string // "block" | "warn"
	Rule     string
	Line     int
	Message  string
}

// ScanResult is the per-file output of a CodeScanner.
type ScanResult struct {
	Findings []Finding
}

// HasBlocking returns true if any finding has SeverityBlock.
func (r *ScanResult) HasBlocking() bool {
	if r == nil {
		return false
	}
	for _, f := range r.Findings {
		if f.Severity == SeverityBlock {
			return true
		}
	}
	return false
}

// CodeScanner inspects file content for static-analysis findings. Scan is
// called once per successful edit; path is the workspace-relative path
// being written, content is the proposed new bytes.
//
// Implementations must not modify content. They should return findings in
// a deterministic order so callers can produce stable error messages.
type CodeScanner interface {
	Scan(ctx context.Context, path string, content []byte) (*ScanResult, error)
}

// NoneScanner is a no-op CodeScanner that always returns an empty result.
// It is used when scanning is disabled (Type == "none" or config absent).
type NoneScanner struct{}

// Scan returns an empty ScanResult.
func (NoneScanner) Scan(ctx context.Context, path string, content []byte) (*ScanResult, error) {
	return &ScanResult{}, nil
}

// New constructs a CodeScanner from a CodeScannerConfig. A nil or empty
// config maps to NoneScanner so callers that have not migrated continue
// to work. Unknown Type values return an error; in production these are
// already rejected by ValidateRunConfig, but defending in depth keeps
// this constructor safe for direct callers (tests, future tooling).
func New(cfg *types.CodeScannerConfig) (CodeScanner, error) {
	if cfg == nil || cfg.Type == "" || cfg.Type == "none" {
		return NoneScanner{}, nil
	}
	switch cfg.Type {
	case "patterns":
		return NewPatternScanner(), nil
	case "semgrep":
		return NewSemgrepScanner(cfg.SemgrepConfigPath), nil
	case "composite":
		return newComposite(cfg.Scanners, cfg.SemgrepConfigPath)
	default:
		return nil, fmt.Errorf("codescanner: unknown type %q", cfg.Type)
	}
}
