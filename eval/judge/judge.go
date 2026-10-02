// Package judge evaluates whether a harness run's output meets the criteria
// defined in an EvalJudge. It supports test-command, file-exists, file-contains,
// diff-review, and composite judge types.
package judge

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
)

const commandTimeout = 5 * time.Minute

// KnownJudgeTypes returns the set of judge types Evaluate accepts. It is
// the single source of truth for authoring-time validation (e.g. the HCL
// spec loader) so a new judge type here cannot drift out of sync.
func KnownJudgeTypes() []string {
	return []string{
		"test-command",
		"file-exists",
		"file-contains",
		"diff-review",
		"tool-trace",
		"composite",
	}
}

// ContainsType reports whether j, or any judge nested under it, has the given
// type.
func ContainsType(j types.EvalJudge, judgeType string) bool {
	if j.Type == judgeType {
		return true
	}
	for _, sub := range j.Judges {
		if ContainsType(sub, judgeType) {
			return true
		}
	}
	return false
}

// JudgeContext provides the environment for judging a run's outcome.
type JudgeContext struct {
	WorkspaceDir string // path to the workspace after the run

	// Trace is the run's parsed RunTrace, used by the "tool-trace" judge.
	// Nil for callers that judge only workspace state.
	Trace *types.RunTrace

	// Baseline is the commit "diff-review" judges diff the workspace
	// against. Nil falls back to the HEAD of the workspace's own
	// repository.
	Baseline *Baseline

	// Options carries invocation-scoped settings for LLM-backed judges.
	Options
}

// Evaluate applies the judge criteria to the workspace and returns a verdict.
// A non-nil error means the judge could not rule. An LLM-backed judge then
// also returns an error-status verdict carrying its Record; every other judge
// returns the zero verdict. A composite reports a sub-judge that could not
// rule through Status "error" rather than an error, and returns an error only
// for an invalid judge tree.
func Evaluate(ctx context.Context, j types.EvalJudge, jctx JudgeContext) (eval.JudgeVerdict, error) {
	verdict, err := evaluate(ctx, j, jctx)
	if err == nil && verdict.Status == "" {
		verdict.Status = types.JudgeStatusFail
		if verdict.Passed {
			verdict.Status = types.JudgeStatusPass
		}
	}
	return verdict, err
}

func evaluate(ctx context.Context, j types.EvalJudge, jctx JudgeContext) (eval.JudgeVerdict, error) {
	switch j.Type {
	case "test-command":
		return evaluateTestCommand(ctx, j, jctx)
	case "file-exists":
		return evaluateFileExists(j, jctx)
	case "file-contains":
		return evaluateFileContains(j, jctx)
	case "diff-review":
		return evaluateDiffReview(ctx, j, jctx)
	case "tool-trace":
		return evaluateToolTrace(j, jctx)
	case "composite":
		return evaluateComposite(ctx, j, jctx)
	default:
		return eval.JudgeVerdict{}, fmt.Errorf("unknown judge type: %q", j.Type)
	}
}

// resolvePath resolves a relative path within the workspace, returning an error
// if the resolved path escapes the workspace directory.
func resolvePath(workspaceDir, relPath string) (string, error) {
	absWorkspace, err := filepath.Abs(workspaceDir)
	if err != nil {
		return "", fmt.Errorf("resolving workspace: %w", err)
	}

	joined := filepath.Join(absWorkspace, relPath)
	resolved, err := filepath.Abs(filepath.Clean(joined))
	if err != nil {
		return "", fmt.Errorf("resolving path %q: %w", relPath, err)
	}

	// Ensure the resolved path is within or equal to the workspace.
	if !strings.HasPrefix(resolved, absWorkspace+string(filepath.Separator)) && resolved != absWorkspace {
		return "", fmt.Errorf("path %q resolves outside workspace", relPath)
	}

	return resolved, nil
}

