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
		must := require.New(t)
		opts, err := parseJudgeFlags(t).options()
		must.NoError(err)
		must.Equal(judge.CacheLive, opts.CacheMode)
		must.Nil(opts.Cache)
	})

	t.Run("each mode is accepted with a directory", func(t *testing.T) {
		for _, mode := range judge.CacheModes() {
			t.Run(string(mode), func(t *testing.T) {
				must := require.New(t)
				opts, err := parseJudgeFlags(t, "--judge-cache", string(mode), "--judge-cache-dir", t.TempDir()).options()
				must.NoError(err)
				must.Equal(mode, opts.CacheMode)
			})
		}
	})

	t.Run("reading modes need a directory", func(t *testing.T) {
		for _, mode := range []judge.CacheMode{judge.CacheReadThrough, judge.CacheReplayStrict} {
			t.Run(string(mode), func(t *testing.T) {
				must := require.New(t)
				_, err := parseJudgeFlags(t, "--judge-cache", string(mode)).options()
				must.ErrorContains(err, "--judge-cache-dir")
			})
		}
	})

	t.Run("record works without a directory", func(t *testing.T) {
		must := require.New(t)
		_, err := parseJudgeFlags(t, "--judge-cache", "record").options()
		must.NoError(err)
	})

	t.Run("unknown mode is rejected", func(t *testing.T) {
		must := require.New(t)
		_, err := parseJudgeFlags(t, "--judge-cache", "replay").options()
		must.ErrorContains(err, "--judge-cache")
	})
}

func TestJudgeFlags_OpenCache(t *testing.T) {
	t.Run("live opens nothing", func(t *testing.T) {
		must := require.New(t)
		dir := filepath.Join(t.TempDir(), "never")
		jf := parseJudgeFlags(t, "--judge-cache-dir", dir)
		opts, err := jf.options()
		must.NoError(err)

		got, err := jf.openCache(io.Discard, &opts, filepath.Join(t.TempDir(), "record-default"), nil)
		must.NoError(err)
		must.Empty(got)
		must.Nil(opts.Cache)
		must.Nil(opts.CacheStats)

		_, err = os.Stat(dir)
		must.ErrorIs(err, os.ErrNotExist, "live mode created the cache directory")
	})

	t.Run("record defaults to the record directory", func(t *testing.T) {
		must := require.New(t)
		want := filepath.Join(t.TempDir(), "out", judgeCacheDirName)
		jf := parseJudgeFlags(t, "--judge-cache", "record")
		opts, err := jf.options()
		must.NoError(err)

		got, err := jf.openCache(io.Discard, &opts, want, nil)
		must.NoError(err)
		must.Equal(want, got)

		resolved, err := filepath.EvalSymlinks(want)
		must.NoError(err, "cache directory not created")
		fc, ok := opts.Cache.(*judge.FileCache)
		must.True(ok, "cache is not a file cache")
		must.Equal(resolved, fc.Dir())
		must.NotNil(opts.CacheStats)
	})

	for _, mode := range []string{"record", "read-through", "replay-strict"} {
		t.Run(mode+" uses an explicit directory", func(t *testing.T) {
			must := require.New(t)
			want := t.TempDir()
			jf := parseJudgeFlags(t, "--judge-cache", mode, "--judge-cache-dir", want)
			opts, err := jf.options()
			must.NoError(err)

			recordDir := filepath.Join(t.TempDir(), "unused")
			got, err := jf.openCache(io.Discard, &opts, recordDir, nil)
			must.NoError(err)
			must.Equal(want, got)

			_, err = os.Stat(recordDir)
			must.ErrorIs(err, os.ErrNotExist, "the record directory was created")
		})
	}

	t.Run("a directory inside a forbidden root is refused", func(t *testing.T) {
		must := require.New(t)
		workspace := t.TempDir()
		jf := parseJudgeFlags(t, "--judge-cache", "record", "--judge-cache-dir", filepath.Join(workspace, "cache"))
		opts, err := jf.options()
		must.NoError(err)

		_, err = jf.openCache(io.Discard, &opts, "", []string{workspace})
		must.ErrorContains(err, "--judge-cache-dir")
		must.Nil(opts.Cache, "a refused directory was installed as the cache")
	})
}

