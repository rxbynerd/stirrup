package judge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rxbynerd/stirrup/types"
)

const (
	// BaselineSidecarName is the file `run` retains beside a diff-review
	// task's trace; `replay --judge-baseline` reads it.
	BaselineSidecarName = "judge-baseline.json"

	baselineBundleName      = "judge-baseline.bundle"
	baselineSidecarVersion  = 1
	maxBaselineSidecarBytes = 4096
)

// Baseline is the commit a diff-review judge diffs the workspace against.
// GitDir is a judge-owned bare repository outside the workspace, so git
// never reads the workspace's own .git.
type Baseline struct {
	GitDir string
	Rev    string

	// Source is a types.JudgeBaseline* value recorded on the verdict.
	Source string
}

// recorded reports whether the baseline was taken by the runner before the
// agent ran, so an empty diff means the agent changed nothing.
func (b Baseline) recorded() bool {
	return b.Source == types.JudgeBaselineRunner || b.Source == types.JudgeBaselineSidecar
}

// baselineSidecar is the on-disk form of BaselineSidecarName.
type baselineSidecar struct {
	SchemaVersion int `json:"schemaVersion"`

	// BaselineRev is the baseline commit in the judge-owned repository.
	BaselineRev string `json:"baselineRev"`

	// Bundle names the git bundle, beside the sidecar, that holds the
	// baseline commit and its tree.
	Bundle string `json:"bundle"`
}

// CreateBaseline commits the workspace's current content, ignored files
// included, to a new bare repository at gitDir and returns it as a runner
// baseline. A .git inside the workspace is never read.
func CreateBaseline(ctx context.Context, workspaceDir, gitDir string) (Baseline, error) {
	ctx, cancel := context.WithTimeout(ctx, gitCaptureTimeout)
	defer cancel()

	gitDir, err := initJudgeGitDir(ctx, gitDir)
	if err != nil {
		return Baseline{}, err
	}
	env := worktreeEnv(gitDir, workspaceDir)
	if _, err := runGit(ctx, workspaceDir, env, nil, "add", "-A", "-f"); err != nil {
		return Baseline{}, err
	}
	if _, err := runGit(ctx, workspaceDir, env, nil,
		"-c", "user.name=stirrup-eval", "-c", "user.email=stirrup-eval@localhost.invalid", "-c", "commit.gpgsign=false",
		"commit", "-q", "--allow-empty", "--no-verify", "-m", "baseline"); err != nil {
		return Baseline{}, err
	}
	rev, err := runGit(ctx, workspaceDir, env, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return Baseline{}, err
	}
	return Baseline{GitDir: gitDir, Rev: strings.TrimSpace(string(rev)), Source: types.JudgeBaselineRunner}, nil
}

// WriteBaselineSidecar writes BaselineSidecarName and the bundle holding
// the baseline commit into dir, so a later replay can diff against the
// same baseline.
func WriteBaselineSidecar(ctx context.Context, b Baseline, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, gitCaptureTimeout)
	defer cancel()

	bundlePath, err := filepath.Abs(filepath.Join(dir, baselineBundleName))
	if err != nil {
		return fmt.Errorf("resolving baseline bundle path: %w", err)
	}
	if _, err := runGit(ctx, dir, []string{"GIT_DIR=" + b.GitDir}, nil, "bundle", "create", "--quiet", bundlePath, "HEAD"); err != nil {
		return fmt.Errorf("bundling baseline: %w", err)
	}
	data, err := json.MarshalIndent(baselineSidecar{
		SchemaVersion: baselineSidecarVersion,
		BaselineRev:   b.Rev,
		Bundle:        baselineBundleName,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling baseline sidecar: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, BaselineSidecarName), data, 0o600); err != nil {
		return fmt.Errorf("writing baseline sidecar: %w", err)
	}
	return nil
}

// LoadBaselineSidecar restores the baseline described by the sidecar at
// path into a new bare repository at gitDir.
func LoadBaselineSidecar(ctx context.Context, path, gitDir string) (Baseline, error) {
	ctx, cancel := context.WithTimeout(ctx, gitCaptureTimeout)
	defer cancel()

	sc, err := readBaselineSidecar(path)
	if err != nil {
		return Baseline{}, err
	}
	gitDir, err = initJudgeGitDir(ctx, gitDir)
	if err != nil {
		return Baseline{}, err
	}
	bundlePath, err := filepath.Abs(filepath.Join(filepath.Dir(path), sc.Bundle))
	if err != nil {
		return Baseline{}, fmt.Errorf("resolving baseline bundle path: %w", err)
	}
	env := []string{"GIT_DIR=" + gitDir}
	if _, err := runGit(ctx, gitDir, env, nil, "bundle", "unbundle", bundlePath); err != nil {
		return Baseline{}, fmt.Errorf("restoring baseline bundle: %w", err)
	}
	if _, err := runGit(ctx, gitDir, env, nil, "cat-file", "-e", sc.BaselineRev+"^{commit}"); err != nil {
		return Baseline{}, fmt.Errorf("baseline bundle does not contain commit %s: %w", sc.BaselineRev, err)
	}
	return Baseline{GitDir: gitDir, Rev: sc.BaselineRev, Source: types.JudgeBaselineSidecar}, nil
}

