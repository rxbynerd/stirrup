package runner

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rxbynerd/stirrup/eval/judge"
	"github.com/rxbynerd/stirrup/types"
)

// workspaceProbeHarness stands in for the harness: it records what the
// workspace looked like when the agent started, then plays the agent by
// creating a file.
const workspaceProbeHarness = `#!/bin/sh
CONFIG=""
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --config) CONFIG="$2"; shift 2 ;;
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -n "$CONFIG" ]; then
  TRACE=$(sed -n 's/.*"filePath":"\([^"]*\)".*/\1/p' "$CONFIG")
fi
HEAD_SUBJECT=$(git log -1 --format=%s 2>/dev/null || echo none)
echo "$(basename "$PWD")|$HEAD_SUBJECT|$(ls -A | tr '\n' ' ')" >> "$PROBE_LOG"
echo "agent output" > created.txt
[ -n "$TRACE" ] && echo '{"id":"t","turns":1,"outcome":"success"}' > "$TRACE"
exit 0
`

// judgeStub is an Anthropic-shaped endpoint that records every request body.
type judgeStub struct {
	srv *httptest.Server

	mu     sync.Mutex
	bodies []string
}

func newJudgeStub(t *testing.T, status int, reply string) *judgeStub {
	t.Helper()
	s := &judgeStub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, string(raw))
		s.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *judgeStub) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

const stubPassReply = `{"model":"stub-judge","content":[{"type":"text","text":"{\"reasoning\":\"ok\",\"verdict\":\"pass\",\"feedback\":\"looks good\"}"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`

func diffReviewJudgeFor(stub *judgeStub) types.EvalJudge {
	return types.EvalJudge{
		Type:     "diff-review",
		Criteria: "the agent created created.txt",
		LLM:      &types.JudgeLLMConfig{Model: "stub-model", BaseURL: stub.srv.URL, APIKeyRef: "secret://JUDGE_E2E_KEY"},
	}
}

// probeLog parses the harness's probe lines keyed by task ID.
func probeLog(t *testing.T, path string, taskIDs ...string) map[string][2]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading probe log: %v", err)
	}
	out := map[string][2]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			t.Fatalf("malformed probe line %q", line)
		}
		for _, id := range taskIDs {
			if strings.HasPrefix(parts[0], "evaltask-"+id+"-") {
				out[id] = [2]string{parts[1], parts[2]}
			}
		}
	}
	return out
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func TestRunSuite_GitBaselineOnlyForDiffReviewTasks(t *testing.T) {
	stub := newJudgeStub(t, 200, stubPassReply)
	t.Setenv("JUDGE_E2E_KEY", "k")
	probe := filepath.Join(t.TempDir(), "probe.log")
	t.Setenv("PROBE_LOG", probe)
	harness := writeFakeHarness(t, workspaceProbeHarness)

	upstream := t.TempDir()
	gitIn(t, upstream, "init", "-q")
	gitIn(t, upstream, "config", "user.name", "seed")
	gitIn(t, upstream, "config", "user.email", "seed@example.invalid")
	if err := os.WriteFile(filepath.Join(upstream, "upstream.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, upstream, "add", "-A")
	gitIn(t, upstream, "commit", "-q", "-m", "seed")

	seed := map[string]string{"a.txt": "seed-content\n"}
	suite := types.EvalSuite{
		ID: "baseline-suite",
		Tasks: []types.EvalTask{
			{ID: "plain", Prompt: "p", Files: seed, Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"created.txt"}}},
			{ID: "reviewed", Prompt: "p", Files: seed, Judge: diffReviewJudgeFor(stub)},
			{ID: "nested", Prompt: "p", Files: seed, Judge: types.EvalJudge{
				Type:   "composite",
				Judges: []types.EvalJudge{{Type: "file-exists", Paths: []string{"created.txt"}}, diffReviewJudgeFor(stub)},
			}},
			{ID: "empty", Prompt: "p", Judge: diffReviewJudgeFor(stub)},
			{ID: "cloned", Prompt: "p", Repo: upstream, Judge: diffReviewJudgeFor(stub)},
		},
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness, OutputDir: t.TempDir()})
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	for _, tr := range result.Tasks {
		if tr.Outcome != "pass" {
			t.Errorf("task %s: outcome %q (error %q, reason %q)", tr.TaskID, tr.Outcome, tr.Error, tr.JudgeVerdict.Reason)
		}
	}

	got := probeLog(t, probe, "plain", "reviewed", "nested", "empty", "cloned")
	if head, listing := got["plain"][0], got["plain"][1]; head != "none" || strings.Contains(listing, ".git") || !strings.Contains(listing, "a.txt") {
		t.Errorf("plain task workspace changed: head=%q listing=%q", head, listing)
	}
	for _, id := range []string{"reviewed", "nested", "empty"} {
		if head, listing := got[id][0], got[id][1]; head != "baseline" || !strings.Contains(listing, ".git") {
			t.Errorf("task %s: head=%q listing=%q, want a baseline commit", id, head, listing)
		}
	}
	if head := got["cloned"][0]; head != "seed" {
		t.Errorf("cloned task head = %q, want the upstream commit (no baseline for repo tasks)", head)
	}
}

