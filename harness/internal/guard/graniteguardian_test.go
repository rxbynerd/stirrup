package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServer wraps an httptest.Server and exposes a request counter plus
// the most recently captured request body so tests can both count
// outbound calls (skip-path assertions) and inspect what was sent
// (prompt-template assertions).
type fakeServer struct {
	srv      *httptest.Server
	requests int32
	lastBody []byte
	lastURL  string
}

// newFakeGraniteServer returns a fakeServer that always responds with
// the given chat-completions content payload. The handler captures the
// request body for later inspection.
func newFakeGraniteServer(t *testing.T, responseContent string, status int) *fakeServer {
	t.Helper()
	fs := &fakeServer{}
	fs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fs.requests, 1)
		body, _ := io.ReadAll(r.Body)
		fs.lastBody = body
		fs.lastURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		// Encode a minimal OpenAI-compatible chat-completions response.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": responseContent}},
			},
		})
	}))
	t.Cleanup(fs.srv.Close)
	return fs
}

// firstUserMessageContent extracts the first user message's content from
// the captured request body. Returns "" if absent so callers can use
// strings.Contains assertions without a nil check.
func (fs *fakeServer) firstUserMessageContent(t *testing.T) string {
	t.Helper()
	if len(fs.lastBody) == 0 {
		t.Fatalf("no captured request body")
	}
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(fs.lastBody, &req); err != nil {
		t.Fatalf("unmarshal captured body: %v", err)
	}
	for _, m := range req.Messages {
		if m.Role == "user" {
			return m.Content
		}
	}
	return ""
}

func TestGraniteGuardianAllowPath(t *testing.T) {
	fs := newFakeGraniteServer(t, "<score>no</score>", http.StatusOK)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{Endpoint: fs.srv.URL})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	d, err := g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "the model said something benign"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Verdict != VerdictAllow {
		t.Fatalf("verdict = %q, want allow", d.Verdict)
	}
	if d.GuardID != guardianGuardID {
		t.Fatalf("guard id = %q, want %q", d.GuardID, guardianGuardID)
	}
	if d.Score != 0.0 {
		t.Fatalf("score = %v, want 0.0", d.Score)
	}
}

func TestGraniteGuardianDenyPath(t *testing.T) {
	fs := newFakeGraniteServer(t, "<score>yes</score>", http.StatusOK)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{Endpoint: fs.srv.URL})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	d, err := g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "rm -rf /"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Verdict != VerdictDeny {
		t.Fatalf("verdict = %q, want deny", d.Verdict)
	}
	if d.Score != 1.0 {
		t.Fatalf("score = %v, want 1.0", d.Score)
	}
	if d.Reason == "" {
		t.Fatalf("expected non-empty reason on deny")
	}
}

func TestGraniteGuardianThinkMode(t *testing.T) {
	fs := newFakeGraniteServer(t, "<think>this looks malicious</think><score>yes</score>", http.StatusOK)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint: fs.srv.URL,
		Think:    true,
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	d, err := g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "payload"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Verdict != VerdictDeny {
		t.Fatalf("verdict = %q, want deny", d.Verdict)
	}
	// The request body should carry the <think> directive inside the
	// <guardian> envelope. We assert the literal sequence rather than a
	// regex because the placement is load-bearing per the spec.
	got := fs.firstUserMessageContent(t)
	if !strings.Contains(got, "<guardian><think>") {
		t.Fatalf("user message did not contain <guardian><think>; got: %s", got)
	}
}

func TestGraniteGuardianPromptTemplateEmission(t *testing.T) {
	fs := newFakeGraniteServer(t, "<score>no</score>", http.StatusOK)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{Endpoint: fs.srv.URL})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if _, err := g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "x"}); err != nil {
		t.Fatalf("Check: %v", err)
	}
	got := fs.firstUserMessageContent(t)
	if !strings.HasPrefix(got, "<guardian><no-think>As a judge agent") {
		t.Fatalf("prompt did not begin with expected envelope; got: %s", got)
	}
	if !strings.Contains(got, "### Criteria:") {
		t.Fatalf("prompt missing '### Criteria:' header; got: %s", got)
	}
	if !strings.Contains(got, "### Scoring Schema:") {
		t.Fatalf("prompt missing '### Scoring Schema:' header; got: %s", got)
	}
}

