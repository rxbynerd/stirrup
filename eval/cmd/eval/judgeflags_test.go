package main

import (
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func parseJudgeFlags(t *testing.T, args ...string) *judgeFlags {
	t.Helper()
	must := require.New(t)
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jf := addJudgeFlags(fs)
	must.NoError(fs.Parse(args))
	return jf
}

func TestJudgeFlags_Options_ValidatesAndBuildsDefaults(t *testing.T) {
	t.Run("unset leaves the built-in default", func(t *testing.T) {
		must := require.New(t)
		opts, err := parseJudgeFlags(t).options()
		must.NoError(err)
		must.Nil(opts.LLMDefaults)
	})

	t.Run("set flags become defaults", func(t *testing.T) {
		must := require.New(t)
		opts, err := parseJudgeFlags(t,
			"--judge-provider", "openai-compatible",
			"--judge-model", "m",
			"--judge-base-url", "https://gateway.example/v1",
			"--judge-api-key-ref", "secret://GATEWAY_KEY",
		).options()
		must.NoError(err)
		want := types.JudgeLLMConfig{Provider: "openai-compatible", Model: "m", BaseURL: "https://gateway.example/v1", APIKeyRef: "secret://GATEWAY_KEY"}
		must.NotNil(opts.LLMDefaults)
		must.Equal(want, *opts.LLMDefaults)
	})

	t.Run("model alone is accepted for the default provider", func(t *testing.T) {
		must := require.New(t)
		_, err := parseJudgeFlags(t, "--judge-model", "claude-sonnet-5-5").options()
		must.NoError(err)
	})

	invalid := []struct {
		name string
		args []string
		want string
	}{
		{"unknown provider", []string{"--judge-provider", "bogus", "--judge-model", "m"}, "provider"},
		{"openai-compatible without base url", []string{"--judge-provider", "openai-compatible", "--judge-model", "m"}, "base"},
		{"openai-compatible without model", []string{"--judge-provider", "openai-compatible", "--judge-base-url", "https://x.example"}, "model"},
		{"literal key", []string{"--judge-api-key-ref", "sk-live-abc123"}, "secret://"},
		{"base url without host", []string{"--judge-base-url", "https://"}, "base"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, err := parseJudgeFlags(t, tc.args...).options()
			must.Error(err)
			must.ErrorContains(err, "--judge-*", "error should name the flags")
			must.Contains(strings.ToLower(err.Error()), tc.want)
			must.NotContains(err.Error(), "sk-live-abc123", "error must not echo the literal key")
		})
	}
}

func TestCompletion_JudgeFlagsRegistered(t *testing.T) {
	judgeFlagNames := []string{"judge-provider", "judge-model", "judge-base-url", "judge-api-key-ref"}
	for _, sub := range []string{"run", "replay"} {
		t.Run(sub, func(t *testing.T) {
			must := require.New(t)
			for _, f := range judgeFlagNames {
				must.Contains(evalCompletionFlags[sub], f, "completion is missing -%s", f)
			}
		})
	}
}

// judgeEndpoint is an Anthropic-shaped stub that records request bodies and
// the API key header.
type judgeEndpoint struct {
	srv *httptest.Server

	mu      sync.Mutex
	bodies  []string
	apiKeys []string
}

func newJudgeEndpoint(t *testing.T) *judgeEndpoint {
	t.Helper()
	e := &judgeEndpoint{}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.bodies = append(e.bodies, string(raw))
		e.apiKeys = append(e.apiKeys, r.Header.Get("x-api-key"))
		e.mu.Unlock()
		nonce := ""
		if m := regexp.MustCompile(`UNTRUSTED_DIFF_([0-9a-f]{32})`).FindSubmatch(raw); m != nil {
			nonce = string(m[1])
		}
		_, _ = io.WriteString(w, `{"model":"served-model","content":[{"type":"text","text":"{\"nonce\":\"`+nonce+`\",\"reasoning\":\"ok\",\"verdict\":\"pass\",\"feedback\":\"fine\"}"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":2}}`)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *judgeEndpoint) seen() (bodies, apiKeys []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.bodies...), append([]string(nil), e.apiKeys...)
}

const diffReviewSuiteHCL = `
suite "judge-flags-suite" {
  description = "fixture for the --judge-* flags"

  task "review" {
    prompt = "create created.txt"
    judge {
      type = "diff-review"
      criteria = "created.txt exists"
    }
  }
}
`

