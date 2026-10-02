package judge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

const decisionPassReply = `{
  "model": "jev-1.13.0",
  "answers": {"verdict": {"type": "choice", "choice": "pass", "probabilities": {"pass": 0.91, "fail": 0.09}, "confidence": 0.82}},
  "usage": {"input_tokens": 412, "output_tokens": 20}
}`

// decisionServer answers /v1/systemone requests with replies in turn,
// repeating the last, and records every request body.
type decisionServer struct {
	srv *httptest.Server

	mu      sync.Mutex
	bodies  []map[string]any
	headers []http.Header
}

type decisionReply struct {
	status int
	header map[string]string
	body   string
}

func newDecisionServer(t *testing.T, replies ...decisionReply) *decisionServer {
	t.Helper()
	s := &decisionServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := map[string]any{}
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		n := len(s.bodies)
		s.bodies = append(s.bodies, body)
		s.headers = append(s.headers, r.Header.Clone())
		s.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" {
			http.Error(w, "unexpected request "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		reply := replies[min(n, len(replies)-1)]
		for k, v := range reply.header {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		_, _ = io.WriteString(w, reply.body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *decisionServer) requests() ([]map[string]any, []http.Header) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.bodies...), append([]http.Header(nil), s.headers...)
}

func okDecision(body string) decisionReply { return decisionReply{status: http.StatusOK, body: body} }

func newTestDecisionClient(t *testing.T, baseURL, apiKey string) *decisionClient {
	t.Helper()
	c, err := newDecisionClient(&http.Client{Timeout: 5 * time.Second}, baseURL, apiKey, "jev-latest")
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = noSleep
	return c
}

func testDecisionRequest() decisionRequest {
	return buildDecisionRequest("the change adds a test", workspaceDiff{Stat: " a.txt | 1 +", Head: "+two\n", Size: 5}, dataFence{nonce: strings.Repeat("ab", fenceNonceBytes)})
}

func TestDecisionClient_RequestShape(t *testing.T) {
	srv := newDecisionServer(t, okDecision(decisionPassReply))

	resp, err := newTestDecisionClient(t, srv.srv.URL, "ts-test-key").Decide(context.Background(), testDecisionRequest())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	bodies, headers := srv.requests()
	if len(bodies) != 1 {
		t.Fatalf("requests = %d, want 1", len(bodies))
	}
	if got := headers[0].Get("Authorization"); got != "Bearer ts-test-key" {
		t.Errorf("Authorization = %q", got)
	}
	if got := headers[0].Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	body := bodies[0]
	if body["model"] != "jev-latest" {
		t.Errorf("model = %v", body["model"])
	}
	state, _ := body["state"].(string)
	if !strings.Contains(state, "<<<UNTRUSTED_DIFF_") || !strings.Contains(state, "+two") {
		t.Errorf("state = %q, want the fenced diff", state)
	}
	questions, _ := body["questions"].(map[string]any)
	q, _ := questions["verdict"].(map[string]any)
	if len(questions) != 1 || q["type"] != "choice" {
		t.Fatalf("questions = %v, want one choice question named verdict", body["questions"])
	}
	criteria, _ := q["criteria"].(map[string]any)
	if len(criteria) != 2 || criteria["pass"] == nil || criteria["fail"] == nil {
		t.Errorf("criteria = %v, want the pass and fail options", q["criteria"])
	}
	instructions, _ := q["instructions"].(map[string]any)
	if instructions["criteria"] != "the change adds a test" || instructions["question"] != decisionQuestionText {
		t.Errorf("instructions = %v", q["instructions"])
	}
	if boundary, _ := instructions["boundary"].(string); !strings.Contains(boundary, "untrusted data") {
		t.Errorf("instructions boundary = %q, want the fence notice", boundary)
	}
	if strings.Contains(state, "the change adds a test") {
		t.Error("criteria leaked into the agent-authored state")
	}

	if resp.Model != "jev-1.13.0" || resp.InputTokens != 412 || resp.OutputTokens != 20 || len(resp.Answers) != 1 {
		t.Errorf("response = %+v", resp)
	}
}

func TestDecisionClient_NoKeySendsNoAuthorization(t *testing.T) {
	srv := newDecisionServer(t, okDecision(decisionPassReply))
	if _, err := newTestDecisionClient(t, srv.srv.URL, "").Decide(context.Background(), testDecisionRequest()); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if _, headers := srv.requests(); headers[0].Get("Authorization") != "" {
		t.Errorf("Authorization = %q, want none without a key", headers[0].Get("Authorization"))
	}
}

func TestDecisionClient_DefaultEndpoint(t *testing.T) {
	c, err := newDecisionClient(&http.Client{}, "", "", "jev-latest")
	if err != nil {
		t.Fatal(err)
	}
	if c.endpoint != "https://api.typesafe.ai/v1/systemone" {
		t.Errorf("endpoint = %q", c.endpoint)
	}
	c, err = newDecisionClient(&http.Client{}, "https://openrouter.ai/api/", "", "jev-latest")
	if err != nil {
		t.Fatal(err)
	}
	if c.endpoint != "https://openrouter.ai/api/v1/systemone" {
		t.Errorf("gateway endpoint = %q", c.endpoint)
	}
}

func TestDecisionClient_RetriesRateLimitAndOverload(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, 529} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := newDecisionServer(t,
				decisionReply{status: status, header: map[string]string{"Retry-After": "1"}, body: `{"error":"slow down"}`},
				okDecision(decisionPassReply))
			var waits []time.Duration
			c := newTestDecisionClient(t, srv.srv.URL, "k-12345678")
			c.sleep = func(_ context.Context, d time.Duration) error {
				waits = append(waits, d)
				return nil
			}

			resp, err := c.Decide(context.Background(), testDecisionRequest())
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if bodies, _ := srv.requests(); len(bodies) != 2 || resp.Model != "jev-1.13.0" {
				t.Fatalf("requests = %d, response = %+v; want one retry then success", len(bodies), resp)
			}
			if len(waits) != 1 || waits[0] != time.Second {
				t.Errorf("waits = %v, want the provider's 1s Retry-After", waits)
			}
		})
	}
}

