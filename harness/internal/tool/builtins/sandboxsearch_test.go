package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

// sandboxExecExecutor is a CanExec fake with no HostPathWorkspace and no
// TreeLister: the shape container/k8s executors present to grep_files and
// find_files, where Exec is the only route to the workspace. ResolvePath
// mirrors ContainerExecutor/podExecCore's in-sandbox absolute paths, not a
// host directory, so a native walker given resolvedDir would search nothing
// (or the harness host's own filesystem, if it happened to exist there).
type sandboxExecExecutor struct {
	resolvedDir string
	execFn      func(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error)
	commands    []string
}

func (s *sandboxExecExecutor) ReadFile(context.Context, string) (string, error) {
	return "", fmt.Errorf("not used by these tests")
}

func (s *sandboxExecExecutor) WriteFile(context.Context, string, string) error {
	return fmt.Errorf("write operations not supported")
}

func (s *sandboxExecExecutor) ListDirectory(context.Context, string) ([]string, error) {
	return nil, fmt.Errorf("not used by these tests")
}

func (s *sandboxExecExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error) {
	s.commands = append(s.commands, command)
	return s.execFn(ctx, command, timeout)
}

func (s *sandboxExecExecutor) ResolvePath(relativePath string) (string, error) {
	if relativePath == "." || relativePath == "" {
		return s.resolvedDir, nil
	}
	return s.resolvedDir + "/" + relativePath, nil
}

func (s *sandboxExecExecutor) Capabilities() executor.ExecutorCapabilities {
	return executor.ExecutorCapabilities{CanRead: true, CanExec: true, MaxTimeout: time.Minute}
}

// TestGrepFilesTool_SandboxExecRoutesThroughExec pins the CanExec-without-
// HostPathWorkspace dispatch branch: the search must run entirely through
// Exec (rg probed via the executor, then the search itself), never touch
// the harness host's own filesystem, and parse rg's structured output the
// same way the host-backed branch does.
func TestGrepFilesTool_SandboxExecRoutesThroughExec(t *testing.T) {
	plantHostCanary(t)
	fe := &sandboxExecExecutor{
		resolvedDir: "/workspace",
		execFn: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			switch {
			case strings.HasPrefix(command, "rg --version"):
				return &executor.ExecResult{ExitCode: 0}, nil
			case strings.HasPrefix(command, "rg "):
				return &executor.ExecResult{
					ExitCode: 0,
					Stdout:   `{"type":"match","data":{"path":{"text":"a.go"},"lines":{"text":"needle\n"},"line_number":3}}` + "\n",
				}, nil
			default:
				return nil, fmt.Errorf("unexpected command: %q", command)
			}
		},
	}

	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	out, err := invokeText(context.Background(), grep, input)
	require.NoError(t, err)
	assert.Contains(t, out, "a.go:3:needle")
	require.NotContains(t, out, "HOSTFS_CANARY", "host filesystem content leaked into results")
	require.NotContains(t, out, "host_canary", "host filesystem content leaked into results")
	require.Len(t, fe.commands, 2, "expected an rg --version probe then an rg search, got: %v", fe.commands)
	assert.True(t, strings.HasPrefix(fe.commands[0], "rg --version"), "expected the rg --version probe first, got %q", fe.commands[0])
	assert.True(t, strings.HasPrefix(fe.commands[1], "rg "), "expected the rg search second, got %q", fe.commands[1])
}

