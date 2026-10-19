package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no golden", nil, "--golden is required"},
		{"missing golden", []string{"--golden", filepath.Join(t.TempDir(), "absent.json")}, "loading golden set"},
		{"zero repeats", []string{"--golden", seedGoldenPath, "--repeats", "0"}, "--repeats must be between 1 and 100"},
		{"too many repeats", []string{"--golden", seedGoldenPath, "--repeats", "101"}, "--repeats must be between 1 and 100"},
		{"unknown flag", []string{"--golden", seedGoldenPath, "--bogus"}, "flag provided but not defined"},
		{"positional argument", []string{"--golden", seedGoldenPath, "extra"}, "unexpected arguments"},
		{"negative price", []string{"--golden", seedGoldenPath, "--price-input", "-1"}, "must be finite and not negative"},
		{"NaN price", []string{"--golden", seedGoldenPath, "--price-input", "NaN"}, "must be finite and not negative"},
		{"infinite price", []string{"--golden", seedGoldenPath, "--price-output", "+Inf"}, "must be finite and not negative"},
		{"bad cache mode", []string{"--golden", seedGoldenPath, "--judge-cache", "sometimes"}, "--judge-cache"},
		{
			"read-through without a cache dir",
			[]string{"--golden", seedGoldenPath, "--judge-cache", "read-through", "--output", filepath.Join(t.TempDir(), "r.json")},
			"--judge-cache read-through needs --judge-cache-dir",
		},
		{
			"replay-strict without a cache dir",
			[]string{"--golden", seedGoldenPath, "--judge-cache", "replay-strict"},
			"--judge-cache replay-strict needs --judge-cache-dir",
		},
		{
			"record without a cache dir or output",
			[]string{"--golden", seedGoldenPath, "--judge-cache", "record"},
			"--judge-cache record needs --judge-cache-dir or --output",
		},
		{"bad provider", []string{"--golden", seedGoldenPath, "--judge-provider", "bedrock", "--judge-model", "m"}, "judge configuration"},
		{"missing base url", []string{"--golden", seedGoldenPath, "--judge-provider", "openai-compatible", "--judge-model", "m"}, "base_url is required"},
		{"missing config file", []string{"--golden", seedGoldenPath, "--judge-config", missingConfig}, "--judge-config"},
		{
			"unresolvable key",
			[]string{"--golden", seedGoldenPath, "--judge-model", "m", "--judge-base-url", "http://127.0.0.1:9",
				"--judge-api-key-ref", "secret://CALIBRATE_UNSET_KEY"},
			"secret://CALIBRATE_UNSET_KEY",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runCalibrate(t, tc.args...)
			require.Equal(t, 2, code, "stderr %q", stderr)
			require.Contains(t, stderr, tc.want)
			require.Empty(t, stdout, "a usage error printed a report")
		})
	}
}

func TestCmdJudgeCalibrate_HelpExits0(t *testing.T) {
	code, _, stderr := runCalibrate(t, "-help")
	require.Equal(t, 0, code, "stderr %q", stderr)
	require.Contains(t, stderr, "-golden")
	require.Contains(t, stderr, "decision")
	require.Equal(t, 0, run([]string{"judge-calibrate", "-help"}, io.Discard), "run() dispatch exit")
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
	require.Equal(t, 0, code, "stderr:\n%s", stderr)
	for _, want := range []string{
		"Judge calibration: diff-review-seed (24 cases x 2 repeats = 48 judgments)",
		"Judge: anthropic stub-judge at " + endpoint.srv.URL,
		"Adversarial flip rate", "Estimated cost: $",
	} {
		assert.Contains(t, stdout, want)
	}
	bodies, _ := endpoint.seen()
	assert.Len(t, bodies, 48, "judge requests")

	report := readCalibrationReport(t, output)
	m := report.Metrics
	assert.Len(t, report.Judgments, 48)
	assert.Equal(t, 24, m.TPR.K, "TPR sample size")
	assert.Equal(t, 0, m.TNR.K, "TNR sample size")
	assert.Equal(t, 12, m.AdversarialFlipRate.K, "adversarial flip sample size")
	assert.NotNil(t, m.Cost, "cost estimate")

	_, err := os.Stat(cacheDir)
	require.NoError(t, err, "judge cache beside the report")
	assert.Contains(t, stderr, "Using judge cache directory "+cacheDir+" (mode record)")
	assert.Contains(t, stderr, "Judge cache (record): 0 hits, 48 misses, 48 stored")

	raw, _ := os.ReadFile(output)
	assert.NotContains(t, string(raw), "cli-secret", "the report file carries the resolved API key")
	assert.NotContains(t, stdout, "cli-secret", "the report output carries the resolved API key")

	code, stdout, stderr = runCalibrate(t, append(args, "--judge-cache", "read-through", "--judge-cache-dir", cacheDir)...)
	require.Equal(t, 0, code, "second run stderr:\n%s", stderr)
	require.Contains(t, stdout, "including 48 judgments served from the cache")
	bodies, _ = endpoint.seen()
	assert.Len(t, bodies, 48, "judge requests after a cached re-run")
}