func writeSuite(t *testing.T, src string) string {
	t.Helper()
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "suite.hcl")
	must.NoError(os.WriteFile(path, []byte(src), 0o644))
	return path
}

// createsFileHarness writes created.txt and a successful trace.
const createsFileHarness = `#!/bin/sh
shift
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
echo "agent output" > created.txt
if [ -n "$TRACE" ]; then
  echo '{"id":"run-1","turns":1,"cost":0.0,"outcome":"success"}' > "$TRACE"
fi
`

func TestCmdRun_JudgeFlagsConfigureDiffReviewJudge(t *testing.T) {
	must := require.New(t)
	endpoint := newJudgeEndpoint(t)
	t.Setenv("CLI_JUDGE_KEY", "cli-secret")

	harnessPath := writeFakeHarness(t, createsFileHarness)

	outputDir := filepath.Join(t.TempDir(), "out")
	exitCode := run([]string{
		"run",
		"--suite", writeSuite(t, diffReviewSuiteHCL),
		"--harness", harnessPath,
		"--output", outputDir,
		"--judge-model", "cli-judge-model",
		"--judge-base-url", endpoint.srv.URL,
		"--judge-api-key-ref", "secret://CLI_JUDGE_KEY",
	}, io.Discard)
	must.Equal(0, exitCode, "run() exit code")

	result, err := loadResult(filepath.Join(outputDir, "result.json"))
	must.NoError(err)
	must.Len(result.Tasks, 1)
	task := result.Tasks[0]
	must.Equal("pass", task.Outcome)
	must.Equal(types.JudgeStatusPass, task.JudgeVerdict.Status)
	rec := task.JudgeVerdict.Record
	must.NotNil(rec)
	must.Equal("cli-judge-model", rec.RequestedModel)
	must.Equal("served-model", rec.ServedModel)

	bodies, keys := endpoint.seen()
	must.Len(bodies, 1, "judge should receive exactly one request")
	must.Equal("cli-secret", keys[0], "x-api-key should carry the resolved secret")
	must.Contains(bodies[0], "created.txt", "judge request should include the agent's change")
	must.Contains(bodies[0], "agent output", "judge request should include the agent's change")

	raw, err := os.ReadFile(filepath.Join(outputDir, "result.json"))
	must.NoError(err)
	must.NotContains(string(raw), "cli-secret", "result.json must not contain the resolved API key")
}

func TestCmdReplay_JudgeFlagsConfigureDiffReviewJudge(t *testing.T) {
	must := require.New(t)
	endpoint := newJudgeEndpoint(t)
	t.Setenv("CLI_JUDGE_KEY", "cli-secret")

	workspace := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.name", "t"},
		{"config", "user.email", "t@example.invalid"},
		{"commit", "-q", "--allow-empty", "-m", "baseline"},
	} {
		cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = workspace
		out, err := cmd.CombinedOutput()
		must.NoError(err, "git %v\n%s", args, out)
	}
	must.NoError(os.WriteFile(filepath.Join(workspace, "created.txt"), []byte("agent output\n"), 0o644))

	lakehouse := seedRecordings(t, []string{"r1"}, []string{"success"})
	output := filepath.Join(t.TempDir(), "replay.json")
	exitCode := run([]string{
		"replay",
		"--lakehouse", lakehouse,
		"--suite", writeSuite(t, diffReviewSuiteHCL),
		"--workspace", workspace,
		"--output", output,
		"--judge-model", "replay-judge-model",
		"--judge-base-url", endpoint.srv.URL,
		"--judge-api-key-ref", "secret://CLI_JUDGE_KEY",
	}, io.Discard)
	must.Equal(0, exitCode, "run() exit code")

	result, err := loadResult(output)
	must.NoError(err)
	must.Len(result.Tasks, 1)
	must.Equal("pass", result.Tasks[0].Outcome)
	rec := result.Tasks[0].JudgeVerdict.Record
	must.NotNil(rec)
	must.Equal("replay-judge-model", rec.RequestedModel)
}