// TestGrepFilesTool_SandboxExecFallsBackToGrepWhenRipgrepMissing covers the
// second leg of the sandbox dispatch: when the in-sandbox rg probe fails,
// grep_files falls back to a portable `grep -r -n -E` invocation and
// relativizes the absolute in-sandbox paths grep reports to the search
// root, matching every other search path's rendering.
func TestGrepFilesTool_SandboxExecFallsBackToGrepWhenRipgrepMissing(t *testing.T) {
	plantHostCanary(t)
	fe := &sandboxExecExecutor{
		resolvedDir: "/workspace/sub",
		execFn: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			switch {
			case strings.HasPrefix(command, "rg --version"):
				return &executor.ExecResult{ExitCode: 127, Stderr: "rg: not found"}, nil
			case strings.HasPrefix(command, "grep "):
				return &executor.ExecResult{
					ExitCode: 0,
					Stdout:   "/workspace/sub/a.go:5:needle here\n",
				}, nil
			default:
				return nil, fmt.Errorf("unexpected command: %q", command)
			}
		},
	}

	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	out, err := invokeText(context.Background(), grep, input)
	require.NoError(t, err)
	assert.Equal(t, "a.go:5:needle here", out)
	require.NotContains(t, out, "HOSTFS_CANARY", "host filesystem content leaked into results")
	require.NotContains(t, out, "host_canary", "host filesystem content leaked into results")
	require.Len(t, fe.commands, 2, "expected an rg probe then a grep fallback, got: %v", fe.commands)
	assert.True(t, strings.HasPrefix(fe.commands[1], "grep "), "expected grep fallback command, got %q", fe.commands[1])
	assert.NotContains(t, fe.commands[1], "--include", "grep fallback must not use GNU-only include/exclude flags")
	assert.NotContains(t, fe.commands[1], "--exclude", "grep fallback must not use GNU-only include/exclude flags")
	assert.Contains(t, fe.commands[1], " -I ", "grep fallback must pass -I to honour the binary-files-skipped contract")
}

// TestGrepFilesTool_SandboxExecGrepHardErrorSurfaces pins the exit-code
// convention on the grep fallback: an exit code of 2 or higher with no
// usable stdout is a real invocation failure (bad regex, missing binary),
// not "no matches", and must surface as a clear tool error rather than an
// empty result.
func TestGrepFilesTool_SandboxExecGrepHardErrorSurfaces(t *testing.T) {
	fe := &sandboxExecExecutor{
		resolvedDir: "/workspace",
		execFn: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			switch {
			case strings.HasPrefix(command, "rg --version"):
				return &executor.ExecResult{ExitCode: 127}, nil
			case strings.HasPrefix(command, "grep "):
				return &executor.ExecResult{ExitCode: 2, Stderr: "grep: invalid option"}, nil
			default:
				return nil, fmt.Errorf("unexpected command: %q", command)
			}
		},
	}

	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	_, err := invokeText(context.Background(), grep, input)
	require.Error(t, err, "expected a hard grep error to surface")
	require.ErrorContains(t, err, "grep failed")
}

// TestGrepFilesTool_SandboxExecGrepPartialResultsOnMidWalkError pins the
// graceful-degradation contract: a nonzero grep exit alongside nonempty
// stdout (e.g. matches found in most of the tree, then a permission error
// on one subdirectory) must return the matches grep did find, flagged
// incomplete, rather than discarding them as a hard failure. This mirrors
// the tree-backed path's incomplete-on-partial-failure behavior.
func TestGrepFilesTool_SandboxExecGrepPartialResultsOnMidWalkError(t *testing.T) {
	fe := &sandboxExecExecutor{
		resolvedDir: "/workspace",
		execFn: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			switch {
			case strings.HasPrefix(command, "rg --version"):
				return &executor.ExecResult{ExitCode: 127}, nil
			case strings.HasPrefix(command, "grep "):
				return &executor.ExecResult{
					ExitCode: 2,
					Stdout:   "/workspace/a.go:3:needle here\n",
					Stderr:   "grep: /workspace/locked: Permission denied",
				}, nil
			default:
				return nil, fmt.Errorf("unexpected command: %q", command)
			}
		},
	}

	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	sr := decodeSearchResult(t, grep, input)
	require.Len(t, sr.Matches, 1, "expected the one match grep did find, got %+v", sr.Matches)
	require.Equal(t, "a.go", sr.Matches[0].Path)
	require.True(t, sr.Truncated, "a mid-walk grep error must be reported as an incomplete scan")
}

// TestGrepFilesTool_SandboxExecGrepFallback_ColonInPathParsedCorrectly pins
// the colon-tolerant parser: a matching file whose path itself contains a
// colon (legal on POSIX filesystems) must not be silently dropped just
// because the naive "split on the first colon" reading misidentifies the
// line-number field.
func TestGrepFilesTool_SandboxExecGrepFallback_ColonInPathParsedCorrectly(t *testing.T) {
	fe := &sandboxExecExecutor{
		resolvedDir: "/workspace",
		execFn: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			switch {
			case strings.HasPrefix(command, "rg --version"):
				return &executor.ExecResult{ExitCode: 127}, nil
			case strings.HasPrefix(command, "grep "):
				return &executor.ExecResult{
					ExitCode: 0,
					Stdout:   "/workspace/notes:draft.txt:5:needle here\n",
				}, nil
			default:
				return nil, fmt.Errorf("unexpected command: %q", command)
			}
		},
	}

	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	sr := decodeSearchResult(t, grep, input)
	assert.False(t, sr.Truncated, "a cleanly parsed colon-containing path must not be reported incomplete")
	require.Len(t, sr.Matches, 1, "match in a colon-containing path was dropped, got %+v", sr.Matches)
	got := sr.Matches[0]
	assert.Equal(t, "notes:draft.txt", got.Path)
	assert.Equal(t, 5, got.Line)
	assert.Equal(t, "needle here", got.Text)
}

