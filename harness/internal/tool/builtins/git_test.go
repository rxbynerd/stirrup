package builtins

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

func TestValidateGitRef_RejectsOptionInjection(t *testing.T) {
	cases := []struct {
		name string
		ref  string
	}{
		{"long option with value", "--upload-pack=/bin/sh"},
		{"short option", "-x"},
		{"long option", "--output=foo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, validateGitRef(tc.ref), "leading '-' must be rejected")
		})
	}
}

func TestValidateGitRef_RejectsShellMetacharacters(t *testing.T) {
	cases := []struct {
		name string
		ref  string
	}{
		{"command substitution", "$(rm -rf /)"},
		{"semicolon", "foo; echo pwned"},
		{"backticks", "a`id`b"},
		{"pipe", "a|b"},
		{"ampersand", "a&b"},
		{"space", "a b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, validateGitRef(tc.ref), "shell metacharacter must be rejected")
		})
	}
}

func TestValidateGitRef_AcceptsNormalRefs(t *testing.T) {
	cases := []struct {
		name string
		ref  string
	}{
		{"bare HEAD", "HEAD"},
		{"relative HEAD", "HEAD~3"},
		{"branch name", "main"},
		{"semver tag", "v1.2.3"},
		{"abbreviated sha", "abc123def"},
		{"branch with slash", "feature/x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, validateGitRef(tc.ref))
		})
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
	_, err := tl.StructuredHandler(context.Background(), in)
	require.NoError(t, err)

	// Find the `git diff` command (not the rev-parse guard) and assert the
	// hostile path was single-quoted so sh treats it as one literal argument.
	var diffCmd string
	for _, c := range captured {
		if strings.Contains(c, "'diff'") {
			diffCmd = c
		}
	}
	require.NotEmpty(t, diffCmd, "no `git diff` command was issued; captured=%v", captured)
	require.Contains(t, diffCmd, "'foo; rm -rf /'", "path was not shell-quoted; command = %q", diffCmd)
	// The `;` must be inside the quotes — there must be exactly one command.
	require.Equal(t, 1, strings.Count(diffCmd, ";"), "unexpected `;` outside quotes; command = %q", diffCmd)
}

func TestGitShow_RefWithOptionInjectionIsRejected(t *testing.T) {
	var captured []string
	mock := captureExec(&captured)
	tl := GitShowTool(mock)

	in := json.RawMessage(`{"ref": "--upload-pack=/bin/sh"}`)
	_, err := tl.StructuredHandler(context.Background(), in)
	require.Error(t, err, "handler accepted option-injection ref")
	// The rejection must happen before any `git show` reaches the shell.
	for _, c := range captured {
		require.NotContains(t, c, "'show'", "git show was executed despite hostile ref: %q", c)
	}
}

func TestGitShow_RefWithCommandSubstitutionIsRejected(t *testing.T) {
	var captured []string
	mock := captureExec(&captured)
	tl := GitShowTool(mock)

	in := json.RawMessage(`{"ref": "$(touch /tmp/pwned)"}`)
	_, err := tl.StructuredHandler(context.Background(), in)
	require.Error(t, err, "handler accepted command-substitution ref")
	for _, c := range captured {
		require.NotContains(t, c, "'show'", "git show executed despite hostile ref: %q", c)
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
	require.ErrorContains(t, err, "not a git repository")
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
	cases := []struct {
		name string
		path string
	}{
		{"parent directory escape", "../etc/passwd"},
		{"absolute path", "/etc/passwd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := json.RawMessage(`{"path": ` + jsonString(tc.path) + `}`)
			_, err := tl.StructuredHandler(context.Background(), in)
			require.Error(t, err, "path %q was accepted; want traversal rejection", tc.path)
		})
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
	require.NoError(t, err)
	require.Contains(t, res.Text, gitDiffTruncSentinel, "truncation sentinel missing from text output")

	var payload gitDiffResult
	require.NoError(t, json.Unmarshal(res.Structured, &payload))
	require.True(t, payload.Truncated, "structured payload not marked truncated")

	lineCount := strings.Count(payload.Diff, "\n")
	// The +2 allows for the sentinel line.
	require.LessOrEqual(t, lineCount, gitDiffMaxLines+2,
		"diff has %d lines, exceeds line cap %d", lineCount, gitDiffMaxLines)
}

// TestGitDiff_PathLeadingDashRejected covers the option-injection guard on the
// `path` argument (the ref equivalent is covered by
// TestValidateGitRef_RejectsOptionInjection). A path like "-p" must be rejected
// before any git command is issued, so it can never be read as a git flag.
func TestGitDiff_PathLeadingDashRejected(t *testing.T) {
	var captured []string
	mock := captureExec(&captured)
	tl := GitDiffTool(mock)

	_, err := tl.StructuredHandler(context.Background(), json.RawMessage(`{"path": "-p"}`))
	require.Error(t, err, "handler accepted leading-dash path")
	for _, c := range captured {
		require.NotContains(t, c, "'diff'", "git diff was executed despite hostile path: %q", c)
	}
}

// TestGitShow_ColonRefTraversalRejected covers the colon-ref bypass: a treeish
// ref of the form `<rev>:<path>` carries its path in the ref string itself,
// which would otherwise skip validateGitPath. The path portion must still be
// containment-checked, and git show must not run when it escapes.
func TestGitShow_ColonRefTraversalRejected(t *testing.T) {
	cases := []struct {
		name string
		ref  string
	}{
		{"parent directory escape", "HEAD:../../../etc/passwd"},
		{"absolute path", "HEAD:/etc/passwd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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

			in := json.RawMessage(`{"ref": ` + jsonString(tc.ref) + `}`)
			_, err := tl.StructuredHandler(context.Background(), in)
			require.Error(t, err, "colon-ref %q was accepted; want traversal rejection", tc.ref)
			for _, c := range captured {
				require.NotContains(t, c, "'show'", "git show executed despite traversal colon-ref %q: %q", tc.ref, c)
			}
		})
	}
}

