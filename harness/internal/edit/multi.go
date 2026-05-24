package edit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

// multiSchema is the JSON Schema for the explicit-operation edit tool.
//
// Issue #225 removes the prior inference-based routing (the model passed
// whichever subset of {diff, old_string, content} matched its intent and
// the harness guessed). The operation field is now required and declares
// the intent; the field set the operation requires is described inline so
// strict-mode normalization (#228) can lift it without re-reading this
// file.
//
// Operation values:
//   - "replace": find old_string in the file and substitute new_string.
//     Required fields: old_string, new_string.
//   - "delete":  remove old_string from the file. Required fields:
//     old_string. (new_string is forbidden — pass "replace" if a
//     non-empty replacement is intended.)
//   - "rewrite": replace the entire file with content. Required fields:
//     content.
//   - "patch":   apply a unified diff. Required fields: diff.
//
// The schema below describes the union of fields. Per-operation field
// requirements are enforced by Apply, not the JSON Schema, because most
// JSON Schema validators struggle with conditional requireds and the
// strict-mode lint (#228) can downcast this surface per provider later.
var multiSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"path": {
			"type": "string",
			"description": "File path relative to the workspace root."
		},
		"operation": {
			"type": "string",
			"enum": ["replace", "delete", "rewrite", "patch"],
			"description": "The edit operation. 'replace' requires old_string + new_string; 'delete' requires old_string only; 'rewrite' requires content; 'patch' requires diff."
		},
		"old_string": {
			"type": "string",
			"description": "Text to find. Required for 'replace' and 'delete'."
		},
		"new_string": {
			"type": "string",
			"description": "Replacement text. Required for 'replace'."
		},
		"content": {
			"type": "string",
			"description": "Whole-file content. Required for 'rewrite'."
		},
		"diff": {
			"type": "string",
			"description": "Unified diff to apply. Required for 'patch'."
		}
	},
	"required": ["path", "operation"],
	"additionalProperties": false
}`)

// MultiStrategy wraps multiple edit strategies and dispatches based on the
// explicit `operation` field on the input. Compatible alternate fields on
// the same call still queue as soft fallbacks (e.g. "replace" with both
// old_string and content will fall through to a whole-file rewrite if the
// search-replace fails), preserving the multi-strategy resilience the
// edit_file tool was designed for without forcing the model to guess
// which strategy to ask for.
type MultiStrategy struct {
	udiff         EditStrategy
	searchReplace EditStrategy
	wholeFile     EditStrategy

	// Metrics is optional. When set, every Apply records
	// stirrup.edit.attempts (per candidate, with strategy + fell_back_from
	// + success) and stirrup.edit.duration_ms (with strategy) once at the
	// end. Field-injected from the factory; nil is safe everywhere.
	Metrics *observability.Metrics
}

// NewMultiStrategy creates a MultiStrategy with the standard strategy set:
// udiff (with the given fuzzy threshold), search-replace, and whole-file.
func NewMultiStrategy(fuzzyThreshold float64) *MultiStrategy {
	return &MultiStrategy{
		udiff:         NewUdiffStrategy(fuzzyThreshold),
		searchReplace: NewSearchReplaceStrategy(),
		wholeFile:     NewWholeFileStrategy(),
	}
}

// ToolDefinition returns the unified tool definition for the multi-strategy.
func (m *MultiStrategy) ToolDefinition() types.ToolDefinition {
	return types.ToolDefinition{
		Name: "edit_file",
		Description: "Modify an existing file in the workspace. Use this for targeted edits on a file that already exists; use write_file to create a NEW file or when the file may not exist yet. " +
			"The operation field is required and selects the strategy:\n" +
			"  - 'replace' substitutes new_string for old_string. Requires old_string + new_string. old_string must occur exactly once in the file.\n" +
			"  - 'delete' removes old_string from the file. Requires old_string only; passing new_string is rejected — choose 'replace' for any non-empty substitution.\n" +
			"  - 'rewrite' replaces the entire content of an EXISTING file with content. Requires content. Prefer write_file when authoring a new file.\n" +
			"  - 'patch' applies a unified diff. Requires diff. Useful for multi-hunk edits.\n" +
			"Example (replace): {\"path\": \"main.go\", \"operation\": \"replace\", \"old_string\": \"return nil\", \"new_string\": \"return err\"}",
		InputSchema: multiSchema,
	}
}

// multiInput holds the parsed fields from the unified input schema. Only the
// fields relevant to the selected operation are read; extra fields are
// rejected at the JSON Schema layer by additionalProperties:false.
type multiInput struct {
	Path      string  `json:"path"`
	Operation string  `json:"operation"`
	Content   *string `json:"content,omitempty"`
	Diff      *string `json:"diff,omitempty"`
	OldString *string `json:"old_string,omitempty"`
	NewString *string `json:"new_string,omitempty"`
}

// strategyCandidate pairs a strategy with the re-marshalled input it should receive.
type strategyCandidate struct {
	name  string
	strat EditStrategy
	input json.RawMessage
}

// Apply enforces the explicit-operation contract: operation is required,
// the fields required by the named operation must be present, and the
// dispatcher routes to the matching strategy. When alternate fields are
// also present they queue as soft fallbacks so a model that provides
// both a regex-friendly search-replace and a whole-file safety net does
// not lose the safety net just because it set operation explicitly.
func (m *MultiStrategy) Apply(ctx context.Context, input json.RawMessage, exec executor.Executor) (*EditResult, error) {
	start := time.Now()
	// appliedStrategy tracks the last strategy that ran (or attempted
	// to run) so we can record the edit.duration_ms histogram with
	// the strategy label that actually carried the work — the one
	// that succeeded if any did, otherwise the final candidate that
	// failed. Empty if we never reached the candidate loop (parse
	// error or no candidates).
	var appliedStrategy string
	defer func() {
		if m.Metrics != nil && appliedStrategy != "" {
			m.Metrics.EditDuration.Record(ctx, float64(time.Since(start).Milliseconds()),
				metric.WithAttributes(attribute.String("strategy", appliedStrategy)),
			)
		}
	}()

	var params multiInput
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	if params.Path == "" {
		return &EditResult{
			Applied: false,
			Error:   "path is required",
		}, nil
	}
	if params.Operation == "" {
		return &EditResult{
			Path:    params.Path,
			Applied: false,
			Error:   "operation is required: one of 'replace', 'delete', 'rewrite', 'patch'",
		}, nil
	}

	if err := validateOperationFields(params); err != nil {
		return &EditResult{
			Path:    params.Path,
			Applied: false,
			Error:   err.Error(),
		}, nil
	}

	candidates, err := m.buildCandidates(params)
	if err != nil {
		return &EditResult{
			Path:    params.Path,
			Applied: false,
			Error:   err.Error(),
		}, nil
	}

	var failures []string
	var fellBackFrom string
	for _, c := range candidates {
		appliedStrategy = c.name
		result, err := c.strat.Apply(ctx, c.input, exec)
		// Record the attempt regardless of outcome. A hard error still
		// counts as an attempt so dashboards show that the strategy
		// was tried — recording only after a clean return would hide
		// crashy strategies behind silence.
		applied := err == nil && result != nil && result.Applied
		m.recordAttempt(ctx, c.name, fellBackFrom, applied)
		if err != nil {
			return nil, err
		}
		if result.Applied {
			return result, nil
		}
		failures = append(failures, fmt.Sprintf("%s: %s", c.name, result.Error))
		// Subsequent attempts are fallbacks from this candidate.
		fellBackFrom = c.name
	}

	return &EditResult{
		Path:    params.Path,
		Applied: false,
		Error:   fmt.Sprintf("all strategies failed: %s", strings.Join(failures, "; ")),
	}, nil
}

// validateOperationFields enforces the per-operation field contract before
// the dispatcher even considers a candidate. The error messages name the
// expected fields so a model that picked the wrong operation can self-
// correct on the next turn.
func validateOperationFields(p multiInput) error {
	switch p.Operation {
	case "replace":
		if p.OldString == nil {
			return fmt.Errorf("operation 'replace' requires old_string")
		}
		if p.NewString == nil {
			return fmt.Errorf("operation 'replace' requires new_string (use operation 'delete' if you want to remove old_string)")
		}
	case "delete":
		if p.OldString == nil {
			return fmt.Errorf("operation 'delete' requires old_string")
		}
		if p.NewString != nil {
			return fmt.Errorf("operation 'delete' must not set new_string; use operation 'replace' for a non-empty substitution")
		}
	case "rewrite":
		if p.Content == nil {
			return fmt.Errorf("operation 'rewrite' requires content")
		}
	case "patch":
		if p.Diff == nil {
			return fmt.Errorf("operation 'patch' requires diff")
		}
	default:
		return fmt.Errorf("unknown operation %q (expected one of 'replace', 'delete', 'rewrite', 'patch')", p.Operation)
	}
	return nil
}

// recordAttempt emits stirrup.edit.attempts for a single candidate.
// fellBackFrom is the previous candidate's name when this is a fallback,
// or "" when this is the primary attempt. A nil Metrics short-circuits.
func (m *MultiStrategy) recordAttempt(ctx context.Context, strategy, fellBackFrom string, success bool) {
	if m.Metrics == nil {
		return
	}
	m.Metrics.EditAttempts.Add(ctx, 1, metric.WithAttributes(
		attribute.String("strategy", strategy),
		attribute.String("fell_back_from", fellBackFrom),
		attribute.Bool("success", success),
	))
}

// buildCandidates routes the operation to its primary strategy and appends
// any other applicable strategies as soft fallbacks. The primary strategy
// is fixed by `operation`; fallbacks follow only when the input also
// carries the alternate strategy's required fields.
func (m *MultiStrategy) buildCandidates(params multiInput) ([]strategyCandidate, error) {
	primary, ok := m.primaryFor(params)
	if !ok {
		// validateOperationFields already returned an error before we
		// reach here, so this branch is unreachable in practice. The
		// belt-and-braces check keeps a future contributor from adding
		// a new operation to validateOperationFields without also
		// wiring it here.
		return nil, fmt.Errorf("no strategy mapped for operation %q", params.Operation)
	}
	candidates := []strategyCandidate{primary}

	// Append other strategies in priority order (udiff → search-replace →
	// whole-file), skipping the primary and any whose required fields
	// were not supplied. This preserves the multi-strategy resilience
	// the tool was designed for: explicit operation declares intent;
	// soft fallback handles the case where the chosen strategy fails on
	// real-world inputs (fuzzy diff context, etc).
	if params.Diff != nil && primary.name != "udiff" {
		if cand, ok := buildUdiffCandidate(m.udiff, params); ok {
			candidates = append(candidates, cand)
		}
	}
	if params.OldString != nil && primary.name != "search-replace" {
		if cand, ok := buildSearchReplaceCandidate(m.searchReplace, params); ok {
			candidates = append(candidates, cand)
		}
	}
	if params.Content != nil && primary.name != "whole-file" {
		if cand, ok := buildWholeFileCandidate(m.wholeFile, params); ok {
			candidates = append(candidates, cand)
		}
	}
	return candidates, nil
}

// primaryFor returns the candidate for the operation's primary strategy.
// The bool is false only when the operation is an unrecognised string;
// validateOperationFields rejects that earlier.
func (m *MultiStrategy) primaryFor(params multiInput) (strategyCandidate, bool) {
	switch params.Operation {
	case "patch":
		return buildUdiffCandidate(m.udiff, params)
	case "replace", "delete":
		return buildSearchReplaceCandidate(m.searchReplace, params)
	case "rewrite":
		return buildWholeFileCandidate(m.wholeFile, params)
	}
	return strategyCandidate{}, false
}

// buildUdiffCandidate marshals the udiff strategy's required input from
// the unified params. The bool is false only when JSON marshaling fails,
// which is functionally impossible for these field types but kept as a
// hard guard so callers do not need to special-case it.
func buildUdiffCandidate(strat EditStrategy, params multiInput) (strategyCandidate, bool) {
	if params.Diff == nil {
		return strategyCandidate{}, false
	}
	in, err := json.Marshal(struct {
		Path string `json:"path"`
		Diff string `json:"diff"`
	}{Path: params.Path, Diff: *params.Diff})
	if err != nil {
		return strategyCandidate{}, false
	}
	return strategyCandidate{name: "udiff", strat: strat, input: in}, true
}

// buildSearchReplaceCandidate marshals the search-replace strategy's
// required input. For operation="delete" the caller has not supplied a
// new_string (and is forbidden from doing so); we default it to "" here
// so the strategy sees a complete record.
func buildSearchReplaceCandidate(strat EditStrategy, params multiInput) (strategyCandidate, bool) {
	if params.OldString == nil {
		return strategyCandidate{}, false
	}
	newString := ""
	if params.NewString != nil {
		newString = *params.NewString
	}
	in, err := json.Marshal(struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}{Path: params.Path, OldString: *params.OldString, NewString: newString})
	if err != nil {
		return strategyCandidate{}, false
	}
	return strategyCandidate{name: "search-replace", strat: strat, input: in}, true
}

// buildWholeFileCandidate marshals the whole-file strategy's required
// input. Returns false when no content field is present.
func buildWholeFileCandidate(strat EditStrategy, params multiInput) (strategyCandidate, bool) {
	if params.Content == nil {
		return strategyCandidate{}, false
	}
	in, err := json.Marshal(struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}{Path: params.Path, Content: *params.Content})
	if err != nil {
		return strategyCandidate{}, false
	}
	return strategyCandidate{name: "whole-file", strat: strat, input: in}, true
}