func TestDecisionClient_GivesUpAfterRetries(t *testing.T) {
	srv := newDecisionServer(t, decisionReply{status: 529, body: `{"error":"overloaded"}`})
	_, err := newTestDecisionClient(t, srv.srv.URL, "").Decide(context.Background(), testDecisionRequest())
	if err == nil || !strings.Contains(err.Error(), "HTTP 529") || !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("err = %v, want the 529 reported after 3 attempts", err)
	}
}

func TestDecisionClient_DoesNotRetryValidationErrors(t *testing.T) {
	srv := newDecisionServer(t, decisionReply{status: http.StatusUnprocessableEntity, body: `{"detail":"questions.verdict.criteria: required"}`})
	_, err := newTestDecisionClient(t, srv.srv.URL, "").Decide(context.Background(), testDecisionRequest())
	if bodies, _ := srv.requests(); err == nil || len(bodies) != 1 || !strings.Contains(err.Error(), "HTTP 422") {
		t.Fatalf("err = %v after %d requests, want one unretried 422", err, len(bodies))
	}
}

func TestDecisionClient_RedactsKeyFromErrors(t *testing.T) {
	srv := newDecisionServer(t, decisionReply{status: http.StatusUnauthorized, body: `{"error":"bad key ts-secret-value"}`})
	_, err := newTestDecisionClient(t, srv.srv.URL, "ts-secret-value").Decide(context.Background(), testDecisionRequest())
	if err == nil || strings.Contains(err.Error(), "ts-secret-value") || !strings.Contains(err.Error(), redactedSecret) {
		t.Fatalf("err = %v, want the key redacted", err)
	}
}

// nonDeciding marks jctx as a judgment that decides no outcome, which a
// decision judge requires.
func nonDeciding(jctx JudgeContext) JudgeContext {
	jctx.NonDeciding = true
	return jctx
}

// decisionJudge is a diff-review judge on the decision provider at baseURL.
func decisionJudge(t *testing.T, baseURL string) types.EvalJudge {
	t.Helper()
	t.Setenv("DECISION_TEST_KEY", "ts-test-key-123")
	return types.EvalJudge{Type: "diff-review", Criteria: "a.txt gains a second line", LLM: &types.JudgeLLMConfig{
		Provider: types.JudgeProviderDecision, Model: "jev-latest", BaseURL: baseURL, APIKeyRef: "secret://DECISION_TEST_KEY",
	}}
}