// TestValidateColonRefPath_ContainmentByRefForm checks the helper's three
// shapes: no colon, a colon with an empty path, and a colon with an escaping path.
func TestValidateColonRefPath_ContainmentByRefForm(t *testing.T) {
	mock := &mockExecutor{resolvePathFunc: func(p string) (string, error) {
		if strings.Contains(p, "..") {
			return "", os.ErrPermission
		}
		return "/workspace/" + p, nil
	}}
	t.Run("ref without colon is accepted", func(t *testing.T) {
		require.NoError(t, validateColonRefPath(mock, "HEAD"))
	})
	t.Run("empty path after colon is accepted", func(t *testing.T) {
		require.NoError(t, validateColonRefPath(mock, "HEAD:"))
	})
	t.Run("escaping path after colon is rejected", func(t *testing.T) {
		require.Error(t, validateColonRefPath(mock, "HEAD:../escape"))
	})
}

// TestGitShow_ColonRefInWorkspaceAccepted confirms the colon-ref guard does not
// reject a legitimate in-workspace path.
func TestGitShow_ColonRefInWorkspaceAccepted(t *testing.T) {
	var captured []string
	mock := captureExec(&captured)
	tl := GitShowTool(mock)

	in := json.RawMessage(`{"ref": "HEAD:README.md"}`)
	_, err := tl.StructuredHandler(context.Background(), in)
	require.NoError(t, err, "in-workspace colon-ref rejected")

	ran := slices.ContainsFunc(captured, func(c string) bool {
		return strings.Contains(c, "'show'")
	})
	require.True(t, ran, "git show did not run for a valid colon-ref; captured=%v", captured)
}

func TestGitErrText_PrefersStderrThenStdoutThenExitCode(t *testing.T) {
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
			require.Equal(t, tc.want, gitErrText(&tc.res))
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
	require.Error(t, err, "expected error for non-existent ref")
	require.ErrorContains(t, err, "bad revision 'nope'")
}

func TestParsePorcelainZ_RenameAndCopy(t *testing.T) {
	// Rename (R) and copy (C) records each carry the original path in the
	// following NUL field: `XY <new>\0<orig>\0`. Build a fixture covering a
	// rename, a copy, and a plain modification.
	out := "R  new.go\x00old.go\x00C  copy.go\x00src.go\x00 M plain.go\x00"
	entries, truncated := parsePorcelainZ(out, 100)
	require.False(t, truncated, "unexpected truncation")
	require.Len(t, entries, 3, "entries: %+v", entries)

	t.Run("rename", func(t *testing.T) {
		require.Equal(t, "R ", entries[0].Code)
		require.Equal(t, "new.go", entries[0].Path)
		require.Equal(t, "old.go", entries[0].OrigPath)
	})
	t.Run("copy", func(t *testing.T) {
		require.Equal(t, "C ", entries[1].Code)
		require.Equal(t, "copy.go", entries[1].Path)
		require.Equal(t, "src.go", entries[1].OrigPath)
	})
	t.Run("plain modification", func(t *testing.T) {
		require.Equal(t, " M", entries[2].Code)
		require.Equal(t, "plain.go", entries[2].Path)
		require.Empty(t, entries[2].OrigPath)
	})
}