// TestGrepFilesTool_SandboxExecGrepFallback_NULByteMatchSkipped pins the
// client-side backstop for the binary-files-skipped contract. busybox grep
// ignores -I and still matches inside NUL-containing files, so a matched line
// whose text contains a NUL byte must be dropped rather than surfaced to the
// model.
func TestGrepFilesTool_SandboxExecGrepFallback_NULByteMatchSkipped(t *testing.T) {
	fe := &sandboxExecExecutor{
		resolvedDir: "/workspace",
		execFn: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			switch {
			case strings.HasPrefix(command, "rg --version"):
				return &executor.ExecResult{ExitCode: 127}, nil
			case strings.HasPrefix(command, "grep "):
				return &executor.ExecResult{
					ExitCode: 0,
					Stdout:   "/workspace/blob.bin:1:needle\x00binary\n/workspace/text.txt:1:needle\n",
				}, nil
			default:
				return nil, fmt.Errorf("unexpected command: %q", command)
			}
		},
	}

	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	sr := decodeSearchResult(t, grep, input)
	assert.False(t, sr.Truncated, "a NUL-byte skip is an intentional binary filter, not an incomplete scan")
	require.Len(t, sr.Matches, 1, "expected only the non-binary match, got %+v", sr.Matches)
	require.Equal(t, "text.txt", sr.Matches[0].Path)
}

// TestGrepFilesTool_SandboxExecGrepFallback_UnparseableLineMarksIncomplete
// covers a line with no colon-delimited line number. The matches that did
// parse are still returned, but the scan is flagged incomplete rather than
// reported as exhaustive.
func TestGrepFilesTool_SandboxExecGrepFallback_UnparseableLineMarksIncomplete(t *testing.T) {
	fe := &sandboxExecExecutor{
		resolvedDir: "/workspace",
		execFn: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			switch {
			case strings.HasPrefix(command, "rg --version"):
				return &executor.ExecResult{ExitCode: 127}, nil
			case strings.HasPrefix(command, "grep "):
				return &executor.ExecResult{
					ExitCode: 0,
					Stdout:   "garbage line with no line number\n/workspace/a.go:3:needle here\n",
				}, nil
			default:
				return nil, fmt.Errorf("unexpected command: %q", command)
			}
		},
	}

	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	sr := decodeSearchResult(t, grep, input)
	require.Len(t, sr.Matches, 1, "expected the one parseable match, got %+v", sr.Matches)
	require.Equal(t, "a.go", sr.Matches[0].Path)
	require.True(t, sr.Truncated, "an unparseable line must mark the scan incomplete rather than vanish silently")
}

// TestFindFilesTool_SandboxExecRoutesThroughExec pins find_files' CanExec
// dispatch branch: `find <dir> -type f` runs through Exec, output paths are
// relativized to the search root, and the harness host's own filesystem is
// never touched.
func TestFindFilesTool_SandboxExecRoutesThroughExec(t *testing.T) {
	plantHostCanary(t)
	fe := &sandboxExecExecutor{
		resolvedDir: "/workspace",
		execFn: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			if !strings.HasPrefix(command, "find ") {
				return nil, fmt.Errorf("unexpected command: %q", command)
			}
			return &executor.ExecResult{
				ExitCode: 0,
				Stdout:   "/workspace/a.go\n/workspace/sub/b.go\n/workspace/notes.txt\n",
			}, nil
		},
	}

	find := FindFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"name": "*.go"})
	out, err := invokeText(context.Background(), find, input)
	require.NoError(t, err)
	require.Equal(t, "a.go\nsub/b.go", out)
	require.NotContains(t, out, "HOSTFS_CANARY", "host filesystem content leaked into results")
	require.NotContains(t, out, "host_canary", "host filesystem content leaked into results")
	require.Len(t, fe.commands, 1, "expected a single find command, got %v", fe.commands)
	require.True(t, strings.HasPrefix(fe.commands[0], "find "), "expected a find command, got %q", fe.commands[0])
}

