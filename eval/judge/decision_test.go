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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	must := require.New(t)
	c, err := newDecisionClient(&http.Client{Timeout: 5 * time.Second}, baseURL, apiKey, "jev-latest")
	must.NoError(err)
	c.sleep = noSleep
	return c
}

func testDecisionRequest() decisionRequest {
	return buildDecisionRequest("the change adds a test", workspaceDiff{Stat: " a.txt | 1 +", Head: "+two\n", Size: 5}, dataFence{nonce: strings.Repeat("ab", fenceNonceBytes)})
}

func TestDecisionClient_RequestShape(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv := newDecisionServer(t, okDecision(decisionPassReply))

	resp, err := newTestDecisionClient(t, srv.srv.URL, "ts-test-key").Decide(context.Background(), testDecisionRequest())
	must.NoError(err)

	bodies, headers := srv.requests()
	must.Len(bodies, 1)
	body := bodies[0]

	check.Equal("Bearer ts-test-key", headers[0].Get("Authorization"))
	check.Equal("application/json", headers[0].Get("Content-Type"))
	check.Equal("jev-latest", body["model"])

	state, _ := body["state"].(string)
	check.Contains(state, "<<<UNTRUSTED_DIFF_")
	check.Contains(state, "+two")

	questions, _ := body["questions"].(map[string]any)
	must.Len(questions, 1, "questions = %v, want one question named verdict", body["questions"])
	q, _ := questions["verdict"].(map[string]any)
	must.Equal("choice", q["type"], "questions = %v, want one choice question named verdict", body["questions"])

	criteria, _ := q["criteria"].(map[string]any)
	check.Len(criteria, 2, "criteria = %v, want the pass and fail options", q["criteria"])
	check.NotNil(criteria["pass"])
	check.NotNil(criteria["fail"])

	instructions, _ := q["instructions"].(map[string]any)
	check.Equal("the change adds a test", instructions["criteria"])
	check.Equal(decisionQuestionText, instructions["question"])
	boundary, _ := instructions["boundary"].(string)
	check.Contains(boundary, "untrusted data", "instructions boundary should carry the fence notice")
	check.NotContains(state, "the change adds a test", "criteria leaked into the agent-authored state")

	check.Equal("jev-1.13.0", resp.Model)
	check.Equal(412, resp.InputTokens)
	check.Equal(20, resp.OutputTokens)
	check.Len(resp.Answers, 1)
}

func TestDecisionClient_NoKeySendsNoAuthorization(t *testing.T) {
	must := require.New(t)
	srv := newDecisionServer(t, okDecision(decisionPassReply))
	_, err := newTestDecisionClient(t, srv.srv.URL, "").Decide(context.Background(), testDecisionRequest())
	must.NoError(err)

	_, headers := srv.requests()
	must.Empty(headers[0].Get("Authorization"), "Authorization should be absent without a key")
}

func TestDecisionClient_DefaultEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{name: "empty base URL", baseURL: "", want: "https://api.typesafe.ai/v1/systemone"},
		{name: "gateway base URL", baseURL: "https://openrouter.ai/api/", want: "https://openrouter.ai/api/v1/systemone"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			c, err := newDecisionClient(&http.Client{}, tc.baseURL, "", "jev-latest")
			must.NoError(err)
			must.Equal(tc.want, c.endpoint)
		})
	}
}

func TestDecisionClient_RetriesRateLimitAndOverload(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{name: "rate limited", status: http.StatusTooManyRequests},
		{name: "overloaded", status: 529},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			srv := newDecisionServer(t,
				decisionReply{status: tc.status, header: map[string]string{"Retry-After": "1"}, body: `{"error":"slow down"}`},
				okDecision(decisionPassReply))
			var waits []time.Duration
			c := newTestDecisionClient(t, srv.srv.URL, "k-12345678")
			c.sleep = func(_ context.Context, d time.Duration) error {
				waits = append(waits, d)
				return nil
			}

			resp, err := c.Decide(context.Background(), testDecisionRequest())
			must.NoError(err)
			bodies, _ := srv.requests()
			must.Len(bodies, 2, "one retry then success")
			must.Equal("jev-1.13.0", resp.Model)
			must.Equal([]time.Duration{time.Second}, waits, "the provider's Retry-After")
		})
	}
}

func TestDecisionClient_GivesUpAfterRetries(t *testing.T) {
	must := require.New(t)
	srv := newDecisionServer(t, decisionReply{status: 529, body: `{"error":"overloaded"}`})
	_, err := newTestDecisionClient(t, srv.srv.URL, "").Decide(context.Background(), testDecisionRequest())
	must.ErrorContains(err, "HTTP 529")
	must.ErrorContains(err, "after 3 attempts")
}