func evaluateTestCommand(ctx context.Context, j types.EvalJudge, jctx JudgeContext) (eval.JudgeVerdict, error) {
	if j.Command == "" {
		return eval.JudgeVerdict{}, fmt.Errorf("test-command judge requires a command")
	}

	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "sh", "-c", j.Command)
	cmd.Dir = jctx.WorkspaceDir
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return eval.JudgeVerdict{Passed: true, Reason: "command exited 0"}, nil
	}

	if ctx.Err() == context.DeadlineExceeded {
		return eval.JudgeVerdict{
			Passed: false,
			Reason: fmt.Sprintf("command timed out after %s", commandTimeout),
		}, nil
	}

	combined := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
	return eval.JudgeVerdict{
		Passed: false,
		Reason: fmt.Sprintf("command failed: %v\n%s", err, combined),
	}, nil
}

func evaluateFileExists(j types.EvalJudge, jctx JudgeContext) (eval.JudgeVerdict, error) {
	if len(j.Paths) == 0 {
		return eval.JudgeVerdict{Passed: true, Reason: "no paths to check"}, nil
	}

	var missing []string
	for _, p := range j.Paths {
		resolved, err := resolvePath(jctx.WorkspaceDir, p)
		if err != nil {
			return eval.JudgeVerdict{}, err
		}
		if _, err := os.Stat(resolved); os.IsNotExist(err) {
			missing = append(missing, p)
		} else if err != nil {
			return eval.JudgeVerdict{}, fmt.Errorf("checking path %q: %w", p, err)
		}
	}

	if len(missing) == 0 {
		return eval.JudgeVerdict{Passed: true, Reason: "all paths exist"}, nil
	}

	return eval.JudgeVerdict{
		Passed: false,
		Reason: fmt.Sprintf("missing paths: %s", strings.Join(missing, ", ")),
	}, nil
}

func evaluateFileContains(j types.EvalJudge, jctx JudgeContext) (eval.JudgeVerdict, error) {
	if j.Path == "" {
		return eval.JudgeVerdict{}, fmt.Errorf("file-contains judge requires a path")
	}
	if j.Pattern == "" {
		return eval.JudgeVerdict{}, fmt.Errorf("file-contains judge requires a pattern")
	}

	resolved, err := resolvePath(jctx.WorkspaceDir, j.Path)
	if err != nil {
		return eval.JudgeVerdict{}, err
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return eval.JudgeVerdict{
				Passed: false,
				Reason: fmt.Sprintf("file %q does not exist", j.Path),
			}, nil
		}
		return eval.JudgeVerdict{}, fmt.Errorf("reading %q: %w", j.Path, err)
	}

	matched, err := regexp.MatchString(j.Pattern, string(data))
	if err != nil {
		return eval.JudgeVerdict{}, fmt.Errorf("invalid pattern %q: %w", j.Pattern, err)
	}

	if matched {
		return eval.JudgeVerdict{
			Passed: true,
			Reason: fmt.Sprintf("pattern %q found in %s", j.Pattern, j.Path),
		}, nil
	}

	return eval.JudgeVerdict{
		Passed: false,
		Reason: fmt.Sprintf("pattern %q not found in %s", j.Pattern, j.Path),
	}, nil
}

