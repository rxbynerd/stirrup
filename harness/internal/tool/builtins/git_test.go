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
			must := require.New(t)
			must.Error(validateGitRef(tc.ref), "leading '-' must be rejected")
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
			must := require.New(t)
			must.Error(validateGitRef(tc.ref), "shell metacharacter must be rejected")
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
			must := require.New(t)
			must.NoError(validateGitRef(tc.ref))
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
	must := require.New(t)
	var captured []string
	mock := captureExec(&captured)
	tl := GitDiffTool(mock)

	// A path that, unquoted, would run `rm -rf /` as a second command.
	in := json.RawMessage(`{"path": "foo; rm -rf /"}`)
	_, err := tl.StructuredHandler(context.Background(), in)
	must.NoError(err)

	// Find the `git diff` command (not the rev-parse guard) and assert the
	// hostile path was single-quoted so sh treats it as one literal argument.
	var diffCmd string
	for _, c := range captured {
		if strings.Contains(c, "'diff'") {
			diffCmd = c
		}
	}
	must.NotEmpty(diffCmd, "no `git diff` command was issued; captured=%v", captured)
	must.Contains(diffCmd, "'foo; rm -rf /'", "path was not shell-quoted; command = %q", diffCmd)
	// The `;` must be inside the quotes — there must be exactly one command.
	must.Equal(1, strings.Count(diffCmd, ";"), "unexpected `;` outside quotes; command = %q", diffCmd)
}

func TestGitShow_RefWithOptionInjectionIsRejected(t *testing.T) {
	must := require.New(t)
	var captured []string
	mock := captureExec(&captured)
	tl := GitShowTool(mock)

	in := json.RawMessage(`{"ref": "--upload-pack=/bin/sh"}`)
	_, err := tl.StructuredHandler(context.Background(), in)
	must.Error(err, "handler accepted option-injection ref")
	// The rejection must happen before any `git show` reaches the shell.
	for _, c := range captured {
		must.NotContains(c, "'show'", "git show was executed despite hostile ref: %q", c)
	}
}

func TestGitShow_RefWithCommandSubstitutionIsRejected(t *testing.T) {
	must := require.New(t)
	var captured []string
	mock := captureExec(&captured)
	tl := GitShowTool(mock)

	in := json.RawMessage(`{"ref": "$(touch /tmp/pwned)"}`)
	_, err := tl.StructuredHandler(context.Background(), in)
	must.Error(err, "handler accepted command-substitution ref")
	for _, c := range captured {
		must.NotContains(c, "'show'", "git show executed despite hostile ref: %q", c)
	}
}

func TestGitStatus_NonGitWorkspaceErrorsClearly(t *testing.T) {
	must := require.New(t)
	mock := &mockExecutor{
		execFunc: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			// Simulate git reporting the directory is not a work tree.
			return &executor.ExecResult{ExitCode: 128, Stderr: "fatal: not a git repository"}, nil
		},
	}
	tl := GitStatusTool(mock)
	_, err := tl.StructuredHandler(context.Background(), json.RawMessage(`{}`))
	must.ErrorContains(err, "not a git repository")
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
			must := require.New(t)
			in := json.RawMessage(`{"path": ` + jsonString(tc.path) + `}`)
			_, err := tl.StructuredHandler(context.Background(), in)
			must.Error(err, "path %q was accepted; want traversal rejection", tc.path)
		})
	}
}

func TestGitDiff_OutputTruncated(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	must.Contains(res.Text, gitDiffTruncSentinel, "truncation sentinel missing from text output")

	var payload gitDiffResult
	must.NoError(json.Unmarshal(res.Structured, &payload))
	must.True(payload.Truncated, "structured payload not marked truncated")

	lineCount := strings.Count(payload.Diff, "\n")
	// The +2 allows for the sentinel line.
	must.LessOrEqual(lineCount, gitDiffMaxLines+2,
		"diff has %d lines, exceeds line cap %d", lineCount, gitDiffMaxLines)
}

// TestGitDiff_PathLeadingDashRejected covers the option-injection guard on the
// `path` argument (the ref equivalent is covered by
// TestValidateGitRef_RejectsOptionInjection). A path like "-p" must be rejected
// before any git command is issued, so it can never be read as a git flag.
func TestGitDiff_PathLeadingDashRejected(t *testing.T) {
	must := require.New(t)
	var captured []string
	mock := captureExec(&captured)
	tl := GitDiffTool(mock)

	_, err := tl.StructuredHandler(context.Background(), json.RawMessage(`{"path": "-p"}`))
	must.Error(err, "handler accepted leading-dash path")
	for _, c := range captured {
		must.NotContains(c, "'diff'", "git diff was executed despite hostile path: %q", c)
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
			must := require.New(t)
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
			must.Error(err, "colon-ref %q was accepted; want traversal rejection", tc.ref)
			for _, c := range captured {
				must.NotContains(c, "'show'", "git show executed despite traversal colon-ref %q: %q", tc.ref, c)
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
		must := require.New(t)
		must.NoError(validateColonRefPath(mock, "HEAD"))
	})
	t.Run("empty path after colon is accepted", func(t *testing.T) {
		must := require.New(t)
		must.NoError(validateColonRefPath(mock, "HEAD:"))
	})
	t.Run("escaping path after colon is rejected", func(t *testing.T) {
		must := require.New(t)
		must.Error(validateColonRefPath(mock, "HEAD:../escape"))
	})
}