// TestFindFilesTool_SandboxExecHardErrorSurfaces pins fail-closed behaviour
// when find produces no usable output at all inside the sandbox.
func TestFindFilesTool_SandboxExecHardErrorSurfaces(t *testing.T) {
	fe := &sandboxExecExecutor{
		resolvedDir: "/workspace",
		execFn: func(context.Context, string, time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{ExitCode: 1, Stderr: "find: permission denied"}, nil
		},
	}

	find := FindFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"name": "*.go"})
	_, err := invokeText(context.Background(), find, input)
	require.Error(t, err, "expected a hard find error to surface")
	require.ErrorContains(t, err, "find failed")
}

// TestFindFilesTool_SandboxExecPartialResultsOnMidWalkError mirrors the
// grep-side graceful-degradation contract for find: a nonzero exit
// alongside nonempty stdout (one unreadable subdirectory, everything else
// listed fine) must return the files find did find, flagged incomplete,
// not discard them.
func TestFindFilesTool_SandboxExecPartialResultsOnMidWalkError(t *testing.T) {
	fe := &sandboxExecExecutor{
		resolvedDir: "/workspace",
		execFn: func(context.Context, string, time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{
				ExitCode: 1,
				Stdout:   "/workspace/a.go\n",
				Stderr:   "find: /workspace/locked: Permission denied",
			}, nil
		},
	}

	find := FindFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"name": "*.go"})
	fr := decodeFindResult(t, find, input)
	require.Len(t, fr.Paths, 1, "expected the one path find did find, got %v", fr.Paths)
	require.Equal(t, "a.go", fr.Paths[0])
	require.True(t, fr.Truncated, "a mid-walk find error must be reported as an incomplete scan")
}

// TestGrepFilesTool_SandboxExecRipgrepOutputCapMarksIncomplete pins the
// ExecResult.OutputTruncated wiring: an executor-level output cap that cut
// off rg's stdout must surface as an incomplete scan, even though the
// invocation itself succeeded (exit 0) and every match up to the cap parsed
// cleanly — a clean parse of a truncated stream is not a complete result.
func TestGrepFilesTool_SandboxExecRipgrepOutputCapMarksIncomplete(t *testing.T) {
	fe := &sandboxExecExecutor{
		resolvedDir: "/workspace",
		execFn: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			switch {
			case strings.HasPrefix(command, "rg --version"):
				return &executor.ExecResult{ExitCode: 0}, nil
			case strings.HasPrefix(command, "rg "):
				return &executor.ExecResult{
					ExitCode:        0,
					Stdout:          `{"type":"match","data":{"path":{"text":"a.go"},"lines":{"text":"hit\n"},"line_number":1}}` + "\n",
					OutputTruncated: true,
				}, nil
			default:
				return nil, fmt.Errorf("unexpected command: %q", command)
			}
		},
	}

	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"pattern": "hit"})
	sr := decodeSearchResult(t, grep, input)
	require.Len(t, sr.Matches, 1, "expected the one parsed match, got %+v", sr.Matches)
	require.True(t, sr.Truncated, "an executor-level output cap must mark the scan incomplete")
}

// TestGrepFilesTool_SandboxExecGrepFallback_OutputCapMarksIncomplete covers
// the executor output-cap contract on the grep fallback, as the rg path does.
func TestGrepFilesTool_SandboxExecGrepFallback_OutputCapMarksIncomplete(t *testing.T) {
	fe := &sandboxExecExecutor{
		resolvedDir: "/workspace",
		execFn: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			switch {
			case strings.HasPrefix(command, "rg --version"):
				return &executor.ExecResult{ExitCode: 127}, nil
			case strings.HasPrefix(command, "grep "):
				return &executor.ExecResult{
					ExitCode:        0,
					Stdout:          "/workspace/a.go:1:hit\n",
					OutputTruncated: true,
				}, nil
			default:
				return nil, fmt.Errorf("unexpected command: %q", command)
			}
		},
	}

	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"pattern": "hit"})
	sr := decodeSearchResult(t, grep, input)
	require.Len(t, sr.Matches, 1, "expected the one parsed match, got %+v", sr.Matches)
	require.True(t, sr.Truncated, "an executor-level output cap must mark the scan incomplete")
}

