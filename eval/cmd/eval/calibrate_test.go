package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rxbynerd/stirrup/eval/calibrate"
	"github.com/rxbynerd/stirrup/types"
)

const seedGoldenPath = "../../golden/diff-review-seed.json"

func runCalibrate(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cmdJudgeCalibrate(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestCmdJudgeCalibrate_UsageErrorsExit2(t *testing.T) {
	t.Setenv("CALIBRATE_UNSET_KEY", "")
	missingConfig := filepath.Join(t.TempDir(), "absent.hcl")
	cases := map[string]struct {
		args []string
		want string
	}{
		"no golden":           {nil, "--golden is required"},
		"missing golden":      {[]string{"--golden", filepath.Join(t.TempDir(), "absent.json")}, "loading golden set"},
		"zero repeats":        {[]string{"--golden", seedGoldenPath, "--repeats", "0"}, "--repeats must be between 1 and 100"},
		"too many repeats":    {[]string{"--golden", seedGoldenPath, "--repeats", "101"}, "--repeats must be between 1 and 100"},
		"unknown flag":        {[]string{"--golden", seedGoldenPath, "--bogus"}, "flag provided but not defined"},
		"positional argument": {[]string{"--golden", seedGoldenPath, "extra"}, "unexpected arguments"},
		"negative price":      {[]string{"--golden", seedGoldenPath, "--price-input", "-1"}, "must be finite and not negative"},
		"NaN price":           {[]string{"--golden", seedGoldenPath, "--price-input", "NaN"}, "must be finite and not negative"},
		"infinite price":      {[]string{"--golden", seedGoldenPath, "--price-output", "+Inf"}, "must be finite and not negative"},
		"bad cache mode":      {[]string{"--golden", seedGoldenPath, "--judge-cache", "sometimes"}, "--judge-cache"},
		"read-through without a cache dir": {[]string{"--golden", seedGoldenPath, "--judge-cache", "read-through", "--output", filepath.Join(t.TempDir(), "r.json")},
			"--judge-cache read-through needs --judge-cache-dir"},
		"replay-strict without a cache dir": {[]string{"--golden", seedGoldenPath, "--judge-cache", "replay-strict"},
			"--judge-cache replay-strict needs --judge-cache-dir"},
		"record without a cache dir or output": {[]string{"--golden", seedGoldenPath, "--judge-cache", "record"},
			"--judge-cache record needs --judge-cache-dir or --output"},
		"bad provider":        {[]string{"--golden", seedGoldenPath, "--judge-provider", "bedrock", "--judge-model", "m"}, "judge configuration"},
		"missing base url":    {[]string{"--golden", seedGoldenPath, "--judge-provider", "openai-compatible", "--judge-model", "m"}, "base_url is required"},
		"missing config file": {[]string{"--golden", seedGoldenPath, "--judge-config", missingConfig}, "--judge-config"},
		"unresolvable key": {[]string{"--golden", seedGoldenPath, "--judge-model", "m", "--judge-base-url", "http://127.0.0.1:9",
			"--judge-api-key-ref", "secret://CALIBRATE_UNSET_KEY"}, "secret://CALIBRATE_UNSET_KEY"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := runCalibrate(t, tc.args...)
			if code != 2 || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit %d, stderr %q; want exit 2 and %q", code, stderr, tc.want)
			}
			if stdout != "" {
				t.Errorf("a usage error printed a report: %q", stdout)
			}
		})
	}
}

func TestCmdJudgeCalibrate_HelpExits0(t *testing.T) {
	code, _, stderr := runCalibrate(t, "-help")
	if code != 0 || !strings.Contains(stderr, "-golden") || !strings.Contains(stderr, "decision") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if code := run([]string{"judge-calibrate", "-help"}, io.Discard); code != 0 {
		t.Errorf("run() dispatch exit = %d, want 0", code)
	}
}

func TestCmdJudgeCalibrate_ReportsAndReusesTheCache(t *testing.T) {
	endpoint := newJudgeEndpoint(t)
	t.Setenv("CLI_JUDGE_KEY", "cli-secret")
	output := filepath.Join(t.TempDir(), "calibration.json")
	cacheDir := filepath.Join(filepath.Dir(output), judgeCacheDirName)
	args := []string{
		"--golden", seedGoldenPath, "--judge-model", "stub-judge", "--judge-base-url", endpoint.srv.URL,
		"--judge-api-key-ref", "secret://CLI_JUDGE_KEY", "--repeats", "2",
		"--output", output, "--price-input", "1", "--price-output", "5",
	}

	code, stdout, stderr := runCalibrate(t, append(args, "--judge-cache", "record")...)
	if code != 0 {
		t.Fatalf("exit %d\nstderr:\n%s", code, stderr)
	}
	for _, want := range []string{
		"Judge calibration: diff-review-seed (24 cases x 2 repeats = 48 judgments)",
		"Judge: anthropic stub-judge at " + endpoint.srv.URL,
		"Adversarial flip rate", "Estimated cost: $",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report is missing %q:\n%s", want, stdout)
		}
	}
	if bodies, _ := endpoint.seen(); len(bodies) != 48 {
		t.Errorf("judge saw %d requests, want 48", len(bodies))
	}
	report := readCalibrationReport(t, output)
	m := report.Metrics
	if len(report.Judgments) != 48 || m.TPR.K != 24 || m.TNR.K != 0 || m.AdversarialFlipRate.K != 12 || m.Cost == nil {
		t.Errorf("report metrics = %+v", m)
	}
	if _, err := os.Stat(cacheDir); err != nil {
		t.Errorf("no judge cache beside the report: %v", err)
	}
	if !strings.Contains(stderr, "Judge cache (record): 0 hits, 48 misses, 48 stored") {
		t.Errorf("stderr does not summarise the cache:\n%s", stderr)
	}
	raw, _ := os.ReadFile(output)
	if strings.Contains(string(raw), "cli-secret") || strings.Contains(stdout, "cli-secret") {
		t.Error("the report carries the resolved API key")
	}

	code, stdout, stderr = runCalibrate(t, append(args, "--judge-cache", "read-through", "--judge-cache-dir", cacheDir)...)
	if code != 0 || !strings.Contains(stdout, "including 48 judgments served from the cache") {
		t.Fatalf("second run exit %d:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if bodies, _ := endpoint.seen(); len(bodies) != 48 {
		t.Errorf("judge saw %d requests after a cached re-run, want still 48", len(bodies))
	}
}

