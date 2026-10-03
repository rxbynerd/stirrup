package judge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// gitCaptureTimeout bounds one baseline or capture sequence of git
	// commands.
	gitCaptureTimeout = 60 * time.Second

	// gitWaitDelay bounds how long a cancelled git, and any child still
	// holding its output pipes, may take to exit.
	gitWaitDelay = 2 * time.Second

	// maxGitStderrBytes bounds the git diagnostics kept for an error.
	maxGitStderrBytes = 4096

	// gitStatArgs bounds the summary to 100 files at 160 columns.
	gitStatArgs = "--stat=160,100,100"
)

var (
	errNotGitRepo = errors.New("workspace is not the root of a git repository; replaying a diff-review judge without a runner baseline needs a repository with a baseline commit")
	errNoCommits  = errors.New("workspace repository has no commits; replaying a diff-review judge without a runner baseline needs a baseline commit to diff against")
	errGitlink    = errors.New("workspace contains a nested git repository (gitlink), whose content git cannot diff; diff-review cannot review it")
)

// gitRevPattern matches a full SHA-1 or SHA-256 object name.
var gitRevPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// gitHardeningArgs neutralise configuration that could run commands or
// reshape output. The judge-owned repository carries no such
// configuration; these also cover git invoked against a replayed
// workspace's own repository.
var gitHardeningArgs = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "core.quotePath=true",
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
	"-c", "advice.addEmbeddedRepo=false",
}

// gitDiffFlags make the patch independent of attributes, drivers and
// rename detection, so every changed byte is shown as text.
var gitDiffFlags = []string{
	"--text", "--no-ext-diff", "--no-textconv", "--no-color", "--no-renames",
	"--src-prefix=a/", "--dst-prefix=b/",
}

// gitScrubbedEnv are inherited variables that relocate the repository or
// inject configuration, attributes, replacements, pagers, diff drivers or
// commit identity.
var gitScrubbedEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_PREFIX",
	"GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM",
	"GIT_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM",
	"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS",
	"GIT_ATTR_SOURCE", "GIT_ATTR_NOSYSTEM",
	"GIT_EXTERNAL_DIFF", "GIT_DIFF_OPTS", "GIT_PAGER",
	"GIT_NAMESPACE", "GIT_REPLACE_REF_BASE", "GIT_NO_REPLACE_OBJECTS",
	"GIT_TEMPLATE_DIR",
	"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE",
	"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_DATE",
}

var gitScrubbedEnvPrefixes = []string{"GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_"}

// gitIsolationEnv keeps host and system configuration out of every git
// call, so a capture is identical on every host.
var gitIsolationEnv = []string{
	"GIT_CONFIG_GLOBAL=" + os.DevNull,
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_ATTR_NOSYSTEM=1",
	"GIT_NO_REPLACE_OBJECTS=1",
	"GIT_TERMINAL_PROMPT=0",
}

// workspaceDiff is the change between a baseline commit and the
// workspace, including untracked and ignored files.
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