// TestGitShow_ColonRefInWorkspaceAccepted confirms the colon-ref guard does not
// reject a legitimate in-workspace path.
func TestGitShow_ColonRefInWorkspaceAccepted(t *testing.T) {
	must := require.New(t)
	var captured []string
	mock := captureExec(&captured)
	tl := GitShowTool(mock)

	in := json.RawMessage(`{"ref": "HEAD:README.md"}`)
	_, err := tl.StructuredHandler(context.Background(), in)
	must.NoError(err, "in-workspace colon-ref rejected")

	ran := slices.ContainsFunc(captured, func(c string) bool {
		return strings.Contains(c, "'show'")
	})
	must.True(ran, "git show did not run for a valid colon-ref; captured=%v", captured)
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
			must := require.New(t)
			must.Equal(tc.want, gitErrText(&tc.res))
		})
	}
}

// TestGitShow_NonZeroExitSurfacesStderr drives a handler-level non-zero exit
// (e.g. a non-existent ref) and asserts the stderr text reaches the error.
func TestGitShow_NonZeroExitSurfacesStderr(t *testing.T) {
	must := require.New(t)
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
	must.Error(err, "expected error for non-existent ref")
	must.ErrorContains(err, "bad revision 'nope'")
}

func TestParsePorcelainZ_RenameAndCopy(t *testing.T) {
	must := require.New(t)
	// Rename (R) and copy (C) records each carry the original path in the
	// following NUL field: `XY <new>\0<orig>\0`. Build a fixture covering a
	// rename, a copy, and a plain modification.
	out := "R  new.go\x00old.go\x00C  copy.go\x00src.go\x00 M plain.go\x00"
	entries, truncated := parsePorcelainZ(out, 100)
	must.False(truncated, "unexpected truncation")
	must.Len(entries, 3, "entries: %+v", entries)

	t.Run("rename", func(t *testing.T) {
		must := require.New(t)
		must.Equal("R ", entries[0].Code)
		must.Equal("new.go", entries[0].Path)
		must.Equal("old.go", entries[0].OrigPath)
	})
	t.Run("copy", func(t *testing.T) {
		must := require.New(t)
		must.Equal("C ", entries[1].Code)
		must.Equal("copy.go", entries[1].Path)
		must.Equal("src.go", entries[1].OrigPath)
	})
	t.Run("plain modification", func(t *testing.T) {
		must := require.New(t)
		must.Equal(" M", entries[2].Code)
		must.Equal("plain.go", entries[2].Path)
		must.Empty(entries[2].OrigPath)
	})
}

func TestParsePorcelainZ_Truncation(t *testing.T) {
	must := require.New(t)
	out := " M a\x00 M b\x00 M c\x00"
	entries, truncated := parsePorcelainZ(out, 2)
	must.True(truncated, "expected truncation at cap 2")
	must.Len(entries, 2)
}

func TestParseNameStatusZ_RenameAndCopy(t *testing.T) {
	must := require.New(t)
	// name-status -z for R/C is: status\0 <orig>\0 <new>\0; M is: status\0 <path>\0.
	out := "R100\x00old.go\x00new.go\x00M\x00plain.go\x00C75\x00src.go\x00copy.go\x00"
	files, truncated := parseNameStatusZ(out, 100)
	must.False(truncated, "unexpected truncation")
	must.Len(files, 3, "files: %+v", files)

	t.Run("rename", func(t *testing.T) {
		must := require.New(t)
		must.Equal("R", files[0].Status)
		must.Equal("old.go", files[0].OrigPath)
		must.Equal("new.go", files[0].Path)
	})
	t.Run("modify", func(t *testing.T) {
		must := require.New(t)
		must.Equal("M", files[1].Status)
		must.Equal("plain.go", files[1].Path)
		must.Empty(files[1].OrigPath)
	})
	t.Run("copy", func(t *testing.T) {
		must := require.New(t)
		must.Equal("C", files[2].Status)
		must.Equal("src.go", files[2].OrigPath)
		must.Equal("copy.go", files[2].Path)
	})
}