func TestDecisionClient_DoesNotRetryValidationErrors(t *testing.T) {
	must := require.New(t)
	srv := newDecisionServer(t, decisionReply{status: http.StatusUnprocessableEntity, body: `{"detail":"questions.verdict.criteria: required"}`})
	_, err := newTestDecisionClient(t, srv.srv.URL, "").Decide(context.Background(), testDecisionRequest())
	must.ErrorContains(err, "HTTP 422")
	bodies, _ := srv.requests()
	must.Len(bodies, 1, "a validation error is not retried")
}

func TestDecisionClient_RedactsKeyFromErrors(t *testing.T) {
	must := require.New(t)
	srv := newDecisionServer(t, decisionReply{status: http.StatusUnauthorized, body: `{"error":"bad key ts-secret-value"}`})
	_, err := newTestDecisionClient(t, srv.srv.URL, "ts-secret-value").Decide(context.Background(), testDecisionRequest())
	must.Error(err)
	must.NotContains(err.Error(), "ts-secret-value")
	must.ErrorContains(err, redactedSecret)
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
	must := require.New(t)
	check := assert.New(t)
	srv := newDecisionServer(t, okDecision(decisionPassReply))
	j := decisionJudge(t, srv.srv.URL)

	v := evaluateOK(t, j, nonDeciding(changedJudgeContext(t, Options{})))

	must.Equal(types.JudgeStatusPass, v.Status)
	must.Equal("decision model: p(pass)=0.91 confidence=0.82", v.Reason)

	rec := v.Record
	must.NotNil(rec, "verdict has no record")
	check.Equal(types.JudgeKindDiffReview, rec.Kind)
	check.Equal(types.JudgeProviderDecision, rec.Provider)
	check.Equal("jev-latest", rec.RequestedModel)
	check.Equal("jev-1.13.0", rec.ServedModel)
	check.Equal(412, rec.InputTokens)
	check.Equal(20, rec.OutputTokens)
	check.Equal(types.JudgeParseOK, rec.ParseStatus)
	check.Equal(decisionStopReason, rec.StopReason)
	check.NotEmpty(rec.ConfigHash)
	check.NotEmpty(rec.InputSHA256)
	check.False(rec.Truncated)
	check.Equal(types.JudgeBaselineRunner, rec.BaselineSource)

	bodies, _ := srv.requests()
	state, _ := bodies[0]["state"].(string)
	nonce := promptNonce(state)
	must.NotEmpty(nonce, "state = %q, want the fence nonce", state)
	must.Contains(state, "+two")
	must.Contains(state, "Summary (git diff --stat)")

	boundary := bodies[0]["questions"].(map[string]any)["verdict"].(map[string]any)["instructions"].(map[string]any)["boundary"].(string)
	must.Contains(boundary, nonce, "boundary should name the state's fence nonce")
}

func TestDiffReview_DecisionFailVerdict(t *testing.T) {
	must := require.New(t)
	srv := newDecisionServer(t, okDecision(`{"model":"jev-1.13.0","answers":{"verdict":{"type":"choice","choice":"fail","probabilities":{"pass":0.3,"fail":0.7},"confidence":0.4}},"usage":{"input_tokens":1,"output_tokens":1}}`))
	v := evaluateOK(t, decisionJudge(t, srv.srv.URL), nonDeciding(changedJudgeContext(t, Options{})))

	must.Equal(types.JudgeStatusFail, v.Status)
	must.False(v.Passed)
	must.Equal("decision model: p(pass)=0.30 confidence=0.40", v.Reason)
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
			must := require.New(t)
			srv := newDecisionServer(t, okDecision(reply))
			v, err := Evaluate(context.Background(), decisionJudge(t, srv.srv.URL), nonDeciding(changedJudgeContext(t, Options{})))
			must.Error(err)
			must.Equal(types.JudgeStatusError, v.Status)
			must.False(v.Passed)
			must.NotNil(v.Record, "want a record with parse status schema_violation")
			must.Equal(types.JudgeParseSchemaViolation, v.Record.ParseStatus)
			must.Equal("jev-1.13.0", v.Record.ServedModel)
		})
	}
}

