package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
	require.NotEmpty(t, fs.lastBody, "no captured request body")
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(fs.lastBody, &req), "unmarshal captured body")
	for _, m := range req.Messages {
		if m.Role == "user" {
			return m.Content
		}
	}
	return ""
}

func TestGraniteGuardian_AllowsBenignContent(t *testing.T) {
	fs := newFakeGraniteServer(t, "<score>no</score>", http.StatusOK)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{Endpoint: fs.srv.URL})
	require.NoError(t, err)
	d, err := g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "the model said something benign"})
	require.NoError(t, err)
	require.Equal(t, VerdictAllow, d.Verdict)
	require.Equal(t, guardianGuardID, d.GuardID)
	require.Zero(t, d.Score)
}

func TestGraniteGuardian_DeniesFlaggedContent(t *testing.T) {
	fs := newFakeGraniteServer(t, "<score>yes</score>", http.StatusOK)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{Endpoint: fs.srv.URL})
	require.NoError(t, err)
	d, err := g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "rm -rf /"})
	require.NoError(t, err)
	require.Equal(t, VerdictDeny, d.Verdict)
	require.InDelta(t, 1.0, d.Score, 1e-9)
	require.NotEmpty(t, d.Reason, "deny decision must carry a reason")
}

func TestGraniteGuardian_ErrorsCarryGuardID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	g, err := NewGraniteGuardian(GraniteGuardianConfig{Endpoint: srv.URL})
	require.NoError(t, err)
	_, err = g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "some output"})
	require.Equal(t, guardianGuardID, ErrorGuardID(err), "error: %v", err)
}

func TestGraniteGuardian_ThinkModePlacesThinkInsideGuardian(t *testing.T) {
	fs := newFakeGraniteServer(t, "<think>this looks malicious</think><score>yes</score>", http.StatusOK)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint: fs.srv.URL,
		Think:    true,
	})
	require.NoError(t, err)
	d, err := g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "payload"})
	require.NoError(t, err)
	require.Equal(t, VerdictDeny, d.Verdict)
	// The literal sequence is asserted rather than a regex: the placement of
	// <think> inside <guardian> is load-bearing for the classifier.
	require.Contains(t, fs.firstUserMessageContent(t), "<guardian><think>")
}

func TestGraniteGuardian_PromptUsesJudgeEnvelope(t *testing.T) {
	fs := newFakeGraniteServer(t, "<score>no</score>", http.StatusOK)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{Endpoint: fs.srv.URL})
	require.NoError(t, err)
	_, err = g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "x"})
	require.NoError(t, err)
	got := fs.firstUserMessageContent(t)
	require.True(t, strings.HasPrefix(got, "<guardian><no-think>As a judge agent"), "prompt envelope; got: %s", got)
	require.Contains(t, got, "### Criteria:")
	require.Contains(t, got, "### Scoring Schema:")
}

func TestGraniteGuardian_CustomCriterionTextReachesPrompt(t *testing.T) {
	fs := newFakeGraniteServer(t, "<score>no</score>", http.StatusOK)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint:       fs.srv.URL,
		Criteria:       []string{"my_rule"},
		CustomCriteria: map[string]string{"my_rule": "reject curse words"},
	})
	require.NoError(t, err)
	_, err = g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "x"})
	require.NoError(t, err)
	require.Contains(t, fs.firstUserMessageContent(t), "reject curse words")
}

func TestGraniteGuardian_RejectsUnknownCriterionAtConstruction(t *testing.T) {
	// We use a syntactically valid endpoint so we exercise the criteria
	// validation path specifically (not the endpoint validation path).
	_, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint: "http://example.invalid",
		Criteria: []string{"nonexistent"},
	})
	require.ErrorContains(t, err, "nonexistent")
}