// TestFindFilesTool_SandboxExecOutputCapMarksIncomplete is find_files'
// analogue: `find`'s own successful output, capped by the executor before
// find_files ever saw it, must not be reported as an exhaustive listing.
func TestFindFilesTool_SandboxExecOutputCapMarksIncomplete(t *testing.T) {
	fe := &sandboxExecExecutor{
		resolvedDir: "/workspace",
		execFn: func(context.Context, string, time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{
				ExitCode:        0,
				Stdout:          "/workspace/a.go\n",
				OutputTruncated: true,
			}, nil
		},
	}

	find := FindFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"name": "*.go"})
	fr := decodeFindResult(t, find, input)
	require.Len(t, fr.Paths, 1, "expected the one listed path, got %v", fr.Paths)
	require.Equal(t, "a.go", fr.Paths[0])
	require.True(t, fr.Truncated, "an executor-level output cap must mark the listing incomplete")
}

// TestSandboxRipgrepProbe_TransportErrorNotCached pins the retry-on-flake
// contract: a transport error on the probe (Exec itself failing, not a
// definitive nonzero exit) must not be cached — the next call retries
// rather than staying permanently stuck on the grep fallback.
func TestSandboxRipgrepProbe_TransportErrorNotCached(t *testing.T) {
	callCount := 0
	fe := &sandboxExecExecutor{
		resolvedDir: "/workspace",
		execFn: func(context.Context, string, time.Duration) (*executor.ExecResult, error) {
			callCount++
			if callCount == 1 {
				return nil, fmt.Errorf("transport hiccup")
			}
			return &executor.ExecResult{ExitCode: 0}, nil
		},
	}
	probe := &sandboxRipgrepProbe{}
	assert.False(t, probe.detect(context.Background(), fe), "first (transport-failing) probe must report absent")
	assert.True(t, probe.detect(context.Background(), fe), "second probe must retry and report present, not stay cached on the transport failure")
	assert.Equal(t, 2, callCount, "expected the probe to run exactly twice (retry after transport error)")
}

// TestSandboxRipgrepProbe_DefinitiveAbsentIsCached is the inverse: once the
// probe actually ran and reported a definitive nonzero exit ("rg not
// found"), that answer must be cached — no repeated probing on every call.
func TestSandboxRipgrepProbe_DefinitiveAbsentIsCached(t *testing.T) {
	callCount := 0
	fe := &sandboxExecExecutor{
		resolvedDir: "/workspace",
		execFn: func(context.Context, string, time.Duration) (*executor.ExecResult, error) {
			callCount++
			return &executor.ExecResult{ExitCode: 127}, nil
		},
	}
	probe := &sandboxRipgrepProbe{}
	probe.detect(context.Background(), fe)
	probe.detect(context.Background(), fe)
	probe.detect(context.Background(), fe)
	require.Equal(t, 1, callCount, "expected a definitive absent result to be cached after one probe")
}

// --- Sandbox-branch rg transport-error / cancellation mirrors ---

// TestGrepFilesTool_SandboxExecRipgrepTransportErrorFallsBackToGrep covers
// the sandbox branch: the rg probe reports present, but the rg invocation
// fails at the transport level (not with a bad exit code). That must fall
// back to grep rather than hard-error.
func TestGrepFilesTool_SandboxExecRipgrepTransportErrorFallsBackToGrep(t *testing.T) {
	fe := &sandboxExecExecutor{
		resolvedDir: "/workspace",
		execFn: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			switch {
			case strings.HasPrefix(command, "rg --version"):
				return &executor.ExecResult{ExitCode: 0}, nil
			case strings.HasPrefix(command, "rg "):
				return nil, fmt.Errorf("docker socket: connection refused")
			case strings.HasPrefix(command, "grep "):
				return &executor.ExecResult{ExitCode: 0, Stdout: "/workspace/a.go:1:needle\n"}, nil
			default:
				return nil, fmt.Errorf("unexpected command: %q", command)
			}
		},
	}
	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	out, err := invokeText(context.Background(), grep, input)
	require.NoError(t, err, "transport error must not surface; expected grep fallback")
	require.Equal(t, "a.go:1:needle", out)
}