func TestDiffReview_DecisionVerdictAndRecord(t *testing.T) {
	srv := newDecisionServer(t, okDecision(decisionPassReply))
	j := decisionJudge(t, srv.srv.URL)

	v := evaluateOK(t, j, nonDeciding(changedJudgeContext(t, Options{})))

	if v.Status != types.JudgeStatusPass || v.Reason != "decision model: p(pass)=0.91 confidence=0.82" {
		t.Fatalf("verdict = %+v", v)
	}
	rec := v.Record
	if rec == nil {
		t.Fatal("verdict has no record")
	}
	if rec.Kind != types.JudgeKindDiffReview || rec.Provider != types.JudgeProviderDecision || rec.RequestedModel != "jev-latest" || rec.ServedModel != "jev-1.13.0" {
		t.Errorf("record identity = %+v", rec)
	}
	if rec.InputTokens != 412 || rec.OutputTokens != 20 || rec.ParseStatus != types.JudgeParseOK || rec.StopReason != decisionStopReason {
		t.Errorf("record call fields = %+v", rec)
	}
	if rec.ConfigHash == "" || rec.InputSHA256 == "" || rec.Truncated || rec.BaselineSource != types.JudgeBaselineRunner {
		t.Errorf("record input fields = %+v", rec)
	}

	bodies, _ := srv.requests()
	state, _ := bodies[0]["state"].(string)
	nonce := promptNonce(state)
	if nonce == "" || !strings.Contains(state, "+two") || !strings.Contains(state, "Summary (git diff --stat)") {
		t.Fatalf("state = %q, want the fenced summary and diff", state)
	}
	boundary := bodies[0]["questions"].(map[string]any)["verdict"].(map[string]any)["instructions"].(map[string]any)["boundary"].(string)
	if !strings.Contains(boundary, nonce) {
		t.Errorf("boundary %q does not name the state's fence nonce %s", boundary, nonce)
	}
}

func TestDiffReview_DecisionFailVerdict(t *testing.T) {
	srv := newDecisionServer(t, okDecision(`{"model":"jev-1.13.0","answers":{"verdict":{"type":"choice","choice":"fail","probabilities":{"pass":0.3,"fail":0.7},"confidence":0.4}},"usage":{"input_tokens":1,"output_tokens":1}}`))
	v := evaluateOK(t, decisionJudge(t, srv.srv.URL), nonDeciding(changedJudgeContext(t, Options{})))
	if v.Status != types.JudgeStatusFail || v.Passed || v.Reason != "decision model: p(pass)=0.30 confidence=0.40" {
		t.Fatalf("verdict = %+v, want a fail with its probabilities", v)
	}
}

func TestDiffReview_DecisionMalformedAnswersAreErrors(t *testing.T) {
	answer := func(a string) string {
		return `{"model":"jev-1.13.0","answers":{"verdict":` + a + `},"usage":{"input_tokens":1,"output_tokens":1}}`
	}
	cases := map[string]string{
		"no answers":               `{"model":"jev-1.13.0","answers":{}}`,
		"answers missing":          `{"model":"jev-1.13.0"}`,
		"another question":         `{"model":"jev-1.13.0","answers":{"other":{"type":"choice","choice":"pass","probabilities":{"pass":1,"fail":0},"confidence":1}}}`,
		"extra answer":             `{"model":"jev-1.13.0","answers":{"verdict":{"type":"choice","choice":"pass","probabilities":{"pass":1,"fail":0},"confidence":1},"x":{}}}`,
		"answer is a string":       answer(`"pass"`),
		"noul answer":              answer(`{"type":"noul","noul":0.9}`),
		"unknown choice":           answer(`{"type":"choice","choice":"maybe","probabilities":{"pass":0.5,"fail":0.5},"confidence":0.1}`),
		"missing probability":      answer(`{"type":"choice","choice":"pass","probabilities":{"pass":1},"confidence":1}`),
		"extra probability":        answer(`{"type":"choice","choice":"pass","probabilities":{"pass":0.5,"fail":0.3,"maybe":0.2},"confidence":1}`),
		"probability out of [0,1]": answer(`{"type":"choice","choice":"pass","probabilities":{"pass":1.5,"fail":-0.5},"confidence":1}`),
		"probabilities sum off":    answer(`{"type":"choice","choice":"pass","probabilities":{"pass":0.6,"fail":0.6},"confidence":1}`),
		"choice is not argmax":     answer(`{"type":"choice","choice":"pass","probabilities":{"pass":0.2,"fail":0.8},"confidence":0.7}`),
		"missing confidence":       answer(`{"type":"choice","choice":"pass","probabilities":{"pass":0.9,"fail":0.1}}`),
		"confidence out of range":  answer(`{"type":"choice","choice":"pass","probabilities":{"pass":0.9,"fail":0.1},"confidence":2}`),
		"probability not a number": answer(`{"type":"choice","choice":"pass","probabilities":{"pass":"high","fail":0.1},"confidence":1}`),
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newDecisionServer(t, okDecision(reply))
			v, err := Evaluate(context.Background(), decisionJudge(t, srv.srv.URL), nonDeciding(changedJudgeContext(t, Options{})))
			if err == nil || v.Status != types.JudgeStatusError || v.Passed {
				t.Fatalf("verdict = %+v, err = %v; want an error verdict", v, err)
			}
			if v.Record == nil || v.Record.ParseStatus != types.JudgeParseSchemaViolation || v.Record.ServedModel != "jev-1.13.0" {
				t.Errorf("record = %+v, want parse status schema_violation with the served model", v.Record)
			}
		})
	}
}

