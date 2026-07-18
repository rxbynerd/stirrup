package edit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/security/codescanner"
	"github.com/rxbynerd/stirrup/types"
)

// SecurityEventEmitter is the minimal interface ScannedStrategy needs to
// emit a code_scan_warning event without dragging the security package
// into edit's import graph. *security.SecurityLogger satisfies this.
type SecurityEventEmitter interface {
	Emit(level, event string, data map[string]any)
}

// ScannedStrategy wraps an EditStrategy with a post-Apply CodeScanner
// pass: a block finding rolls the write back and returns Applied=false;
// a warn finding (or a promoted warn under BlockOnWarn) leaves the edit
// in place and emits a code_scan_warning event.
//
// Construction is via NewScannedStrategy. A nil scanner or a scanner of
// type codescanner.NoneScanner short-circuits the wrapper entirely.
type ScannedStrategy struct {
	inner       EditStrategy
	scanner     codescanner.CodeScanner
	scannerName string // "patterns" / "semgrep" / "composite" — for metric attrs
	emitter     SecurityEventEmitter
	blockOnWarn bool

	// Metrics is optional; nil is safe. Field-injected from the factory.
	Metrics *observability.Metrics
}

// NewScannedStrategy returns a ScannedStrategy that delegates to inner
// and consults scanner after each Apply. Pass cfg=nil or a config with
// Type=="none" to disable scanning; the inner strategy is then returned
// unchanged.
//
// emitter may be nil: warn findings still apply, but are logged via
// slog only, with no SecurityEvent emitted. Production wiring should
// always supply the run's SecurityLogger.
func NewScannedStrategy(inner EditStrategy, scanner codescanner.CodeScanner, cfg *types.CodeScannerConfig, emitter SecurityEventEmitter) EditStrategy {
	if scanner == nil {
		return inner
	}
	if _, isNone := scanner.(codescanner.NoneScanner); isNone {
		return inner
	}
	blockOnWarn := false
	scannerName := "unknown"
	if cfg != nil {
		blockOnWarn = cfg.BlockOnWarn
		if cfg.Type != "" {
			scannerName = cfg.Type
		}
	}
	return &ScannedStrategy{
		inner:       inner,
		scanner:     scanner,
		scannerName: scannerName,
		emitter:     emitter,
		blockOnWarn: blockOnWarn,
	}
}

// ToolDefinition delegates to the inner strategy unchanged: the wrapper
// must not alter the tool surface presented to the model.
func (s *ScannedStrategy) ToolDefinition() types.ToolDefinition {
	return s.inner.ToolDefinition()
}

// Inner returns the wrapped EditStrategy, so the factory can reach an
// underlying *MultiStrategy to inject Metrics.
func (s *ScannedStrategy) Inner() EditStrategy {
	return s.inner
}

// Apply runs the inner strategy, then scans the resulting file. On
// blocking findings the file is rolled back to its prior state.
func (s *ScannedStrategy) Apply(ctx context.Context, input json.RawMessage, exec executor.Executor) (*EditResult, error) {
	path := pathFromInput(input)

	// Snapshot the pre-edit contents for rollback on a block. A read
	// error most commonly means the file does not exist yet; existed=false
	// records that so rollback doesn't write content that was never there.
	prevContent, prevExisted := snapshot(ctx, exec, path)

	result, err := s.inner.Apply(ctx, input, exec)
	if err != nil {
		return nil, err
	}
	if result == nil || !result.Applied {
		return result, nil
	}

	// A read-back failure is a hard error: we cannot guarantee the scan ran.
	post, readErr := exec.ReadFile(ctx, result.Path)
	if readErr != nil {
		return nil, fmt.Errorf("scan post-apply: read %q: %w", result.Path, readErr)
	}

	// Record one scan attempt regardless of outcome so dashboards see
	// the rate of scans even when scanners frequently no-op.
	s.recordScan(ctx)

	scanRes, scanErr := s.scanner.Scan(ctx, result.Path, []byte(post))
	if scanErr != nil {
		return nil, fmt.Errorf("scan post-apply: %w", scanErr)
	}
	if scanRes == nil || len(scanRes.Findings) == 0 {
		return result, nil
	}

	blocking, warnings := classify(scanRes.Findings, s.blockOnWarn)
	s.recordFindings(ctx, blocking, true)
	s.recordFindings(ctx, warnings, false)
	if len(blocking) > 0 {
		// Best-effort rollback: a failure here is logged but does not
		// mask the original scan-blocked result.
		if rbErr := rollback(ctx, exec, result.Path, prevContent, prevExisted); rbErr != nil {
			slog.Error("codescanner: rollback failed",
				"path", result.Path,
				"error", rbErr.Error(),
			)
		}
		return &EditResult{
			Path:    result.Path,
			Applied: false,
			Error:   "code scan blocked edit: " + formatFindings(blocking),
		}, nil
	}

	for _, w := range warnings {
		s.emitWarn(result.Path, w)
	}
	return result, nil
}