func TestReplayWorkspaces_KeepsAbsoluteRecordedPaths(t *testing.T) {
	must := require.New(t)
	recordings := []types.RunRecording{
		{Config: types.RunConfig{Executor: types.ExecutorConfig{Workspace: "/srv/runs/r1"}}},
		{Config: types.RunConfig{Executor: types.ExecutorConfig{Workspace: "relative/ws"}}},
		{},
	}
	got := replayWorkspaces("/preserved", recordings)
	must.Equal([]string{"/preserved", "/srv/runs/r1"}, got)
}

func TestFormatJudgeCache_ReportsCountersAndErrors(t *testing.T) {
	must := require.New(t)
	s := eval.JudgeCacheSummary{Mode: "read-through", Hits: 3, Misses: 1, Stored: 1}
	must.Equal("Judge cache (read-through): 3 hits, 1 misses, 1 stored, 0 bypassed", formatJudgeCache(s))

	s.WriteErrors = 2
	got := formatJudgeCache(s)
	must.True(strings.HasSuffix(got, ", 2 write errors"), "got %q, want the write errors reported", got)

	s.Replaced = 1
	must.Equal("Judge cache (read-through): 3 hits, 1 misses, 1 stored, 0 bypassed, 1 replaced, 2 write errors", formatJudgeCache(s))
}

func TestCmdRun_ReadThroughReplacesAnUnusableEntryWithOneWarning(t *testing.T) {
	must := require.New(t)
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
	must.Equal(0, code, "record exit code")

	recorded, err := loadResult(filepath.Join(outputDir, "result.json"))
	must.NoError(err)
	key := recorded.Tasks[0].JudgeVerdict.Record.CacheKey
	entry := filepath.Join(cacheDir, key[:2], key[2:4], key+".json")
	raw, err := os.ReadFile(entry)
	must.NoError(err)
	must.NoError(os.WriteFile(entry, []byte(strings.Replace(string(raw), `"parserVersion": `, `"parserVersion": 9`, 1)), 0o600))

	out, code := runEval(t, nil, append(args, "--judge-cache", "read-through", "--judge-cache-dir", cacheDir)...)
	must.Equal(0, code, "read-through exit code, output:\n%s", out)
	must.Equal(1, strings.Count(out, "warning: replaced an unusable judge cache entry"), "warnings in output:\n%s", out)
	must.Contains(out, key)
	must.Contains(out, "1 replaced")

	result, err := loadResult(filepath.Join(outputDir, "result.json"))
	must.NoError(err)
	must.NotNil(result.JudgeCache)
	must.Equal(1, result.JudgeCache.Replaced)
	must.Equal(1, result.JudgeCache.Stored)

	bodies, _ := endpoint.seen()
	must.Len(bodies, 2, "the unusable entry should be judged again")
}

func TestCompletion_JudgeCacheFlagsRegistered(t *testing.T) {
	must := require.New(t)
	for _, sub := range []string{"run", "replay"} {
		for _, f := range []string{"judge-cache", "judge-cache-dir"} {
			must.Contains(evalCompletionFlags[sub], f, "%s completion", sub)
		}
	}
}