func TestDiffReview_DecisionUndecodableBodyIsError(t *testing.T) {
	srv := newDecisionServer(t, okDecision(`not json`))
	v, err := Evaluate(context.Background(), decisionJudge(t, srv.srv.URL), nonDeciding(changedJudgeContext(t, Options{})))
	if err == nil || v.Status != types.JudgeStatusError || !strings.Contains(v.Reason, "decode response") {
		t.Fatalf("verdict = %+v, err = %v; want a decode error", v, err)
	}
	if v.Record == nil || v.Record.ParseStatus != "" {
		t.Errorf("record = %+v, want no parse status for a transport-level failure", v.Record)
	}
}

// bigWorkspace is a non-deciding judgment of a change whose diff is larger
// than the decision budget but within the default max_input_bytes.
func bigWorkspace(t *testing.T) JudgeContext {
	t.Helper()
	ws, base := newWorkspace(t, map[string]string{"a.txt": "one\n"})
	writeFiles(t, ws, map[string]string{"big.txt": strings.Repeat("0123456789abcdef\n", 2500)})
	return JudgeContext{WorkspaceDir: ws, Baseline: &base, NonDeciding: true}
}

func TestDiffReview_DecisionOverBudgetIsErrorWithoutAllowTruncated(t *testing.T) {
	srv := newDecisionServer(t, okDecision(decisionPassReply))
	v, err := Evaluate(context.Background(), decisionJudge(t, srv.srv.URL), bigWorkspace(t))

	if err == nil || v.Status != types.JudgeStatusError || !strings.Contains(v.Reason, "32000-token input limit") || !strings.Contains(v.Reason, "allow_truncated") {
		t.Fatalf("verdict = %+v, err = %v; want an over-budget error", v, err)
	}
	if v.Record == nil || !v.Record.Truncated {
		t.Errorf("record = %+v, want Truncated", v.Record)
	}
	if bodies, _ := srv.requests(); len(bodies) != 0 {
		t.Errorf("the model was called %d times for an over-budget diff", len(bodies))
	}
}

func TestDiffReview_DecisionAllowTruncatedFitsTheLimit(t *testing.T) {
	srv := newDecisionServer(t, okDecision(decisionPassReply))
	j := decisionJudge(t, srv.srv.URL)
	j.LLM.AllowTruncated = true

	v := evaluateOK(t, j, bigWorkspace(t))

	if v.Status != types.JudgeStatusPass || v.Record == nil || !v.Record.Truncated || v.Record.InputBytes <= decisionStateTokenLimit {
		t.Fatalf("verdict = %+v, want a pass over a truncated diff", v)
	}
	bodies, _ := srv.requests()
	state, _ := bodies[0]["state"].(string)
	q := bodies[0]["questions"].(map[string]any)["verdict"]
	qJSON, _ := json.Marshal(q)
	if got := len(state) + len(qJSON); got > decisionStateTokenLimit-decisionTokenReserve {
		t.Errorf("state plus question = %d bytes, over the %d budget", got, decisionStateTokenLimit-decisionTokenReserve)
	}
	note, _ := q.(map[string]any)["instructions"].(map[string]any)["note"].(string)
	if !strings.Contains(note, "only the first") {
		t.Errorf("instructions note = %q, want the truncation notice outside the state", note)
	}
	if strings.Contains(state, "only the first") {
		t.Error("the truncation notice is inside the agent-authored state")
	}
}