// TestGrepFilesTool_SandboxExecRipgrepContextCancelPropagates covers the
// sandbox branch: a context.Canceled from the rg invocation must propagate,
// not trigger a silent grep fallback.
func TestGrepFilesTool_SandboxExecRipgrepContextCancelPropagates(t *testing.T) {
	fe := &sandboxExecExecutor{
		resolvedDir: "/workspace",
		execFn: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			switch {
			case strings.HasPrefix(command, "rg --version"):
				return &executor.ExecResult{ExitCode: 0}, nil
			case strings.HasPrefix(command, "rg "):
				return nil, context.Canceled
			default:
				return nil, fmt.Errorf("unexpected command: %q", command)
			}
		},
	}
	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	_, err := invokeText(context.Background(), grep, input)
	require.ErrorIs(t, err, context.Canceled)
}

// --- Sandbox-branch include/exclude and max_results end-to-end ---

// TestGrepFilesTool_SandboxExecAppliesIncludeExclude exercises client-side
// include/exclude filtering end-to-end through the grep fallback, rather
// than only against matchesFilters in isolation — a wiring bug (e.g.
// swapped base/rel arguments) would not be caught by the unit-level filter
// tests alone.
func TestGrepFilesTool_SandboxExecAppliesIncludeExclude(t *testing.T) {
	fe := &sandboxExecExecutor{
		resolvedDir: "/workspace",
		execFn: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			switch {
			case strings.HasPrefix(command, "rg --version"):
				return &executor.ExecResult{ExitCode: 127}, nil
			case strings.HasPrefix(command, "grep "):
				return &executor.ExecResult{
					ExitCode: 0,
					Stdout: "/workspace/a.go:1:needle\n" +
						"/workspace/a.txt:1:needle\n" +
						"/workspace/skip.go:1:needle\n",
				}, nil
			default:
				return nil, fmt.Errorf("unexpected command: %q", command)
			}
		},
	}
	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{
		"pattern": "needle",
		"include": []string{"*.go"},
		"exclude": []string{"skip.go"},
	})
	out, err := invokeText(context.Background(), grep, input)
	require.NoError(t, err)
	require.Equal(t, "a.go:1:needle", out)
}

// TestFindFilesTool_SandboxExecAppliesIncludeExclude checks the same
// include/exclude wiring for find_files.
func TestFindFilesTool_SandboxExecAppliesIncludeExclude(t *testing.T) {
	fe := &sandboxExecExecutor{
		resolvedDir: "/workspace",
		execFn: func(context.Context, string, time.Duration) (*executor.ExecResult, error) {
			return &executor.ExecResult{
				ExitCode: 0,
				Stdout:   "/workspace/a.go\n/workspace/a.txt\n/workspace/skip.go\n",
			}, nil
		},
	}
	find := FindFilesTool(fe)
	input, _ := json.Marshal(map[string]any{
		"name":    "*.go",
		"exclude": []string{"skip.go"},
	})
	out, err := invokeText(context.Background(), find, input)
	require.NoError(t, err)
	require.Equal(t, "a.go", out)
}

// TestGrepFilesTool_SandboxExecTruncatedBoundary exercises the look-ahead
// probe end-to-end through the grep fallback: a count landing exactly on
// max_results must report Truncated:false; one past must report true and
// trim the probe element. Mirrors TestGrepFilesTool_TruncatedBoundary_Native
// / _Ripgrep for the sandbox-exec branch.
func TestGrepFilesTool_SandboxExecTruncatedBoundary(t *testing.T) {
	const maxResults = 3
	cases := []struct {
		name          string
		lines         int
		wantTruncated bool
		wantMatches   int
	}{
		{"below cap", maxResults - 1, false, maxResults - 1},
		{"exactly at cap", maxResults, false, maxResults},
		{"one past cap", maxResults + 1, true, maxResults},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout strings.Builder
			for i := 0; i < tc.lines; i++ {
				fmt.Fprintf(&stdout, "/workspace/f%d.go:1:hit\n", i)
			}
			fe := &sandboxExecExecutor{
				resolvedDir: "/workspace",
				execFn: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
					switch {
					case strings.HasPrefix(command, "rg --version"):
						return &executor.ExecResult{ExitCode: 127}, nil
					case strings.HasPrefix(command, "grep "):
						return &executor.ExecResult{ExitCode: 0, Stdout: stdout.String()}, nil
					default:
						return nil, fmt.Errorf("unexpected command: %q", command)
					}
				},
			}
			grep := GrepFilesTool(fe)
			input, _ := json.Marshal(map[string]any{"pattern": "hit", "max_results": maxResults})
			sr := decodeSearchResult(t, grep, input)
			require.Equal(t, tc.wantTruncated, sr.Truncated)
			require.Len(t, sr.Matches, tc.wantMatches)
		})
	}
}