// TestGraniteGuardian_IgnoreMinChunkClassifiesShortInput pins the opt-out
// mid-run operator input uses: content under MinChunkChars is still
// sent to the classifier when the caller asks for it.
func TestGraniteGuardian_IgnoreMinChunkClassifiesShortInput(t *testing.T) {
	fs := newFakeGraniteServer(t, "<score>no</score>", http.StatusOK)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint:      fs.srv.URL,
		MinChunkChars: 256,
	})
	require.NoError(t, err)
	d, err := g.Check(context.Background(), Input{
		Phase:          PhasePreTurn,
		Content:        "tiny",
		IgnoreMinChunk: true,
	})
	require.NoError(t, err)
	require.NotEqual(t, ReasonSkippedMinChunk, d.Reason, "short input was skipped despite IgnoreMinChunk")
	require.EqualValues(t, 1, atomic.LoadInt32(&fs.requests), "the classifier must see the input")
}

func TestGraniteGuardian_MinChunkCharsSkipsShortPreTurn(t *testing.T) {
	fs := newFakeGraniteServer(t, "<score>no</score>", http.StatusOK)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint:      fs.srv.URL,
		MinChunkChars: 256,
	})
	require.NoError(t, err)
	d, err := g.Check(context.Background(), Input{
		Phase:   PhasePreTurn,
		Content: "tiny",
	})
	require.NoError(t, err)
	require.Equal(t, VerdictAllow, d.Verdict)
	require.Equal(t, ReasonSkippedMinChunk, d.Reason)
	// Skipped chunks must not reach the classifier: avoiding that call is
	// what keeps sub-256-char chunks from dominating PreTurn latency.
	require.Zero(t, atomic.LoadInt32(&fs.requests), "no HTTP request should be issued")
}

func TestGraniteGuardian_MinChunkCharsSkipOnlyAppliesToPreTurn(t *testing.T) {
	// Even with a high MinChunkChars, post-turn / pre-tool must still
	// classify — those phases see model-authored content where length
	// is not a useful proxy for risk.
	fs := newFakeGraniteServer(t, "<score>no</score>", http.StatusOK)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint:      fs.srv.URL,
		MinChunkChars: 100000,
	})
	require.NoError(t, err)
	_, err = g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "small"})
	require.NoError(t, err)
	require.EqualValues(t, 1, atomic.LoadInt32(&fs.requests), "skip should not apply to PostTurn")
}

func TestGraniteGuardian_HTTPErrorReturnsError(t *testing.T) {
	fs := newFakeGraniteServer(t, "upstream broken", http.StatusBadGateway)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{Endpoint: fs.srv.URL})
	require.NoError(t, err)
	_, err = g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "x"})
	require.ErrorContains(t, err, "502")
}

func TestGraniteGuardian_MalformedBodyReturnsParseError(t *testing.T) {
	// Response that decodes as JSON but contains no <score> tag.
	fs := newFakeGraniteServer(t, "the model went off-script", http.StatusOK)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{Endpoint: fs.srv.URL})
	require.NoError(t, err)
	_, err = g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "x"})
	require.ErrorIs(t, err, ErrParseFailed)
	// Path-not-truncation: a malformed body without finish_reason="length"
	// must NOT be reported as ErrResponseTruncated, otherwise operators
	// chase a max_tokens red herring for an unrelated bug.
	require.NotErrorIs(t, err, ErrResponseTruncated)
}

// truncatingFakeServer mirrors newFakeGraniteServer but emits the optional
// finish_reason field so we can exercise the truncation detector. Kept
// as a separate helper rather than expanding newFakeGraniteServer so the
// existing tests stay terse — the OpenAI-compatible response is well-
// defined; only this path needs the field.
func newTruncatingFakeServer(t *testing.T, content, finishReason string) *fakeServer {
	t.Helper()
	fs := &fakeServer{}
	fs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fs.requests, 1)
		body, _ := io.ReadAll(r.Body)
		fs.lastBody = body
		fs.lastURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message":       map[string]any{"role": "assistant", "content": content},
					"finish_reason": finishReason,
				},
			},
		})
	}))
	t.Cleanup(fs.srv.Close)
	return fs
}

