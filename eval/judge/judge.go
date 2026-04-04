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
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
)

const commandTimeout = 5 * time.Minute

// JudgeContext provides the environment for judging a run's outcome.
type JudgeContext struct {
	WorkspaceDir string // path to the workspace after the run
}

// Evaluate applies the judge criteria to the workspace and returns a verdict.
func Evaluate(ctx context.Context, j types.EvalJudge, jctx JudgeContext) (eval.JudgeVerdict, error) {
	switch j.Type {
	case "test-command":
		return evaluateTestCommand(ctx, j, jctx)
	case "file-exists":
		return evaluateFileExists(j, jctx)
	case "file-contains":
		return evaluateFileContains(j, jctx)
	case "diff-review":
		return eval.JudgeVerdict{
			Passed: false,
			Reason: "diff-review judge not yet implemented",
		}, nil
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

func evaluateComposite(ctx context.Context, j types.EvalJudge, jctx JudgeContext) (eval.JudgeVerdict, error) {
	if len(j.Judges) == 0 {
		return eval.JudgeVerdict{Passed: true, Reason: "no sub-judges"}, nil
	}

	require := j.Require
	if require == "" {
		require = "all"
	}
	if require != "all" && require != "any" {
		return eval.JudgeVerdict{}, fmt.Errorf("invalid require value: %q (must be \"all\" or \"any\")", require)
	}

	var details []eval.JudgeDetail
	passCount := 0

	for _, sub := range j.Judges {
		verdict, err := Evaluate(ctx, sub, jctx)
		if err != nil {
			return eval.JudgeVerdict{}, fmt.Errorf("sub-judge %q: %w", sub.Type, err)
		}
		details = append(details, eval.JudgeDetail{
			Type:   sub.Type,
			Passed: verdict.Passed,
			Reason: verdict.Reason,
		})
		if verdict.Passed {
			passCount++
		}
	}

	var passed bool
	var reason string

	switch require {
	case "all":
		passed = passCount == len(j.Judges)
		if passed {
			reason = fmt.Sprintf("all %d sub-judges passed", len(j.Judges))
		} else {
			reason = fmt.Sprintf("%d of %d sub-judges passed (require all)", passCount, len(j.Judges))
		}
	case "any":
		passed = passCount > 0
		if passed {
			reason = fmt.Sprintf("%d of %d sub-judges passed (require any)", passCount, len(j.Judges))
		} else {
			reason = fmt.Sprintf("0 of %d sub-judges passed (require any)", len(j.Judges))
		}
	}

	return eval.JudgeVerdict{
		Passed:  passed,
		Reason:  reason,
		Details: details,
	}, nil
}
