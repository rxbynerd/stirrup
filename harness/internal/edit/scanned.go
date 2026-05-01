package edit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
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
// pass. The wrapper intercepts the result, reads the just-written
// content, runs the scanner, and:
//
//   - On a "block" finding (or a "warn" finding when BlockOnWarn is
//     set) it rolls back the write by restoring the previous file
//     contents, then returns an EditResult with Applied=false and an
//     error message naming the rule, line, and message.
//   - On a "warn" finding (without BlockOnWarn) it leaves the edit in
//     place and emits a `code_scan_warning` security event so the run
//     trace records the advisory.
//
// Construction is via NewScannedStrategy. A nil scanner or a scanner of
// type codescanner.NoneScanner short-circuits the wrapper entirely so
// the no-scan path has zero overhead beyond a single nil check.
type ScannedStrategy struct {
	inner       EditStrategy
	scanner     codescanner.CodeScanner
	emitter     SecurityEventEmitter
	blockOnWarn bool
}

// NewScannedStrategy returns a ScannedStrategy that delegates to inner
// and consults scanner after each Apply. Pass cfg=nil or a config with
// Type=="none" to disable scanning; in that case the inner strategy is
// returned unchanged so this wrapper is invisible to non-scan callers.
//
// emitter may be nil — in that case warn findings are still applied
// (the edit succeeds) but they are logged via slog only and no
// SecurityEvent is emitted. Production wiring should always supply the
// run's SecurityLogger.
func NewScannedStrategy(inner EditStrategy, scanner codescanner.CodeScanner, cfg *types.CodeScannerConfig, emitter SecurityEventEmitter) EditStrategy {
	if scanner == nil {
		return inner
	}
	if _, isNone := scanner.(codescanner.NoneScanner); isNone {
		return inner
	}
	blockOnWarn := false
	if cfg != nil {
		blockOnWarn = cfg.BlockOnWarn
	}
	return &ScannedStrategy{
		inner:       inner,
		scanner:     scanner,
		emitter:     emitter,
		blockOnWarn: blockOnWarn,
	}
}

// ToolDefinition delegates to the inner strategy unchanged: the wrapper
// must not alter the tool surface presented to the model.
func (s *ScannedStrategy) ToolDefinition() types.ToolDefinition {
	return s.inner.ToolDefinition()
}

// Apply runs the inner strategy, then scans the resulting file. On
// blocking findings the file is rolled back to its prior state.
func (s *ScannedStrategy) Apply(ctx context.Context, input json.RawMessage, exec executor.Executor) (*EditResult, error) {
	path := pathFromInput(input)

	// Snapshot the current contents so we can roll back on a block.
	// A read error is fine here — it most commonly means the file
	// does not exist yet, in which case "rollback" reduces to writing
	// an empty file. We capture existed=false so we can avoid writing
	// content the user did not have on disk before.
	prevContent, prevExisted := snapshot(ctx, exec, path)

	result, err := s.inner.Apply(ctx, input, exec)
	if err != nil {
		return nil, err
	}
	if result == nil || !result.Applied {
		// The edit did not modify the file; nothing to scan.
		return result, nil
	}

	// Read the post-edit content. If we cannot read it back, treat
	// that as a hard error — we cannot guarantee the scan ran.
	post, readErr := exec.ReadFile(ctx, result.Path)
	if readErr != nil {
		return nil, fmt.Errorf("scan post-apply: read %q: %w", result.Path, readErr)
	}

	scanRes, scanErr := s.scanner.Scan(ctx, result.Path, []byte(post))
	if scanErr != nil {
		return nil, fmt.Errorf("scan post-apply: %w", scanErr)
	}
	if scanRes == nil || len(scanRes.Findings) == 0 {
		return result, nil
	}

	blocking, warnings := classify(scanRes.Findings, s.blockOnWarn)
	if len(blocking) > 0 {
		// Roll back the write before returning the failure so the
		// workspace is left in its prior state. Best-effort: a
		// rollback failure is logged but does not mask the original
		// scan failure.
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

	// Only warnings remain — emit the security event, log, and let
	// the edit stand.
	for _, w := range warnings {
		s.emitWarn(result.Path, w)
	}
	return result, nil
}

// pathFromInput pulls the "path" field from any of the edit-strategy
// input shapes (whole-file, search-replace, udiff, or multi). Returns
// "" if the input is malformed; the caller proceeds anyway since the
// inner strategy will surface its own parse error.
func pathFromInput(input json.RawMessage) string {
	var probe struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(input, &probe)
	return probe.Path
}

// snapshot returns the file's current contents and whether it existed.
// A read error is treated as "did not exist" — for our purposes the
// distinction between ENOENT and EACCES is moot: we will not try to
// roll back content we never read.
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
// exist before, we write an empty string — the Executor interface does
// not expose a delete primitive, and a zero-byte file is a strictly
// less-bad outcome than leaving the secret-bearing content on disk.
func rollback(ctx context.Context, exec executor.Executor, path, prev string, existed bool) error {
	if path == "" {
		return errors.New("empty path")
	}
	if !existed {
		// Best-effort: zero out the file. We document this caveat
		// in the package comment.
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
			// Unknown severities are conservatively treated as
			// warnings: the scanner returned something we don't
			// recognise; log it but don't block on it.
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
// We log first so an emitter outage does not lose the trail.
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