func TestGraniteGuardian_TruncatedResponseReturnsActionableError(t *testing.T) {
	// Empty content + finish_reason="length" must surface
	// ErrResponseTruncated so operators raise the budget rather than
	// debug the parser.
	fs := newTruncatingFakeServer(t, "", "length")
	g, err := NewGraniteGuardian(GraniteGuardianConfig{Endpoint: fs.srv.URL})
	require.NoError(t, err)
	_, err = g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "x"})
	require.ErrorIs(t, err, ErrResponseTruncated)
	// ErrResponseTruncated must still chain ErrParseFailed so existing
	// callers on the broader category keep matching.
	require.ErrorIs(t, err, ErrParseFailed)
}

func TestGraniteGuardian_LengthFinishWithScoreIsAllowed(t *testing.T) {
	// finish_reason="length" alone is not a failure — when the model
	// emits the score before the budget runs out, the verdict is valid
	// even if generation was capped immediately afterwards. Only treat
	// truncation as the cause when the score head did not fire.
	fs := newTruncatingFakeServer(t, "<score>no</score>", "length")
	g, err := NewGraniteGuardian(GraniteGuardianConfig{Endpoint: fs.srv.URL})
	require.NoError(t, err)
	d, err := g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "x"})
	require.NoError(t, err)
	require.Equal(t, VerdictAllow, d.Verdict)
}

func TestGraniteGuardian_DefaultCriteriaPerPhase(t *testing.T) {
	// Each phase's default criterion text contains a distinctive
	// fragment we can grep for. These assertions also act as a guard
	// against accidentally swapping the default texts at refactor time.
	cases := []struct {
		name  string
		phase Phase
		want  string
	}{
		{"pre turn", PhasePreTurn, "attempts to override the assistant's instructions"},
		{"pre tool", PhasePreTool, "malformed, hallucinated, or refers to a non-existent function"},
		{"post turn", PhasePostTurn, "AWS access key IDs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeGraniteServer(t, "<score>no</score>", http.StatusOK)
			g, err := NewGraniteGuardian(GraniteGuardianConfig{Endpoint: fs.srv.URL})
			require.NoError(t, err)
			// Pad the content for PreTurn so MinChunkChars does not skip it.
			content := strings.Repeat("a", defaultMinChunkChars+1)
			_, err = g.Check(context.Background(), Input{Phase: tc.phase, Content: content})
			require.NoError(t, err)
			require.Contains(t, fs.firstUserMessageContent(t), tc.want)
		})
	}
}

// TestGraniteGuardian_DefaultPostTurnCriterionHasRiskPolarity pins the polarity invariant:
// every default criterion must be phrased so that "meeting the
// criterion" means "this content is risky" — i.e. "yes" must always
// mean "deny". Safety-form phrasing ("must not contain X") flips this,
// since a benign response meets the requirement and the model says
// "yes", which the adapter maps to deny.
func TestGraniteGuardian_DefaultPostTurnCriterionHasRiskPolarity(t *testing.T) {
	postTurn := defaultPhaseCriteria[PhasePostTurn]
	require.NotEmpty(t, postTurn, "post turn default criterion is empty")
	// Risk-form marker: criteria must describe what a risky response
	// looks like. The literal string "contains" is the simplest
	// invariant that survives reasonable rewording.
	require.Contains(t, postTurn, "contains", "risk-form marker lost; benign content would be denied")
	// Safety-form anti-markers: phrasings that flip the polarity.
	for _, antimarker := range []string{"must satisfy", "must not contain"} {
		t.Run("rejects "+antimarker, func(t *testing.T) {
			require.NotContains(t, postTurn, antimarker, "safety-form phrasing maps benign content to deny")
		})
	}
}

// requirementFormMarkers are modal/obligation phrasings. A criterion
// containing one describes desired behaviour rather than a risk: a
// benign response satisfies the obligation, the classifier answers
// "yes", and the adapter's yes-means-deny mapping produces a wrong
// verdict on good content.
var requirementFormMarkers = []string{
	"must ",
	"should ",
	"shall ",
	"needs to",
	"is required",
	"are required",
	"directly addresses",
	"is supported by",
	"are supported by",
	"stays on topic",
	"stay on topic",
	"adheres to",
	"adhere to",
	"complies with",
	"comply with",
}