func readBaselineSidecar(path string) (baselineSidecar, error) {
	f, err := os.Open(path)
	if err != nil {
		return baselineSidecar{}, fmt.Errorf("opening baseline sidecar: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxBaselineSidecarBytes+1))
	if err != nil {
		return baselineSidecar{}, fmt.Errorf("reading baseline sidecar: %w", err)
	}
	if len(data) > maxBaselineSidecarBytes {
		return baselineSidecar{}, fmt.Errorf("baseline sidecar exceeds %d bytes", maxBaselineSidecarBytes)
	}
	var sc baselineSidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		return baselineSidecar{}, fmt.Errorf("parsing baseline sidecar: %w", err)
	}
	switch {
	case sc.SchemaVersion != baselineSidecarVersion:
		return baselineSidecar{}, fmt.Errorf("baseline sidecar schemaVersion %d is not %d", sc.SchemaVersion, baselineSidecarVersion)
	case !gitRevPattern.MatchString(sc.BaselineRev):
		return baselineSidecar{}, errors.New("baseline sidecar baselineRev is not a full object name")
	case sc.Bundle == "" || sc.Bundle != filepath.Base(sc.Bundle) || strings.HasPrefix(sc.Bundle, "."):
		return baselineSidecar{}, errors.New("baseline sidecar bundle must name a file beside the sidecar")
	}
	return sc, nil
}

// workspaceHeadBaseline uses the HEAD commit of the workspace's own
// repository as the baseline. The objects are reached through an
// alternates file from a new bare repository at gitDir, so capture writes
// nothing into the workspace. Resolving HEAD runs no hooks or filters.
func workspaceHeadBaseline(ctx context.Context, workspaceDir, gitDir string) (Baseline, error) {
	ctx, cancel := context.WithTimeout(ctx, gitCaptureTimeout)
	defer cancel()

	if err := requireRepoRoot(ctx, workspaceDir); err != nil {
		return Baseline{}, err
	}
	rev, err := runGit(ctx, workspaceDir, nil, nil, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return Baseline{}, errNoCommits
		}
		return Baseline{}, err
	}
	objects, err := runGit(ctx, workspaceDir, nil, nil, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return Baseline{}, err
	}

	gitDir, err = initJudgeGitDir(ctx, gitDir)
	if err != nil {
		return Baseline{}, err
	}
	infoDir := filepath.Join(gitDir, "objects", "info")
	if err := os.MkdirAll(infoDir, 0o700); err != nil {
		return Baseline{}, fmt.Errorf("creating judge object store: %w", err)
	}
	if err := os.WriteFile(filepath.Join(infoDir, "alternates"), []byte(strings.TrimSpace(string(objects))+"\n"), 0o600); err != nil {
		return Baseline{}, fmt.Errorf("linking workspace objects: %w", err)
	}
	return Baseline{GitDir: gitDir, Rev: strings.TrimSpace(string(rev)), Source: types.JudgeBaselineWorkspaceHead}, nil
}

// requireRepoRoot rejects directories that are not themselves a repository
// root: a workspace nested inside another repository would otherwise be
// judged on that repository's diff.
func requireRepoRoot(ctx context.Context, dir string) error {
	out, err := runGit(ctx, dir, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("%w: %v", errNotGitRepo, err)
		}
		return err
	}
	top, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if err != nil {
		return fmt.Errorf("resolving repository root: %w", err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("resolving workspace: %w", err)
	}
	if top != want {
		return errNotGitRepo
	}
	return nil
}

// initJudgeGitDir creates an empty bare repository at gitDir, without
// template hooks, and returns its absolute path.
func initJudgeGitDir(ctx context.Context, gitDir string) (string, error) {
	abs, err := filepath.Abs(gitDir)
	if err != nil {
		return "", fmt.Errorf("resolving judge git dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return "", fmt.Errorf("creating judge git dir: %w", err)
	}
	if _, err := runGit(ctx, filepath.Dir(abs), nil, nil, "init", "-q", "--bare", "--template=", abs); err != nil {
		return "", err
	}
	return abs, nil
}
