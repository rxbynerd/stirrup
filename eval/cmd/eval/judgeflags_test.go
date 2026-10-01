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

	"github.com/rxbynerd/stirrup/types"
)

func parseJudgeFlags(t *testing.T, args ...string) *judgeFlags {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jf := addJudgeFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return jf
}

func TestJudgeFlags_Options(t *testing.T) {
	t.Run("unset leaves the built-in default", func(t *testing.T) {
		opts, err := parseJudgeFlags(t).options()
		if err != nil {
			t.Fatal(err)
		}
		if opts.LLMDefaults != nil {
			t.Errorf("LLMDefaults = %+v, want nil", opts.LLMDefaults)
		}
	})

	t.Run("set flags become defaults", func(t *testing.T) {
		opts, err := parseJudgeFlags(t,
			"--judge-provider", "openai-compatible",
			"--judge-model", "m",
			"--judge-base-url", "https://gateway.example/v1",
			"--judge-api-key-ref", "secret://GATEWAY_KEY",
		).options()
		if err != nil {
			t.Fatal(err)
		}
		want := types.JudgeLLMConfig{Provider: "openai-compatible", Model: "m", BaseURL: "https://gateway.example/v1", APIKeyRef: "secret://GATEWAY_KEY"}
		if opts.LLMDefaults == nil || *opts.LLMDefaults != want {
			t.Errorf("LLMDefaults = %+v, want %+v", opts.LLMDefaults, want)
		}
	})

	t.Run("model alone is accepted for the default provider", func(t *testing.T) {
		if _, err := parseJudgeFlags(t, "--judge-model", "claude-sonnet-5-5").options(); err != nil {
			t.Fatal(err)
		}
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
			_, err := parseJudgeFlags(t, tc.args...).options()
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), "--judge-*") || !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Errorf("error = %q, want it to name the flags and mention %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "sk-live-abc123") {
				t.Errorf("error echoes the literal key: %q", err)
			}
		})
	}
}

func TestCompletion_JudgeFlagsRegistered(t *testing.T) {
	for _, sub := range []string{"run", "replay"} {
		have := map[string]bool{}
		for _, f := range evalCompletionFlags[sub] {
			have[f] = true
		}
		for _, f := range []string{"judge-provider", "judge-model", "judge-base-url", "judge-api-key-ref"} {
			if !have[f] {
				t.Errorf("%s completion is missing -%s", sub, f)
			}
		}
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
	path := filepath.Join(t.TempDir(), "suite.hcl")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
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
	if exitCode != 0 {
		t.Fatalf("run() exit code = %d, want 0", exitCode)
	}

	result, err := loadResult(filepath.Join(outputDir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(result.Tasks))
	}
	task := result.Tasks[0]
	if task.Outcome != "pass" || task.JudgeVerdict.Status != types.JudgeStatusPass {
		t.Fatalf("task = %+v", task)
	}
	rec := task.JudgeVerdict.Record
	if rec == nil || rec.RequestedModel != "cli-judge-model" || rec.ServedModel != "served-model" {
		t.Errorf("record = %+v", rec)
	}

	bodies, keys := endpoint.seen()
	if len(bodies) != 1 {
		t.Fatalf("judge saw %d requests, want 1", len(bodies))
	}
	if keys[0] != "cli-secret" {
		t.Errorf("x-api-key = %q, want the resolved secret", keys[0])
	}
	if !strings.Contains(bodies[0], "created.txt") || !strings.Contains(bodies[0], "agent output") {
		t.Errorf("judge request is missing the agent's change:\n%s", bodies[0])
	}
	raw, err := os.ReadFile(filepath.Join(outputDir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "cli-secret") {
		t.Error("result.json contains the resolved API key")
	}
}

func TestCmdReplay_JudgeFlagsConfigureDiffReviewJudge(t *testing.T) {
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
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, "created.txt"), []byte("agent output\n"), 0o644); err != nil {
		t.Fatal(err)
	}

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
	if exitCode != 0 {
		t.Fatalf("run() exit code = %d, want 0", exitCode)
	}

	result, err := loadResult(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tasks) != 1 || result.Tasks[0].Outcome != "pass" {
		t.Fatalf("tasks = %+v", result.Tasks)
	}
	if rec := result.Tasks[0].JudgeVerdict.Record; rec == nil || rec.RequestedModel != "replay-judge-model" {
		t.Errorf("record = %+v", rec)
	}
}

func TestCmdRun_JudgeOutputNeverCarriesTheAPIKey(t *testing.T) {
	const key = "sk-test-0123456789abcdef"
	anthropicText := func(text, stop string) string {
		quoted, _ := json.Marshal(text)
		return `{"model":"m","content":[{"type":"text","text":` + string(quoted) + `}],"stop_reason":"` + stop + `","usage":{"input_tokens":1,"output_tokens":1}}`
	}
	cases := map[string]struct {
		provider string
		status   int
		reply    func(r *http.Request, nonce string) string
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
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Tasks) != 1 {
				t.Fatalf("got %d tasks, want 1", len(result.Tasks))
			}
			task := result.Tasks[0]
			if task.Outcome == "pass" {
				t.Fatalf("task passed: %+v", task)
			}
			if !strings.Contains(task.JudgeVerdict.Reason+task.Error, "[redacted]") && name != "refusal quotes the key" {
				t.Errorf("reason %q / error %q does not show the redaction", task.JudgeVerdict.Reason, task.Error)
			}
			artefacts := map[string]string{"reason": task.JudgeVerdict.Reason, "error": task.Error, "stdout": stdout.String()}
			for _, path := range []string{filepath.Join(outputDir, "result.json"), junitPath} {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				artefacts[filepath.Base(path)] = string(raw)
			}
			for where, text := range artefacts {
				if strings.Contains(text, key) || strings.Contains(text, key[:12]) {
					t.Errorf("%s carries the API key:\n%s", where, text)
				}
			}
		})
	}
}

// subprocessArgsEnv carries a run() argument list to a re-executed test
// binary, for commands that exit through log.Fatal.
const subprocessArgsEnv = "EVAL_CMD_SUBPROCESS_ARGS"

func TestCmdRun_MissingJudgeKeyFailsBeforeAnyHarnessRun(t *testing.T) {
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
	for _, extra := range [][]string{nil, {"--dry-run"}} {
		args := append([]string{"run", "--suite", suite, "--harness", harnessPath, "--output", t.TempDir()}, extra...)
		cmd := exec.Command(os.Args[0], "-test.run=^TestCmdRun_MissingJudgeKeyFailsBeforeAnyHarnessRun$")
		cmd.Env = append(os.Environ(), subprocessArgsEnv+"="+strings.Join(args, "\x1f"), "ANTHROPIC_API_KEY=")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("%v: exited 0, want a failure\n%s", extra, out)
		}
		for _, want := range []string{`task "first"`, "secret://ANTHROPIC_API_KEY"} {
			if !strings.Contains(string(out), want) {
				t.Errorf("%v: output does not contain %q:\n%s", extra, want, out)
			}
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("the harness ran despite a missing judge key")
	}
}

func TestCmdReplay_MissingJudgeKeyFailsBeforeAnyReplay(t *testing.T) {
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
	if err == nil {
		t.Fatalf("exited 0, want a failure\n%s", out)
	}
	for _, want := range []string{`task "review"`, "secret://REPLAY_UNSET_KEY"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Error("replay wrote a result despite a missing judge key")
	}
}
