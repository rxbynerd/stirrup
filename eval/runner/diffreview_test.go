package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

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
	must := require.New(t)
	data, err := os.ReadFile(path)
	must.NoError(err, "reading probe log")
	out := map[string][2]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		parts := strings.SplitN(line, "|", 3)
		must.Len(parts, 3, "malformed probe line %q", line)
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
	must := require.New(t)
	for _, bin := range []string{"git", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	must.NoError(err)
	t.Setenv("TMPDIR", tmp)
	return tmp
}

// judgeDirs lists judge-owned git dirs left in tmp.
func judgeDirs(t *testing.T, tmp string) []string {
	t.Helper()
	must := require.New(t)
	dirs, err := filepath.Glob(filepath.Join(tmp, "evaljudge-*"))
	must.NoError(err)
	return dirs
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	must := require.New(t)
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	must.NoError(err, "git %s\n%s", strings.Join(args, " "), out)
}

// upstreamRepo is a one-commit repository for `repo` tasks to clone.
func upstreamRepo(t *testing.T) string {
	t.Helper()
	must := require.New(t)
	upstream := t.TempDir()
	gitIn(t, upstream, "init", "-q")
	gitIn(t, upstream, "config", "user.name", "seed")
	gitIn(t, upstream, "config", "user.email", "seed@example.invalid")
	must.NoError(os.WriteFile(filepath.Join(upstream, "upstream.txt"), []byte("hello upstream\n"), 0o644))
	gitIn(t, upstream, "add", "-A")
	gitIn(t, upstream, "commit", "-q", "-m", "seed")
	return upstream
}

func TestRunSuite_DiffReviewWorkspacesHaveNoBaselineGitDir(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	for _, tr := range result.Tasks {
		t.Run(tr.TaskID, func(t *testing.T) {
			must := require.New(t)
			must.Equal("pass", tr.Outcome, "task error %q, reason %q", tr.Error, tr.JudgeVerdict.Reason)
		})
	}

	got := probeLog(t, probe, "plain", "reviewed", "nested", "empty", "cloned")
	for _, id := range []string{"plain", "reviewed", "nested", "empty"} {
		t.Run(id+" workspace", func(t *testing.T) {
			must := require.New(t)
			must.Equal("none", got[id][0], "workspace should not be a git repository")
			must.NotContains(got[id][1], ".git", "workspace listing should not show a git directory")
		})
	}
	must.Equal("seed", got["cloned"][0], "cloned task should keep the upstream commit")
	must.Len(stub.requests(), 4, "judge should see one request per diff-review task")
	must.Empty(judgeDirs(t, tmp), "judge git dirs left behind")
}

func TestRunSuite_RepoTaskDiffHoldsOnlyTheAgentChange(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	tr := result.Tasks[0]
	must.Equal("pass", tr.Outcome, "task error %q", tr.Error)
	must.NotNil(tr.JudgeVerdict.Record)
	must.Equal(types.JudgeBaselineRunner, tr.JudgeVerdict.Record.BaselineSource)

	reqs := stub.requests()
	must.Len(reqs, 1)
	must.Contains(reqs[0], "created.txt", "judge request is missing the agent's change")
	must.Contains(reqs[0], "agent output", "judge request is missing the agent's change")
	for _, leaked := range []string{"seed-content", "seed override", "hello upstream"} {
		t.Run("excludes "+leaked, func(t *testing.T) {
			must := require.New(t)
			must.NotContains(reqs[0], leaked, "judge request attributes content the agent did not write")
		})
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
		"judge error":     {status: 400, reply: `{"error":"bad request"}`, harness: workspaceProbeHarness, outcome: "error"},
		"harness failure": {status: 200, reply: stubPassReply, harness: "#!/bin/sh\nexit 3\n", outcome: "error"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			tmp := isolateGit(t)
			stub := newJudgeStub(t, tc.status, tc.reply)
			t.Setenv("JUDGE_E2E_KEY", "k")
			t.Setenv("PROBE_LOG", filepath.Join(t.TempDir(), "probe.log"))
			harness := writeFakeHarness(t, tc.harness)

			suite := types.EvalSuite{ID: "cleanup-suite", Tasks: []types.EvalTask{{ID: "reviewed", Prompt: "p", Judge: diffReviewJudgeFor(stub)}}}
			result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness})
			must.NoError(err)
			tr := result.Tasks[0]
			must.Equal(tc.outcome, tr.Outcome, "task error %q", tr.Error)
			must.Empty(judgeDirs(t, tmp), "judge git dirs left behind")
		})
	}
}

