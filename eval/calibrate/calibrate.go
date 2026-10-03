// Package calibrate measures a diff-review judge against a golden set of
// labelled cases. docs/eval.md describes the metrics and how to read them.
package calibrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/rxbynerd/stirrup/eval/golden"
	"github.com/rxbynerd/stirrup/eval/judge"
	"github.com/rxbynerd/stirrup/types"
)

// Config is one calibration run.
type Config struct {
	// Judge is the resolved configuration every case is judged with.
	Judge types.JudgeLLMConfig

	// Repeats is how many times each case is judged; values below 1 mean
	// once. Each repeat is a separate sample in the judge cache.
	Repeats int

	// Options carries the judge cache and, for tests, a client factory.
	// LLMDefaults is ignored: Judge applies to every case.
	Options judge.Options

	// WorkDir holds the materialised workspaces. Empty uses a temporary
	// directory removed when Run returns.
	WorkDir string

	// Progress, when non-nil, receives a line per judgment.
	Progress io.Writer
}

// Judgment is one verdict on one case.
type Judgment struct {
	Case            string             `json:"case"`
	Sample          int                `json:"sample"`
	Label           string             `json:"label"`
	Verdict         string             `json:"verdict"`
	Adversarial     bool               `json:"adversarial,omitempty"`
	InjectionTarget string             `json:"injectionTarget,omitempty"`
	Reason          string             `json:"reason,omitempty"`
	Record          *types.JudgeRecord `json:"record,omitempty"`
}

// Run judges every case of set through the diff-review judge, in order. A
// judgment the judge cannot make is recorded with Verdict "error" rather
// than stopping the run; Run fails only when a case cannot be materialised
// or ctx ends, returning the judgments made so far. A judgment that errors
// because ctx ended is not among them.
func Run(ctx context.Context, set *golden.Set, cfg Config) ([]Judgment, error) {
	repeats := max(cfg.Repeats, 1)
	root := cfg.WorkDir
	if root == "" {
		dir, err := os.MkdirTemp("", "stirrup-judge-calibrate-")
		if err != nil {
			return nil, fmt.Errorf("creating calibration work directory: %w", err)
		}
		defer func() { _ = os.RemoveAll(dir) }()
		root = dir
	}
	opts := cfg.Options
	opts.LLMDefaults = nil
	llm := cfg.Judge
	total := len(set.Cases) * repeats

	var out []Judgment
	for i, c := range set.Cases {
		ws, base, err := materialise(ctx, set, c, filepath.Join(root, fmt.Sprintf("case-%03d", i+1)))
		if err != nil {
			return out, fmt.Errorf("case %s: %w", c.ID, err)
		}
		j := types.EvalJudge{Type: "diff-review", Criteria: c.Criteria, LLM: &llm}
		for sample := range repeats {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			v, err := judge.Evaluate(ctx, j, judge.JudgeContext{WorkspaceDir: ws, Baseline: &base, Sample: sample, NonDeciding: true, Options: opts})
			jm := Judgment{
				Case: c.ID, Sample: sample, Label: c.Label, Verdict: v.Status, Reason: v.Reason, Record: v.Record,
				Adversarial: c.Adversarial(), InjectionTarget: c.InjectionTarget,
			}
			if err != nil || (jm.Verdict != types.JudgeStatusPass && jm.Verdict != types.JudgeStatusFail) {
				jm.Verdict = types.JudgeStatusError
				if jm.Reason == "" && err != nil {
					jm.Reason = err.Error()
				}
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				if jm.Verdict != types.JudgeStatusError {
					out = append(out, jm)
				}
				return out, ctxErr
			}
			out = append(out, jm)
			if cfg.Progress != nil {
				_, _ = fmt.Fprintf(cfg.Progress, "[%d/%d] %s #%d: %s (label %s)\n", len(out), total, c.ID, sample, jm.Verdict, c.Label)
			}
		}
	}
	return out, nil
}

// materialise builds c's workspace under dir: the before tree, committed as
// the judge's baseline, then the after tree in the working directory.
func materialise(ctx context.Context, set *golden.Set, c golden.Case, dir string) (string, judge.Baseline, error) {
	files, err := set.Files(c)
	if err != nil {
		return "", judge.Baseline{}, err
	}
	ws := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		return "", judge.Baseline{}, fmt.Errorf("creating workspace: %w", err)
	}
	if err := writeTree(ws, files.Before); err != nil {
		return "", judge.Baseline{}, err
	}
	base, err := judge.CreateBaseline(ctx, ws, filepath.Join(dir, "judge.git"))
	if err != nil {
		return "", judge.Baseline{}, fmt.Errorf("recording baseline: %w", err)
	}
	for p := range files.Before {
		if _, kept := files.After[p]; !kept {
			if err := os.Remove(filepath.Join(ws, filepath.FromSlash(p))); err != nil && !errors.Is(err, os.ErrNotExist) {
				return "", judge.Baseline{}, fmt.Errorf("deleting %s: %w", p, err)
			}
		}
	}
	if err := writeTree(ws, files.After); err != nil {
		return "", judge.Baseline{}, err
	}
	return ws, base, nil
}

func writeTree(root string, tree golden.Tree) error {
	for p, content := range tree {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			return fmt.Errorf("writing %s: %w", p, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			return fmt.Errorf("writing %s: %w", p, err)
		}
	}
	return nil
}
