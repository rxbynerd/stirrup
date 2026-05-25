package builtins

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

// --- shellQuote / validation unit tests ---

func TestValidateGitRef_RejectsOptionInjection(t *testing.T) {
	for _, ref := range []string{"--upload-pack=/bin/sh", "-x", "--output=foo"} {
		if err := validateGitRef(ref); err == nil {
			t.Errorf("validateGitRef(%q) = nil, want rejection of leading '-'", ref)
		}
	}
}

func TestValidateGitRef_RejectsShellMetacharacters(t *testing.T) {
	for _, ref := range []string{"$(rm -rf /)", "foo; echo pwned", "a`id`b", "a|b", "a&b", "a b"} {
		if err := validateGitRef(ref); err == nil {
			t.Errorf("validateGitRef(%q) = nil, want rejection of shell metacharacter", ref)
		}
	}
}

func TestValidateGitRef_AcceptsNormalRefs(t *testing.T) {
	for _, ref := range []string{"HEAD", "HEAD~3", "main", "v1.2.3", "abc123def", "feature/x"} {
		if err := validateGitRef(ref); err != nil {
			t.Errorf("validateGitRef(%q) = %v, want nil", ref, err)
		}
	}
}

// --- shell-injection neutralisation via the mock executor ---

// captureExec returns a mock executor that records the command string Exec was
// invoked with so tests can assert what reached the shell.
func captureExec(captured *[]string) *mockExecutor {
	return &mockExecutor{
		execFunc: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			*captured = append(*captured, command)
			// rev-parse --is-inside-work-tree must report a repo so the
			// handler proceeds to the data-returning invocation under test.
			if strings.Contains(command, "rev-parse") && strings.Contains(command, "is-inside-work-tree") {
				return &executor.ExecResult{ExitCode: 0, Stdout: "true\n"}, nil
			}
			if strings.Contains(command, "rev-parse") && strings.Contains(command, "abbrev-ref") {
				return &executor.ExecResult{ExitCode: 0, Stdout: "main\n"}, nil
			}
			return &executor.ExecResult{ExitCode: 0, Stdout: ""}, nil
		},
		resolvePathFunc: func(p string) (string, error) {
			return "/workspace/" + p, nil
		},
	}
}

func TestGitDiff_PathWithShellMetacharactersIsQuoted(t *testing.T) {
	var captured []string
	mock := captureExec(&captured)
	tl := GitDiffTool(mock)

	// A path that, unquoted, would run `rm -rf /` as a second command.
	in := json.RawMessage(`{"path": "foo; rm -rf /"}`)
	if _, err := tl.StructuredHandler(context.Background(), in); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	// Find the `git diff` command (not the rev-parse guard) and assert the
	// hostile path was single-quoted so sh treats it as one literal argument.
	var diffCmd string
	for _, c := range captured {
		if strings.Contains(c, "'diff'") {
			diffCmd = c
		}
	}
	if diffCmd == "" {
		t.Fatalf("no `git diff` command was issued; captured=%v", captured)
	}
	if !strings.Contains(diffCmd, "'foo; rm -rf /'") {
		t.Errorf("path was not shell-quoted; command = %q", diffCmd)
	}
	// The `;` must be inside the quotes — there must be exactly one command.
	if strings.Count(diffCmd, ";") != 1 {
		t.Errorf("unexpected `;` outside quotes; command = %q", diffCmd)
	}
}

func TestGitShow_RefWithOptionInjectionIsRejected(t *testing.T) {
	var captured []string
	mock := captureExec(&captured)
	tl := GitShowTool(mock)

	in := json.RawMessage(`{"ref": "--upload-pack=/bin/sh"}`)
	_, err := tl.StructuredHandler(context.Background(), in)
	if err == nil {
		t.Fatalf("handler accepted option-injection ref")
	}
	// The rejection must happen before any `git show` reaches the shell.
	for _, c := range captured {
		if strings.Contains(c, "'show'") {
			t.Errorf("git show was executed despite hostile ref: %q", c)
		}
	}
}

func TestGitShow_RefWithCommandSubstitutionIsRejected(t *testing.T) {
	var captured []string
	mock := captureExec(&captured)
	tl := GitShowTool(mock)

	in := json.RawMessage(`{"ref": "$(touch /tmp/pwned)"}`)
	if _, err := tl.StructuredHandler(context.Background(), in); err == nil {
		t.Fatalf("handler accepted command-substitution ref")
	}
	for _, c := range captured {
		if strings.Contains(c, "'show'") {
			t.Errorf("git show executed despite hostile ref: %q", c)
		}
	}
}

// --- non-git workspace ---

func TestGitStatus_NonGitWorkspaceErrorsClearly(t *testing.T) {
	mock := &mockExecutor{
		execFunc: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			// Simulate git reporting the directory is not a work tree.
			return &executor.ExecResult{ExitCode: 128, Stderr: "fatal: not a git repository"}, nil
		},
	}
	tl := GitStatusTool(mock)
	_, err := tl.StructuredHandler(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatalf("expected error for non-git workspace")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error %q does not clearly state the workspace is not a git repository", err)
	}
}

// --- path traversal rejection ---