func TestDiffReview_DecisionUndecodableBodyIsError(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv := newDecisionServer(t, okDecision(`not json`))
	v, err := Evaluate(context.Background(), decisionJudge(t, srv.srv.URL), nonDeciding(changedJudgeContext(t, Options{})))

	must.Error(err)
	must.Equal(types.JudgeStatusError, v.Status)
	must.Contains(v.Reason, "decode response")
	must.NotNil(v.Record)
	check.Empty(v.Record.ParseStatus, "a transport-level failure has no parse status")
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
	must := require.New(t)
	srv := newDecisionServer(t, okDecision(decisionPassReply))
	v, err := Evaluate(context.Background(), decisionJudge(t, srv.srv.URL), bigWorkspace(t))

	must.Error(err)
	must.Equal(types.JudgeStatusError, v.Status)
	must.Contains(v.Reason, "32000-token input limit")
	must.Contains(v.Reason, "allow_truncated")
	must.NotNil(v.Record)
	must.True(v.Record.Truncated)
	bodies, _ := srv.requests()
	must.Empty(bodies, "the model should not be called for an over-budget diff")
}

func TestDiffReview_DecisionAllowTruncatedFitsTheLimit(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv := newDecisionServer(t, okDecision(decisionPassReply))
	j := decisionJudge(t, srv.srv.URL)
	j.LLM.AllowTruncated = true

	v := evaluateOK(t, j, bigWorkspace(t))

	must.Equal(types.JudgeStatusPass, v.Status, "want a pass over a truncated diff")
	must.NotNil(v.Record)
	must.True(v.Record.Truncated)
	must.Greater(v.Record.InputBytes, decisionStateTokenLimit)

	bodies, _ := srv.requests()
	state, _ := bodies[0]["state"].(string)
	q := bodies[0]["questions"].(map[string]any)["verdict"]
	qJSON, _ := json.Marshal(q)
	check.LessOrEqual(len(state)+len(qJSON), decisionStateTokenLimit-decisionTokenReserve, "state plus question must fit the budget")

	note, _ := q.(map[string]any)["instructions"].(map[string]any)["note"].(string)
	check.Contains(note, "only the first", "the truncation notice belongs in the instructions note")
	check.NotContains(state, "only the first", "the truncation notice must not be inside the agent-authored state")
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
			must := require.New(t)
			srv := newDecisionServer(t, okDecision(decisionPassReply))
			j := decisionJudge(t, srv.srv.URL)
			j.LLM.AllowTruncated = true

			v := evaluateOK(t, j, fenceMarkerWorkspace(t, size))

			must.Equal(types.JudgeStatusPass, v.Status, "want a pass over a diff cut to fit once neutralised")
			must.NotNil(v.Record)
			must.True(v.Record.Truncated)

			bodies, _ := srv.requests()
			state, _ := bodies[0]["state"].(string)
			qJSON, _ := json.Marshal(bodies[0]["questions"].(map[string]any)["verdict"])
			must.LessOrEqual(len(state)+len(qJSON), decisionStateTokenLimit-decisionTokenReserve, "state plus question must fit the budget")

			diff := state[strings.Index(state, "Diff:"):strings.LastIndex(state, "<<<END_")]
			must.NotContains(diff, "<<<", "the fenced diff contains an unneutralised marker run")
		})
	}
}

func TestDiffReview_DecisionNeutralisedOverflowNeedsAllowTruncated(t *testing.T) {
	must := require.New(t)
	srv := newDecisionServer(t, okDecision(decisionPassReply))
	v, err := Evaluate(context.Background(), decisionJudge(t, srv.srv.URL), fenceMarkerWorkspace(t, 24_000))

	must.Error(err)
	must.Equal(types.JudgeStatusError, v.Status)
	must.Contains(v.Reason, "allow_truncated")
	bodies, _ := srv.requests()
	must.Empty(bodies, "the model should not be called for an over-budget diff")
}

func TestDiffReview_DecisionCriteriaOverBudgetIsError(t *testing.T) {
	must := require.New(t)
	srv := newDecisionServer(t, okDecision(decisionPassReply))
	j := decisionJudge(t, srv.srv.URL)
	j.Criteria = strings.Repeat("c", decisionStateTokenLimit)
	j.LLM.AllowTruncated = true

	v, err := Evaluate(context.Background(), j, nonDeciding(changedJudgeContext(t, Options{})))

	must.Error(err)
	must.Equal(types.JudgeStatusError, v.Status)
	must.Contains(v.Reason, "criteria and change summary alone")
}

