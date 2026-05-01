package codescanner

import (
	"context"
	"fmt"
)

// CompositeScanner runs a list of child scanners and unions their
// findings. Children are run sequentially in declaration order; the
// returned ScanResult preserves that order. Hard errors from any child
// short-circuit the scan — partial results would mask a misconfigured
// scanner.
type CompositeScanner struct {
	scanners []CodeScanner
}

// NewCompositeScanner wraps an explicit list of scanners. Callers
// typically reach Composite via New() with Type=="composite", but the
// constructor is exported so callers wiring scanners by hand (tests,
// future custom factories) can use the same union semantics.
func NewCompositeScanner(scanners ...CodeScanner) *CompositeScanner {
	return &CompositeScanner{scanners: scanners}
}

// Scan invokes each child in order and concatenates the findings.
func (c *CompositeScanner) Scan(ctx context.Context, path string, content []byte) (*ScanResult, error) {
	var all []Finding
	for _, s := range c.scanners {
		res, err := s.Scan(ctx, path, content)
		if err != nil {
			return nil, err
		}
		if res != nil {
			all = append(all, res.Findings...)
		}
	}
	return &ScanResult{Findings: all}, nil
}

// newComposite is the helper invoked by New() when the config selects
// "composite". It looks up each named scanner from the closed set
// supported by ValidateRunConfig (none, patterns, semgrep) and bundles
// them. Composite-of-composite is intentionally not supported here — the
// types validator rejects it earlier — but we defend against it again to
// keep this constructor usable from tests or future callers. The
// semgrepConfigPath argument is threaded through to any semgrep child;
// it is shared rather than per-child because v1 supports a single
// composite config object.
func newComposite(names []string, semgrepConfigPath string) (CodeScanner, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("codescanner: composite requires at least one scanner")
	}
	children := make([]CodeScanner, 0, len(names))
	for _, n := range names {
		switch n {
		case "none":
			children = append(children, NoneScanner{})
		case "patterns":
			children = append(children, NewPatternScanner())
		case "semgrep":
			children = append(children, NewSemgrepScanner(semgrepConfigPath))
		case "composite":
			return nil, fmt.Errorf("codescanner: composite cannot contain another composite")
		default:
			return nil, fmt.Errorf("codescanner: unknown composite child %q", n)
		}
	}
	return NewCompositeScanner(children...), nil
}
