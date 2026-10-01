package judge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	gitCaptureTimeout = 60 * time.Second

	// gitStatArgs bounds the summary to 100 files at 160 columns.
	gitStatArgs = "--stat=160,100,100"
)

var (
	errNotGitRepo = errors.New("workspace is not the root of a git repository; diff-review needs a repository with a baseline commit")
	errNoCommits  = errors.New("workspace repository has no commits; diff-review needs a baseline commit to diff against")
)

// workspaceDiff is the change between HEAD and the workspace, including
// untracked files.
type workspaceDiff struct {
	// Stat is the `git diff --stat` summary.
	Stat string

	// Head holds the first maxBytes of the diff, cut on a rune boundary.
	Head string

	// Size and SHA256 describe the complete diff, not just Head.
	Size   int
	SHA256 string

	// Truncated reports whether Head omits part of the diff.
	Truncated bool
}

// captureWorkspaceDiff diffs HEAD against the working tree of the repository
// rooted at dir, including untracked files, via a temporary index that leaves
// the workspace's own untouched. Only the first maxBytes are retained; the
// full diff is still hashed and counted.
func captureWorkspaceDiff(ctx context.Context, dir string, maxBytes int) (workspaceDiff, error) {
	ctx, cancel := context.WithTimeout(ctx, gitCaptureTimeout)
	defer cancel()

	if err := requireRepoRoot(ctx, dir); err != nil {
		return workspaceDiff{}, err
	}
	if _, err := runGit(ctx, dir, nil, nil, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return workspaceDiff{}, errNoCommits
		}
		return workspaceDiff{}, err
	}

	indexDir, err := os.MkdirTemp("", "stirrupjudgeindex-")
	if err != nil {
		return workspaceDiff{}, fmt.Errorf("creating temporary git index: %w", err)
	}
	defer func() { _ = os.RemoveAll(indexDir) }()
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(indexDir, "index")}

	if _, err := runGit(ctx, dir, env, nil, "read-tree", "HEAD"); err != nil {
		return workspaceDiff{}, err
	}
	if _, err := runGit(ctx, dir, env, nil, "add", "--all"); err != nil {
		return workspaceDiff{}, err
	}

	diffArgs := []string{"diff", "--cached", "--no-color", "--no-ext-diff", "--no-textconv"}
	stat, err := runGit(ctx, dir, env, nil, append(diffArgs, gitStatArgs, "HEAD")...)
	if err != nil {
		return workspaceDiff{}, err
	}

	sink := newHeadWriter(maxBytes)
	if _, err := runGit(ctx, dir, env, sink, append(diffArgs, "HEAD")...); err != nil {
		return workspaceDiff{}, err
	}

	return workspaceDiff{
		Stat:      strings.TrimRight(string(stat), "\n"),
		Head:      string(trimPartialRune(sink.head)),
		Size:      sink.total,
		SHA256:    hex.EncodeToString(sink.hash.Sum(nil)),
		Truncated: sink.total > maxBytes,
	}, nil
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

// repoLocationEnv are the variables that redirect git away from the working
// directory's own repository.
var repoLocationEnv = []string{
	"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=", "GIT_OBJECT_DIRECTORY=",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES=", "GIT_COMMON_DIR=", "GIT_PREFIX=",
}

// runGit runs git in dir. With a nil stdout the output is returned; otherwise
// it is streamed to stdout. The workspace's repository is agent-controlled,
// so the fsmonitor hook is disabled and external diff drivers are never used.
func runGit(ctx context.Context, dir string, extraEnv []string, stdout *headWriter, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.fsmonitor=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = GitEnv(extraEnv...)

	var out, errOut bytes.Buffer
	if stdout != nil {
		cmd.Stdout = stdout
	} else {
		cmd.Stdout = &out
	}
	cmd.Stderr = &errOut

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("git %s: %w", args[0], ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errOut.String()))
		}
		return nil, fmt.Errorf("running git %s: %w", args[0], err)
	}
	return out.Bytes(), nil
}

// GitEnv returns the process environment prepared for running git against a
// workspace directory: variables that would redirect git to another
// repository are removed, prompts are disabled, and extra is appended.
func GitEnv(extra ...string) []string {
	env := make([]string, 0, len(os.Environ())+len(extra)+1)
outer:
	for _, kv := range os.Environ() {
		for _, prefix := range repoLocationEnv {
			if strings.HasPrefix(kv, prefix) {
				continue outer
			}
		}
		env = append(env, kv)
	}
	env = append(env, "GIT_TERMINAL_PROMPT=0")
	return append(env, extra...)
}

// headWriter keeps the first limit bytes written while hashing and counting
// everything, so a large diff costs bounded memory.
type headWriter struct {
	limit int
	head  []byte
	total int
	hash  hash.Hash
}

func newHeadWriter(limit int) *headWriter {
	return &headWriter{limit: limit, hash: sha256.New()}
}

func (w *headWriter) Write(p []byte) (int, error) {
	_, _ = w.hash.Write(p)
	w.total += len(p)
	if room := w.limit - len(w.head); room > 0 {
		w.head = append(w.head, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// trimPartialRune drops an incomplete trailing UTF-8 sequence left by a
// byte-count cut.
func trimPartialRune(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return b[:i]
			}
			return b
		}
	}
	return b
}