func TestCmdJudgeCalibrate_RefusesAnUntrustedCacheDir(t *testing.T) {
	endpoint := newJudgeEndpoint(t)
	t.Setenv("CLI_JUDGE_KEY", "cli-secret")
	cacheDir := t.TempDir()
	require.NoError(t, os.Chmod(cacheDir, 0o777))

	code, stdout, stderr := runCalibrate(t, "--golden", seedGoldenPath, "--judge-model", "m", "--judge-base-url", endpoint.srv.URL,
		"--judge-api-key-ref", "secret://CLI_JUDGE_KEY", "--judge-cache", "record", "--judge-cache-dir", cacheDir)
	require.Equal(t, 2, code, "stderr %q", stderr)
	require.Contains(t, stderr, "--judge-cache-dir")
	require.Contains(t, stderr, "chmod go-w")

	bodies, _ := endpoint.seen()
	assert.Empty(t, stdout, "an untrusted cache directory printed a report")
	assert.Empty(t, bodies, "an untrusted cache directory still ran judgments")
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
	require.NoError(t, os.WriteFile(config, []byte("llm {\n  provider = \"decision\"\n  model = \"jev-latest\"\n  base_url = \""+srv.URL+"\"\n}\n"), 0o644))
	output := filepath.Join(t.TempDir(), "report.json")

	code, stdout, stderr := runCalibrate(t, "--golden", seedGoldenPath, "--judge-config", config, "--output", output)
	require.Equal(t, 0, code, "stderr:\n%s", stderr)
	assert.Contains(t, stdout, "Judge: decision jev-latest")

	report := readCalibrationReport(t, output)
	assert.Equal(t, 12, report.Metrics.TNR.K, "TNR sample size")
	assert.Equal(t, 0, report.Metrics.TPR.K, "TPR sample size")
	assert.Equal(t, types.JudgeProviderDecision, report.Judgments[0].Record.Provider)
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
	require.Equal(t, 0, code, "a judge that ruled on most cases should exit 0; stderr:\n%s", stderr)
	assert.Contains(t, stdout, "Decided 23 of 24 judgments; 1 error")
	assert.Contains(t, stdout, "HTTP 400")
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
	require.Equal(t, 1, code, "a judge that never rules should exit 1; stderr:\n%s", stderr)
	require.Contains(t, stderr, "all 24 judgments ended in error")
	assert.Contains(t, stdout, "Decided 0 of 24 judgments; 24 errors")
	assert.Contains(t, stdout, "HTTP 400")

	r := readCalibrationReport(t, output)
	assert.Len(t, r.Judgments, 24, "judgments in the JSON report")
	assert.Equal(t, 24, r.Metrics.Errors, "errors in the JSON report")
}

func TestCmdJudgeCalibrate_UnwritableOutputExits1(t *testing.T) {
	endpoint := newJudgeEndpoint(t)
	t.Setenv("CLI_JUDGE_KEY", "cli-secret")
	output := filepath.Join(t.TempDir(), "missing-dir", "report.json")
	code, _, stderr := runCalibrate(t, "--golden", seedGoldenPath, "--judge-model", "m", "--judge-base-url", endpoint.srv.URL,
		"--judge-api-key-ref", "secret://CLI_JUDGE_KEY", "--output", output)
	require.Equal(t, 1, code, "stderr %q", stderr)
	require.Contains(t, stderr, "writing report")
}

func readCalibrationReport(t *testing.T, path string) calibrate.Report {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var r calibrate.Report
	require.NoError(t, json.Unmarshal(raw, &r))
	return r
}

func TestCmdJudgeCalibrate_UnusableTempDirExits1(t *testing.T) {
	endpoint := newJudgeEndpoint(t)
	t.Setenv("CLI_JUDGE_KEY", "cli-secret")
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "absent"))

	code, stdout, stderr := runCalibrate(t, "--golden", seedGoldenPath, "--judge-model", "m", "--judge-base-url", endpoint.srv.URL,
		"--judge-api-key-ref", "secret://CLI_JUDGE_KEY")

	require.Equal(t, 1, code, "stderr %q", stderr)
	require.Contains(t, stderr, "creating work directory")
	bodies, _ := endpoint.seen()
	assert.Empty(t, stdout, "a failed setup printed a report")
	assert.Empty(t, bodies, "a failed setup still made judge requests")
}
