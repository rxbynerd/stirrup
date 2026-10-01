package runner

import (
	"context"
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
		_, _ = io.WriteString(w, echoFenceNonce(string(raw), reply))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *judgeStub) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

// requestNoncePattern finds the diff-review fence nonce in a request body;
// it skips the angle brackets, which JSON encoding escapes.
var requestNoncePattern = regexp.MustCompile(`UNTRUSTED_DIFF_([0-9a-f]{32})`)

// echoFenceNonce replaces @NONCE@ in reply with the fence nonce of the
// request, as a compliant judge model would.
func echoFenceNonce(body, reply string) string {
	m := requestNoncePattern.FindStringSubmatch(body)
	if m == nil {
		return reply
	}
	return strings.ReplaceAll(reply, "@NONCE@", m[1])
}

const stubPassReply = `{"model":"stub-judge","content":[{"type":"text","text":"{\"nonce\":\"@NONCE@\",\"reasoning\":\"ok\",\"verdict\":\"pass\",\"feedback\":\"looks good\"}"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`

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

// isolateGit skips the test without git or sh, keeps host git
// configuration out of the test's own git calls, and points temp dirs at a
// private directory so leaked judge dirs can be counted.
func isolateGit(t *testing.T) string {
	t.Helper()
	for _, bin := range []string{"git", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	return tmp
}

// judgeDirs lists judge-owned git dirs left in tmp.
func judgeDirs(t *testing.T, tmp string) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(tmp, "evaljudge-*"))
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// upstreamRepo is a one-commit repository for `repo` tasks to clone.
func upstreamRepo(t *testing.T) string {
	t.Helper()
	upstream := t.TempDir()
	gitIn(t, upstream, "init", "-q")
	gitIn(t, upstream, "config", "user.name", "seed")
	gitIn(t, upstream, "config", "user.email", "seed@example.invalid")
	if err := os.WriteFile(filepath.Join(upstream, "upstream.txt"), []byte("hello upstream\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, upstream, "add", "-A")
	gitIn(t, upstream, "commit", "-q", "-m", "seed")
	return upstream
}

func TestRunSuite_DiffReviewWorkspacesHaveNoBaselineGitDir(t *testing.T) {
	tmp := isolateGit(t)
	stub := newJudgeStub(t, 200, stubPassReply)
	t.Setenv("JUDGE_E2E_KEY", "k")
	probe := filepath.Join(t.TempDir(), "probe.log")
	t.Setenv("PROBE_LOG", probe)
	harness := writeFakeHarness(t, workspaceProbeHarness)
	upstream := upstreamRepo(t)

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
	if n := len(stub.requests()); n != 4 {
		t.Errorf("judge saw %d requests, want one per diff-review task (4)", n)
	}

	got := probeLog(t, probe, "plain", "reviewed", "nested", "empty", "cloned")
	for _, id := range []string{"plain", "reviewed", "nested", "empty"} {
		if head, listing := got[id][0], got[id][1]; head != "none" || strings.Contains(listing, ".git") {
			t.Errorf("task %s: head=%q listing=%q, want no git repository in the workspace", id, head, listing)
		}
	}
	if head := got["cloned"][0]; head != "seed" {
		t.Errorf("cloned task head = %q, want the upstream commit untouched", head)
	}
	if left := judgeDirs(t, tmp); len(left) != 0 {
		t.Errorf("judge git dirs left behind: %v", left)
	}
}

func TestRunSuite_RepoTaskDiffHoldsOnlyTheAgentChange(t *testing.T) {
	isolateGit(t)
	stub := newJudgeStub(t, 200, stubPassReply)
	t.Setenv("JUDGE_E2E_KEY", "k")
	t.Setenv("PROBE_LOG", filepath.Join(t.TempDir(), "probe.log"))
	harness := writeFakeHarness(t, workspaceProbeHarness)

	suite := types.EvalSuite{
		ID: "repo-files-suite",
		Tasks: []types.EvalTask{{
			ID: "cloned", Prompt: "p", Repo: upstreamRepo(t),
			Files: map[string]string{"seed.txt": "seed-content\n", "upstream.txt": "seed override\n"},
			Judge: diffReviewJudgeFor(stub),
		}},
	}
	result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness})
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	if tr := result.Tasks[0]; tr.Outcome != "pass" || tr.JudgeVerdict.Record == nil || tr.JudgeVerdict.Record.BaselineSource != types.JudgeBaselineRunner {
		t.Fatalf("task = %+v", tr)
	}
	reqs := stub.requests()
	if len(reqs) != 1 {
		t.Fatalf("judge saw %d requests, want 1", len(reqs))
	}
	if !strings.Contains(reqs[0], "created.txt") || !strings.Contains(reqs[0], "agent output") {
		t.Errorf("judge request is missing the agent's change:\n%s", reqs[0])
	}
	for _, leaked := range []string{"seed-content", "seed override", "hello upstream"} {
		if strings.Contains(reqs[0], leaked) {
			t.Errorf("judge request attributes %q, which the agent did not write, to the agent:\n%s", leaked, reqs[0])
		}
	}
}