// fenceMarkerWorkspace is a non-deciding judgment of a change of about size
// bytes made of '<' runs, which the data fence lengthens by half.
func fenceMarkerWorkspace(t *testing.T, size int) JudgeContext {
	t.Helper()
	ws, base := newWorkspace(t, map[string]string{"a.txt": "one\n"})
	line := strings.Repeat("<", 63) + "\n"
	writeFiles(t, ws, map[string]string{"conflict.txt": strings.Repeat(line, size/len(line))})
	return JudgeContext{WorkspaceDir: ws, Baseline: &base, NonDeciding: true}
}

func TestDiffReview_DecisionBudgetCountsFenceNeutralisation(t *testing.T) {
	for name, size := range map[string]int{"raw diff within budget": 24_000, "raw diff over budget": 48_000} {
		t.Run(name, func(t *testing.T) {
			srv := newDecisionServer(t, okDecision(decisionPassReply))
			j := decisionJudge(t, srv.srv.URL)
			j.LLM.AllowTruncated = true

			v := evaluateOK(t, j, fenceMarkerWorkspace(t, size))

			if v.Status != types.JudgeStatusPass || v.Record == nil || !v.Record.Truncated {
				t.Fatalf("verdict = %+v, want a pass over a diff cut to fit once neutralised", v)
			}
			bodies, _ := srv.requests()
			state, _ := bodies[0]["state"].(string)
			qJSON, _ := json.Marshal(bodies[0]["questions"].(map[string]any)["verdict"])
			if got := len(state) + len(qJSON); got > decisionStateTokenLimit-decisionTokenReserve {
				t.Errorf("state plus question = %d bytes, over the %d budget", got, decisionStateTokenLimit-decisionTokenReserve)
			}
			if strings.Contains(state[strings.Index(state, "Diff:"):strings.LastIndex(state, "<<<END_")], "<<<") {
				t.Error("the fenced diff contains an unneutralised marker run")
			}
		})
	}
}

func TestDiffReview_DecisionNeutralisedOverflowNeedsAllowTruncated(t *testing.T) {
	srv := newDecisionServer(t, okDecision(decisionPassReply))
	v, err := Evaluate(context.Background(), decisionJudge(t, srv.srv.URL), fenceMarkerWorkspace(t, 24_000))
	if err == nil || v.Status != types.JudgeStatusError || !strings.Contains(v.Reason, "allow_truncated") {
		t.Fatalf("verdict = %+v, err = %v; want the budget cut refused without allow_truncated", v, err)
	}
	if bodies, _ := srv.requests(); len(bodies) != 0 {
		t.Errorf("the model was called %d times for an over-budget diff", len(bodies))
	}
}

func TestDiffReview_DecisionCriteriaOverBudgetIsError(t *testing.T) {
	srv := newDecisionServer(t, okDecision(decisionPassReply))
	j := decisionJudge(t, srv.srv.URL)
	j.Criteria = strings.Repeat("c", decisionStateTokenLimit)
	j.LLM.AllowTruncated = true

	v, err := Evaluate(context.Background(), j, nonDeciding(changedJudgeContext(t, Options{})))
	if err == nil || v.Status != types.JudgeStatusError || !strings.Contains(v.Reason, "criteria and change summary alone") {
		t.Fatalf("verdict = %+v, err = %v; want an over-budget criteria error", v, err)
	}
}

func TestCheckDecisionLimits(t *testing.T) {
	small := testDecisionRequest()
	if err := checkDecisionLimits(small); err != nil {
		t.Fatalf("small request: %v", err)
	}
	big := small
	big.State = strings.Repeat("x", decisionStateTokenLimit)
	if err := checkDecisionLimits(big); err == nil {
		t.Error("a state over the per-question limit was accepted")
	}
	many := decisionRequest{State: "s", Questions: map[string]decisionQuestion{}}
	for _, k := range []string{"a", "b", "c"} {
		many.Questions[k] = decisionQuestion{Type: "choice", Instructions: strings.Repeat("q", 25_000)}
	}
	if err := checkDecisionLimits(many); err == nil {
		t.Error("questions over the per-request limit were accepted")
	}
}