func TestParsePorcelainZ_Truncation(t *testing.T) {
	out := " M a\x00 M b\x00 M c\x00"
	entries, truncated := parsePorcelainZ(out, 2)
	require.True(t, truncated, "expected truncation at cap 2")
	require.Len(t, entries, 2)
}

func TestParseNameStatusZ_RenameAndCopy(t *testing.T) {
	// name-status -z for R/C is: status\0 <orig>\0 <new>\0; M is: status\0 <path>\0.
	out := "R100\x00old.go\x00new.go\x00M\x00plain.go\x00C75\x00src.go\x00copy.go\x00"
	files, truncated := parseNameStatusZ(out, 100)
	require.False(t, truncated, "unexpected truncation")
	require.Len(t, files, 3, "files: %+v", files)

	t.Run("rename", func(t *testing.T) {
		require.Equal(t, "R", files[0].Status)
		require.Equal(t, "old.go", files[0].OrigPath)
		require.Equal(t, "new.go", files[0].Path)
	})
	t.Run("modify", func(t *testing.T) {
		require.Equal(t, "M", files[1].Status)
		require.Equal(t, "plain.go", files[1].Path)
		require.Empty(t, files[1].OrigPath)
	})
	t.Run("copy", func(t *testing.T) {
		require.Equal(t, "C", files[2].Status)
		require.Equal(t, "src.go", files[2].OrigPath)
		require.Equal(t, "copy.go", files[2].Path)
	})
}

func TestParseNameStatusZ_Truncation(t *testing.T) {
	out := "M\x00a\x00M\x00b\x00M\x00c\x00"
	files, truncated := parseNameStatusZ(out, 2)
	require.True(t, truncated, "expected truncation at cap 2")
	require.Len(t, files, 2)
}

func TestFormatGitStatus_RenameRendering(t *testing.T) {
	r := gitStatusResult{
		Branch:  "main",
		Entries: []gitStatusEntry{{Code: "R ", Path: "new.go", OrigPath: "old.go"}},
	}
	require.Contains(t, formatGitStatus(r), "R  old.go -> new.go", "rename must render in arrow form")
}

func TestFormatGitStatus_CleanAndTruncated(t *testing.T) {
	clean := formatGitStatus(gitStatusResult{Branch: "main", Clean: true})
	require.Contains(t, clean, "working tree clean")

	trunc := formatGitStatus(gitStatusResult{
		Branch:    "main",
		Entries:   []gitStatusEntry{{Code: " M", Path: "a"}},
		Truncated: true,
	})
	require.Contains(t, trunc, "status truncated", "truncated status must carry the sentinel")
}

func TestFormatGitChangedFiles_EmptyRenameAndTruncated(t *testing.T) {
	t.Run("empty list", func(t *testing.T) {
		require.Equal(t, "no changed files", formatGitChangedFiles(gitChangedFilesResult{}))
	})
	t.Run("rename in arrow form", func(t *testing.T) {
		out := formatGitChangedFiles(gitChangedFilesResult{
			Files: []gitChangedFile{{Status: "R", Path: "new.go", OrigPath: "old.go"}},
		})
		require.Contains(t, out, "old.go -> new.go")
	})
	t.Run("truncated list carries sentinel", func(t *testing.T) {
		out := formatGitChangedFiles(gitChangedFilesResult{
			Files:     []gitChangedFile{{Status: "M", Path: "a"}},
			Truncated: true,
		})
		require.Contains(t, out, "truncated")
	})
}