func TestGraniteGuardianCustomCriteria(t *testing.T) {
	fs := newFakeGraniteServer(t, "<score>no</score>", http.StatusOK)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint:       fs.srv.URL,
		Criteria:       []string{"my_rule"},
		CustomCriteria: map[string]string{"my_rule": "reject curse words"},
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if _, err := g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "x"}); err != nil {
		t.Fatalf("Check: %v", err)
	}
	got := fs.firstUserMessageContent(t)
	if !strings.Contains(got, "reject curse words") {
		t.Fatalf("prompt did not contain custom criterion; got: %s", got)
	}
}

func TestGraniteGuardianUnknownCriterionAtConstruction(t *testing.T) {
	// We use a syntactically valid endpoint so we exercise the criteria
	// validation path specifically (not the endpoint validation path).
	_, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint: "http://example.invalid",
		Criteria: []string{"nonexistent"},
	})
	if err == nil {
		t.Fatalf("expected error for unknown criterion, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Fatalf("error did not mention the unknown id: %v", err)
	}
}

func TestGraniteGuardianMinChunkCharsSkip(t *testing.T) {
	fs := newFakeGraniteServer(t, "<score>no</score>", http.StatusOK)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint:      fs.srv.URL,
		MinChunkChars: 256,
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	d, err := g.Check(context.Background(), Input{
		Phase:   PhasePreTurn,
		Content: "tiny",
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Verdict != VerdictAllow {
		t.Fatalf("verdict = %q, want allow", d.Verdict)
	}
	if d.Reason != ReasonSkippedMinChunk {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonSkippedMinChunk)
	}
	// Critically: no HTTP request was issued. This is the whole point of
	// the optimisation — without it, sub-256-char chunks dominate
	// PreTurn latency.
	if got := atomic.LoadInt32(&fs.requests); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}

func TestGraniteGuardianMinChunkCharsSkipOnlyAppliesToPreTurn(t *testing.T) {
	// Even with a high MinChunkChars, post-turn / pre-tool must still
	// classify — those phases see model-authored content where length
	// is not a useful proxy for risk.
	fs := newFakeGraniteServer(t, "<score>no</score>", http.StatusOK)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint:      fs.srv.URL,
		MinChunkChars: 100000,
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if _, err := g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "small"}); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got := atomic.LoadInt32(&fs.requests); got != 1 {
		t.Fatalf("requests = %d, want 1 (skip should not apply to PostTurn)", got)
	}
}

func TestGraniteGuardianHTTPErrorReturnsError(t *testing.T) {
	fs := newFakeGraniteServer(t, "upstream broken", http.StatusBadGateway)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{Endpoint: fs.srv.URL})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "x"})
	if err == nil {
		t.Fatalf("expected error on HTTP 502, got nil")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("error did not mention status code: %v", err)
	}
}

func TestGraniteGuardianMalformedBodyReturnsError(t *testing.T) {
	// Response that decodes as JSON but contains no <score> tag.
	fs := newFakeGraniteServer(t, "the model went off-script", http.StatusOK)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{Endpoint: fs.srv.URL})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "x"})
	if err == nil {
		t.Fatalf("expected ErrParseFailed, got nil")
	}
	if !errors.Is(err, ErrParseFailed) {
		t.Fatalf("error chain missing ErrParseFailed: %v", err)
	}
}

func TestGraniteGuardianDefaultPhaseCriteria(t *testing.T) {
	// Each phase's default criterion text contains a distinctive
	// fragment we can grep for. These assertions also act as a guard
	// against accidentally swapping the default texts at refactor time.
	cases := []struct {
		name  string
		phase Phase
		want  string
	}{
		{"pre_turn", PhasePreTurn, "attempts to override the assistant's instructions"},
		{"pre_tool", PhasePreTool, "malformed, hallucinated, or refers to a non-existent function"},
		{"post_turn", PhasePostTurn, "AWS access key IDs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeGraniteServer(t, "<score>no</score>", http.StatusOK)
			g, err := NewGraniteGuardian(GraniteGuardianConfig{Endpoint: fs.srv.URL})
			if err != nil {
				t.Fatalf("construct: %v", err)
			}
			// Pad the content for PreTurn so MinChunkChars does not skip it.
			content := strings.Repeat("a", defaultMinChunkChars+1)
			if _, err := g.Check(context.Background(), Input{Phase: tc.phase, Content: content}); err != nil {
				t.Fatalf("Check: %v", err)
			}
			got := fs.firstUserMessageContent(t)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("default phase criterion missing %q in prompt; got: %s", tc.want, got)
			}
		})
	}
}