func TestCmdJudgeCalibrate_RefusesAnUntrustedCacheDir(t *testing.T) {
	endpoint := newJudgeEndpoint(t)
	t.Setenv("CLI_JUDGE_KEY", "cli-secret")
	cacheDir := t.TempDir()
	if err := os.Chmod(cacheDir, 0o777); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCalibrate(t, "--golden", seedGoldenPath, "--judge-model", "m", "--judge-base-url", endpoint.srv.URL,
		"--judge-api-key-ref", "secret://CLI_JUDGE_KEY", "--judge-cache", "record", "--judge-cache-dir", cacheDir)
	if code != 2 || !strings.Contains(stderr, "--judge-cache-dir") || !strings.Contains(stderr, "chmod go-w") {
		t.Fatalf("exit %d, stderr %q; want exit 2 refusing the group-writable directory", code, stderr)
	}
	if bodies, _ := endpoint.seen(); stdout != "" || len(bodies) != 0 {
		t.Errorf("an untrusted cache directory still ran %d judgments", len(bodies))
	}
}

func TestCmdJudgeCalibrate_DecisionJudgeFromConfigFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"model":"jev-stub","answers":{"verdict":{"type":"choice","choice":"fail","probabilities":{"pass":0.25,"fail":0.75},"confidence":0.5}},"usage":{"input_tokens":200,"output_tokens":3}}`)
	}))
	defer srv.Close()
	config := filepath.Join(t.TempDir(), "judge.hcl")
	if err := os.WriteFile(config, []byte("llm {\n  provider = \"decision\"\n  model = \"jev-latest\"\n  base_url = \""+srv.URL+"\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "report.json")

	code, stdout, stderr := runCalibrate(t, "--golden", seedGoldenPath, "--judge-config", config, "--output", output)
	if code != 0 {
		t.Fatalf("exit %d\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "Judge: decision jev-latest") {
		t.Errorf("report does not name the decision judge:\n%s", stdout)
	}
	report := readCalibrationReport(t, output)
	if report.Metrics.TNR.K != 12 || report.Metrics.TPR.K != 0 || report.Judgments[0].Record.Provider != types.JudgeProviderDecision {
		t.Errorf("report = %+v", report.Metrics)
	}
}

func TestCmdJudgeCalibrate_SomeJudgeErrorsStillExit0(t *testing.T) {
	endpoint := newJudgeEndpoint(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}
		endpoint.srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	t.Setenv("CLI_JUDGE_KEY", "cli-secret")

	code, stdout, stderr := runCalibrate(t, "--golden", seedGoldenPath, "--judge-model", "m", "--judge-base-url", srv.URL, "--judge-api-key-ref", "secret://CLI_JUDGE_KEY")
	if code != 0 {
		t.Fatalf("exit %d, want 0 for a judge that ruled on most cases\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "Decided 23 of 24 judgments; 1 error") || !strings.Contains(stdout, "HTTP 400") {
		t.Errorf("report does not show the error:\n%s", stdout)
	}
}

func TestCmdJudgeCalibrate_AllJudgmentsErroredExits1WithTheReport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	t.Setenv("CLI_JUDGE_KEY", "cli-secret")
	output := filepath.Join(t.TempDir(), "report.json")

	code, stdout, stderr := runCalibrate(t, "--golden", seedGoldenPath, "--judge-model", "m", "--judge-base-url", srv.URL,
		"--judge-api-key-ref", "secret://CLI_JUDGE_KEY", "--output", output)
	if code != 1 || !strings.Contains(stderr, "all 24 judgments ended in error") {
		t.Fatalf("exit %d, want 1 for a judge that never rules\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "Decided 0 of 24 judgments; 24 errors") || !strings.Contains(stdout, "HTTP 400") {
		t.Errorf("report does not show the errors:\n%s", stdout)
	}
	if r := readCalibrationReport(t, output); len(r.Judgments) != 24 || r.Metrics.Errors != 24 {
		t.Errorf("JSON report = %d judgments, %d errors; want 24 of each", len(r.Judgments), r.Metrics.Errors)
	}
}

func TestCmdJudgeCalibrate_UnwritableOutputExits1(t *testing.T) {
	endpoint := newJudgeEndpoint(t)
	t.Setenv("CLI_JUDGE_KEY", "cli-secret")
	output := filepath.Join(t.TempDir(), "missing-dir", "report.json")
	code, _, stderr := runCalibrate(t, "--golden", seedGoldenPath, "--judge-model", "m", "--judge-base-url", endpoint.srv.URL,
		"--judge-api-key-ref", "secret://CLI_JUDGE_KEY", "--output", output)
	if code != 1 || !strings.Contains(stderr, "writing report") {
		t.Fatalf("exit %d, stderr %q; want exit 1 for an unwritable report", code, stderr)
	}
}

func readCalibrationReport(t *testing.T, path string) calibrate.Report {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r calibrate.Report
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}