// captureWorkspaceDiff diffs the baseline commit against the workspace
// through the baseline's judge-owned repository and a temporary index, so
// git never reads the workspace's own .git. Only the first maxBytes are
// retained; the full diff is still hashed and counted.
func captureWorkspaceDiff(ctx context.Context, workspaceDir string, base Baseline, maxBytes int) (workspaceDiff, error) {
	if !gitRevPattern.MatchString(base.Rev) {
		return workspaceDiff{}, errors.New("baseline revision is not a full object name")
	}
	ctx, cancel := context.WithTimeout(ctx, gitCaptureTimeout)
	defer cancel()

	indexDir, err := os.MkdirTemp("", "stirrupjudgeindex-")
	if err != nil {
		return workspaceDiff{}, fmt.Errorf("creating temporary git index: %w", err)
	}
	defer func() { _ = os.RemoveAll(indexDir) }()
	env := append(worktreeEnv(base.GitDir, workspaceDir), "GIT_INDEX_FILE="+filepath.Join(indexDir, "index"))

	if _, err := runGit(ctx, workspaceDir, env, nil, "read-tree", base.Rev); err != nil {
		return workspaceDiff{}, err
	}
	if _, err := runGit(ctx, workspaceDir, env, nil, "add", "-A", "-f"); err != nil {
		return workspaceDiff{}, err
	}

	diffArgs := func(extra ...string) []string {
		return slices.Concat([]string{"diff", "--cached"}, gitDiffFlags, extra, []string{base.Rev, "--"})
	}

	links := &gitlinkScanner{}
	if _, err := runGit(ctx, workspaceDir, env, links, diffArgs("--raw")...); err != nil {
		return workspaceDiff{}, err
	}
	if links.found() {
		return workspaceDiff{}, errGitlink
	}

	stat, err := runGit(ctx, workspaceDir, env, nil, diffArgs(gitStatArgs)...)
	if err != nil {
		return workspaceDiff{}, err
	}

	sink := newHeadWriter(maxBytes)
	if _, err := runGit(ctx, workspaceDir, env, sink, diffArgs()...); err != nil {
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

// worktreeEnv points git at a judge-owned repository with the workspace
// as its work tree.
func worktreeEnv(gitDir, workspaceDir string) []string {
	return []string{"GIT_DIR=" + gitDir, "GIT_WORK_TREE=" + workspaceDir}
}

// runGit runs git in dir with the hardening arguments and an isolated
// environment. With a nil stdout the output is returned; otherwise it is
// streamed to stdout. Cancellation kills git's whole process group.
func runGit(ctx context.Context, dir string, extraEnv []string, stdout io.Writer, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", slices.Concat(gitHardeningArgs, args)...)
	cmd.Dir = dir
	cmd.Env = gitEnv(extraEnv...)
	cmd.WaitDelay = gitWaitDelay
	isolateProcessGroup(cmd)

	var out bytes.Buffer
	if stdout != nil {
		cmd.Stdout = stdout
	} else {
		cmd.Stdout = &out
	}
	errOut := &cappedBuffer{limit: maxGitStderrBytes}
	cmd.Stderr = errOut

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("git %s: %w", args[0], ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(errOut.buf)))
		}
		return nil, fmt.Errorf("running git %s: %w", args[0], err)
	}
	return out.Bytes(), nil
}

// gitEnv returns the process environment with gitScrubbedEnv removed,
// gitIsolationEnv applied and extra appended.
func gitEnv(extra ...string) []string {
	environ := os.Environ()
	env := make([]string, 0, len(environ)+len(gitIsolationEnv)+len(extra))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(gitScrubbedEnv, name) || slices.ContainsFunc(gitScrubbedEnvPrefixes, func(p string) bool {
			return strings.HasPrefix(name, p)
		}) {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, gitIsolationEnv...)
	return append(env, extra...)
}

// gitlinkScanner reads `git diff --raw` output and records whether either
// side of any entry is a gitlink (mode 160000).
type gitlinkScanner struct {
	line   []byte
	linked bool
}

// gitRawModePrefix covers ":<src mode> <dst mode> ", the part of a raw
// diff line that carries both modes.
const gitRawModePrefix = len(":000000 000000 ")

func (s *gitlinkScanner) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if room := gitRawModePrefix - len(s.line); room > 0 {
			s.line = append(s.line, chunk[:min(room, len(chunk))]...)
		}
		if i < 0 {
			break
		}
		s.check()
		p = p[i+1:]
	}
	return n, nil
}

func (s *gitlinkScanner) check() {
	if len(s.line) == gitRawModePrefix && (bytes.HasPrefix(s.line, []byte(":160000 ")) || bytes.Equal(s.line[8:], []byte("160000 "))) {
		s.linked = true
	}
	s.line = s.line[:0]
}

func (s *gitlinkScanner) found() bool {
	if len(s.line) > 0 {
		s.check()
	}
	return s.linked
}

// cappedBuffer keeps the first limit bytes written and discards the rest.
type cappedBuffer struct {
	limit int
	buf   []byte
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
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
