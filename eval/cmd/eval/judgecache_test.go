package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
		opts, err := parseJudgeFlags(t, "--judge-cache", string(mode)).options()
		if err != nil || opts.CacheMode != mode {
			t.Errorf("--judge-cache %s: mode %q, err %v", mode, opts.CacheMode, err)
		}
	}
	if _, err := parseJudgeFlags(t, "--judge-cache", "replay").options(); err == nil || !strings.Contains(err.Error(), "--judge-cache") {
		t.Errorf("unknown mode: err = %v, want one naming --judge-cache", err)
	}
}

func TestJudgeFlags_OpenCache(t *testing.T) {
	unused := func() (string, error) {
		t.Error("the default directory was consulted")
		return "", nil
	}

	t.Run("live opens nothing", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "never")
		jf := parseJudgeFlags(t, "--judge-cache-dir", dir)
		opts, err := jf.options()
		if err != nil {
			t.Fatal(err)
		}
		got, err := jf.openCache(&opts, unused)
		if err != nil || got != "" || opts.Cache != nil || opts.CacheStats != nil {
			t.Errorf("dir %q, err %v, opts %+v; want no cache in live mode", got, err, opts)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("live mode created %s", dir)
		}
	})

	t.Run("default directory", func(t *testing.T) {
		want := filepath.Join(t.TempDir(), "out", judgeCacheDirName)
		jf := parseJudgeFlags(t, "--judge-cache", "read-through")
		opts, err := jf.options()
		if err != nil {
			t.Fatal(err)
		}
		got, err := jf.openCache(&opts, func() (string, error) { return want, nil })
		if err != nil || got != want {
			t.Fatalf("dir %q, err %v; want %q", got, err, want)
		}
		resolved, err := filepath.EvalSymlinks(want)
		if err != nil {
			t.Fatal(err)
		}
		fc, ok := opts.Cache.(*judge.FileCache)
		if !ok || fc.Dir() != resolved || opts.CacheStats == nil {
			t.Errorf("opts = %+v", opts)
		}
		if info, err := os.Stat(want); err != nil || !info.IsDir() {
			t.Errorf("cache directory not created: %v", err)
		}
	})

	t.Run("explicit directory wins", func(t *testing.T) {
		want := filepath.Join(t.TempDir(), "mine")
		jf := parseJudgeFlags(t, "--judge-cache", "record", "--judge-cache-dir", want)
		opts, err := jf.options()
		if err != nil {
			t.Fatal(err)
		}
		if got, err := jf.openCache(&opts, unused); err != nil || got != want {
			t.Errorf("dir %q, err %v; want %q", got, err, want)
		}
	})
}

func TestReplayCacheDir(t *testing.T) {
	t.Run("beside the recordings", func(t *testing.T) {
		lakehouse := t.TempDir()
		want := filepath.Join(lakehouse, judgeCacheDirName)
		if err := os.Mkdir(want, 0o755); err != nil {
			t.Fatal(err)
		}
		if got, err := replayCacheDir(lakehouse)(); err != nil || got != want {
			t.Errorf("dir %q, err %v; want %q", got, err, want)
		}
	})

	for name, setup := range map[string]func(lakehouse string){
		"absent": func(string) {},
		"not a directory": func(lakehouse string) {
			if err := os.WriteFile(filepath.Join(lakehouse, judgeCacheDirName), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			lakehouse := t.TempDir()
			setup(lakehouse)
			got, err := replayCacheDir(lakehouse)()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(filepath.Base(got), "stirrup-judge-cache-") || strings.HasPrefix(got, lakehouse) {
				t.Errorf("dir %q, want a new temporary directory", got)
			}
			if info, err := os.Stat(got); err != nil || !info.IsDir() {
				t.Errorf("temporary cache directory not created: %v", err)
			}
		})
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

	runWith := func(mode string) eval.SuiteResult {
		t.Helper()
		if code := run([]string{
			"run", "--suite", suitePath, "--harness", harnessPath, "--output", outputDir,
			"--judge-model", "m", "--judge-base-url", endpoint.srv.URL, "--judge-api-key-ref", "secret://CLI_JUDGE_KEY",
			"--judge-cache", mode,
		}, io.Discard); code != 0 {
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

	replayed := runWith("read-through")
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

func TestCmdReplay_ReplayStrictUsesTheLakehouseCache(t *testing.T) {
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

	if err := os.Mkdir(filepath.Join(lakehouse, judgeCacheDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	missed := replay("--judge-cache", "replay-strict")
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
	strict := replay("--judge-cache", "replay-strict")
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
