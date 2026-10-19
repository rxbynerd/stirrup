package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/eval/judge"
	"github.com/rxbynerd/stirrup/types"
)

func TestJudgeFlags_CacheMode(t *testing.T) {
	t.Run("default is live without a cache", func(t *testing.T) {
		opts, err := parseJudgeFlags(t).options()
		require.NoError(t, err)
		require.Equal(t, judge.CacheLive, opts.CacheMode)
		require.Nil(t, opts.Cache)
	})

	t.Run("each mode is accepted with a directory", func(t *testing.T) {
		for _, mode := range judge.CacheModes() {
			t.Run(string(mode), func(t *testing.T) {
				opts, err := parseJudgeFlags(t, "--judge-cache", string(mode), "--judge-cache-dir", t.TempDir()).options()
				require.NoError(t, err)
				require.Equal(t, mode, opts.CacheMode)
			})
		}
	})

	t.Run("reading modes need a directory", func(t *testing.T) {
		for _, mode := range []judge.CacheMode{judge.CacheReadThrough, judge.CacheReplayStrict} {
			t.Run(string(mode), func(t *testing.T) {
				_, err := parseJudgeFlags(t, "--judge-cache", string(mode)).options()
				require.ErrorContains(t, err, "--judge-cache-dir")
			})
		}
	})

	t.Run("record works without a directory", func(t *testing.T) {
		_, err := parseJudgeFlags(t, "--judge-cache", "record").options()
		require.NoError(t, err)
	})

	t.Run("unknown mode is rejected", func(t *testing.T) {
		_, err := parseJudgeFlags(t, "--judge-cache", "replay").options()
		require.ErrorContains(t, err, "--judge-cache")
	})
}

func TestJudgeFlags_OpenCache(t *testing.T) {
	t.Run("live opens nothing", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "never")
		jf := parseJudgeFlags(t, "--judge-cache-dir", dir)
		opts, err := jf.options()
		require.NoError(t, err)

		got, err := jf.openCache(io.Discard, &opts, filepath.Join(t.TempDir(), "record-default"), nil)
		require.NoError(t, err)
		require.Empty(t, got)
		require.Nil(t, opts.Cache)
		require.Nil(t, opts.CacheStats)

		_, err = os.Stat(dir)
		require.ErrorIs(t, err, os.ErrNotExist, "live mode created the cache directory")
	})

	t.Run("record defaults to the record directory", func(t *testing.T) {
		want := filepath.Join(t.TempDir(), "out", judgeCacheDirName)
		jf := parseJudgeFlags(t, "--judge-cache", "record")
		opts, err := jf.options()
		require.NoError(t, err)

		got, err := jf.openCache(io.Discard, &opts, want, nil)
		require.NoError(t, err)
		require.Equal(t, want, got)

		resolved, err := filepath.EvalSymlinks(want)
		require.NoError(t, err, "cache directory not created")
		fc, ok := opts.Cache.(*judge.FileCache)
		require.True(t, ok, "cache is not a file cache")
		require.Equal(t, resolved, fc.Dir())
		require.NotNil(t, opts.CacheStats)
	})

	for _, mode := range []string{"record", "read-through", "replay-strict"} {
		t.Run(mode+" uses an explicit directory", func(t *testing.T) {
			want := t.TempDir()
			jf := parseJudgeFlags(t, "--judge-cache", mode, "--judge-cache-dir", want)
			opts, err := jf.options()
			require.NoError(t, err)

			recordDir := filepath.Join(t.TempDir(), "unused")
			got, err := jf.openCache(io.Discard, &opts, recordDir, nil)
			require.NoError(t, err)
			require.Equal(t, want, got)

			_, err = os.Stat(recordDir)
			require.ErrorIs(t, err, os.ErrNotExist, "the record directory was created")
		})
	}

	t.Run("a directory inside a forbidden root is refused", func(t *testing.T) {
		workspace := t.TempDir()
		jf := parseJudgeFlags(t, "--judge-cache", "record", "--judge-cache-dir", filepath.Join(workspace, "cache"))
		opts, err := jf.options()
		require.NoError(t, err)

		_, err = jf.openCache(io.Discard, &opts, "", []string{workspace})
		require.ErrorContains(t, err, "--judge-cache-dir")
		require.Nil(t, opts.Cache, "a refused directory was installed as the cache")
	})
}