func TestParseNameStatusZ_Truncation(t *testing.T) {
	must := require.New(t)
	out := "M\x00a\x00M\x00b\x00M\x00c\x00"
	files, truncated := parseNameStatusZ(out, 2)
	must.True(truncated, "expected truncation at cap 2")
	must.Len(files, 2)
}

func TestFormatGitStatus_RenameRendering(t *testing.T) {
	must := require.New(t)
	r := gitStatusResult{
		Branch:  "main",
		Entries: []gitStatusEntry{{Code: "R ", Path: "new.go", OrigPath: "old.go"}},
	}
	must.Contains(formatGitStatus(r), "R  old.go -> new.go", "rename must render in arrow form")
}

func TestFormatGitStatus_CleanAndTruncated(t *testing.T) {
	must := require.New(t)
	clean := formatGitStatus(gitStatusResult{Branch: "main", Clean: true})
	must.Contains(clean, "working tree clean")

	trunc := formatGitStatus(gitStatusResult{
		Branch:    "main",
		Entries:   []gitStatusEntry{{Code: " M", Path: "a"}},
		Truncated: true,
	})
	must.Contains(trunc, "status truncated", "truncated status must carry the sentinel")
}

func TestFormatGitChangedFiles_EmptyRenameAndTruncated(t *testing.T) {
	t.Run("empty list", func(t *testing.T) {
		must := require.New(t)
		must.Equal("no changed files", formatGitChangedFiles(gitChangedFilesResult{}))
	})
	t.Run("rename in arrow form", func(t *testing.T) {
		must := require.New(t)
		out := formatGitChangedFiles(gitChangedFilesResult{
			Files: []gitChangedFile{{Status: "R", Path: "new.go", OrigPath: "old.go"}},
		})
		must.Contains(out, "old.go -> new.go")
	})
	t.Run("truncated list carries sentinel", func(t *testing.T) {
		must := require.New(t)
		out := formatGitChangedFiles(gitChangedFilesResult{
			Files:     []gitChangedFile{{Status: "M", Path: "a"}},
			Truncated: true,
		})
		must.Contains(out, "truncated")
	})
}

func TestCurrentBranch_ReportsDetachedUnbornAndUnknown(t *testing.T) {
	t.Run("detached HEAD", func(t *testing.T) {
		must := require.New(t)
		branch := currentBranch(context.Background(), &mockExecutor{
			execFunc: func(_ context.Context, _ string, _ time.Duration) (*executor.ExecResult, error) {
				return &executor.ExecResult{ExitCode: 0, Stdout: "HEAD\n"}, nil
			},
		})
		must.Equal("HEAD (detached)", branch)
	})
	t.Run("unborn branch", func(t *testing.T) {
		must := require.New(t)
		branch := currentBranch(context.Background(), &mockExecutor{
			execFunc: func(_ context.Context, _ string, _ time.Duration) (*executor.ExecResult, error) {
				return &executor.ExecResult{ExitCode: 128, Stderr: "no commit yet"}, nil
			},
		})
		must.Equal("(unborn)", branch)
	})
	t.Run("command failure", func(t *testing.T) {
		must := require.New(t)
		branch := currentBranch(context.Background(), &mockExecutor{
			execFunc: func(_ context.Context, _ string, _ time.Duration) (*executor.ExecResult, error) {
				return nil, context.Canceled
			},
		})
		must.Equal("(unknown)", branch)
	})
}

// TestBoundDiff_ByteCapTrimsToNewline asserts the byte cap trims back to the
// last newline so the result is rune-safe and line-complete (no mid-rune slice,
// no ragged partial final line). Uses a single line longer than the byte cap so
// only the byte cap fires.
func TestBoundDiff_ByteCapTrimsToNewline(t *testing.T) {
	must := require.New(t)
	// Two complete lines whose combined length exceeds the byte cap, with the
	// second line straddling the cap boundary. After the byte cap the output
	// must end in a newline and contain no partial trailing line.
	line := strings.Repeat("x", gitDiffMaxBytes-10) + "\n"
	tail := strings.Repeat("y", 100) + "\n"
	got, truncated := boundDiff(line + tail)
	must.True(truncated, "expected truncation")

	body := strings.TrimSuffix(got, gitDiffTruncSentinel)
	must.True(strings.HasSuffix(body, "\n"),
		"byte-capped body does not end at a line boundary: %q", body[max(0, len(body)-20):])
	must.NotContains(body, "y", "byte-capped body included the partial straddling line")
}