// evaluateComposite runs sub-judges in declared order and stops at the first
// one that decides the outcome: a fail or error under "all", a pass under
// "any". Sub-judge errors are carried in Status; only an invalid judge tree
// returns an error.
func evaluateComposite(ctx context.Context, j types.EvalJudge, jctx JudgeContext) (eval.JudgeVerdict, error) {
	if err := validateComposite(j); err != nil {
		return eval.JudgeVerdict{}, err
	}

	require := j.Require
	if require == "" {
		require = "all"
	}
	requireAny := require == "any"
	total := len(j.Judges)

	details := make([]eval.JudgeDetail, 0, total)
	decider, errored, firstErr := -1, 0, -1
	for i, sub := range j.Judges {
		d := evaluateSubJudge(ctx, sub, jctx)
		details = append(details, d)
		switch d.Status {
		case types.JudgeStatusPass:
			if requireAny {
				decider = i
			}
		case types.JudgeStatusFail:
			if !requireAny {
				decider = i
			}
		default:
			errored++
			if firstErr < 0 {
				firstErr = i
			}
			if !requireAny {
				decider = i
			}
		}
		if decider >= 0 {
			break
		}
	}

	skipped := total - len(details)
	for i := len(details); i < total; i++ {
		details = append(details, eval.JudgeDetail{
			Type:   j.Judges[i].Type,
			Status: eval.JudgeStatusSkipped,
			Reason: fmt.Sprintf("not evaluated: sub-judge %d of %d had already decided", decider+1, total),
		})
	}

	var status, reason string
	switch {
	case decider >= 0:
		d := details[decider]
		status = d.Status
		reason = fmt.Sprintf("sub-judge %d of %d (%s) %s (require %s)", decider+1, total, d.Type, subJudgeVerb(d.Status), require)
		if skipped > 0 {
			reason += fmt.Sprintf("; %d skipped", skipped)
		}
		if d.Status == types.JudgeStatusError {
			reason += ": " + d.Reason
		}
	case requireAny && errored > 0:
		status = types.JudgeStatusError
		reason = fmt.Sprintf("0 of %d sub-judges passed (require any); %d errored, first: sub-judge %d of %d (%s): %s",
			total, errored, firstErr+1, total, details[firstErr].Type, details[firstErr].Reason)
	case requireAny:
		status = types.JudgeStatusFail
		reason = fmt.Sprintf("0 of %d sub-judges passed (require any)", total)
	default:
		status = types.JudgeStatusPass
		reason = fmt.Sprintf("all %d sub-judges passed", total)
	}

	return eval.JudgeVerdict{
		Passed:  status == types.JudgeStatusPass,
		Status:  status,
		Reason:  reason,
		Details: details,
	}, nil
}

// evaluateSubJudge runs one sub-judge and reports its verdict as a detail. An
// error from the sub-judge becomes an error-status detail instead of aborting
// the composite.
func evaluateSubJudge(ctx context.Context, sub types.EvalJudge, jctx JudgeContext) eval.JudgeDetail {
	verdict, err := Evaluate(ctx, sub, jctx)
	d := eval.JudgeDetail{
		Type:    sub.Type,
		Passed:  verdict.Passed,
		Status:  verdict.Status,
		Reason:  verdict.Reason,
		Record:  verdict.Record,
		Details: verdict.Details,
	}
	if err != nil {
		d.Passed = false
		d.Status = types.JudgeStatusError
		if d.Reason == "" {
			d.Reason = err.Error()
		}
	}
	return d
}

func subJudgeVerb(status string) string {
	switch status {
	case types.JudgeStatusPass:
		return "passed"
	case types.JudgeStatusFail:
		return "failed"
	default:
		return "errored"
	}
}

// validateComposite rejects a judge tree that cannot be evaluated. It runs
// before any sub-judge so a configuration error is never mistaken for a
// sub-judge error or hidden behind a short-circuit.
func validateComposite(j types.EvalJudge) error {
	if len(j.Judges) == 0 {
		return fmt.Errorf("composite judge requires at least one sub-judge")
	}
	if j.Require != "" && j.Require != "all" && j.Require != "any" {
		return fmt.Errorf("invalid require value: %q (must be \"all\" or \"any\")", j.Require)
	}
	known := KnownJudgeTypes()
	for i, sub := range j.Judges {
		if !slices.Contains(known, sub.Type) {
			return fmt.Errorf("sub-judge %d: unknown judge type: %q", i+1, sub.Type)
		}
		if sub.Type == "composite" {
			if err := validateComposite(sub); err != nil {
				return fmt.Errorf("sub-judge %d: %w", i+1, err)
			}
		}
	}
	return nil
}