func TestCurrentBranch_ReportsDetachedUnbornAndUnknown(t *testing.T) {
	t.Run("detached HEAD", func(t *testing.T) {
		branch := currentBranch(context.Background(), &mockExecutor{
			execFunc: func(_ context.Context, _ string, _ time.Duration) (*executor.ExecResult, error) {
				return &executor.ExecResult{ExitCode: 0, Stdout: "HEAD\n"}, nil
			},
		})
		require.Equal(t, "HEAD (detached)", branch)
	})
	t.Run("unborn branch", func(t *testing.T) {
		branch := currentBranch(context.Background(), &mockExecutor{
			execFunc: func(_ context.Context, _ string, _ time.Duration) (*executor.ExecResult, error) {
				return &executor.ExecResult{ExitCode: 128, Stderr: "no commit yet"}, nil
			},
		})
		require.Equal(t, "(unborn)", branch)
	})
	t.Run("command failure", func(t *testing.T) {
		branch := currentBranch(context.Background(), &mockExecutor{
			execFunc: func(_ context.Context, _ string, _ time.Duration) (*executor.ExecResult, error) {
				return nil, context.Canceled
			},
		})
		require.Equal(t, "(unknown)", branch)
	})
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
	require.True(t, truncated, "expected truncation")

	body := strings.TrimSuffix(got, gitDiffTruncSentinel)
	require.True(t, strings.HasSuffix(body, "\n"),
		"byte-capped body does not end at a line boundary: %q", body[max(0, len(body)-20):])
	require.NotContains(t, body, "y", "byte-capped body included the partial straddling line")
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
	require.True(t, truncated, "expected truncation")

	body := strings.TrimSuffix(got, gitDiffTruncSentinel)
	require.True(t, utf8.ValidString(body), "byte cap produced invalid UTF-8")
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
	require.NoError(t, err)
	dir = resolved

	runGitCmd := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := c.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}

	runGitCmd("init", "-q")
	runGitCmd("config", "user.email", "test@example.com")
	runGitCmd("config", "user.name", "test")

	tracked := filepath.Join(dir, "tracked.txt")
	require.NoError(t, os.WriteFile(tracked, []byte("original\n"), 0o644))
	runGitCmd("add", "tracked.txt")
	runGitCmd("commit", "-q", "-m", "initial")

	require.NoError(t, os.WriteFile(tracked, []byte("original\nmodified\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new\n"), 0o644))

	ex, err := executor.NewLocalExecutor(dir)
	require.NoError(t, err)
	ctx := context.Background()

	statusRes, err := GitStatusTool(ex).StructuredHandler(ctx, json.RawMessage(`{}`))
	require.NoError(t, err)
	var status gitStatusResult
	require.NoError(t, json.Unmarshal(statusRes.Structured, &status))
	require.False(t, status.Clean, "status reported clean, expected changes")
	require.True(t, slices.ContainsFunc(status.Entries, func(e gitStatusEntry) bool {
		return e.Path == "tracked.txt" && e.Unstaged == "M"
	}), "git_status did not report modified tracked.txt; entries=%+v", status.Entries)
	require.True(t, slices.ContainsFunc(status.Entries, func(e gitStatusEntry) bool {
		return e.Path == "untracked.txt" && e.Code == "??"
	}), "git_status did not report untracked.txt; entries=%+v", status.Entries)

	cfRes, err := GitChangedFilesTool(ex).StructuredHandler(ctx, json.RawMessage(`{"staged": false}`))
	require.NoError(t, err)
	var changed gitChangedFilesResult
	require.NoError(t, json.Unmarshal(cfRes.Structured, &changed))
	require.True(t, slices.ContainsFunc(changed.Files, func(f gitChangedFile) bool {
		return f.Path == "tracked.txt" && f.Status == "M"
	}), "git_changed_files did not report modified tracked.txt; files=%+v", changed.Files)

	diffRes, err := GitDiffTool(ex).StructuredHandler(ctx, json.RawMessage(`{}`))
	require.NoError(t, err)
	require.Contains(t, diffRes.Text, "+modified", "git_diff text missing the added line")

	diffPathRes, err := GitDiffTool(ex).StructuredHandler(ctx, json.RawMessage(`{"path": "tracked.txt"}`))
	require.NoError(t, err)
	require.Contains(t, diffPathRes.Text, "tracked.txt", "scoped git_diff missing path")

	showRes, err := GitShowTool(ex).StructuredHandler(ctx, json.RawMessage(`{"ref": "HEAD", "path": "tracked.txt"}`))
	require.NoError(t, err)
	require.Contains(t, showRes.Text, "original", "git_show missing committed content")

	// Staging last, since it empties the unstaged view checked above.
	runGitCmd("add", "tracked.txt")
	stagedFiles, err := GitChangedFilesTool(ex).StructuredHandler(ctx, json.RawMessage(`{"staged": true}`))
	require.NoError(t, err)
	var staged gitChangedFilesResult
	require.NoError(t, json.Unmarshal(stagedFiles.Structured, &staged))
	require.True(t, staged.Staged, "staged result not flagged staged")
	require.True(t, slices.ContainsFunc(staged.Files, func(f gitChangedFile) bool {
		return f.Path == "tracked.txt" && f.Status == "M"
	}), "staged git_changed_files did not report tracked.txt; files=%+v", staged.Files)

	stagedDiff, err := GitDiffTool(ex).StructuredHandler(ctx, json.RawMessage(`{"staged": true}`))
	require.NoError(t, err)
	require.Contains(t, stagedDiff.Text, "+modified", "staged git_diff missing the added line")
}

// jsonString returns a JSON-encoded string literal for use inside hand-built
// JSON payloads in tests.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
