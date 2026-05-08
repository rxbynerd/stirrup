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

// multiSchema is the JSON Schema for the unified multi-strategy edit tool input.
// It accepts fields from all three strategies and routes to the appropriate one
// based on which fields are present.
var multiSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"path": {
			"type": "string",
			"description": "File path relative to workspace"
		},
		"content": {
			"type": "string",
			"description": "Complete file content (whole-file mode)"
		},
		"diff": {
			"type": "string",
			"description": "Unified diff to apply (udiff mode)"
		},
		"old_string": {
			"type": "string",
			"description": "Text to find (search-replace mode)"
		},
		"new_string": {
			"type": "string",
			"description": "Replacement text (search-replace mode)"
		}
	},
	"required": ["path"],
	"additionalProperties": false
}`)

// MultiStrategy wraps multiple edit strategies and tries them in order until
// one succeeds. It presents a unified input schema that accepts fields from all
// strategies and routes to the appropriate one based on which fields are present.
// If the selected strategy fails (Applied == false), it falls back to the next
// applicable strategy.
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
		Name:        "edit_file",
		Description: "Edit a file using the best available strategy. Supports three modes: unified diff (provide 'diff'), search-replace (provide 'old_string' and 'new_string'), or whole-file replacement (provide 'content'). If the primary mode fails, applicable fallback strategies are tried automatically.",
		InputSchema: multiSchema,
	}
}

// multiInput holds the parsed fields from the unified input schema. Only the
// fields relevant to the selected strategy will be populated.
type multiInput struct {
	Path      string  `json:"path"`
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

// Apply determines which strategies are applicable based on the input fields,
// then tries them in priority order (udiff > search-replace > whole-file) until
// one succeeds. A strategy "fails" when it returns Applied == false without a
// hard error; hard errors (non-nil error return) are propagated immediately.
func (m *MultiStrategy) Apply(ctx context.Context, input json.RawMessage, exec executor.Executor) (*EditResult, error) {
	start := time.Now()
	// appliedStrategy tracks the last strategy that ran (or attempted
	// to run) so we can record the edit.duration_ms histogram with
	// the strategy label that actually carried the work — the one
	// that succeeded if any did, otherwise the final candidate that
	// failed. Empty if we never reached the candidate loop (parse
	// error or no candidates). Renamed from "lastStrategy" because
	// "last" was ambiguous about success vs failure attribution.
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

	candidates := m.buildCandidates(params)
	if len(candidates) == 0 {
		return &EditResult{
			Path:    params.Path,
			Applied: false,
			Error:   "no applicable edit strategy: provide 'diff', 'old_string', or 'content'",
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

// buildCandidates returns the ordered list of applicable strategies based on
// which input fields are present. Priority: udiff > search-replace > whole-file.
func (m *MultiStrategy) buildCandidates(params multiInput) []strategyCandidate {
	var candidates []strategyCandidate

	if params.Diff != nil {
		input, err := json.Marshal(struct {
			Path string `json:"path"`
			Diff string `json:"diff"`
		}{Path: params.Path, Diff: *params.Diff})
		if err == nil {
			candidates = append(candidates, strategyCandidate{
				name:  "udiff",
				strat: m.udiff,
				input: input,
			})
		}
	}

	if params.OldString != nil {
		newString := ""
		if params.NewString != nil {
			newString = *params.NewString
		}
		input, err := json.Marshal(struct {
			Path      string `json:"path"`
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		}{Path: params.Path, OldString: *params.OldString, NewString: newString})
		if err == nil {
			candidates = append(candidates, strategyCandidate{
				name:  "search-replace",
				strat: m.searchReplace,
				input: input,
			})
		}
	}

	if params.Content != nil {
		input, err := json.Marshal(struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}{Path: params.Path, Content: *params.Content})
		if err == nil {
			candidates = append(candidates, strategyCandidate{
				name:  "whole-file",
				strat: m.wholeFile,
				input: input,
			})
		}
	}

	return candidates
}