func TestCheckDecisionLimits_EnforcesBudgets(t *testing.T) {
	must := require.New(t)
	t.Run("small request is accepted", func(t *testing.T) {
		must := require.New(t)
		must.NoError(checkDecisionLimits(testDecisionRequest()))
	})

	empty := testDecisionRequest()
	empty.State = ""
	question, _, err := decisionSizes(empty)
	must.NoError(err)

	atLimit := testDecisionRequest()
	atLimit.State = strings.Repeat("x", decisionStateTokenLimit-decisionTokenReserve-question)

	t.Run("state at the budget is accepted", func(t *testing.T) {
		must := require.New(t)
		must.NoError(checkDecisionLimits(atLimit))
	})

	t.Run("state one byte over the budget is refused", func(t *testing.T) {
		must := require.New(t)
		overLimit := atLimit
		overLimit.State += "x"
		must.ErrorContains(checkDecisionLimits(overLimit), "31488-byte budget for the provider's 32000-token limit on the state plus a question")
	})

	t.Run("questions within the state budget but over the request budget are refused", func(t *testing.T) {
		must := require.New(t)
		many := decisionRequest{State: "s", Questions: map[string]decisionQuestion{}}
		for _, k := range []string{"a", "b", "c"} {
			many.Questions[k] = decisionQuestion{Type: "choice", Instructions: strings.Repeat("q", 25_000)}
		}
		must.ErrorContains(checkDecisionLimits(many), "63488-byte budget for the provider's 64000-token limit on a request")
	})
}

func TestDiffReview_DecisionVerdictsAreCached(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := newDecisionServer(t, okDecision(decisionPassReply))
	j := decisionJudge(t, srv.srv.URL)
	cache := newMemCache()
	jctx := nonDeciding(changedJudgeContext(t, Options{Cache: cache, CacheMode: CacheReadThrough}))

	first := evaluateOK(t, j, jctx)
	second := evaluateOK(t, j, jctx)

	must.Equal(types.JudgeCacheStored, first.Record.CacheStatus, "first verdict")
	must.Equal(types.JudgeCacheHit, second.Record.CacheStatus, "second verdict")
	bodies, _ := srv.requests()
	check.Len(bodies, 1, "model calls")
	check.Equal(first.Reason, second.Reason, "cached verdict should keep the stored reason")
	check.Equal(decisionStopReason, second.Record.StopReason)
	check.Equal("jev-1.13.0", second.Record.ServedModel)
}

func TestDiffReviewConfigHash_DecisionIsDistinctAndStable(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	decision := types.JudgeLLMConfig{Provider: types.JudgeProviderDecision, Model: "m"}
	anthropic := types.JudgeLLMConfig{Provider: types.JudgeProviderAnthropic, Model: "m"}
	h1, err := diffReviewConfigHash(decision, "c")
	must.NoError(err)
	h2, _ := diffReviewConfigHash(decision, "c")
	h3, _ := diffReviewConfigHash(anthropic, "c")
	explicit := decision
	explicit.BaseURL = decisionDefaultBaseURL
	h4, _ := diffReviewConfigHash(explicit, "c")

	check.Equal(h1, h2, "the same configuration should hash the same")
	check.NotEqual(h1, h3, "decision and anthropic configurations should hash differently")
	check.Equal(h1, h4, "the explicit default endpoint should hash like the implicit one")
}

func TestResolveLLMConfig_DecisionDefaults(t *testing.T) {
	t.Run("hosted endpoint gets the default model and key", func(t *testing.T) {
		must := require.New(t)
		cfg, err := ResolveLLMConfig(nil, &types.JudgeLLMConfig{Provider: types.JudgeProviderDecision})
		must.NoError(err)
		must.Equal(DefaultDecisionModel, cfg.Model)
		must.Equal(DefaultDecisionKeyRef, cfg.APIKeyRef)
	})

	t.Run("self-hosted base URL has no default key", func(t *testing.T) {
		must := require.New(t)
		cfg, err := ResolveLLMConfig(nil, &types.JudgeLLMConfig{Provider: types.JudgeProviderDecision, BaseURL: "http://127.0.0.1:8000"})
		must.NoError(err)
		must.Empty(cfg.APIKeyRef)
	})

	t.Run("explicit block without a model is refused", func(t *testing.T) {
		must := require.New(t)
		_, err := ResolveLLMConfig(&types.JudgeLLMConfig{Provider: types.JudgeProviderDecision}, nil)
		must.ErrorContains(err, "model is required")
	})
}

func TestNewClient_RefusesDecisionProvider(t *testing.T) {
	must := require.New(t)
	_, err := NewClient(types.JudgeLLMConfig{Provider: types.JudgeProviderDecision, Model: "m"}, "")
	must.Error(err, "NewClient built a text client for the decision provider")
}