func TestRunSuite_JudgeDirRemovedOnEveryPath(t *testing.T) {
	cases := map[string]struct {
		status  int
		reply   string
		harness string
		outcome string
	}{
		"success":         {status: 200, reply: stubPassReply, harness: workspaceProbeHarness, outcome: "pass"},
		"judge error":     {status: 503, reply: `{"error":"overloaded"}`, harness: workspaceProbeHarness, outcome: "error"},
		"harness failure": {status: 200, reply: stubPassReply, harness: "#!/bin/sh\nexit 3\n", outcome: "error"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tmp := isolateGit(t)
			stub := newJudgeStub(t, tc.status, tc.reply)
			t.Setenv("JUDGE_E2E_KEY", "k")
			t.Setenv("PROBE_LOG", filepath.Join(t.TempDir(), "probe.log"))
			harness := writeFakeHarness(t, tc.harness)

			suite := types.EvalSuite{ID: "cleanup-suite", Tasks: []types.EvalTask{{ID: "reviewed", Prompt: "p", Judge: diffReviewJudgeFor(stub)}}}
			result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness})
			if err != nil {
				t.Fatalf("RunSuite: %v", err)
			}
			if got := result.Tasks[0].Outcome; got != tc.outcome {
				t.Errorf("outcome = %q, want %q (%s)", got, tc.outcome, result.Tasks[0].Error)
			}
			if left := judgeDirs(t, tmp); len(left) != 0 {
				t.Errorf("judge git dirs left behind: %v", left)
			}
		})
	}
}

func TestRunSuite_RetainsJudgeBaselineForReplay(t *testing.T) {
	isolateGit(t)
	stub := newJudgeStub(t, 200, stubPassReply)
	t.Setenv("JUDGE_E2E_KEY", "k")
	t.Setenv("PROBE_LOG", filepath.Join(t.TempDir(), "probe.log"))
	harness := writeFakeHarness(t, workspaceProbeHarness)
	out := t.TempDir()

	seed := map[string]string{"a.txt": "seed-content\n"}
	task := types.EvalTask{ID: "reviewed", Prompt: "p", Files: seed, Judge: diffReviewJudgeFor(stub)}
	if _, err := RunSuite(context.Background(), types.EvalSuite{ID: "retain-suite", Tasks: []types.EvalTask{task}}, RunConfig{HarnessPath: harness, OutputDir: out}); err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	sidecar := filepath.Join(out, "retain-suite", "reviewed", judge.BaselineSidecarName)
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("baseline sidecar not retained: %v", err)
	}

	workspace := t.TempDir()
	for name, content := range map[string]string{"a.txt": "seed-content\n", "created.txt": "replayed change\n"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	result, err := ReplayRecording(context.Background(), types.RunRecording{RunID: "r1"}, task, workspace, judge.Options{}, sidecar)
	if err != nil {
		t.Fatalf("ReplayRecording: %v", err)
	}
	if result.Outcome != "pass" || result.JudgeVerdict.Record == nil || result.JudgeVerdict.Record.BaselineSource != types.JudgeBaselineSidecar {
		t.Errorf("result = %+v", result)
	}
	reqs := stub.requests()
	last := reqs[len(reqs)-1]
	if !strings.Contains(last, "replayed change") || strings.Contains(last, "seed-content") {
		t.Errorf("replay did not diff against the retained baseline:\n%s", last)
	}
}

func TestRunSuite_DiffReviewJudgesBaselineDiffNotSeededFiles(t *testing.T) {
	isolateGit(t)
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
	isolateGit(t)
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
	isolateGit(t)
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
	isolateGit(t)
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
	}, "")
	if err != nil {
		t.Fatalf("ReplayRecording: %v", err)
	}
	if result.Outcome != "pass" || result.JudgeVerdict.Record == nil || result.JudgeVerdict.Record.RequestedModel != "replay-model" ||
		result.JudgeVerdict.Record.BaselineSource != types.JudgeBaselineWorkspaceHead {
		t.Errorf("result = %+v", result)
	}
}

func TestReplayRecordingJudgeErrorReturnsErrorOutcomeWithRecord(t *testing.T) {
	isolateGit(t)
	stub := newJudgeStub(t, 503, `{"error":"overloaded"}`)
	t.Setenv("JUDGE_E2E_KEY", "k")

	workspace := t.TempDir()
	gitIn(t, workspace, "init", "-q")
	gitIn(t, workspace, "config", "user.name", "t")
	gitIn(t, workspace, "config", "user.email", "t@example.invalid")
	gitIn(t, workspace, "commit", "-q", "--allow-empty", "-m", "baseline")

	task := types.EvalTask{ID: "r", Judge: diffReviewJudgeFor(stub)}
	result, err := ReplayRecording(context.Background(), types.RunRecording{RunID: "r1"}, task, workspace, judge.Options{}, "")
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
