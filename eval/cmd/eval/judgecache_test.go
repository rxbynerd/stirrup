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

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/eval/judge"
	"github.com/rxbynerd/stirrup/types"
)

func TestJudgeFlags_CacheMode(t *testing.T) {
	opts, err := parseJudgeFlags(t).options()
	if err != nil {
		t.Fatal(err)
	}
	if opts.CacheMode != judge.CacheLive || opts.Cache != nil {
		t.Errorf("default options = %+v, want live with no cache", opts)
	}
	for _, mode := range judge.CacheModes() {
		opts, err := parseJudgeFlags(t, "--judge-cache", string(mode), "--judge-cache-dir", t.TempDir()).options()
		if err != nil || opts.CacheMode != mode {
			t.Errorf("--judge-cache %s: mode %q, err %v", mode, opts.CacheMode, err)
		}
	}
	for _, mode := range []judge.CacheMode{judge.CacheReadThrough, judge.CacheReplayStrict} {
		_, err := parseJudgeFlags(t, "--judge-cache", string(mode)).options()
		if err == nil || !strings.Contains(err.Error(), "--judge-cache-dir") {
			t.Errorf("--judge-cache %s without a directory: err = %v, want one naming --judge-cache-dir", mode, err)
		}
	}
	if _, err := parseJudgeFlags(t, "--judge-cache", "record").options(); err != nil {
		t.Errorf("--judge-cache record without a directory: %v", err)
	}
	if _, err := parseJudgeFlags(t, "--judge-cache", "replay").options(); err == nil || !strings.Contains(err.Error(), "--judge-cache") {
		t.Errorf("unknown mode: err = %v, want one naming --judge-cache", err)
	}
}

func TestJudgeFlags_OpenCache(t *testing.T) {
	t.Run("live opens nothing", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "never")
		jf := parseJudgeFlags(t, "--judge-cache-dir", dir)
		opts, err := jf.options()
		if err != nil {
			t.Fatal(err)
		}
		got, err := jf.openCache(&opts, filepath.Join(t.TempDir(), "record-default"), nil)
		if err != nil || got != "" || opts.Cache != nil || opts.CacheStats != nil {
			t.Errorf("dir %q, err %v, opts %+v; want no cache in live mode", got, err, opts)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("live mode created %s", dir)
		}
	})

	t.Run("record defaults to the record directory", func(t *testing.T) {
		want := filepath.Join(t.TempDir(), "out", judgeCacheDirName)
		jf := parseJudgeFlags(t, "--judge-cache", "record")
		opts, err := jf.options()
		if err != nil {
			t.Fatal(err)
		}
		got, err := jf.openCache(&opts, want, nil)
		if err != nil || got != want {
			t.Fatalf("dir %q, err %v; want %q", got, err, want)
		}
		resolved, err := filepath.EvalSymlinks(want)
		if err != nil {
			t.Fatalf("cache directory not created: %v", err)
		}
		fc, ok := opts.Cache.(*judge.FileCache)
		if !ok || fc.Dir() != resolved || opts.CacheStats == nil {
			t.Errorf("opts = %+v", opts)
		}
	})

	for _, mode := range []string{"record", "read-through", "replay-strict"} {
		t.Run(mode+" uses an explicit directory", func(t *testing.T) {
			want := t.TempDir()
			jf := parseJudgeFlags(t, "--judge-cache", mode, "--judge-cache-dir", want)
			opts, err := jf.options()
			if err != nil {
				t.Fatal(err)
			}
			recordDir := filepath.Join(t.TempDir(), "unused")
			if got, err := jf.openCache(&opts, recordDir, nil); err != nil || got != want {
				t.Errorf("dir %q, err %v; want %q", got, err, want)
			}
			if _, err := os.Stat(recordDir); !os.IsNotExist(err) {
				t.Errorf("the record directory was created: %v", err)
			}
		})
	}

	t.Run("a directory inside a forbidden root is refused", func(t *testing.T) {
		workspace := t.TempDir()
		jf := parseJudgeFlags(t, "--judge-cache", "record", "--judge-cache-dir", filepath.Join(workspace, "cache"))
		opts, err := jf.options()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := jf.openCache(&opts, "", []string{workspace}); err == nil || !strings.Contains(err.Error(), "--judge-cache-dir") {
			t.Errorf("err = %v, want the directory refused", err)
		}
		if opts.Cache != nil {
			t.Error("a refused directory was installed as the cache")
		}
	})
}

func TestReplayWorkspaces(t *testing.T) {
	recordings := []types.RunRecording{
		{Config: types.RunConfig{Executor: types.ExecutorConfig{Workspace: "/srv/runs/r1"}}},
		{Config: types.RunConfig{Executor: types.ExecutorConfig{Workspace: "relative/ws"}}},
		{},
	}
	got := replayWorkspaces("/preserved", recordings)
	if want := []string{"/preserved", "/srv/runs/r1"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("replayWorkspaces = %q, want %q", got, want)
	}
}