func TestRunSuite_RetainsJudgeBaselineForReplay(t *testing.T) {
	must := require.New(t)
	isolateGit(t)
	stub := newJudgeStub(t, 200, stubPassReply)
	t.Setenv("JUDGE_E2E_KEY", "k")
	t.Setenv("PROBE_LOG", filepath.Join(t.TempDir(), "probe.log"))
	harness := writeFakeHarness(t, workspaceProbeHarness)
	out := t.TempDir()

	seed := map[string]string{"a.txt": "seed-content\n"}
	task := types.EvalTask{ID: "reviewed", Prompt: "p", Files: seed, Judge: diffReviewJudgeFor(stub)}
	_, err := RunSuite(context.Background(), types.EvalSuite{ID: "retain-suite", Tasks: []types.EvalTask{task}}, RunConfig{HarnessPath: harness, OutputDir: out})
	must.NoError(err)
	sidecar := filepath.Join(out, "retain-suite", "reviewed", judge.BaselineSidecarName)
	_, err = os.Stat(sidecar)
	must.NoError(err, "baseline sidecar not retained")

	workspace := t.TempDir()
	for name, content := range map[string]string{"a.txt": "seed-content\n", "created.txt": "replayed change\n"} {
		must.NoError(os.WriteFile(filepath.Join(workspace, name), []byte(content), 0o644))
	}
	result, err := ReplayRecording(context.Background(), types.RunRecording{RunID: "r1"}, task, workspace, judge.Options{}, sidecar)
	must.NoError(err)
	must.Equal("pass", result.Outcome, "result = %+v", result)
	must.NotNil(result.JudgeVerdict.Record)
	must.Equal(types.JudgeBaselineSidecar, result.JudgeVerdict.Record.BaselineSource)

	reqs := stub.requests()
	last := reqs[len(reqs)-1]
	must.Contains(last, "replayed change", "replay did not diff against the retained baseline")
	must.NotContains(last, "seed-content", "replay diff should not include the retained baseline")
}

func TestRunSuite_DiffReviewJudgesBaselineDiffNotSeededFiles(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)

	reviewed := result.Tasks[0]
	must.Equal("pass", reviewed.Outcome, "reviewed task = %+v", reviewed)
	must.Equal(types.JudgeStatusPass, reviewed.JudgeVerdict.Status)
	must.Equal("looks good", reviewed.JudgeVerdict.Reason)

	rec := reviewed.JudgeVerdict.Record
	must.NotNil(rec, "reviewed task has no judge record")
	must.Equal("stub-judge", rec.ServedModel)
	must.Equal("stub-model", rec.RequestedModel)
	must.Equal(5, rec.InputTokens)
	must.Equal(3, rec.OutputTokens)
	must.NotZero(rec.InputBytes)

	reqs := stub.requests()
	must.Len(reqs, 1)
	body := reqs[0]
	must.Contains(body, "created.txt", "judge request is missing the agent's untracked file")
	must.Contains(body, "agent output", "judge request is missing the agent's untracked file")
	for _, leaked := range []string{"seed-content", "runconfig.json"} {
		t.Run("excludes "+leaked, func(t *testing.T) {
			must := require.New(t)
			must.NotContains(body, leaked, "judge request contains content outside the agent's change")
		})
	}

	listings := probeLog(t, probe, "reviewed", "plain")
	must.NotContains(listings["reviewed"][1], "runconfig.json", "diff-reviewed workspace should not hold runconfig.json")
	must.Contains(listings["plain"][1], "runconfig.json", "non-diff task workspace should keep runconfig.json")
}