func TestReplayWorkspaces_KeepsAbsoluteRecordedPaths(t *testing.T) {
	recordings := []types.RunRecording{
		{Config: types.RunConfig{Executor: types.ExecutorConfig{Workspace: "/srv/runs/r1"}}},
		{Config: types.RunConfig{Executor: types.ExecutorConfig{Workspace: "relative/ws"}}},
		{},
	}
	got := replayWorkspaces("/preserved", recordings)
	require.Equal(t, []string{"/preserved", "/srv/runs/r1"}, got)
}

func TestFormatJudgeCache_ReportsCountersAndErrors(t *testing.T) {
	s := eval.JudgeCacheSummary{Mode: "read-through", Hits: 3, Misses: 1, Stored: 1}
	require.Equal(t, "Judge cache (read-through): 3 hits, 1 misses, 1 stored, 0 bypassed", formatJudgeCache(s))

	s.WriteErrors = 2
	got := formatJudgeCache(s)
	require.True(t, strings.HasSuffix(got, ", 2 write errors"), "got %q, want the write errors reported", got)

	s.Replaced = 1
	require.Equal(t, "Judge cache (read-through): 3 hits, 1 misses, 1 stored, 0 bypassed, 1 replaced, 2 write errors", formatJudgeCache(s))
}

func TestCmdRun_ReadThroughReplacesAnUnusableEntryWithOneWarning(t *testing.T) {
	endpoint := newJudgeEndpoint(t)
	t.Setenv("CLI_JUDGE_KEY", "cli-secret")
	harnessPath := writeFakeHarness(t, createsFileHarness)
	suitePath := writeSuite(t, diffReviewSuiteHCL)
	outputDir := filepath.Join(t.TempDir(), "out")
	cacheDir := filepath.Join(outputDir, judgeCacheDirName)
	args := []string{
		"run", "--suite", suitePath, "--harness", harnessPath, "--output", outputDir,
		"--judge-model", "m", "--judge-base-url", endpoint.srv.URL, "--judge-api-key-ref", "secret://CLI_JUDGE_KEY",
	}
	code := run(append(args, "--judge-cache", "record"), io.Discard)
	require.Equal(t, 0, code, "record exit code")

	recorded, err := loadResult(filepath.Join(outputDir, "result.json"))
	require.NoError(t, err)
	key := recorded.Tasks[0].JudgeVerdict.Record.CacheKey
	entry := filepath.Join(cacheDir, key[:2], key[2:4], key+".json")
	raw, err := os.ReadFile(entry)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(entry, []byte(strings.Replace(string(raw), `"parserVersion": `, `"parserVersion": 9`, 1)), 0o600))

	out, code := runEval(t, nil, append(args, "--judge-cache", "read-through", "--judge-cache-dir", cacheDir)...)
	require.Equal(t, 0, code, "read-through exit code, output:\n%s", out)
	require.Equal(t, 1, strings.Count(out, "warning: replaced an unusable judge cache entry"), "warnings in output:\n%s", out)
	require.Contains(t, out, key)
	require.Contains(t, out, "1 replaced")

	result, err := loadResult(filepath.Join(outputDir, "result.json"))
	require.NoError(t, err)
	require.NotNil(t, result.JudgeCache)
	require.Equal(t, 1, result.JudgeCache.Replaced)
	require.Equal(t, 1, result.JudgeCache.Stored)

	bodies, _ := endpoint.seen()
	require.Len(t, bodies, 2, "the unusable entry should be judged again")
}