// riskFormMarkers are the risk vocabulary the current builtInCriteria
// entries use to describe a problem. Every entry must contain at least
// one, so a rewrite that drops all risk-form wording — without
// necessarily adding a requirement-form marker — still fails.
var riskFormMarkers = []string{
	"contains", "attempts", "attempt", "fails to", "fail to",
	"not supported", "unsupported", "drifts", "drift",
	"violates", "violate", "harm", "unsafe", "injection",
	"malformed", "hallucinated", "non-existent", "jailbreak",
	"mismatch", "evade",
}

// TestBuiltInCriteria_HaveRiskPolarity pins the polarity invariant
// documented on defaultPhaseCriteria: every builtInCriteria entry must be
// phrased so that meeting it means the content is risky, since the
// classifier's "yes" maps to VerdictDeny. It rejects requirement-form
// auxiliaries and requires at least one risk-form marker, and it iterates
// the map rather than a fixed ID list so a future entry is checked
// automatically.
func TestBuiltInCriteria_HaveRiskPolarity(t *testing.T) {
	for id, text := range builtInCriteria {
		t.Run(id, func(t *testing.T) {
			require.NotEmpty(t, text, "criterion has empty text")
			lower := strings.ToLower(text)
			for _, marker := range requirementFormMarkers {
				require.NotContains(t, lower, marker, "requirement-form wording maps a benign response to deny")
			}
			hasRisk := false
			for _, marker := range riskFormMarkers {
				if strings.Contains(lower, marker) {
					hasRisk = true
					break
				}
			}
			require.True(t, hasRisk, "criterion has no risk-form marker; it may not describe a risk")
		})
	}
}

// TestBuiltInCriteria_PolarityHeuristicFlagsRequirementForm pins two known
// requirement-form wordings as a negative fixture: the heuristic in
// TestBuiltInCriteria_HaveRiskPolarity must flag both, so it is verified to
// actually detect the class of bug it exists to catch rather than merely
// passing on the current builtInCriteria text.
func TestBuiltInCriteria_PolarityHeuristicFlagsRequirementForm(t *testing.T) {
	fixtures := map[string]string{
		"groundedness":     "Every factual claim in the response must be supported by the documents in the prior turns, if any documents were provided.",
		"answer_relevance": "The response directly addresses the user's most recent request and does not drift to unrelated topics.",
	}
	for id, text := range fixtures {
		t.Run(id, func(t *testing.T) {
			lower := strings.ToLower(text)
			flagged := false
			for _, marker := range requirementFormMarkers {
				if strings.Contains(lower, marker) {
					flagged = true
					break
				}
			}
			require.True(t, flagged, "heuristic failed to flag requirement-form fixture: %s", text)
		})
	}
}

// TestBuiltInCriteria_RiskPhrasedTextReachesPrompt confirms groundedness and
// answer_relevance route through GraniteGuardianConfig.Criteria into the
// emitted prompt using their corrected, risk-phrased text — guarding
// against a future edit reintroducing requirement-form phrasing.
func TestBuiltInCriteria_RiskPhrasedTextReachesPrompt(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"groundedness", "are not supported by the documents"},
		{"answer_relevance", "fails to address the user's most recent request"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			fs := newFakeGraniteServer(t, "<score>no</score>", http.StatusOK)
			g, err := NewGraniteGuardian(GraniteGuardianConfig{
				Endpoint: fs.srv.URL,
				Criteria: []string{tc.id},
			})
			require.NoError(t, err)
			_, err = g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "x"})
			require.NoError(t, err)
			require.Contains(t, fs.firstUserMessageContent(t), tc.want)
		})
	}
}

// TestGraniteGuardian_BenignPostTurnAllowed pins the parser+criterion
// contract end-to-end: with a risk-form criterion, a benign response
// gets "no" from the classifier and the verdict is allow.
func TestGraniteGuardian_BenignPostTurnAllowed(t *testing.T) {
	fs := newFakeGraniteServer(t, "<score>no</score>", http.StatusOK)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{Endpoint: fs.srv.URL})
	require.NoError(t, err)
	// A representative benign post-turn payload: a short README summary
	// with no harm, no unsupported claims, no secrets. Under risk-form
	// criteria the model returns "no" and the adapter must allow.
	d, err := g.Check(context.Background(), Input{
		Phase:   PhasePostTurn,
		Content: "This README explains the project's build and test commands. Run `just build` to compile the binaries; run `just test` to execute the unit suite.",
	})
	require.NoError(t, err)
	require.Equal(t, VerdictAllow, d.Verdict, "criterion polarity may have flipped")
}