func TestRunSuite_JudgeErrorKeepsRecordAndIsNotAFail(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)

	tr := result.Tasks[0]
	must.Equal("error", tr.Outcome, "an unreachable judge is not a failed task")
	must.Contains(tr.Error, "HTTP 503")
	must.Contains(tr.Error, "after 3 attempts")
	must.Len(stub.requests(), 3, "judge should be called once per attempt")
	must.Equal(types.JudgeStatusError, tr.JudgeVerdict.Status)
	must.False(tr.JudgeVerdict.Passed)
	must.NotNil(tr.JudgeVerdict.Record, "error verdict lost its record")
	must.Len(tr.JudgeVerdict.Record.ConfigHash, 64)
}

func TestRunSuite_JudgeDefaultsFlowToDiffReviewJudges(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	for _, tr := range result.Tasks {
		t.Run(tr.TaskID, func(t *testing.T) {
			must := require.New(t)
			must.Equal("pass", tr.Outcome, "task error %q", tr.Error)
		})
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
	must.Equal([]string{"cli-model", "stub-model"}, models,
		"the block-less judge should use the CLI default model and the explicit block its own model")
}

func TestReplayRecording_ForwardsJudgeOptions(t *testing.T) {
	must := require.New(t)
	isolateGit(t)
	stub := newJudgeStub(t, 200, stubPassReply)
	t.Setenv("JUDGE_E2E_KEY", "k")

	workspace := t.TempDir()
	gitIn(t, workspace, "init", "-q")
	gitIn(t, workspace, "config", "user.name", "t")
	gitIn(t, workspace, "config", "user.email", "t@example.invalid")
	gitIn(t, workspace, "commit", "-q", "--allow-empty", "-m", "baseline")
	must.NoError(os.WriteFile(filepath.Join(workspace, "new.txt"), []byte("x"), 0o644))

	task := types.EvalTask{ID: "r", Judge: types.EvalJudge{Type: "diff-review", Criteria: "c"}}
	result, err := ReplayRecording(context.Background(), types.RunRecording{RunID: "r1"}, task, workspace, judge.Options{
		LLMDefaults: &types.JudgeLLMConfig{Model: "replay-model", BaseURL: stub.srv.URL, APIKeyRef: "secret://JUDGE_E2E_KEY"},
	}, "")
	must.NoError(err)
	must.Equal("pass", result.Outcome, "result = %+v", result)
	must.NotNil(result.JudgeVerdict.Record)
	must.Equal("replay-model", result.JudgeVerdict.Record.RequestedModel)
	must.Equal(types.JudgeBaselineWorkspaceHead, result.JudgeVerdict.Record.BaselineSource)
}

func TestReplayRecording_JudgeErrorReturnsErrorOutcomeWithRecord(t *testing.T) {
	must := require.New(t)
	isolateGit(t)
	stub := newJudgeStub(t, 400, `{"error":"bad request"}`)
	t.Setenv("JUDGE_E2E_KEY", "k")

	workspace := t.TempDir()
	gitIn(t, workspace, "init", "-q")
	gitIn(t, workspace, "config", "user.name", "t")
	gitIn(t, workspace, "config", "user.email", "t@example.invalid")
	gitIn(t, workspace, "commit", "-q", "--allow-empty", "-m", "baseline")

	task := types.EvalTask{ID: "r", Judge: diffReviewJudgeFor(stub)}
	result, err := ReplayRecording(context.Background(), types.RunRecording{RunID: "r1"}, task, workspace, judge.Options{}, "")
	must.Error(err, "expected the judge error to be returned")
	must.Equal("r", result.TaskID)
	must.Equal("error", result.Outcome, "result = %+v", result)
	must.Equal(types.JudgeStatusError, result.JudgeVerdict.Status)
	must.NotNil(result.JudgeVerdict.Record, "error verdict lost its record")
}

func TestReplayRecording_CompositeErrorReturnsErrorOutcomeNotError(t *testing.T) {
	must := require.New(t)
	workspace := t.TempDir()
	must.NoError(os.Mkdir(filepath.Join(workspace, "dir"), 0o755))
	task := types.EvalTask{ID: "c", Judge: types.EvalJudge{Type: "composite", Require: "all", Judges: []types.EvalJudge{
		{Type: "file-contains", Path: "dir", Pattern: "x"},
		{Type: "file-exists", Paths: []string{"dir"}},
	}}}

	result, err := ReplayRecording(context.Background(), types.RunRecording{RunID: "r1"}, task, workspace, judge.Options{}, "")
	must.NoError(err)

	must.Equal("error", result.Outcome, "result = %+v, want the error outcome", result)
	must.Equal(types.JudgeStatusError, result.JudgeVerdict.Status)
	must.False(result.JudgeVerdict.Passed)
	must.True(strings.HasPrefix(result.Error, "sub-judge 1 of 2 (file-contains) errored (require all); 1 skipped: "),
		"Error = %q, want the composite reason", result.Error)
	must.Len(result.JudgeVerdict.Details, 2, "want the evaluated and skipped sub-judges kept")
	must.Equal("skipped", result.JudgeVerdict.Details[1].Status)
}

func TestRunSuite_HarnessFailureResults(t *testing.T) {
	t.Run("no trace", func(t *testing.T) {
		must := require.New(t)
		isolateGit(t)
		stub := newJudgeStub(t, 200, stubPassReply)
		t.Setenv("JUDGE_E2E_KEY", "k")
		harness := writeFakeHarness(t, "#!/bin/sh\nexit 3\n")
		suite := types.EvalSuite{ID: "harness-fail", Tasks: []types.EvalTask{{ID: "t", Prompt: "p", Judge: diffReviewJudgeFor(stub)}}}

		result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness})
		must.NoError(err)
		tr := result.Tasks[0]
		must.Equal("error", tr.Outcome, "task = %+v", tr)
		must.Equal(types.JudgeStatusError, tr.JudgeVerdict.Status)
		must.Contains(tr.Error, "harness failed")
		must.Empty(stub.requests(), "judge should not be called without a trace")
	})

	t.Run("trace left and judge errors", func(t *testing.T) {
		must := require.New(t)
		isolateGit(t)
		stub := newJudgeStub(t, http.StatusBadRequest, `{"type":"error","error":{"message":"bad request"}}`)
		t.Setenv("JUDGE_E2E_KEY", "k")
		harness := writeFakeHarness(t, `#!/bin/sh
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
echo "agent output" > created.txt
echo '{"id":"t","turns":1,"outcome":"error"}' > "$TRACE"
exit 3
`)
		suite := types.EvalSuite{ID: "harness-fail", Tasks: []types.EvalTask{{ID: "t", Prompt: "p", Judge: diffReviewJudgeFor(stub)}}}

		result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness})
		must.NoError(err)
		tr := result.Tasks[0]
		must.Equal("error", tr.Outcome, "task = %+v", tr)
		must.Contains(tr.Error, "judge failed after harness error")
		must.NotNil(tr.Trace, "the harness trace should be kept")
		must.Equal("t", tr.Trace.ID)
		must.Equal(types.JudgeStatusError, tr.JudgeVerdict.Status, "want the diff-review error verdict with its record")
		must.NotNil(tr.JudgeVerdict.Record)
		must.Equal(types.JudgeKindDiffReview, tr.JudgeVerdict.Record.Kind)
		must.Len(stub.requests(), 1, "judge should be called once")
	})
}