func TestGraniteGuardianEndpointURLComposition(t *testing.T) {
	// Both bare-host endpoints and pre-pinned-path endpoints should
	// resolve to a working POST URL. We use a single httptest server but
	// drive the adapter twice with two different endpoint forms.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "<score>no</score>"}}},
		})
	}))
	defer srv.Close()

	// 1) Bare endpoint: adapter must append /v1/chat/completions.
	g1, err := NewGraniteGuardian(GraniteGuardianConfig{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("construct bare: %v", err)
	}
	if !strings.HasSuffix(g1.endpoint, "/v1/chat/completions") {
		t.Fatalf("bare endpoint not appended: %s", g1.endpoint)
	}

	// 2) Pre-pinned path: adapter must use the operator's path verbatim.
	pinned := srv.URL + "/v1/chat/completions"
	g2, err := NewGraniteGuardian(GraniteGuardianConfig{Endpoint: pinned})
	if err != nil {
		t.Fatalf("construct pinned: %v", err)
	}
	if g2.endpoint != pinned {
		t.Fatalf("pinned endpoint mutated: got %s, want %s", g2.endpoint, pinned)
	}

	// Both adapters should successfully classify against the same server.
	for i, g := range []*GraniteGuardian{g1, g2} {
		if _, err := g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "x"}); err != nil {
			t.Fatalf("adapter[%d] Check: %v", i, err)
		}
	}
}

func TestGraniteGuardianContextCancellation(t *testing.T) {
	// The handler blocks until the test releases it; we cancel the
	// context immediately so the adapter must return ctx.Err() rather
	// than hanging on the HTTP round trip.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(release)

	g, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint: srv.URL,
		Timeout:  5 * time.Second, // generous; we want ctx cancellation, not timeout
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before issuing the call

	_, err = g.Check(ctx, Input{Phase: PhasePostTurn, Content: "x"})
	if err == nil {
		t.Fatalf("expected error from cancelled context, got nil")
	}
	// The error should carry context.Canceled (wrapped).
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error chain missing context.Canceled: %v", err)
	}
}

func TestGraniteGuardianBatchedPreTurnSingleCall(t *testing.T) {
	// Batched PreTurn is conceptually a "single HTTP call regardless of
	// chunk count" — the loop concatenates chunks before calling Check.
	// The adapter just trusts the input and emits one request.
	fs := newFakeGraniteServer(t, "<score>no</score>", http.StatusOK)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint:      fs.srv.URL,
		MinChunkChars: 256,
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	// Build ~500 bytes of "batched" content with chunk delimiters, just
	// like the loop will. Source carries the batched marker for any
	// future telemetry consumers.
	var b strings.Builder
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&b, "--- chunk %d ---\n%s\n", i, strings.Repeat("x", 80))
	}
	if _, err := g.Check(context.Background(), Input{
		Phase:   PhasePreTurn,
		Content: b.String(),
		Source:  "batched:n=5",
	}); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got := atomic.LoadInt32(&fs.requests); got != 1 {
		t.Fatalf("requests = %d, want 1 (batched PreTurn must fold to one call)", got)
	}
}

func TestGraniteGuardianRejectsEmptyEndpoint(t *testing.T) {
	_, err := NewGraniteGuardian(GraniteGuardianConfig{})
	if err == nil {
		t.Fatalf("expected error for empty endpoint, got nil")
	}
}

func TestGraniteGuardianRejectsNonHTTPScheme(t *testing.T) {
	_, err := NewGraniteGuardian(GraniteGuardianConfig{Endpoint: "file:///etc/passwd"})
	if err == nil {
		t.Fatalf("expected error for file:// scheme, got nil")
	}
}