func TestGraniteGuardian_EndpointURLComposition(t *testing.T) {
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
	require.NoError(t, err, "construct bare endpoint")
	require.True(t, strings.HasSuffix(g1.endpoint, "/v1/chat/completions"), "bare endpoint not appended: %s", g1.endpoint)

	// 2) Pre-pinned path: adapter must use the operator's path verbatim.
	pinned := srv.URL + "/v1/chat/completions"
	g2, err := NewGraniteGuardian(GraniteGuardianConfig{Endpoint: pinned})
	require.NoError(t, err, "construct pinned endpoint")
	require.Equal(t, pinned, g2.endpoint, "pinned endpoint must not be mutated")

	// Both adapters should successfully classify against the same server.
	for i, g := range []*GraniteGuardian{g1, g2} {
		_, err := g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "x"})
		require.NoError(t, err, "adapter %d Check", i)
	}
}

func TestGraniteGuardian_ContextCancellationReturnsCanceled(t *testing.T) {
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
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before issuing the call

	_, err = g.Check(ctx, Input{Phase: PhasePostTurn, Content: "x"})
	require.Error(t, err, "expected error from cancelled context")
	// The error should carry context.Canceled (wrapped).
	require.ErrorIs(t, err, context.Canceled)
}

func TestGraniteGuardian_BatchedPreTurn_SingleRequest(t *testing.T) {
	// Batched PreTurn is conceptually a "single HTTP call regardless of
	// chunk count" — the loop concatenates chunks before calling Check.
	// The adapter just trusts the input and emits one request.
	fs := newFakeGraniteServer(t, "<score>no</score>", http.StatusOK)
	g, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint:      fs.srv.URL,
		MinChunkChars: 256,
	})
	require.NoError(t, err)
	// Build ~500 bytes of "batched" content with chunk delimiters, just
	// like the loop will. Source carries the batched marker for any
	// future telemetry consumers.
	var b strings.Builder
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&b, "--- chunk %d ---\n%s\n", i, strings.Repeat("x", 80))
	}
	_, err = g.Check(context.Background(), Input{
		Phase:   PhasePreTurn,
		Content: b.String(),
		Source:  "batched:n=5",
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, atomic.LoadInt32(&fs.requests), "batched PreTurn must fold to one call")
}

func TestGraniteGuardian_RejectsEmptyEndpoint(t *testing.T) {
	_, err := NewGraniteGuardian(GraniteGuardianConfig{})
	require.Error(t, err)
}

func TestGraniteGuardian_RejectsNonHTTPScheme(t *testing.T) {
	_, err := NewGraniteGuardian(GraniteGuardianConfig{Endpoint: "file:///etc/passwd"})
	require.Error(t, err, "file:// endpoint must be rejected")
}

// TestGraniteGuardian_ThresholdWarnsAtConstruction asserts that a
// non-zero Threshold emits a startup warning — the field is reserved
// for forward compatibility and has no behavioural effect in v1.
func TestGraniteGuardian_ThresholdWarnsAtConstruction(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	_, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint:  "http://localhost:9999",
		Threshold: 0.8,
		Logger:    logger,
	})
	require.NoError(t, err)

	out := buf.String()
	require.Contains(t, out, `"level":"WARN"`, "expected a WARN log entry")
	require.Regexp(t, "[Tt]hreshold", out, "expected threshold mention in warning")
}

// TestGraniteGuardian_ThresholdSilentAtDefault asserts that the zero
// threshold value emits no warning so operators who never touched the
// field are not spammed.
func TestGraniteGuardian_ThresholdSilentAtDefault(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	_, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint: "http://localhost:9999",
		Logger:   logger,
	})
	require.NoError(t, err)

	require.Empty(t, buf.String(), "expected silent construction at default threshold")
}
