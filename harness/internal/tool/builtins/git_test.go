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
	"unicode/utf8"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

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

// TestGitDiff_PathLeadingDashRejected covers the option-injection guard on the
// `path` argument (the ref equivalent is covered by
// TestValidateGitRef_RejectsOptionInjection). A path like "-p" must be rejected
// before any git command is issued, so it can never be read as a git flag.
func TestGitDiff_PathLeadingDashRejected(t *testing.T) {
	var captured []string
	mock := captureExec(&captured)
	tl := GitDiffTool(mock)

	if _, err := tl.StructuredHandler(context.Background(), json.RawMessage(`{"path": "-p"}`)); err == nil {
		t.Fatalf("handler accepted leading-dash path")
	}
	for _, c := range captured {
		if strings.Contains(c, "'diff'") {
			t.Errorf("git diff was executed despite hostile path: %q", c)
		}
	}
}

// TestGitShow_ColonRefTraversalRejected covers the colon-ref bypass: a treeish
// ref of the form `<rev>:<path>` carries its path in the ref string itself,
// which would otherwise skip validateGitPath. The path portion must still be
// containment-checked, and git show must not run when it escapes.
func TestGitShow_ColonRefTraversalRejected(t *testing.T) {
	var captured []string
	mock := captureExec(&captured)
	// Reject any escaping path the way the local executor would.
	mock.resolvePathFunc = func(p string) (string, error) {
		if strings.Contains(p, "..") || strings.HasPrefix(p, "/") {
			return "", os.ErrPermission
		}
		return "/workspace/" + p, nil
	}
	tl := GitShowTool(mock)

	for _, ref := range []string{"HEAD:../../../etc/passwd", "HEAD:/etc/passwd"} {
		captured = nil
		in := json.RawMessage(`{"ref": ` + jsonString(ref) + `}`)
		if _, err := tl.StructuredHandler(context.Background(), in); err == nil {
			t.Errorf("colon-ref %q was accepted; want traversal rejection", ref)
		}
		for _, c := range captured {
			if strings.Contains(c, "'show'") {
				t.Errorf("git show executed despite traversal colon-ref %q: %q", ref, c)
			}
		}
	}
}

// TestValidateColonRefPath_Branches covers the helper's three arms directly:
// no colon (no-op), trailing colon with empty path (no-op), and an escaping
// path (rejected).
func TestValidateColonRefPath_Branches(t *testing.T) {
	mock := &mockExecutor{resolvePathFunc: func(p string) (string, error) {
		if strings.Contains(p, "..") {
			return "", os.ErrPermission
		}
		return "/workspace/" + p, nil
	}}
	if err := validateColonRefPath(mock, "HEAD"); err != nil {
		t.Errorf("no-colon ref rejected: %v", err)
	}
	if err := validateColonRefPath(mock, "HEAD:"); err != nil {
		t.Errorf("empty-path colon-ref rejected: %v", err)
	}
	if err := validateColonRefPath(mock, "HEAD:../escape"); err == nil {
		t.Errorf("escaping colon-ref accepted")
	}
}

// TestGitShow_ColonRefInWorkspaceAccepted confirms the colon-ref guard does not
// reject a legitimate in-workspace path (so the bypass fix is not over-broad).
func TestGitShow_ColonRefInWorkspaceAccepted(t *testing.T) {
	var captured []string
	mock := captureExec(&captured)
	tl := GitShowTool(mock)

	in := json.RawMessage(`{"ref": "HEAD:README.md"}`)
	if _, err := tl.StructuredHandler(context.Background(), in); err != nil {
		t.Fatalf("in-workspace colon-ref rejected: %v", err)
	}
	ran := false
	for _, c := range captured {
		if strings.Contains(c, "'show'") {
			ran = true
		}
	}
	if !ran {
		t.Errorf("git show did not run for a valid colon-ref; captured=%v", captured)
	}
}