func TestRunSuite_DiffReviewJudgesBaselineDiffNotSeededFiles(t *testing.T) {
	stub := newJudgeStub(t, 200, stubPassReply)
	t.Setenv("JUDGE_E2E_KEY", "k")
	probe := filepath.Join(t.TempDir(), "probe.log")
	t.Setenv("PROBE_LOG", probe)
	harness := writeFakeHarness(t, workspaceProbeHarness)

	suite := types.EvalSuite{
		ID:        "e2e-suite",
		RunConfig: baselineRunConfig(),
		Tasks: []types.EvalTask{
			{ID: "reviewed", Prompt: "p", Files: map[string]string{"a.txt": "seed-content\n"}, Judge: diffReviewJudgeFor(stub)},
			{ID: "plain", Prompt: "p", Files: map[string]string{"a.txt": "seed-content\n"}, Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"created.txt"}}},
		},
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness, OutputDir: t.TempDir()})
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}

	reviewed := result.Tasks[0]
	if reviewed.Outcome != "pass" || reviewed.JudgeVerdict.Status != types.JudgeStatusPass || reviewed.JudgeVerdict.Reason != "looks good" {
		t.Fatalf("reviewed task = %+v", reviewed)
	}
	rec := reviewed.JudgeVerdict.Record
	if rec == nil || rec.ServedModel != "stub-judge" || rec.RequestedModel != "stub-model" || rec.InputTokens != 5 || rec.OutputTokens != 3 || rec.InputBytes == 0 {
		t.Errorf("record = %+v", rec)
	}

	reqs := stub.requests()
	if len(reqs) != 1 {
		t.Fatalf("judge saw %d requests, want 1", len(reqs))
	}
	body := reqs[0]
	if !strings.Contains(body, "created.txt") || !strings.Contains(body, "agent output") {
		t.Errorf("judge request is missing the agent's untracked file:\n%s", body)
	}
	for _, leaked := range []string{"seed-content", "runconfig.json"} {
		if strings.Contains(body, leaked) {
			t.Errorf("judge request contains %q, which is not part of the agent's change:\n%s", leaked, body)
		}
	}

	listings := probeLog(t, probe, "reviewed", "plain")
	if strings.Contains(listings["reviewed"][1], "runconfig.json") {
		t.Errorf("diff-reviewed workspace holds runconfig.json: %q", listings["reviewed"][1])
	}
	if !strings.Contains(listings["plain"][1], "runconfig.json") {
		t.Errorf("non-diff task workspace lost runconfig.json: %q", listings["plain"][1])
	}
}

func TestRunSuite_JudgeErrorKeepsRecordAndIsNotAFail(t *testing.T) {
	stub := newJudgeStub(t, 503, `{"error":"overloaded"}`)
	t.Setenv("JUDGE_E2E_KEY", "k")
	t.Setenv("PROBE_LOG", filepath.Join(t.TempDir(), "probe.log"))
	harness := writeFakeHarness(t, workspaceProbeHarness)

	suite := types.EvalSuite{
		ID:    "err-suite",
		Tasks: []types.EvalTask{{ID: "reviewed", Prompt: "p", Judge: diffReviewJudgeFor(stub)}},
	}
	result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness, OutputDir: t.TempDir()})
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}

	tr := result.Tasks[0]
	if tr.Outcome != "error" {
		t.Errorf("outcome = %q, want error: an unreachable judge is not a failed task", tr.Outcome)
	}
	if !strings.Contains(tr.Error, "HTTP 503") {
		t.Errorf("error = %q", tr.Error)
	}
	if tr.JudgeVerdict.Status != types.JudgeStatusError || tr.JudgeVerdict.Passed {
		t.Errorf("verdict = %+v", tr.JudgeVerdict)
	}
	if tr.JudgeVerdict.Record == nil || len(tr.JudgeVerdict.Record.ConfigHash) != 64 {
		t.Errorf("error verdict lost its record: %+v", tr.JudgeVerdict.Record)
	}
}