func TestCompletion_JudgeCacheFlagsRegistered(t *testing.T) {
	for _, sub := range []string{"run", "replay"} {
		for _, f := range []string{"judge-cache", "judge-cache-dir"} {
			require.Contains(t, evalCompletionFlags[sub], f, "%s completion", sub)
		}
	}
}

func TestCmdRun_JudgeCacheRecordThenReadThrough(t *testing.T) {
	endpoint := newJudgeEndpoint(t)
	t.Setenv("CLI_JUDGE_KEY", "cli-secret")
	harnessPath := writeFakeHarness(t, createsFileHarness)
	suitePath := writeSuite(t, diffReviewSuiteHCL)
	outputDir := filepath.Join(t.TempDir(), "out")

	runWith := func(mode string, extra ...string) eval.SuiteResult {
		t.Helper()
		code := run(append([]string{
			"run", "--suite", suitePath, "--harness", harnessPath, "--output", outputDir,
			"--judge-model", "m", "--judge-base-url", endpoint.srv.URL, "--judge-api-key-ref", "secret://CLI_JUDGE_KEY",
			"--judge-cache", mode,
		}, extra...), io.Discard)
		require.Equal(t, 0, code, "%s exit code", mode)

		result, err := loadResult(filepath.Join(outputDir, "result.json"))
		require.NoError(t, err)
		require.Len(t, result.Tasks, 1, "%s task count", mode)
		require.Equal(t, "pass", result.Tasks[0].Outcome, "%s tasks: %+v", mode, result.Tasks)
		return result
	}

	recorded := runWith("record")
	require.NotNil(t, recorded.JudgeCache)
	require.Equal(t, eval.JudgeCacheSummary{Mode: "record", Misses: 1, Stored: 1}, *recorded.JudgeCache)
	key := recorded.Tasks[0].JudgeVerdict.Record.CacheKey
	require.FileExists(t, filepath.Join(outputDir, judgeCacheDirName, key[:2], key[2:4], key+".json"), "entry not under <output>/judge-cache")

	replayed := runWith("read-through", "--judge-cache-dir", filepath.Join(outputDir, judgeCacheDirName))
	require.NotNil(t, replayed.JudgeCache)
	require.Equal(t, eval.JudgeCacheSummary{Mode: "read-through", Hits: 1}, *replayed.JudgeCache)
	rec := replayed.Tasks[0].JudgeVerdict.Record
	require.Equal(t, types.JudgeCacheHit, rec.CacheStatus)
	require.Equal(t, key, rec.CacheKey)

	bodies, _ := endpoint.seen()
	require.Len(t, bodies, 1, "only the recording run should call the judge")
}

func TestCmdRun_DryRunOpensNoJudgeCache(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	suitePath := writeSuite(t, `
suite "dry-cache" {
  task "a" {
    prompt = "p"
    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`)
	code := run([]string{"run", "--suite", suitePath, "--dry-run", "--judge-cache", "record", "--judge-cache-dir", cacheDir}, io.Discard)
	require.Equal(t, 0, code, "exit code")

	_, err := os.Stat(cacheDir)
	require.ErrorIs(t, err, os.ErrNotExist, "--dry-run created the judge cache directory")
}