// TestGitErrText covers the three arms of the error-message surface.
func TestGitErrText(t *testing.T) {
	cases := []struct {
		name string
		res  executor.ExecResult
		want string
	}{
		{"stderr", executor.ExecResult{ExitCode: 1, Stderr: "fatal: bad revision", Stdout: "ignored"}, "fatal: bad revision"},
		{"stdout", executor.ExecResult{ExitCode: 1, Stderr: "  \n", Stdout: "some stdout detail"}, "some stdout detail"},
		{"exitcode", executor.ExecResult{ExitCode: 3}, "exit code 3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gitErrText(&tc.res)
			if got != tc.want {
				t.Errorf("gitErrText = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGitShow_NonZeroExitSurfacesStderr drives a handler-level non-zero exit
// (e.g. a non-existent ref) and asserts the stderr text reaches the error.
func TestGitShow_NonZeroExitSurfacesStderr(t *testing.T) {
	mock := &mockExecutor{
		execFunc: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			if strings.Contains(command, "is-inside-work-tree") {
				return &executor.ExecResult{ExitCode: 0, Stdout: "true\n"}, nil
			}
			return &executor.ExecResult{ExitCode: 128, Stderr: "fatal: bad revision 'nope'"}, nil
		},
	}
	tl := GitShowTool(mock)
	_, err := tl.StructuredHandler(context.Background(), json.RawMessage(`{"ref": "nope"}`))
	if err == nil {
		t.Fatalf("expected error for non-existent ref")
	}
	if !strings.Contains(err.Error(), "bad revision 'nope'") {
		t.Errorf("error %q does not surface git stderr", err)
	}
}

func TestParsePorcelainZ_RenameAndCopy(t *testing.T) {
	// Rename (R) and copy (C) records each carry the original path in the
	// following NUL field: `XY <new>\0<orig>\0`. Build a fixture covering a
	// rename, a copy, and a plain modification.
	out := "R  new.go\x00old.go\x00C  copy.go\x00src.go\x00 M plain.go\x00"
	entries, truncated := parsePorcelainZ(out, 100)
	if truncated {
		t.Fatalf("unexpected truncation")
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(entries), entries)
	}
	if entries[0].Code != "R " || entries[0].Path != "new.go" || entries[0].OrigPath != "old.go" {
		t.Errorf("rename entry wrong: %+v", entries[0])
	}
	if entries[1].Code != "C " || entries[1].Path != "copy.go" || entries[1].OrigPath != "src.go" {
		t.Errorf("copy entry wrong: %+v", entries[1])
	}
	if entries[2].Code != " M" || entries[2].Path != "plain.go" || entries[2].OrigPath != "" {
		t.Errorf("plain entry wrong: %+v", entries[2])
	}
}

func TestParsePorcelainZ_Truncation(t *testing.T) {
	out := " M a\x00 M b\x00 M c\x00"
	entries, truncated := parsePorcelainZ(out, 2)
	if !truncated {
		t.Errorf("expected truncation at cap 2")
	}
	if len(entries) != 2 {
		t.Errorf("got %d entries, want 2", len(entries))
	}
}

func TestParseNameStatusZ_RenameAndCopy(t *testing.T) {
	// name-status -z for R/C is: status\0 <orig>\0 <new>\0; M is: status\0 <path>\0.
	out := "R100\x00old.go\x00new.go\x00M\x00plain.go\x00C75\x00src.go\x00copy.go\x00"
	files, truncated := parseNameStatusZ(out, 100)
	if truncated {
		t.Fatalf("unexpected truncation")
	}
	if len(files) != 3 {
		t.Fatalf("got %d files, want 3: %+v", len(files), files)
	}
	if files[0].Status != "R" || files[0].OrigPath != "old.go" || files[0].Path != "new.go" {
		t.Errorf("rename file wrong: %+v", files[0])
	}
	if files[1].Status != "M" || files[1].Path != "plain.go" || files[1].OrigPath != "" {
		t.Errorf("modify file wrong: %+v", files[1])
	}
	if files[2].Status != "C" || files[2].OrigPath != "src.go" || files[2].Path != "copy.go" {
		t.Errorf("copy file wrong: %+v", files[2])
	}
}

func TestParseNameStatusZ_Truncation(t *testing.T) {
	out := "M\x00a\x00M\x00b\x00M\x00c\x00"
	files, truncated := parseNameStatusZ(out, 2)
	if !truncated {
		t.Errorf("expected truncation at cap 2")
	}
	if len(files) != 2 {
		t.Errorf("got %d files, want 2", len(files))
	}
}

func TestFormatGitStatus_RenameRendering(t *testing.T) {
	r := gitStatusResult{
		Branch:  "main",
		Entries: []gitStatusEntry{{Code: "R ", Path: "new.go", OrigPath: "old.go"}},
	}
	got := formatGitStatus(r)
	if !strings.Contains(got, "R  old.go -> new.go") {
		t.Errorf("rename rendering missing arrow form; got:\n%s", got)
	}
}

func TestFormatGitStatus_CleanAndTruncated(t *testing.T) {
	clean := formatGitStatus(gitStatusResult{Branch: "main", Clean: true})
	if !strings.Contains(clean, "working tree clean") {
		t.Errorf("clean rendering wrong: %q", clean)
	}
	trunc := formatGitStatus(gitStatusResult{
		Branch:    "main",
		Entries:   []gitStatusEntry{{Code: " M", Path: "a"}},
		Truncated: true,
	})
	if !strings.Contains(trunc, "status truncated") {
		t.Errorf("truncated rendering missing sentinel: %q", trunc)
	}
}

func TestFormatGitChangedFiles_RenameAndEmptyAndTruncated(t *testing.T) {
	empty := formatGitChangedFiles(gitChangedFilesResult{})
	if empty != "no changed files" {
		t.Errorf("empty rendering wrong: %q", empty)
	}
	rename := formatGitChangedFiles(gitChangedFilesResult{
		Files: []gitChangedFile{{Status: "R", Path: "new.go", OrigPath: "old.go"}},
	})
	if !strings.Contains(rename, "old.go -> new.go") {
		t.Errorf("rename rendering missing arrow form; got: %q", rename)
	}
	trunc := formatGitChangedFiles(gitChangedFilesResult{
		Files:     []gitChangedFile{{Status: "M", Path: "a"}},
		Truncated: true,
	})
	if !strings.Contains(trunc, "truncated") {
		t.Errorf("truncated rendering missing sentinel: %q", trunc)
	}
}

func TestCurrentBranch_DetachedUnbornUnknown(t *testing.T) {
	detached := currentBranch(context.Background(), &mockExecutor{
		execFunc: func(_ context.Context, _ string, _ time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{ExitCode: 0, Stdout: "HEAD\n"}, nil
		},
	})
	if detached != "HEAD (detached)" {
		t.Errorf("detached = %q, want %q", detached, "HEAD (detached)")
	}
	unborn := currentBranch(context.Background(), &mockExecutor{
		execFunc: func(_ context.Context, _ string, _ time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{ExitCode: 128, Stderr: "no commit yet"}, nil
		},
	})
	if unborn != "(unborn)" {
		t.Errorf("unborn = %q, want %q", unborn, "(unborn)")
	}
	unknown := currentBranch(context.Background(), &mockExecutor{
		execFunc: func(_ context.Context, _ string, _ time.Duration) (*executor.ExecResult, error) {
			return nil, context.Canceled
		},
	})
	if unknown != "(unknown)" {
		t.Errorf("unknown = %q, want %q", unknown, "(unknown)")
	}
}

// TestBoundDiff_ByteCapTrimsToNewline asserts the byte cap trims back to the
// last newline so the result is rune-safe and line-complete (no mid-rune slice,
// no ragged partial final line). Uses a single line longer than the byte cap so
// only the byte cap fires.
func TestBoundDiff_ByteCapTrimsToNewline(t *testing.T) {
	// Two complete lines whose combined length exceeds the byte cap, with the
	// second line straddling the cap boundary. After the byte cap the output
	// must end in a newline and contain no partial trailing line.
	line := strings.Repeat("x", gitDiffMaxBytes-10) + "\n"
	tail := strings.Repeat("y", 100) + "\n"
	got, truncated := boundDiff(line + tail)
	if !truncated {
		t.Fatalf("expected truncation")
	}
	body := strings.TrimSuffix(got, gitDiffTruncSentinel)
	if !strings.HasSuffix(body, "\n") {
		t.Errorf("byte-capped body does not end at a line boundary: %q", body[max(0, len(body)-20):])
	}
	if strings.Contains(body, "y") {
		t.Errorf("byte-capped body included the partial straddling line")
	}
}

// TestBoundDiff_MidRuneByteCap asserts the byte cap never slices through a
// multi-byte rune (which would make json.Marshal emit a silent U+FFFD). A line
// of multi-byte runes longer than the cap, with no newline before the cap,
// trims to empty rather than to a broken rune.
func TestBoundDiff_MidRuneByteCap(t *testing.T) {
	// '€' is 3 bytes; a run with no newline before the cap forces the
	// no-newline branch, which must yield empty (not a half-rune).
	huge := strings.Repeat("€", gitDiffMaxBytes) // far exceeds the byte cap
	got, truncated := boundDiff(huge)
	if !truncated {
		t.Fatalf("expected truncation")
	}
	body := strings.TrimSuffix(got, gitDiffTruncSentinel)
	if !utf8.ValidString(body) {
		t.Errorf("byte cap produced invalid UTF-8")
	}
}

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

	diffRes, err := GitDiffTool(ex).StructuredHandler(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("git_diff: %v", err)
	}
	if !strings.Contains(diffRes.Text, "+modified") {
		t.Errorf("git_diff text missing the added line; got:\n%s", diffRes.Text)
	}

	diffPathRes, err := GitDiffTool(ex).StructuredHandler(ctx, json.RawMessage(`{"path": "tracked.txt"}`))
	if err != nil {
		t.Fatalf("git_diff path: %v", err)
	}
	if !strings.Contains(diffPathRes.Text, "tracked.txt") {
		t.Errorf("scoped git_diff missing path; got:\n%s", diffPathRes.Text)
	}

	showRes, err := GitShowTool(ex).StructuredHandler(ctx, json.RawMessage(`{"ref": "HEAD", "path": "tracked.txt"}`))
	if err != nil {
		t.Fatalf("git_show: %v", err)
	}
	if !strings.Contains(showRes.Text, "original") {
		t.Errorf("git_show missing committed content; got:\n%s", showRes.Text)
	}

	// Staging last, since it empties the unstaged view checked above.
	runGitCmd("add", "tracked.txt")
	stagedFiles, err := GitChangedFilesTool(ex).StructuredHandler(ctx, json.RawMessage(`{"staged": true}`))
	if err != nil {
		t.Fatalf("git_changed_files staged: %v", err)
	}
	var staged gitChangedFilesResult
	if err := json.Unmarshal(stagedFiles.Structured, &staged); err != nil {
		t.Fatalf("unmarshal staged: %v", err)
	}
	if !staged.Staged {
		t.Errorf("staged result not flagged staged")
	}
	foundStaged := false
	for _, f := range staged.Files {
		if f.Path == "tracked.txt" && f.Status == "M" {
			foundStaged = true
		}
	}
	if !foundStaged {
		t.Errorf("staged git_changed_files did not report tracked.txt; files=%+v", staged.Files)
	}
	stagedDiff, err := GitDiffTool(ex).StructuredHandler(ctx, json.RawMessage(`{"staged": true}`))
	if err != nil {
		t.Fatalf("git_diff staged: %v", err)
	}
	if !strings.Contains(stagedDiff.Text, "+modified") {
		t.Errorf("staged git_diff missing the added line; got:\n%s", stagedDiff.Text)
	}
}

// jsonString returns a JSON-encoded string literal for use inside hand-built
// JSON payloads in tests.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