func TestDiffReview_DecisionVerdictsAreCached(t *testing.T) {
	srv := newDecisionServer(t, okDecision(decisionPassReply))
	j := decisionJudge(t, srv.srv.URL)
	cache := newMemCache()
	jctx := nonDeciding(changedJudgeContext(t, Options{Cache: cache, CacheMode: CacheReadThrough}))

	first := evaluateOK(t, j, jctx)
	second := evaluateOK(t, j, jctx)

	if first.Record.CacheStatus != types.JudgeCacheStored || second.Record.CacheStatus != types.JudgeCacheHit {
		t.Fatalf("cache statuses = %q, %q; want stored then hit", first.Record.CacheStatus, second.Record.CacheStatus)
	}
	if bodies, _ := srv.requests(); len(bodies) != 1 {
		t.Errorf("model calls = %d, want 1", len(bodies))
	}
	if second.Reason != first.Reason || second.Record.StopReason != decisionStopReason || second.Record.ServedModel != "jev-1.13.0" {
		t.Errorf("cached verdict = %+v, want the stored reason and record", second)
	}
}

func TestDiffReviewConfigHash_DecisionIsDistinctAndStable(t *testing.T) {
	decision := types.JudgeLLMConfig{Provider: types.JudgeProviderDecision, Model: "m"}
	anthropic := types.JudgeLLMConfig{Provider: types.JudgeProviderAnthropic, Model: "m"}
	h1, err := diffReviewConfigHash(decision, "c")
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := diffReviewConfigHash(decision, "c")
	h3, _ := diffReviewConfigHash(anthropic, "c")
	explicit := decision
	explicit.BaseURL = decisionDefaultBaseURL
	h4, _ := diffReviewConfigHash(explicit, "c")
	if h1 != h2 || h1 == h3 || h1 != h4 {
		t.Errorf("hashes: decision %s / %s, anthropic %s, explicit default endpoint %s", h1, h2, h3, h4)
	}
}