func TestCmdRun_JudgeCacheRecordThenReadThrough(t *testing.T) {
	must := require.New(t)
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
		must.Equal(0, code, "%s exit code", mode)

		result, err := loadResult(filepath.Join(outputDir, "result.json"))
		must.NoError(err)
		must.Len(result.Tasks, 1, "%s task count", mode)
		must.Equal("pass", result.Tasks[0].Outcome, "%s tasks: %+v", mode, result.Tasks)
		return result
	}

	recorded := runWith("record")
	must.NotNil(recorded.JudgeCache)
	must.Equal(eval.JudgeCacheSummary{Mode: "record", Misses: 1, Stored: 1}, *recorded.JudgeCache)
	key := recorded.Tasks[0].JudgeVerdict.Record.CacheKey
	must.FileExists(filepath.Join(outputDir, judgeCacheDirName, key[:2], key[2:4], key+".json"), "entry not under <output>/judge-cache")

	replayed := runWith("read-through", "--judge-cache-dir", filepath.Join(outputDir, judgeCacheDirName))
	must.NotNil(replayed.JudgeCache)
	must.Equal(eval.JudgeCacheSummary{Mode: "read-through", Hits: 1}, *replayed.JudgeCache)
	rec := replayed.Tasks[0].JudgeVerdict.Record
	must.Equal(types.JudgeCacheHit, rec.CacheStatus)
	must.Equal(key, rec.CacheKey)

	bodies, _ := endpoint.seen()
	must.Len(bodies, 1, "only the recording run should call the judge")
}

func TestCmdRun_DryRunOpensNoJudgeCache(t *testing.T) {
	must := require.New(t)
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
	must.Equal(0, code, "exit code")

	_, err := os.Stat(cacheDir)
	must.ErrorIs(err, os.ErrNotExist, "--dry-run created the judge cache directory")
}

func TestCmdReplay_ReplayStrictServesTheRecordedCache(t *testing.T) {
	must := require.New(t)
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
		must.Equal(0, code, "replay %v exit code", extra)

		result, err := loadResult(output)
		must.NoError(err)
		return result
	}

	cacheDir := filepath.Join(lakehouse, judgeCacheDirName)
	must.NoError(os.Mkdir(cacheDir, 0o700))

	missed := replay("--judge-cache", "replay-strict", "--judge-cache-dir", cacheDir)
	tr := missed.Tasks[0]
	must.Equal("error", tr.Outcome, "strict replay of an empty cache: %+v", tr)
	must.Contains(tr.Error, "replay-strict never calls the model")

	bodies, _ := endpoint.seen()
	must.Empty(bodies, "replay-strict called the judge")

	recorded := replay("--judge-cache", "record")
	must.Equal("pass", recorded.Tasks[0].Outcome, "record replay tasks: %+v", recorded.Tasks)
	must.NotNil(recorded.JudgeCache)
	must.Equal(1, recorded.JudgeCache.Stored)

	t.Setenv("CLI_JUDGE_KEY", "")
	strict := replay("--judge-cache", "replay-strict", "--judge-cache-dir", cacheDir)
	must.Equal("pass", strict.Tasks[0].Outcome, "strict replay: %+v", strict.Tasks[0])
	must.Equal(types.JudgeCacheHit, strict.Tasks[0].JudgeVerdict.Record.CacheStatus)
	must.NotNil(strict.JudgeCache)
	must.Equal(eval.JudgeCacheSummary{Mode: "replay-strict", Hits: 1}, *strict.JudgeCache)

	bodies, _ = endpoint.seen()
	must.Len(bodies, 1, "only the recording replay should call the judge")
}

// gitWorkspaceWithChange is a git repository with one commit and an
// uncommitted created.txt, for replay's workspace-head baseline.
func gitWorkspaceWithChange(t *testing.T) string {
	t.Helper()
	must := require.New(t)
	workspace := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.name", "t"},
		{"config", "user.email", "t@example.invalid"},
		{"commit", "-q", "--allow-empty", "-m", "baseline"},
	} {
		gitRun(t, workspace, args...)
	}
	must.NoError(os.WriteFile(filepath.Join(workspace, "created.txt"), []byte("agent output\n"), 0o644))
	return workspace
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	must := require.New(t)
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull)
	out, err := cmd.CombinedOutput()
	must.NoError(err, "git %v:\n%s", args, out)
}