func TestCmdReplay_ReplayStrictServesTheRecordedCache(t *testing.T) {
	endpoint := newJudgeEndpoint(t)
	t.Setenv("CLI_JUDGE_KEY", "cli-secret")
	workspace := gitWorkspaceWithChange(t)
	lakehouse := seedRecordings(t, []string{"r1"}, []string{"success"})
	suitePath := writeSuite(t, diffReviewSuiteHCL)

	replay := func(extra ...string) eval.SuiteResult {
		t.Helper()
		output := filepath.Join(t.TempDir(), "replay.json")
		args := append([]string{
			"replay", "--lakehouse", lakehouse, "--suite", suitePath, "--workspace", workspace, "--output", output,
			"--judge-model", "m", "--judge-base-url", endpoint.srv.URL, "--judge-api-key-ref", "secret://CLI_JUDGE_KEY",
		}, extra...)
		code := run(args, io.Discard)
		require.Equal(t, 0, code, "replay %v exit code", extra)

		result, err := loadResult(output)
		require.NoError(t, err)
		return result
	}

	cacheDir := filepath.Join(lakehouse, judgeCacheDirName)
	require.NoError(t, os.Mkdir(cacheDir, 0o700))

	missed := replay("--judge-cache", "replay-strict", "--judge-cache-dir", cacheDir)
	tr := missed.Tasks[0]
	require.Equal(t, "error", tr.Outcome, "strict replay of an empty cache: %+v", tr)
	require.Contains(t, tr.Error, "replay-strict never calls the model")

	bodies, _ := endpoint.seen()
	require.Empty(t, bodies, "replay-strict called the judge")

	recorded := replay("--judge-cache", "record")
	require.Equal(t, "pass", recorded.Tasks[0].Outcome, "record replay tasks: %+v", recorded.Tasks)
	require.NotNil(t, recorded.JudgeCache)
	require.Equal(t, 1, recorded.JudgeCache.Stored)

	t.Setenv("CLI_JUDGE_KEY", "")
	strict := replay("--judge-cache", "replay-strict", "--judge-cache-dir", cacheDir)
	require.Equal(t, "pass", strict.Tasks[0].Outcome, "strict replay: %+v", strict.Tasks[0])
	require.Equal(t, types.JudgeCacheHit, strict.Tasks[0].JudgeVerdict.Record.CacheStatus)
	require.NotNil(t, strict.JudgeCache)
	require.Equal(t, eval.JudgeCacheSummary{Mode: "replay-strict", Hits: 1}, *strict.JudgeCache)

	bodies, _ = endpoint.seen()
	require.Len(t, bodies, 1, "only the recording replay should call the judge")
}

// gitWorkspaceWithChange is a git repository with one commit and an
// uncommitted created.txt, for replay's workspace-head baseline.
func gitWorkspaceWithChange(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.name", "t"},
		{"config", "user.email", "t@example.invalid"},
		{"commit", "-q", "--allow-empty", "-m", "baseline"},
	} {
		gitRun(t, workspace, args...)
	}
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "created.txt"), []byte("agent output\n"), 0o644))
	return workspace
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v:\n%s", args, out)
}

func TestLoadResult_CommittedBaselinesHaveNoCacheSummary(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "baselines", "*.json"))
	require.NoError(t, err, "glob committed baselines")
	require.NotEmpty(t, paths, "no committed baselines found")

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			result, err := loadResult(path)
			require.NoError(t, err)
			require.NotEmpty(t, result.Tasks)
			require.Nil(t, result.JudgeCache)
		})
	}
}

// TestEvalSubprocess_RunsCLIArgsFromEnv is the re-executed half of runEval and
// does nothing in a normal test run.
func TestEvalSubprocess_RunsCLIArgsFromEnv(t *testing.T) {
	args := os.Getenv(subprocessArgsEnv)
	if args == "" {
		t.Skip("runs only when re-executed by runEval")
	}
	os.Exit(run(strings.Split(args, "\x1f"), io.Discard))
}

// runEval runs the eval CLI with args in a re-executed test binary, for
// commands that exit through log.Fatal, and returns its combined output
// and exit code.
func runEval(t *testing.T, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestEvalSubprocess_RunsCLIArgsFromEnv$")
	cmd.Env = append(append(os.Environ(), env...), subprocessArgsEnv+"="+strings.Join(args, "\x1f"))
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(out), exitErr.ExitCode()
	}
	require.NoError(t, err)
	return string(out), 0
}