// TestBoundDiff_MidRuneByteCap asserts the byte cap never slices through a
// multi-byte rune (which would make json.Marshal emit a silent U+FFFD). A line
// of multi-byte runes longer than the cap, with no newline before the cap,
// trims to empty rather than to a broken rune.
func TestBoundDiff_MidRuneByteCap(t *testing.T) {
	must := require.New(t)
	// '€' is 3 bytes; a run with no newline before the cap forces the
	// no-newline branch, which must yield empty (not a half-rune).
	huge := strings.Repeat("€", gitDiffMaxBytes) // far exceeds the byte cap
	got, truncated := boundDiff(huge)
	must.True(truncated, "expected truncation")

	body := strings.TrimSuffix(got, gitDiffTruncSentinel)
	must.True(utf8.ValidString(body), "byte cap produced invalid UTF-8")
}

func TestGitTools_RealRepoEndToEnd(t *testing.T) {
	must := require.New(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	// EvalSymlinks so the local executor's containment check (which resolves
	// symlinks) agrees with the workspace root on macOS where TempDir is under
	// a /var -> /private/var symlink.
	resolved, err := filepath.EvalSymlinks(dir)
	must.NoError(err)
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
		must.NoError(err, "git %v: %s", args, out)
	}

	runGitCmd("init", "-q")
	runGitCmd("config", "user.email", "test@example.com")
	runGitCmd("config", "user.name", "test")

	tracked := filepath.Join(dir, "tracked.txt")
	must.NoError(os.WriteFile(tracked, []byte("original\n"), 0o644))
	runGitCmd("add", "tracked.txt")
	runGitCmd("commit", "-q", "-m", "initial")

	must.NoError(os.WriteFile(tracked, []byte("original\nmodified\n"), 0o644))
	must.NoError(os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new\n"), 0o644))

	ex, err := executor.NewLocalExecutor(dir)
	must.NoError(err)
	ctx := context.Background()

	statusRes, err := GitStatusTool(ex).StructuredHandler(ctx, json.RawMessage(`{}`))
	must.NoError(err)
	var status gitStatusResult
	must.NoError(json.Unmarshal(statusRes.Structured, &status))
	must.False(status.Clean, "status reported clean, expected changes")
	must.True(slices.ContainsFunc(status.Entries, func(e gitStatusEntry) bool {
		return e.Path == "tracked.txt" && e.Unstaged == "M"
	}), "git_status did not report modified tracked.txt; entries=%+v", status.Entries)
	must.True(slices.ContainsFunc(status.Entries, func(e gitStatusEntry) bool {
		return e.Path == "untracked.txt" && e.Code == "??"
	}), "git_status did not report untracked.txt; entries=%+v", status.Entries)

	cfRes, err := GitChangedFilesTool(ex).StructuredHandler(ctx, json.RawMessage(`{"staged": false}`))
	must.NoError(err)
	var changed gitChangedFilesResult
	must.NoError(json.Unmarshal(cfRes.Structured, &changed))
	must.True(slices.ContainsFunc(changed.Files, func(f gitChangedFile) bool {
		return f.Path == "tracked.txt" && f.Status == "M"
	}), "git_changed_files did not report modified tracked.txt; files=%+v", changed.Files)

	diffRes, err := GitDiffTool(ex).StructuredHandler(ctx, json.RawMessage(`{}`))
	must.NoError(err)
	must.Contains(diffRes.Text, "+modified", "git_diff text missing the added line")

	diffPathRes, err := GitDiffTool(ex).StructuredHandler(ctx, json.RawMessage(`{"path": "tracked.txt"}`))
	must.NoError(err)
	must.Contains(diffPathRes.Text, "tracked.txt", "scoped git_diff missing path")

	showRes, err := GitShowTool(ex).StructuredHandler(ctx, json.RawMessage(`{"ref": "HEAD", "path": "tracked.txt"}`))
	must.NoError(err)
	must.Contains(showRes.Text, "original", "git_show missing committed content")

	// Staging last, since it empties the unstaged view checked above.
	runGitCmd("add", "tracked.txt")
	stagedFiles, err := GitChangedFilesTool(ex).StructuredHandler(ctx, json.RawMessage(`{"staged": true}`))
	must.NoError(err)
	var staged gitChangedFilesResult
	must.NoError(json.Unmarshal(stagedFiles.Structured, &staged))
	must.True(staged.Staged, "staged result not flagged staged")
	must.True(slices.ContainsFunc(staged.Files, func(f gitChangedFile) bool {
		return f.Path == "tracked.txt" && f.Status == "M"
	}), "staged git_changed_files did not report tracked.txt; files=%+v", staged.Files)

	stagedDiff, err := GitDiffTool(ex).StructuredHandler(ctx, json.RawMessage(`{"staged": true}`))
	must.NoError(err)
	must.Contains(stagedDiff.Text, "+modified", "staged git_diff missing the added line")
}

// jsonString returns a JSON-encoded string literal for use inside hand-built
// JSON payloads in tests.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