func TestRunSuite_PreflightFailsBeforeAnyHarnessRun(t *testing.T) {
	must := require.New(t)
	isolateGit(t)
	t.Setenv("JUDGE_E2E_KEY", "")
	marker := filepath.Join(t.TempDir(), "harness-ran")
	harness := writeFakeHarness(t, "#!/bin/sh\ntouch "+marker+"\n")
	stub := newJudgeStub(t, 200, stubPassReply)
	suite := types.EvalSuite{ID: "preflight", Tasks: []types.EvalTask{
		{ID: "first", Prompt: "p", Judge: diffReviewJudgeFor(stub)},
		{ID: "second", Prompt: "p", Judge: diffReviewJudgeFor(stub)},
	}}
	for _, tc := range []struct {
		name   string
		dryRun bool
	}{
		{name: "real run", dryRun: false},
		{name: "dry run", dryRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness, DryRun: tc.dryRun})
			must.ErrorContains(err, `task "first"`)
			must.ErrorContains(err, "secret://JUDGE_E2E_KEY")
		})
	}
	_, err := os.Stat(marker)
	must.ErrorIs(err, os.ErrNotExist, "the harness ran despite a failed preflight")
	must.Empty(stub.requests(), "judge should not be called after a failed preflight")
}

func TestRunSuite_JSONSuitesGetTheLoaderLLMChecks(t *testing.T) {
	cases := map[string]struct {
		suite   string
		wantErr string
	}{
		"llm on a file-exists judge": {
			suite:   `{"id":"s","tasks":[{"id":"t","prompt":"p","judge":{"type":"file-exists","paths":["a"],"llm":{"model":"m"}}}]}`,
			wantErr: `task "t": judge.type "file-exists" does not support an llm block`,
		},
		"llm on a nested file-exists judge": {
			suite:   `{"id":"s","tasks":[{"id":"t","prompt":"p","judge":{"type":"composite","judges":[{"type":"file-exists","paths":["a"],"llm":{"model":"m"}}]}}]}`,
			wantErr: "does not support an llm block",
		},
		"invalid llm": {
			suite:   `{"id":"s","tasks":[{"id":"t","prompt":"p","judge":{"type":"diff-review","criteria":"c","llm":{"model":"m","apiKeyRef":"sk-raw"}}}]}`,
			wantErr: `task "t": llm block: api_key_ref must be a secret:// reference`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			var suite types.EvalSuite
			must.NoError(json.Unmarshal([]byte(tc.suite), &suite))
			_, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
			must.ErrorContains(err, tc.wantErr)
		})
	}
}