func TestLoadResult_CommittedBaselinesHaveNoCacheSummary(t *testing.T) {
	must := require.New(t)
	paths, err := filepath.Glob(filepath.Join("..", "..", "baselines", "*.json"))
	must.NoError(err, "glob committed baselines")
	must.NotEmpty(paths, "no committed baselines found")

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			must := require.New(t)
			result, err := loadResult(path)
			must.NoError(err)
			must.NotEmpty(result.Tasks)
			must.Nil(result.JudgeCache)
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
	must := require.New(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestEvalSubprocess_RunsCLIArgsFromEnv$")
	cmd.Env = append(append(os.Environ(), env...), subprocessArgsEnv+"="+strings.Join(args, "\x1f"))
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(out), exitErr.ExitCode()
	}
	must.NoError(err)
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
				must := require.New(t)
				tmp := t.TempDir()
				args, recordDir := cacheCommand(t, sub)
				out, code := runEval(t, []string{"TMPDIR=" + tmp, "CLI_JUDGE_KEY=k"}, append(args, "--judge-cache", mode)...)
				must.Equal(1, code, "want a usage error, output:\n%s", out)
				must.Contains(out, "--judge-cache "+mode+" needs --judge-cache-dir")

				_, err := os.Stat(recordDir)
				must.ErrorIs(err, os.ErrNotExist, "a reading mode created the record directory")

				entries, err := os.ReadDir(tmp)
				must.NoError(err)
				var caches []string
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), "stirrup-judge-cache-") {
						caches = append(caches, e.Name())
					}
				}
				must.Empty(caches, "temporary cache directories created")
			})
		}
	}
}

func TestCmd_UntrustedCacheDirectoriesAreRefused(t *testing.T) {
	for _, sub := range []string{"run", "replay"} {
		for _, mode := range []string{"record", "read-through", "replay-strict"} {
			t.Run(sub+"/"+mode+"/world-writable", func(t *testing.T) {
				must := require.New(t)
				dir := filepath.Join(t.TempDir(), "judge-cache")
				must.NoError(os.Mkdir(dir, 0o700))
				must.NoError(os.Chmod(dir, 0o777))

				args, _ := cacheCommand(t, sub)
				out, code := runEval(t, []string{"CLI_JUDGE_KEY=k"}, append(args, "--judge-cache", mode, "--judge-cache-dir", dir)...)
				must.NotZero(code, "want the directory refused:\n%s", out)
				must.Contains(out, "lets other users write")
			})
		}
	}

	t.Run("replay/inside the workspace", func(t *testing.T) {
		must := require.New(t)
		args, _ := cacheCommand(t, "replay")
		workspace := args[slices.Index(args, "--workspace")+1]
		dir := filepath.Join(workspace, "judge-cache")

		out, code := runEval(t, []string{"CLI_JUDGE_KEY=k"}, append(args, "--judge-cache", "record", "--judge-cache-dir", dir)...)
		must.NotZero(code, "want the directory refused:\n%s", out)
		must.Contains(out, "agent under test can write")

		_, err := os.Stat(dir)
		must.ErrorIs(err, os.ErrNotExist, "the refused directory was created")
	})
}

func TestCmdReplay_ReplayStrictFailsFastOnAMissingCache(t *testing.T) {
	must := require.New(t)
	args, _ := cacheCommand(t, "replay")
	missing := filepath.Join(t.TempDir(), "no-such-cache")
	output := filepath.Join(t.TempDir(), "replay.json")

	out, code := runEval(t, nil, append(args, "--output", output, "--judge-cache", "replay-strict", "--judge-cache-dir", missing)...)
	must.NotZero(code, "want a failure naming the missing directory:\n%s", out)
	must.Contains(out, "--judge-cache-dir")
	must.Contains(out, missing)
	must.Contains(out, "does not exist")

	for _, path := range []string{missing, output} {
		_, err := os.Stat(path)
		must.ErrorIs(err, os.ErrNotExist, "%s exists after the failed open", path)
	}
}