func TestFormatJudgeCache(t *testing.T) {
	s := eval.JudgeCacheSummary{Mode: "read-through", Hits: 3, Misses: 1, Stored: 1}
	if got, want := formatJudgeCache(s), "Judge cache (read-through): 3 hits, 1 misses, 1 stored, 0 bypassed"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	s.WriteErrors = 2
	if got := formatJudgeCache(s); !strings.HasSuffix(got, ", 2 write errors") {
		t.Errorf("got %q, want the write errors reported", got)
	}
	s.Replaced = 1
	if got, want := formatJudgeCache(s), "Judge cache (read-through): 3 hits, 1 misses, 1 stored, 0 bypassed, 1 replaced, 2 write errors"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
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
	if code := run(append(args, "--judge-cache", "record"), io.Discard); code != 0 {
		t.Fatalf("record: exit code %d", code)
	}
	recorded, err := loadResult(filepath.Join(outputDir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	key := recorded.Tasks[0].JudgeVerdict.Record.CacheKey
	entry := filepath.Join(cacheDir, key[:2], key[2:4], key+".json")
	raw, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte(strings.Replace(string(raw), `"parserVersion": `, `"parserVersion": 9`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}

	out, code := runEval(t, nil, append(args, "--judge-cache", "read-through", "--judge-cache-dir", cacheDir)...)
	if code != 0 {
		t.Fatalf("read-through: exit code %d:\n%s", code, out)
	}
	if n := strings.Count(out, "warning: replaced an unusable judge cache entry"); n != 1 || !strings.Contains(out, key) {
		t.Errorf("want one warning naming %s, got %d:\n%s", key, n, out)
	}
	if !strings.Contains(out, "1 replaced") {
		t.Errorf("summary line does not report the replacement:\n%s", out)
	}
	result, err := loadResult(filepath.Join(outputDir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if result.JudgeCache == nil || result.JudgeCache.Replaced != 1 || result.JudgeCache.Stored != 1 {
		t.Errorf("summary = %+v, want one replaced and stored entry", result.JudgeCache)
	}
	if bodies, _ := endpoint.seen(); len(bodies) != 2 {
		t.Errorf("judge saw %d requests, want the unusable entry judged again", len(bodies))
	}
}

func TestCompletion_JudgeCacheFlagsRegistered(t *testing.T) {
	for _, sub := range []string{"run", "replay"} {
		have := map[string]bool{}
		for _, f := range evalCompletionFlags[sub] {
			have[f] = true
		}
		for _, f := range []string{"judge-cache", "judge-cache-dir"} {
			if !have[f] {
				t.Errorf("%s completion is missing -%s", sub, f)
			}
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
		if code := run(append([]string{
			"run", "--suite", suitePath, "--harness", harnessPath, "--output", outputDir,
			"--judge-model", "m", "--judge-base-url", endpoint.srv.URL, "--judge-api-key-ref", "secret://CLI_JUDGE_KEY",
			"--judge-cache", mode,
		}, extra...), io.Discard); code != 0 {
			t.Fatalf("%s: exit code %d", mode, code)
		}
		result, err := loadResult(filepath.Join(outputDir, "result.json"))
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Tasks) != 1 || result.Tasks[0].Outcome != "pass" {
			t.Fatalf("%s: tasks = %+v", mode, result.Tasks)
		}
		return result
	}

	recorded := runWith("record")
	if want := (eval.JudgeCacheSummary{Mode: "record", Misses: 1, Stored: 1}); recorded.JudgeCache == nil || *recorded.JudgeCache != want {
		t.Errorf("record summary = %+v, want %+v", recorded.JudgeCache, want)
	}
	key := recorded.Tasks[0].JudgeVerdict.Record.CacheKey
	if _, err := os.Stat(filepath.Join(outputDir, judgeCacheDirName, key[:2], key[2:4], key+".json")); err != nil {
		t.Errorf("entry not under <output>/judge-cache: %v", err)
	}

	replayed := runWith("read-through", "--judge-cache-dir", filepath.Join(outputDir, judgeCacheDirName))
	if want := (eval.JudgeCacheSummary{Mode: "read-through", Hits: 1}); replayed.JudgeCache == nil || *replayed.JudgeCache != want {
		t.Errorf("read-through summary = %+v, want %+v", replayed.JudgeCache, want)
	}
	if rec := replayed.Tasks[0].JudgeVerdict.Record; rec.CacheStatus != types.JudgeCacheHit || rec.CacheKey != key {
		t.Errorf("record = %+v", rec)
	}
	if bodies, _ := endpoint.seen(); len(bodies) != 1 {
		t.Errorf("judge saw %d requests, want only the recording run's", len(bodies))
	}
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
	if code := run([]string{"run", "--suite", suitePath, "--dry-run", "--judge-cache", "record", "--judge-cache-dir", cacheDir}, io.Discard); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Errorf("--dry-run created the judge cache directory: %v", err)
	}
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
		if code := run(args, io.Discard); code != 0 {
			t.Fatalf("%v: exit code %d", extra, code)
		}
		result, err := loadResult(output)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	cacheDir := filepath.Join(lakehouse, judgeCacheDirName)
	if err := os.Mkdir(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	missed := replay("--judge-cache", "replay-strict", "--judge-cache-dir", cacheDir)
	if tr := missed.Tasks[0]; tr.Outcome != "error" || !strings.Contains(tr.Error, "replay-strict never calls the model") {
		t.Errorf("strict replay of an empty cache: %+v", tr)
	}
	if bodies, _ := endpoint.seen(); len(bodies) != 0 {
		t.Fatalf("replay-strict called the judge %d times", len(bodies))
	}

	recorded := replay("--judge-cache", "record")
	if recorded.Tasks[0].Outcome != "pass" || recorded.JudgeCache == nil || recorded.JudgeCache.Stored != 1 {
		t.Fatalf("record replay: %+v, summary %+v", recorded.Tasks, recorded.JudgeCache)
	}

	t.Setenv("CLI_JUDGE_KEY", "")
	strict := replay("--judge-cache", "replay-strict", "--judge-cache-dir", cacheDir)
	if strict.Tasks[0].Outcome != "pass" || strict.Tasks[0].JudgeVerdict.Record.CacheStatus != types.JudgeCacheHit {
		t.Errorf("strict replay: %+v", strict.Tasks[0])
	}
	if want := (eval.JudgeCacheSummary{Mode: "replay-strict", Hits: 1}); strict.JudgeCache == nil || *strict.JudgeCache != want {
		t.Errorf("strict summary = %+v, want %+v", strict.JudgeCache, want)
	}
	if bodies, _ := endpoint.seen(); len(bodies) != 1 {
		t.Errorf("judge saw %d requests, want only the recording replay's", len(bodies))
	}
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
	if err := os.WriteFile(filepath.Join(workspace, "created.txt"), []byte("agent output\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return workspace
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestLoadResult_CommittedBaselinesHaveNoCacheSummary(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "baselines", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no committed baselines found: %v", err)
	}
	for _, path := range paths {
		result, err := loadResult(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if len(result.Tasks) == 0 || result.JudgeCache != nil {
			t.Errorf("%s: %d tasks, judge cache %+v", path, len(result.Tasks), result.JudgeCache)
		}
	}
}

// TestEvalSubprocess is the re-executed half of runEval and does nothing
// in a normal test run.
func TestEvalSubprocess(t *testing.T) {
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
	cmd := exec.Command(os.Args[0], "-test.run=^TestEvalSubprocess$")
	cmd.Env = append(append(os.Environ(), env...), subprocessArgsEnv+"="+strings.Join(args, "\x1f"))
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(out), exitErr.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
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
				if code != 1 || !strings.Contains(out, "--judge-cache "+mode+" needs --judge-cache-dir") {
					t.Fatalf("exit code %d, want 1 with a usage error naming --judge-cache-dir:\n%s", code, out)
				}
				if _, err := os.Stat(recordDir); !os.IsNotExist(err) {
					t.Errorf("a reading mode created the record directory: %v", err)
				}
				entries, err := os.ReadDir(tmp)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), "stirrup-judge-cache-") {
						t.Errorf("a temporary cache directory was created: %s", e.Name())
					}
				}
			})
		}
	}
}

func TestCmd_UntrustedCacheDirectoriesAreRefused(t *testing.T) {
	for _, sub := range []string{"run", "replay"} {
		for _, mode := range []string{"record", "read-through", "replay-strict"} {
			t.Run(sub+"/"+mode+"/world-writable", func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "judge-cache")
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(dir, 0o777); err != nil {
					t.Fatal(err)
				}
				args, _ := cacheCommand(t, sub)
				out, code := runEval(t, []string{"CLI_JUDGE_KEY=k"}, append(args, "--judge-cache", mode, "--judge-cache-dir", dir)...)
				if code == 0 || !strings.Contains(out, "lets other users write") {
					t.Errorf("exit code %d, want the directory refused:\n%s", code, out)
				}
			})
		}
	}

	t.Run("replay/inside the workspace", func(t *testing.T) {
		args, _ := cacheCommand(t, "replay")
		workspace := args[slices.Index(args, "--workspace")+1]
		dir := filepath.Join(workspace, "judge-cache")
		out, code := runEval(t, []string{"CLI_JUDGE_KEY=k"}, append(args, "--judge-cache", "record", "--judge-cache-dir", dir)...)
		if code == 0 || !strings.Contains(out, "agent under test can write") {
			t.Errorf("exit code %d, want the directory refused:\n%s", code, out)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("the refused directory was created: %v", err)
		}
	})
}

func TestCmdReplay_ReplayStrictFailsFastOnAMissingCache(t *testing.T) {
	args, _ := cacheCommand(t, "replay")
	missing := filepath.Join(t.TempDir(), "no-such-cache")
	output := filepath.Join(t.TempDir(), "replay.json")
	out, code := runEval(t, nil, append(args, "--output", output, "--judge-cache", "replay-strict", "--judge-cache-dir", missing)...)
	if code == 0 || !strings.Contains(out, "--judge-cache-dir") || !strings.Contains(out, missing) || !strings.Contains(out, "does not exist") {
		t.Errorf("exit code %d, want a failure naming the missing directory:\n%s", code, out)
	}
	for _, path := range []string{missing, output} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s exists after the failed open: %v", path, err)
		}
	}
}