func TestGitDiff_PathTraversalRejected(t *testing.T) {
	mock := &mockExecutor{
		execFunc: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			if strings.Contains(command, "is-inside-work-tree") {
				return &executor.ExecResult{ExitCode: 0, Stdout: "true\n"}, nil
			}
			return &executor.ExecResult{ExitCode: 0}, nil
		},
		resolvePathFunc: func(p string) (string, error) {
			// Mirror the local executor: a path escaping the workspace fails.
			if strings.Contains(p, "..") || strings.HasPrefix(p, "/etc") {
				return "", os.ErrPermission
			}
			return "/workspace/" + p, nil
		},
	}
	tl := GitDiffTool(mock)
	for _, p := range []string{"../etc/passwd", "/etc/passwd"} {
		in := json.RawMessage(`{"path": ` + jsonString(p) + `}`)
		if _, err := tl.StructuredHandler(context.Background(), in); err == nil {
			t.Errorf("path %q was accepted; want traversal rejection", p)
		}
	}
}

// --- bounded output ---

func TestGitDiff_OutputTruncated(t *testing.T) {
	// Build a diff larger than both caps.
	var big strings.Builder
	for i := 0; i < gitDiffMaxLines+1000; i++ {
		big.WriteString("+a line of diff content that is reasonably long to push bytes up\n")
	}
	mock := &mockExecutor{
		execFunc: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			if strings.Contains(command, "is-inside-work-tree") {
				return &executor.ExecResult{ExitCode: 0, Stdout: "true\n"}, nil
			}
			return &executor.ExecResult{ExitCode: 0, Stdout: big.String()}, nil
		},
	}
	tl := GitDiffTool(mock)
	res, err := tl.StructuredHandler(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !strings.Contains(res.Text, gitDiffTruncSentinel) {
		t.Errorf("truncation sentinel missing from text output")
	}
	var payload gitDiffResult
	if err := json.Unmarshal(res.Structured, &payload); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	if !payload.Truncated {
		t.Errorf("structured payload not marked truncated")
	}
	lineCount := strings.Count(payload.Diff, "\n")
	if lineCount > gitDiffMaxLines+2 { // +sentinel line
		t.Errorf("diff has %d lines, exceeds line cap %d", lineCount, gitDiffMaxLines)
	}
}

// --- end-to-end against a real git repo via the local executor ---

func TestGitTools_RealRepoEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	// EvalSymlinks so the local executor's containment check (which resolves
	// symlinks) agrees with the workspace root on macOS where TempDir is under
	// a /var -> /private/var symlink.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	dir = resolved

	runGitCmd := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	runGitCmd("init", "-q")
	runGitCmd("config", "user.email", "test@example.com")
	runGitCmd("config", "user.name", "test")

	tracked := filepath.Join(dir, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("original\n"), 0o644); err != nil {
		t.Fatalf("write tracked: %v", err)
	}
	runGitCmd("add", "tracked.txt")
	runGitCmd("commit", "-q", "-m", "initial")

	// Modify the tracked file and add an untracked file.
	if err := os.WriteFile(tracked, []byte("original\nmodified\n"), 0o644); err != nil {
		t.Fatalf("modify tracked: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatalf("write untracked: %v", err)
	}

	ex, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("new local executor: %v", err)
	}
	ctx := context.Background()

	// git_status
	statusRes, err := GitStatusTool(ex).StructuredHandler(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("git_status: %v", err)
	}
	var status gitStatusResult
	if err := json.Unmarshal(statusRes.Structured, &status); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if status.Clean {
		t.Errorf("status reported clean, expected changes")
	}
	gotModified, gotUntracked := false, false
	for _, e := range status.Entries {
		if e.Path == "tracked.txt" && e.Unstaged == "M" {
			gotModified = true
		}
		if e.Path == "untracked.txt" && e.Code == "??" {
			gotUntracked = true
		}
	}
	if !gotModified {
		t.Errorf("git_status did not report modified tracked.txt; entries=%+v", status.Entries)
	}
	if !gotUntracked {
		t.Errorf("git_status did not report untracked.txt; entries=%+v", status.Entries)
	}

	// git_changed_files (unstaged)
	cfRes, err := GitChangedFilesTool(ex).StructuredHandler(ctx, json.RawMessage(`{"staged": false}`))
	if err != nil {
		t.Fatalf("git_changed_files: %v", err)
	}
	var changed gitChangedFilesResult
	if err := json.Unmarshal(cfRes.Structured, &changed); err != nil {
		t.Fatalf("unmarshal changed: %v", err)
	}
	foundTracked := false
	for _, f := range changed.Files {
		if f.Path == "tracked.txt" && f.Status == "M" {
			foundTracked = true
		}
	}
	if !foundTracked {
		t.Errorf("git_changed_files did not report modified tracked.txt; files=%+v", changed.Files)
	}

	// git_diff (whole worktree)
	diffRes, err := GitDiffTool(ex).StructuredHandler(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("git_diff: %v", err)
	}
	if !strings.Contains(diffRes.Text, "+modified") {
		t.Errorf("git_diff text missing the added line; got:\n%s", diffRes.Text)
	}

	// git_diff scoped to a single path.
	diffPathRes, err := GitDiffTool(ex).StructuredHandler(ctx, json.RawMessage(`{"path": "tracked.txt"}`))
	if err != nil {
		t.Fatalf("git_diff path: %v", err)
	}
	if !strings.Contains(diffPathRes.Text, "tracked.txt") {
		t.Errorf("scoped git_diff missing path; got:\n%s", diffPathRes.Text)
	}

	// git_show on HEAD restricted to the committed file.
	showRes, err := GitShowTool(ex).StructuredHandler(ctx, json.RawMessage(`{"ref": "HEAD", "path": "tracked.txt"}`))
	if err != nil {
		t.Fatalf("git_show: %v", err)
	}
	if !strings.Contains(showRes.Text, "original") {
		t.Errorf("git_show missing committed content; got:\n%s", showRes.Text)
	}
}

// jsonString returns a JSON-encoded string literal for use inside hand-built
// JSON payloads in tests.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