func TestRunSuite_JSONSuitesGetTheJudgeTreeChecksBeforeAnyHarnessRun(t *testing.T) {
	cases := map[string]struct {
		suite   string
		wantErr string
	}{
		"empty nested composite": {
			suite:   `{"id":"s","tasks":[{"id":"t","prompt":"p","judge":{"type":"composite","judges":[{"type":"file-exists","paths":["a"]},{"type":"composite","require":"any","judges":[]}]}}]}`,
			wantErr: `task "t": sub-judge 2: composite judge requires at least one sub-judge`,
		},
		"invalid require": {
			suite:   `{"id":"s","tasks":[{"id":"t","prompt":"p","judge":{"type":"composite","require":"most","judges":[{"type":"file-exists","paths":["a"]}]}}]}`,
			wantErr: `task "t": invalid require value: "most"`,
		},
		"mis-specified leaf behind a passing sub-judge": {
			suite:   `{"id":"s","tasks":[{"id":"t","prompt":"p","judge":{"type":"composite","require":"any","judges":[{"type":"file-exists","paths":["a"]},{"type":"file-contains","path":"a","pattern":"("}]}}]}`,
			wantErr: `task "t": sub-judge 2: invalid pattern "("`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			marker := filepath.Join(t.TempDir(), "harness-ran")
			harness := writeFakeHarness(t, "#!/bin/sh\ntouch "+marker+"\n")
			var suite types.EvalSuite
			must.NoError(json.Unmarshal([]byte(tc.suite), &suite))

			_, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness})
			must.ErrorContains(err, tc.wantErr)
			_, err = os.Stat(marker)
			must.ErrorIs(err, os.ErrNotExist, "the harness ran despite an invalid judge tree")
		})
	}
}

func TestReplayRecording_RefusesDecidingDecisionJudge(t *testing.T) {
	must := require.New(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected call", http.StatusTeapot)
	}))
	defer srv.Close()
	task := types.EvalTask{ID: "d", Judge: types.EvalJudge{Type: "diff-review", Criteria: "c", LLM: &types.JudgeLLMConfig{
		Provider: types.JudgeProviderDecision, Model: "jev-latest", BaseURL: srv.URL,
	}}}

	result, err := ReplayRecording(context.Background(), types.RunRecording{RunID: "r1"}, task, t.TempDir(), judge.Options{}, "")

	must.Error(err, "the deciding decision judge must be refused")
	must.ErrorContains(err, `task "d"`)
	must.ErrorContains(err, "usable only on a shadow judge")
	must.Equal("error", result.Outcome)
	must.Equal("d", result.TaskID)
	must.Zero(calls.Load(), "the decision endpoint must not be called")
}