// cacheCommand returns run or replay arguments for a diff-review suite,
// and the directory record mode defaults to for that command.
func cacheCommand(t *testing.T, sub string) (args []string, recordDir string) {
	t.Helper()
	suite := writeSuite(t, diffReviewSuiteHCL)
	judgeArgs := []string{"--judge-model", "m", "--judge-base-url", "https://judge.invalid", "--judge-api-key-ref", "secret://CLI_JUDGE_KEY"}
	if sub == "run" {
		output := filepath.Join(t.TempDir(), "out")
		harness := writeFakeHarness(t, createsFileHarness)
		return append([]string{"run", "--suite", suite, "--harness", harness, "--output", output}, judgeArgs...), filepath.Join(output, judgeCacheDirName)
	}
	lakehouse := seedRecordings(t, []string{"r1"}, []string{"success"})
	return append([]string{"replay", "--lakehouse", lakehouse, "--suite", suite, "--workspace", gitWorkspaceWithChange(t)}, judgeArgs...), filepath.Join(lakehouse, judgeCacheDirName)
}

func TestCmd_ReadingCacheModesNeedAnExplicitDirectory(t *testing.T) {
	for _, sub := range []string{"run", "replay"} {
		for _, mode := range []string{"read-through", "replay-strict"} {
			t.Run(sub+"/"+mode, func(t *testing.T) {
				tmp := t.TempDir()
				args, recordDir := cacheCommand(t, sub)
				out, code := runEval(t, []string{"TMPDIR=" + tmp, "CLI_JUDGE_KEY=k"}, append(args, "--judge-cache", mode)...)
				require.Equal(t, 1, code, "want a usage error, output:\n%s", out)
				require.Contains(t, out, "--judge-cache "+mode+" needs --judge-cache-dir")

				_, err := os.Stat(recordDir)
				require.ErrorIs(t, err, os.ErrNotExist, "a reading mode created the record directory")

				entries, err := os.ReadDir(tmp)
				require.NoError(t, err)
				var caches []string
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), "stirrup-judge-cache-") {
						caches = append(caches, e.Name())
					}
				}
				require.Empty(t, caches, "temporary cache directories created")
			})
		}
	}
}

func TestCmd_UntrustedCacheDirectoriesAreRefused(t *testing.T) {
	for _, sub := range []string{"run", "replay"} {
		for _, mode := range []string{"record", "read-through", "replay-strict"} {
			t.Run(sub+"/"+mode+"/world-writable", func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "judge-cache")
				require.NoError(t, os.Mkdir(dir, 0o700))
				require.NoError(t, os.Chmod(dir, 0o777))

				args, _ := cacheCommand(t, sub)
				out, code := runEval(t, []string{"CLI_JUDGE_KEY=k"}, append(args, "--judge-cache", mode, "--judge-cache-dir", dir)...)
				require.NotZero(t, code, "want the directory refused:\n%s", out)
				require.Contains(t, out, "lets other users write")
			})
		}
	}

	t.Run("replay/inside the workspace", func(t *testing.T) {
		args, _ := cacheCommand(t, "replay")
		workspace := args[slices.Index(args, "--workspace")+1]
		dir := filepath.Join(workspace, "judge-cache")

		out, code := runEval(t, []string{"CLI_JUDGE_KEY=k"}, append(args, "--judge-cache", "record", "--judge-cache-dir", dir)...)
		require.NotZero(t, code, "want the directory refused:\n%s", out)
		require.Contains(t, out, "agent under test can write")

		_, err := os.Stat(dir)
		require.ErrorIs(t, err, os.ErrNotExist, "the refused directory was created")
	})
}

func TestCmdReplay_ReplayStrictFailsFastOnAMissingCache(t *testing.T) {
	args, _ := cacheCommand(t, "replay")
	missing := filepath.Join(t.TempDir(), "no-such-cache")
	output := filepath.Join(t.TempDir(), "replay.json")

	out, code := runEval(t, nil, append(args, "--output", output, "--judge-cache", "replay-strict", "--judge-cache-dir", missing)...)
	require.NotZero(t, code, "want a failure naming the missing directory:\n%s", out)
	require.Contains(t, out, "--judge-cache-dir")
	require.Contains(t, out, missing)
	require.Contains(t, out, "does not exist")

	for _, path := range []string{missing, output} {
		_, err := os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist, "%s exists after the failed open", path)
	}
}