// TestFindFilesTool_SandboxExecTruncatedBoundary is the find-side analogue.
func TestFindFilesTool_SandboxExecTruncatedBoundary(t *testing.T) {
	const maxResults = 3
	cases := []struct {
		name          string
		lines         int
		wantTruncated bool
		wantPaths     int
	}{
		{"below cap", maxResults - 1, false, maxResults - 1},
		{"exactly at cap", maxResults, false, maxResults},
		{"one past cap", maxResults + 1, true, maxResults},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout strings.Builder
			for i := 0; i < tc.lines; i++ {
				fmt.Fprintf(&stdout, "/workspace/f%d.go\n", i)
			}
			fe := &sandboxExecExecutor{
				resolvedDir: "/workspace",
				execFn: func(context.Context, string, time.Duration) (*executor.ExecResult, error) {
					return &executor.ExecResult{ExitCode: 0, Stdout: stdout.String()}, nil
				},
			}
			find := FindFilesTool(fe)
			input, _ := json.Marshal(map[string]any{"name": "*.go", "max_results": maxResults})
			fr := decodeFindResult(t, find, input)
			require.Equal(t, tc.wantTruncated, fr.Truncated)
			require.Len(t, fr.Paths, tc.wantPaths)
		})
	}
}

// --- Dispatch precedence: HostPathWorkspace > CanExec > TreeLister ---

// hostAndExecExecutor implements both HostPathWorkspace and CanExec at
// once. No production executor combines these today (LocalExecutor is the
// only HostPathWorkspace implementer), but nothing proves the dispatch
// switch's ordering without a fixture that forces the choice. Exec panics,
// so a regression that lets CanExec win fails loudly instead of silently
// routing through the sandbox-exec branch.
type hostAndExecExecutor struct {
	root string
}

func (e *hostAndExecExecutor) ReadFile(context.Context, string) (string, error) {
	return "", fmt.Errorf("not used by these tests")
}

func (e *hostAndExecExecutor) WriteFile(context.Context, string, string) error {
	return fmt.Errorf("write operations not supported")
}

func (e *hostAndExecExecutor) ListDirectory(context.Context, string) ([]string, error) {
	return nil, fmt.Errorf("not used by these tests")
}

func (e *hostAndExecExecutor) Exec(context.Context, string, time.Duration) (*executor.ExecResult, error) {
	panic("Exec must not be called: HostPathWorkspace must win over CanExec")
}

func (e *hostAndExecExecutor) ResolvePath(relativePath string) (string, error) {
	if relativePath == "." || relativePath == "" {
		return e.root, nil
	}
	return filepath.Join(e.root, relativePath), nil
}

func (e *hostAndExecExecutor) Capabilities() executor.ExecutorCapabilities {
	return executor.ExecutorCapabilities{CanRead: true, CanExec: true, MaxTimeout: time.Minute}
}

func (e *hostAndExecExecutor) HostWorkspaceRoot() string { return e.root }

func TestGrepFilesTool_HostPathWorkspaceWinsOverCanExec(t *testing.T) {
	withRipgrepProbe(t, false)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("needle\n"), 0o644))
	grep := GrepFilesTool(&hostAndExecExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	out, err := invokeText(context.Background(), grep, input)
	require.NoError(t, err, "Exec must not have been reached")
	require.Contains(t, out, "a.go", "expected native walker match")
}

func TestFindFilesTool_HostPathWorkspaceWinsOverCanExec(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644))
	find := FindFilesTool(&hostAndExecExecutor{root: dir})
	input, _ := json.Marshal(map[string]any{"name": "*.go"})
	out, err := invokeText(context.Background(), find, input)
	require.NoError(t, err, "Exec must not have been reached")
	require.Contains(t, out, "a.go", "expected native walker match")
}

// execAndTreeListerExecutor implements both CanExec and TreeLister at once,
// pinning that the sandbox-exec branch takes priority over remote-tree
// enumeration when an executor exposes both. ListTree panics so a
// regression that reorders the switch fails loudly instead of silently
// picking the slower, network-bound tree path.
type execAndTreeListerExecutor struct {
	sandboxExecExecutor
}