func TestValidateTree_DecisionRequiresShadow(t *testing.T) {
	decisionLeaf := types.EvalJudge{Type: "diff-review", Criteria: "c", LLM: &types.JudgeLLMConfig{Provider: types.JudgeProviderDecision, Model: "jev-latest"}}
	deciding := types.EvalJudge{Type: "file-exists", Paths: []string{"a"}}

	t.Run("top-level decision judge is refused", func(t *testing.T) {
		must := require.New(t)
		must.ErrorContains(ValidateTree(decisionLeaf), "usable only on a shadow judge")
	})

	t.Run("deciding decision sub-judge is refused", func(t *testing.T) {
		must := require.New(t)
		must.ErrorContains(ValidateTree(composite("all", deciding, decisionLeaf)), "sub-judge 2")
	})

	t.Run("shadow decision sub-judge is accepted", func(t *testing.T) {
		must := require.New(t)
		must.NoError(ValidateTree(composite("all", deciding, shadow(decisionLeaf))))
	})
}

func TestValidateTree_DecisionRequiresShadowInJSONSuites(t *testing.T) {
	must := require.New(t)
	var task types.EvalTask
	src := `{"id":"t","judge":{"type":"composite","judges":[
	  {"type":"file-exists","paths":["a"]},
	  {"type":"diff-review","criteria":"c","llm":{"provider":"decision","model":"jev-latest"}}]}}`
	must.NoError(json.Unmarshal([]byte(src), &task))

	must.ErrorContains(ValidateTree(task.Judge), "usable only on a shadow judge", "JSON suite with a deciding decision judge")

	task.Judge.Judges[1].Shadow = true
	must.NoError(ValidateTree(task.Judge), "JSON suite with a shadow decision judge")
}

func TestPreflightSuite_RefusesDecisionDefaults(t *testing.T) {
	must := require.New(t)
	tasks := []types.EvalTask{{ID: "t", Judge: types.EvalJudge{Type: "diff-review", Criteria: "c"}}}
	err := PreflightSuite(context.Background(), tasks, Options{LLMDefaults: &types.JudgeLLMConfig{Provider: types.JudgeProviderDecision}})
	must.ErrorContains(err, "cannot be the default")
}

func TestDiffReview_DecisionRefusesADecidingJudgment(t *testing.T) {
	must := require.New(t)
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
			must := require.New(t)
			v, err := Evaluate(context.Background(), tc.judge, changedJudgeContext(t, tc.opts))
			must.Error(err)
			must.Equal(types.JudgeStatusError, v.Status)
			must.Contains(v.Reason, "cannot decide a task")
		})
	}
	bodies, _ := srv.requests()
	must.Empty(bodies, "the decision endpoint should not be called")
}

func TestDiffReview_DecisionCachedVerdictIsNotServedToADecidingJudgment(t *testing.T) {
	must := require.New(t)
	srv := newDecisionServer(t, okDecision(decisionPassReply))
	j := decisionJudge(t, srv.srv.URL)
	jctx := nonDeciding(changedJudgeContext(t, Options{Cache: newMemCache(), CacheMode: CacheReadThrough}))
	evaluateOK(t, j, jctx)

	jctx.NonDeciding = false
	v, err := Evaluate(context.Background(), j, jctx)
	must.Error(err, "a stored decision verdict must be refused to a deciding judgment")
	must.Equal(types.JudgeStatusError, v.Status)
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
			must := require.New(t)
			v := evaluateOK(t, j, jctx)
			must.Equal(types.JudgeStatusPass, v.Status, "want pass decided by file-exists")
			must.Contains(fmt.Sprintf("%+v", v.Details), "decision model: p(pass)=0.91", "details should record the decision shadow's verdict")
		})
	}
}

func TestDiffReview_DecisionUnresolvableKeyIsErrorWithoutACall(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := newDecisionServer(t, okDecision(decisionPassReply))
	j := decisionJudge(t, srv.srv.URL)
	t.Setenv("DECISION_UNSET_KEY", "")
	j.LLM.APIKeyRef = "secret://DECISION_UNSET_KEY"

	v, err := Evaluate(context.Background(), j, nonDeciding(changedJudgeContext(t, Options{})))

	must.Error(err)
	must.Equal(types.JudgeStatusError, v.Status)
	must.Contains(v.Reason, "resolving api_key_ref")
	must.NotNil(v.Record, "the decision record should be kept")
	check.Equal(types.JudgeProviderDecision, v.Record.Provider)
	check.NotEmpty(v.Record.InputSHA256)
	bodies, _ := srv.requests()
	must.Empty(bodies, "the decision endpoint should not be called without a key")
}

func TestNewDecisionClient_RejectsAnInvalidBaseURL(t *testing.T) {
	must := require.New(t)
	_, err := newDecisionClient(&http.Client{}, "http://[::1", "k", "jev-latest")
	must.ErrorContains(err, "not a valid URL")
}