func TestCmdRun_JudgeOutputNeverCarriesTheAPIKey(t *testing.T) {
	const key = "sk-test-0123456789abcdef"
	anthropicText := func(text, stop string) string {
		quoted, _ := json.Marshal(text)
		return `{"model":"m","content":[{"type":"text","text":` + string(quoted) + `}],"stop_reason":"` + stop + `","usage":{"input_tokens":1,"output_tokens":1}}`
	}
	cases := map[string]struct {
		provider          string
		status            int
		reply             func(r *http.Request, nonce string) string
		redactionOptional bool
	}{
		"error body echoes the headers": {
			provider: "anthropic", status: http.StatusUnauthorized,
			reply: func(r *http.Request, _ string) string {
				return "invalid key\nx-api-key=" + r.Header.Get("x-api-key") + " auth=" + r.Header.Get("Authorization")
			},
		},
		"bearer header echoed": {
			provider: "openai-compatible", status: http.StatusUnauthorized,
			reply: func(r *http.Request, _ string) string {
				return `{"error":{"message":"bad ` + r.Header.Get("Authorization") + `"}}`
			},
		},
		"error message on HTTP 200": {
			provider: "openai-compatible", status: http.StatusOK,
			reply: func(r *http.Request, _ string) string {
				return `{"error":{"message":"key ` + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") + ` is revoked"}}`
			},
		},
		"reply without a verdict quotes the key": {
			provider: "anthropic", status: http.StatusOK,
			reply: func(r *http.Request, _ string) string {
				return anthropicText("I saw "+r.Header.Get("x-api-key"), "end_turn")
			},
		},
		"refusal quotes the key": {
			provider: "anthropic", status: http.StatusOK,
			reply: func(r *http.Request, _ string) string {
				return anthropicText("no: "+r.Header.Get("x-api-key"), "refusal")
			},
			redactionOptional: true,
		},
		"served model and stop reason echo the key": {
			provider: "anthropic", status: http.StatusOK,
			reply: func(r *http.Request, nonce string) string {
				key := r.Header.Get("x-api-key")
				return `{"model":"` + key + `","content":[{"type":"text","text":"{}"}],"stop_reason":"` + key + `","usage":{"input_tokens":1,"output_tokens":1}}`
			},
		},
		"verdict feedback quotes the key": {
			provider: "anthropic", status: http.StatusOK,
			reply: func(r *http.Request, nonce string) string {
				return anthropicText(`{"nonce":"`+nonce+`","reasoning":"r","verdict":"fail","feedback":"leaked `+r.Header.Get("x-api-key")+`"}`, "end_turn")
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			t.Setenv("CLI_JUDGE_KEY", key)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				nonce := ""
				if m := regexp.MustCompile(`UNTRUSTED_DIFF_([0-9a-f]{32})`).FindSubmatch(raw); m != nil {
					nonce = string(m[1])
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.reply(r, nonce))
			}))
			t.Cleanup(srv.Close)

			harnessPath := writeFakeHarness(t, createsFileHarness)
			outputDir := filepath.Join(t.TempDir(), "out")
			junitPath := filepath.Join(t.TempDir(), "junit.xml")
			var stdout strings.Builder
			run([]string{
				"run",
				"--suite", writeSuite(t, diffReviewSuiteHCL),
				"--harness", harnessPath,
				"--output", outputDir,
				"--junit", junitPath,
				"--judge-provider", tc.provider,
				"--judge-model", "m",
				"--judge-base-url", srv.URL,
				"--judge-api-key-ref", "secret://CLI_JUDGE_KEY",
			}, &stdout)

			result, err := loadResult(filepath.Join(outputDir, "result.json"))
			must.NoError(err)
			must.Len(result.Tasks, 1)
			task := result.Tasks[0]
			must.NotEqual("pass", task.Outcome, "task should not pass")
			if !tc.redactionOptional {
				must.Contains(task.JudgeVerdict.Reason+task.Error, "[redacted]", "reason or error should show the redaction")
			}
			artefacts := map[string]string{"reason": task.JudgeVerdict.Reason, "error": task.Error, "stdout": stdout.String()}
			for _, path := range []string{filepath.Join(outputDir, "result.json"), junitPath} {
				raw, err := os.ReadFile(path)
				must.NoError(err)
				artefacts[filepath.Base(path)] = string(raw)
			}
			for where, text := range artefacts {
				must.NotContains(text, key, "%s carries the API key", where)
				must.NotContains(text, key[:12], "%s carries a prefix of the API key", where)
			}
		})
	}
}

// subprocessArgsEnv carries a run() argument list to a re-executed test
// binary, for commands that exit through log.Fatal.
const subprocessArgsEnv = "EVAL_CMD_SUBPROCESS_ARGS"

func TestCmdRun_MissingJudgeKeyFailsBeforeAnyHarnessRun(t *testing.T) {
	must := require.New(t)
	if args := os.Getenv(subprocessArgsEnv); args != "" {
		os.Exit(run(strings.Split(args, "\x1f"), io.Discard))
	}
	marker := filepath.Join(t.TempDir(), "harness-ran")
	harnessPath := writeFakeHarness(t, "#!/bin/sh\ntouch "+marker+"\n")
	suite := writeSuite(t, `
suite "preflight-suite" {
  task "first" {
    prompt = "p"
    judge {
      type = "diff-review"
      criteria = "c"
    }
  }
  task "second" {
    prompt = "p"
    judge {
      type = "diff-review"
      criteria = "c"
    }
  }
}
`)
	for _, tc := range []struct {
		name  string
		extra []string
	}{
		{name: "without dry-run"},
		{name: "with dry-run", extra: []string{"--dry-run"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			args := append([]string{"run", "--suite", suite, "--harness", harnessPath, "--output", t.TempDir()}, tc.extra...)
			cmd := exec.Command(os.Args[0], "-test.run=^TestCmdRun_MissingJudgeKeyFailsBeforeAnyHarnessRun$")
			cmd.Env = append(os.Environ(), subprocessArgsEnv+"="+strings.Join(args, "\x1f"), "ANTHROPIC_API_KEY=")
			out, err := cmd.CombinedOutput()
			must.Error(err, "exited 0, want a failure\n%s", out)
			must.Contains(string(out), `task "first"`)
			must.Contains(string(out), "secret://ANTHROPIC_API_KEY")
		})
	}
	_, err := os.Stat(marker)
	must.ErrorIs(err, os.ErrNotExist, "the harness ran despite a missing judge key")
}

func TestCmdRun_DecisionJudgeProviderFailsBeforeAnyHarnessRun(t *testing.T) {
	must := require.New(t)
	if args := os.Getenv(subprocessArgsEnv); args != "" {
		os.Exit(run(strings.Split(args, "\x1f"), io.Discard))
	}
	marker := filepath.Join(t.TempDir(), "harness-ran")
	harnessPath := writeFakeHarness(t, "#!/bin/sh\ntouch "+marker+"\n")
	args := []string{
		"run", "--suite", writeSuite(t, diffReviewSuiteHCL), "--harness", harnessPath, "--output", t.TempDir(),
		"--judge-provider", "decision", "--judge-model", "jev-latest", "--judge-base-url", "http://127.0.0.1:1",
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCmdRun_DecisionJudgeProviderFailsBeforeAnyHarnessRun$")
	cmd.Env = append(os.Environ(), subprocessArgsEnv+"="+strings.Join(args, "\x1f"))
	out, err := cmd.CombinedOutput()
	must.Error(err, "exited 0, want a failure\n%s", out)
	must.Contains(string(out), `"decision" cannot be the default`)
	_, err = os.Stat(marker)
	must.ErrorIs(err, os.ErrNotExist, "the harness ran with the decision provider as the judge default")
}

func TestCmdReplay_MissingJudgeKeyFailsBeforeAnyReplay(t *testing.T) {
	must := require.New(t)
	if args := os.Getenv(subprocessArgsEnv); args != "" {
		os.Exit(run(strings.Split(args, "\x1f"), io.Discard))
	}
	lakehouse := seedRecordings(t, []string{"r1"}, []string{"success"})
	output := filepath.Join(t.TempDir(), "replay.json")
	args := []string{
		"replay", "--lakehouse", lakehouse, "--suite", writeSuite(t, diffReviewSuiteHCL),
		"--workspace", t.TempDir(), "--output", output, "--judge-api-key-ref", "secret://REPLAY_UNSET_KEY",
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCmdReplay_MissingJudgeKeyFailsBeforeAnyReplay$")
	cmd.Env = append(os.Environ(), subprocessArgsEnv+"="+strings.Join(args, "\x1f"), "REPLAY_UNSET_KEY=")
	out, err := cmd.CombinedOutput()
	must.Error(err, "exited 0, want a failure\n%s", out)
	must.Contains(string(out), `task "review"`)
	must.Contains(string(out), "secret://REPLAY_UNSET_KEY")
	_, err = os.Stat(output)
	must.ErrorIs(err, os.ErrNotExist, "replay wrote a result despite a missing judge key")
}