func (e *execAndTreeListerExecutor) ListTree(context.Context, string) (executor.TreeListing, error) {
	panic("ListTree must not be called: CanExec must win over TreeLister")
}

func TestGrepFilesTool_CanExecWinsOverTreeLister(t *testing.T) {
	fe := &execAndTreeListerExecutor{sandboxExecExecutor{
		resolvedDir: "/workspace",
		execFn: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			switch {
			case strings.HasPrefix(command, "rg --version"):
				return &executor.ExecResult{ExitCode: 0}, nil
			case strings.HasPrefix(command, "rg "):
				return &executor.ExecResult{
					ExitCode: 0,
					Stdout:   `{"type":"match","data":{"path":{"text":"a.go"},"lines":{"text":"needle\n"},"line_number":1}}` + "\n",
				}, nil
			default:
				return nil, fmt.Errorf("unexpected command: %q", command)
			}
		},
	}}
	grep := GrepFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	out, err := invokeText(context.Background(), grep, input)
	require.NoError(t, err, "ListTree must not have been reached")
	require.Contains(t, out, "a.go", "expected exec-backed match")
}

func TestFindFilesTool_CanExecWinsOverTreeLister(t *testing.T) {
	fe := &execAndTreeListerExecutor{sandboxExecExecutor{
		resolvedDir: "/workspace",
		execFn: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			if !strings.HasPrefix(command, "find ") {
				return nil, fmt.Errorf("unexpected command: %q", command)
			}
			return &executor.ExecResult{ExitCode: 0, Stdout: "/workspace/a.go\n"}, nil
		},
	}}
	find := FindFilesTool(fe)
	input, _ := json.Marshal(map[string]any{"name": "*.go"})
	out, err := invokeText(context.Background(), find, input)
	require.NoError(t, err, "ListTree must not have been reached")
	require.Contains(t, out, "a.go", "expected exec-backed match")
}

// TestGrepFilesTool_SandboxRipgrepPopulatesColumn confirms the sandboxed rg
// path shares the host path's column semantics, and that the `grep` fallback
// taken when rg is absent still omits the field.
func TestGrepFilesTool_SandboxRipgrepPopulatesColumn(t *testing.T) {
	rgJSON := `{"type":"match","data":{"path":{"text":"/workspace/sub/a.go"},"lines":{"text":"héllo needle here\n"},` +
		`"line_number":3,"submatches":[{"match":{"text":"needle"},"start":7,"end":13}]}}`
	withRG := &sandboxExecExecutor{
		resolvedDir: "/workspace/sub",
		execFn: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			switch {
			case strings.HasPrefix(command, "rg --version"):
				return &executor.ExecResult{ExitCode: 0, Stdout: "ripgrep 14.1.0"}, nil
			case strings.HasPrefix(command, "rg --json"):
				return &executor.ExecResult{ExitCode: 0, Stdout: rgJSON}, nil
			default:
				return nil, fmt.Errorf("unexpected command: %q", command)
			}
		},
	}

	input, _ := json.Marshal(map[string]any{"pattern": "needle"})
	got := decodeSearchResult(t, GrepFilesTool(withRG), input)
	want := searchMatch{Path: "/workspace/sub/a.go", Line: 3, Column: 8, Text: "héllo needle here"}
	require.Len(t, got.Matches, 1)
	require.Equal(t, want, got.Matches[0], "sandboxed rg match")

	withGrep := &sandboxExecExecutor{
		resolvedDir: "/workspace/sub",
		execFn: func(_ context.Context, command string, _ time.Duration) (*executor.ExecResult, error) {
			switch {
			case strings.HasPrefix(command, "rg --version"):
				return &executor.ExecResult{ExitCode: 127, Stderr: "rg: not found"}, nil
			case strings.HasPrefix(command, "grep "):
				return &executor.ExecResult{ExitCode: 0, Stdout: "/workspace/sub/a.go:3:héllo needle here\n"}, nil
			default:
				return nil, fmt.Errorf("unexpected command: %q", command)
			}
		},
	}
	fallback, err := GrepFilesTool(withGrep).StructuredHandler(context.Background(), input)
	require.NoError(t, err)
	assertNoColumnKey(t, fallback.Structured)
}