func TestResolveLLMConfig_DecisionDefaults(t *testing.T) {
	cfg, err := ResolveLLMConfig(nil, &types.JudgeLLMConfig{Provider: types.JudgeProviderDecision})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != DefaultDecisionModel || cfg.APIKeyRef != DefaultDecisionKeyRef {
		t.Errorf("resolved = %+v, want the decision model and key defaults", cfg)
	}

	cfg, err = ResolveLLMConfig(nil, &types.JudgeLLMConfig{Provider: types.JudgeProviderDecision, BaseURL: "http://127.0.0.1:8000"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKeyRef != "" {
		t.Errorf("api_key_ref = %q, want no default key for a self-hosted server", cfg.APIKeyRef)
	}

	if _, err := ResolveLLMConfig(&types.JudgeLLMConfig{Provider: types.JudgeProviderDecision}, nil); err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Errorf("explicit block without a model: err = %v", err)
	}
}

func TestNewClient_RefusesDecisionProvider(t *testing.T) {
	if _, err := NewClient(types.JudgeLLMConfig{Provider: types.JudgeProviderDecision, Model: "m"}, ""); err == nil {
		t.Error("NewClient built a text client for the decision provider")
	}
}

func TestValidateTree_DecisionRequiresShadow(t *testing.T) {
	decisionLeaf := types.EvalJudge{Type: "diff-review", Criteria: "c", LLM: &types.JudgeLLMConfig{Provider: types.JudgeProviderDecision, Model: "jev-latest"}}
	deciding := types.EvalJudge{Type: "file-exists", Paths: []string{"a"}}

	if err := ValidateTree(decisionLeaf); err == nil || !strings.Contains(err.Error(), "usable only on a shadow judge") {
		t.Errorf("top-level decision judge: err = %v", err)
	}
	if err := ValidateTree(composite("all", deciding, decisionLeaf)); err == nil || !strings.Contains(err.Error(), "sub-judge 2") {
		t.Errorf("deciding decision sub-judge: err = %v", err)
	}
	if err := ValidateTree(composite("all", deciding, shadow(decisionLeaf))); err != nil {
		t.Errorf("shadow decision sub-judge: %v", err)
	}
}

func TestValidateTree_DecisionRequiresShadowInJSONSuites(t *testing.T) {
	var task types.EvalTask
	src := `{"id":"t","judge":{"type":"composite","judges":[
	  {"type":"file-exists","paths":["a"]},
	  {"type":"diff-review","criteria":"c","llm":{"provider":"decision","model":"jev-latest"}}]}}`
	if err := json.Unmarshal([]byte(src), &task); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTree(task.Judge); err == nil || !strings.Contains(err.Error(), "usable only on a shadow judge") {
		t.Errorf("JSON suite with a deciding decision judge: err = %v", err)
	}
	task.Judge.Judges[1].Shadow = true
	if err := ValidateTree(task.Judge); err != nil {
		t.Errorf("JSON suite with a shadow decision judge: %v", err)
	}
}

func TestPreflightSuite_RefusesDecisionDefaults(t *testing.T) {
	tasks := []types.EvalTask{{ID: "t", Judge: types.EvalJudge{Type: "diff-review", Criteria: "c"}}}
	err := PreflightSuite(context.Background(), tasks, Options{LLMDefaults: &types.JudgeLLMConfig{Provider: types.JudgeProviderDecision}})
	if err == nil || !strings.Contains(err.Error(), "cannot be the default") {
		t.Fatalf("err = %v, want the decision default refused", err)
	}
}

func TestDiffReview_DecisionRefusesADecidingJudgment(t *testing.T) {
	srv := newDecisionServer(t, okDecision(decisionPassReply))
	explicit := decisionJudge(t, srv.srv.URL)
	viaDefaults := Options{LLMDefaults: explicit.LLM}
	cases := map[string]struct {
		judge types.EvalJudge
		opts  Options
	}{
		"explicit llm block":  {explicit, Options{}},
		"invocation defaults": {diffReviewJudge(), viaDefaults},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			v, err := Evaluate(context.Background(), tc.judge, changedJudgeContext(t, tc.opts))
			if err == nil || v.Status != types.JudgeStatusError || !strings.Contains(v.Reason, "cannot decide a task") {
				t.Fatalf("verdict = %+v, err = %v; want the deciding decision judgment refused", v, err)
			}
		})
	}
	if bodies, _ := srv.requests(); len(bodies) != 0 {
		t.Errorf("the decision endpoint was called %d times", len(bodies))
	}
}

func TestDiffReview_DecisionCachedVerdictIsNotServedToADecidingJudgment(t *testing.T) {
	srv := newDecisionServer(t, okDecision(decisionPassReply))
	j := decisionJudge(t, srv.srv.URL)
	jctx := nonDeciding(changedJudgeContext(t, Options{Cache: newMemCache(), CacheMode: CacheReadThrough}))
	evaluateOK(t, j, jctx)

	jctx.NonDeciding = false
	if v, err := Evaluate(context.Background(), j, jctx); err == nil || v.Status != types.JudgeStatusError {
		t.Fatalf("verdict = %+v, err = %v; want a stored decision verdict refused to a deciding judgment", v, err)
	}
}

func TestComposite_ShadowDecisionJudgeIsEvaluated(t *testing.T) {
	srv := newDecisionServer(t, okDecision(decisionPassReply))
	jctx := changedJudgeContext(t, Options{})
	nested := composite("all", fileExistsJudge("a.txt"), shadow(decisionJudge(t, srv.srv.URL)))
	cases := map[string]types.EvalJudge{
		"shadow sub-judge":            composite("all", fileExistsJudge("a.txt"), shadow(decisionJudge(t, srv.srv.URL))),
		"inside a shadow composite":   composite("all", fileExistsJudge("a.txt"), shadow(nested)),
		"inside a deciding composite": composite("all", fileExistsJudge("a.txt"), nested),
	}
	for name, j := range cases {
		t.Run(name, func(t *testing.T) {
			v := evaluateOK(t, j, jctx)
			if v.Status != types.JudgeStatusPass {
				t.Fatalf("verdict = %+v, want pass decided by file-exists", v)
			}
			if !strings.Contains(fmt.Sprintf("%+v", v.Details), "decision model: p(pass)=0.91") {
				t.Errorf("details = %+v, want the decision shadow's verdict recorded", v.Details)
			}
		})
	}
}