// pathFromInput pulls the "path" field from any of the edit-strategy
// input shapes (whole-file, search-replace, udiff, or multi). Returns
// "" on malformed input; the caller proceeds anyway since the inner
// strategy will surface its own parse error.
func pathFromInput(input json.RawMessage) string {
	var probe struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(input, &probe)
	return probe.Path
}

// snapshot returns the file's current contents and whether it existed.
// A read error is treated as "did not exist": the distinction between
// ENOENT and EACCES is moot since we won't roll back content never read.
func snapshot(ctx context.Context, exec executor.Executor, path string) (string, bool) {
	if path == "" {
		return "", false
	}
	c, err := exec.ReadFile(ctx, path)
	if err != nil {
		return "", false
	}
	return c, true
}

// rollback restores prev as the file's content. If the file did not
// exist before, it writes an empty string: the Executor interface has
// no delete primitive, and a zero-byte file beats leaving secret-bearing
// content on disk.
func rollback(ctx context.Context, exec executor.Executor, path, prev string, existed bool) error {
	if path == "" {
		return errors.New("empty path")
	}
	if !existed {
		return exec.WriteFile(ctx, path, "")
	}
	return exec.WriteFile(ctx, path, prev)
}

// classify splits findings into blocking and warning buckets, applying
// BlockOnWarn promotion if configured.
func classify(findings []codescanner.Finding, blockOnWarn bool) (block, warn []codescanner.Finding) {
	for _, f := range findings {
		switch f.Severity {
		case codescanner.SeverityBlock:
			block = append(block, f)
		case codescanner.SeverityWarn:
			if blockOnWarn {
				block = append(block, f)
			} else {
				warn = append(warn, f)
			}
		default:
			// Unrecognised severity: treat conservatively as a warning.
			warn = append(warn, f)
		}
	}
	return block, warn
}

// formatFindings produces a single-line summary of blocking findings
// suitable for an EditResult.Error field. Format:
//
//	rule@line: message; rule@line: message
//
// Findings are kept in input order for stability.
func formatFindings(findings []codescanner.Finding) string {
	parts := make([]string, 0, len(findings))
	for _, f := range findings {
		parts = append(parts, fmt.Sprintf("%s@%d: %s", f.Rule, f.Line, f.Message))
	}
	return strings.Join(parts, "; ")
}

// emitWarn records one warn finding via slog and the security emitter.
// Logging first means an emitter outage does not lose the trail.
func (s *ScannedStrategy) emitWarn(path string, f codescanner.Finding) {
	slog.Warn("codescanner: warn finding (edit allowed)",
		"path", path,
		"rule", f.Rule,
		"line", f.Line,
		"message", f.Message,
	)
	if s.emitter == nil {
		return
	}
	s.emitter.Emit("warn", "code_scan_warning", map[string]any{
		"path":    path,
		"rule":    f.Rule,
		"line":    f.Line,
		"message": f.Message,
	})
}

// recordScan records one stirrup.codescanner.scans observation for the
// configured scanner. A nil Metrics short-circuits.
func (s *ScannedStrategy) recordScan(ctx context.Context) {
	if s.Metrics == nil {
		return
	}
	s.Metrics.CodeScannerScans.Add(ctx, 1, metric.WithAttributes(
		attribute.String("scanner", s.scannerName),
	))
}

// recordFindings records one stirrup.codescanner.findings observation
// per finding, tagged with its own Severity and the given blocked flag
// (true for the blocking bucket, which includes BlockOnWarn promotions).
func (s *ScannedStrategy) recordFindings(ctx context.Context, findings []codescanner.Finding, blocked bool) {
	if s.Metrics == nil || len(findings) == 0 {
		return
	}
	for _, f := range findings {
		s.Metrics.CodeScannerFindings.Add(ctx, 1, metric.WithAttributes(
			attribute.String("scanner", s.scannerName),
			attribute.String("severity", f.Severity),
			attribute.Bool("blocked", blocked),
		))
	}
}