func TestRunSuite_JudgeDefaultsFlowToDiffReviewJudges(t *testing.T) {
	stub := newJudgeStub(t, 200, stubPassReply)
	t.Setenv("JUDGE_E2E_KEY", "k")
	t.Setenv("PROBE_LOG", filepath.Join(t.TempDir(), "probe.log"))
	harness := writeFakeHarness(t, workspaceProbeHarness)

	suite := types.EvalSuite{
		ID: "defaults-suite",
		Tasks: []types.EvalTask{
			{ID: "implicit", Prompt: "p", Judge: types.EvalJudge{Type: "diff-review", Criteria: "c"}},
			{ID: "explicit", Prompt: "p", Judge: diffReviewJudgeFor(stub)},
		},
	}
	result, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: harness,
		OutputDir:   t.TempDir(),
		JudgeOptions: judge.Options{LLMDefaults: &types.JudgeLLMConfig{
			Model: "cli-model", BaseURL: stub.srv.URL, APIKeyRef: "secret://JUDGE_E2E_KEY",
		}},
	})
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	for _, tr := range result.Tasks {
		if tr.Outcome != "pass" {
			t.Errorf("task %s: outcome %q (%s)", tr.TaskID, tr.Outcome, tr.Error)
		}
	}

	var models []string
	for _, body := range stub.requests() {
		switch {
		case strings.Contains(body, `"model":"cli-model"`):
			models = append(models, "cli-model")
		case strings.Contains(body, `"model":"stub-model"`):
			models = append(models, "stub-model")
		}
	}
	if len(models) != 2 || models[0] != "cli-model" || models[1] != "stub-model" {
		t.Errorf("models requested = %v, want the CLI default for the block-less judge and the explicit block's model for the other", models)
	}
}

func TestReplayRecordingForwardsJudgeOptions(t *testing.T) {
	stub := newJudgeStub(t, 200, stubPassReply)
	t.Setenv("JUDGE_E2E_KEY", "k")

	workspace := t.TempDir()
	gitIn(t, workspace, "init", "-q")
	gitIn(t, workspace, "config", "user.name", "t")
	gitIn(t, workspace, "config", "user.email", "t@example.invalid")
	gitIn(t, workspace, "commit", "-q", "--allow-empty", "-m", "baseline")
	if err := os.WriteFile(filepath.Join(workspace, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	task := types.EvalTask{ID: "r", Judge: types.EvalJudge{Type: "diff-review", Criteria: "c"}}
	result, err := ReplayRecording(context.Background(), types.RunRecording{RunID: "r1"}, task, workspace, judge.Options{
		LLMDefaults: &types.JudgeLLMConfig{Model: "replay-model", BaseURL: stub.srv.URL, APIKeyRef: "secret://JUDGE_E2E_KEY"},
	})
	if err != nil {
		t.Fatalf("ReplayRecording: %v", err)
	}
	if result.Outcome != "pass" || result.JudgeVerdict.Record == nil || result.JudgeVerdict.Record.RequestedModel != "replay-model" {
		t.Errorf("result = %+v", result)
	}
}

func TestReplayRecordingJudgeErrorReturnsErrorOutcomeWithRecord(t *testing.T) {
	stub := newJudgeStub(t, 503, `{"error":"overloaded"}`)
	t.Setenv("JUDGE_E2E_KEY", "k")

	workspace := t.TempDir()
	gitIn(t, workspace, "init", "-q")
	gitIn(t, workspace, "config", "user.name", "t")
	gitIn(t, workspace, "config", "user.email", "t@example.invalid")
	gitIn(t, workspace, "commit", "-q", "--allow-empty", "-m", "baseline")

	task := types.EvalTask{ID: "r", Judge: diffReviewJudgeFor(stub)}
	result, err := ReplayRecording(context.Background(), types.RunRecording{RunID: "r1"}, task, workspace, judge.Options{})
	if err == nil {
		t.Fatal("expected the judge error to be returned")
	}
	if result.TaskID != "r" || result.Outcome != "error" || result.JudgeVerdict.Status != types.JudgeStatusError {
		t.Errorf("result = %+v", result)
	}
	if result.JudgeVerdict.Record == nil {
		t.Error("error verdict lost its record")
	}
}
