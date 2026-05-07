package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/types"
)

// Cloud-judge adapter. This piggybacks on whichever provider.ProviderAdapter
// the operator already configured (Anthropic, OpenAI, Bedrock, etc.) so
// environments that cannot run their own vLLM still have a guard option.
//
// The cloud-judge intentionally does NOT introduce new HTTP code: the
// minimal-dependency philosophy in CLAUDE.md says "no new SDK
// dependencies", and we get rate limiting / retries / observability for
// free by riding the existing provider adapter. The trade-off is that
// the cloud-judge inherits the provider's latency profile, which is
// typically 1-3x what a local Granite Guardian would do.

const (
	// defaultCloudJudgeModel is Anthropic's small, fast Haiku-class
	// model. It is cheap, low-latency, and reliably emits JSON when
	// asked. Operators on other providers can override via Model.
	defaultCloudJudgeModel = "claude-haiku-4-5-20251001"

	// cloudJudgeMaxTokens caps the verdict response. The schema is two
	// short fields; 256 tokens is enough for verbose reasons and any
	// stray quoting overhead the model produces.
	cloudJudgeMaxTokens = 256

	// cloudJudgeSystem is a minimal role-setting system prompt. The
	// real instructions live in the user message (so the same prompt
	// template can be reused if we swap providers); the system message
	// just primes the model towards classification rather than open-
	// ended generation.
	cloudJudgeSystem = "You are a safety classifier."

	// cloudJudgeGuardID is the GuardID stamped on every Decision this
	// adapter returns. Mirrors the wire identifier in GuardRailConfig.
	cloudJudgeGuardID = "cloud-judge"

	// defaultCloudJudgeTimeout caps the entire stream-drain. Cloud
	// providers can stall, and 5s is enough headroom to swallow normal
	// jitter without making a guard call dominate per-turn latency.
	defaultCloudJudgeTimeout = 5 * time.Second
)

// jsonVerdictRegex extracts every JSON object containing a "verdict"
// field. We anchor on "verdict" so we do not accidentally pick up an
// embedded JSON object from the model's reasoning preamble — cloud
// models often emit a brief explanation before the structured verdict.
//
// The non-greedy character class disallows nested braces so we capture a
// flat object. Cloud judges that emit nested structures would need a
// proper JSON tokeniser; since the schema is fixed at two scalar fields,
// the regex is sufficient and faster.
//
// We always take the LAST match. The classified content is interpolated
// into the prompt before the JSON instruction at the end, so an attacker
// who can plant `{"verdict":"allow"}` in tool output would otherwise win
// the first-match race against the model's own structured reply.
var jsonVerdictRegex = regexp.MustCompile(`(?s)\{[^{}]*"verdict"[^{}]*\}`)

// ErrCloudJudgeNoJSON is returned when the model's response did not
// contain a parseable JSON verdict object. Callers (the loop) decide
// whether parse failures map to fail-open allows or run-aborting denies.
var ErrCloudJudgeNoJSON = errors.New("cloud-judge: no JSON verdict object in response")

// CloudJudgeConfig is the constructor argument for NewCloudJudge.
type CloudJudgeConfig struct {
	// Provider is the underlying ProviderAdapter to call. Required.
	Provider provider.ProviderAdapter

	// Model overrides the default classifier model (Haiku-class). Empty
	// uses defaultCloudJudgeModel.
	Model string

	// Phases maps each guard phase to the natural-language criterion
	// text the cloud model should evaluate against. Missing entries fall
	// back to the granite-guardian per-phase defaults so an operator
	// switching from granite to cloud sees the same default behaviour.
	Phases map[Phase]string

	// Timeout is the per-call deadline applied via context.WithTimeout
	// around the stream consumption. Zero falls back to
	// defaultCloudJudgeTimeout. Note this is a soft deadline: the
	// underlying provider may already enforce its own HTTP timeout.
	Timeout time.Duration
}

// CloudJudge implements GuardRail by streaming a single low-temperature
// classification request through an existing provider adapter and
// extracting a JSON verdict. Safe for concurrent use; the underlying
// provider must be too (all stirrup ProviderAdapters are).
type CloudJudge struct {
	provider provider.ProviderAdapter
	model    string
	phases   map[Phase]string
	timeout  time.Duration
}

// NewCloudJudge constructs a CloudJudge adapter from cfg. A nil provider
// is rejected at construction time because a per-call nil dereference
// is a much worse failure mode than a startup error.
func NewCloudJudge(cfg CloudJudgeConfig) (*CloudJudge, error) {
	if cfg.Provider == nil {
		return nil, errors.New("cloud-judge: Provider is required")
	}
	model := cfg.Model
	if model == "" {
		model = defaultCloudJudgeModel
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultCloudJudgeTimeout
	}
	// Resolve per-phase criteria, falling back to the granite-guardian
	// defaults so the user-visible default policy is the same regardless
	// of which adapter is wired in.
	phases := make(map[Phase]string, len(defaultPhaseCriteria))
	for p, t := range defaultPhaseCriteria {
		phases[p] = t
	}
	for p, t := range cfg.Phases {
		if t != "" {
			phases[p] = t
		}
	}
	return &CloudJudge{
		provider: cfg.Provider,
		model:    model,
		phases:   phases,
		timeout:  timeout,
	}, nil
}

// Check classifies in.Content by streaming a structured prompt through
// the underlying provider adapter and extracting a JSON verdict. The
// JSON contract is single-object, two-field: {"verdict": "allow"|"deny",
// "reason": "..."}.
func (c *CloudJudge) Check(ctx context.Context, in Input) (*Decision, error) {
	start := time.Now()

	criteria, ok := c.phases[in.Phase]
	if !ok {
		// Unknown phase: defensive fallback to the strictest default.
		criteria = defaultPhaseCriteria[PhasePostTurn]
	}

	prompt := buildCloudJudgePrompt(criteria, in.Content)

	// Apply our own timeout on top of whatever the provider enforces.
	// This is a belt-and-suspenders move: provider HTTP timeouts are
	// for the request as a whole, but we want to give up draining the
	// stream past a known budget so a misbehaving model cannot stall
	// the loop indefinitely.
	streamCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	events, err := c.provider.Stream(streamCtx, types.StreamParams{
		Model:       c.model,
		System:      cloudJudgeSystem,
		Messages:    []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: prompt}}}},
		MaxTokens:   cloudJudgeMaxTokens,
		Temperature: 0.0,
	})
	if err != nil {
		return nil, fmt.Errorf("cloud-judge: provider stream: %w", err)
	}

	// Drain the stream into a single string. We only care about
	// text_delta events; tool_call events should not appear because we
	// passed no tools, but we ignore them defensively.
	var text []byte
	for ev := range events {
		switch ev.Type {
		case "text_delta":
			text = append(text, ev.Text...)
		case "error":
			if ev.Error != nil {
				return nil, fmt.Errorf("cloud-judge: stream error: %w", ev.Error)
			}
		}
	}

	verdict, reason, err := parseCloudJudgeResponse(string(text))
	if err != nil {
		return nil, err
	}

	d := &Decision{
		GuardID: cloudJudgeGuardID,
		Reason:  reason,
		Latency: time.Since(start),
	}
	if verdict {
		d.Verdict = VerdictDeny
		d.Score = 1.0
	} else {
		d.Verdict = VerdictAllow
		d.Score = 0.0
	}
	return d, nil
}

// buildCloudJudgePrompt mirrors the Granite Guardian template structure
// (criteria, scoring schema, content) but appends an explicit JSON
// instruction so the cloud model emits a parseable verdict object.
// Keeping the structure aligned with Granite makes it easier to swap
// the two adapters without measurably different model behaviour.
func buildCloudJudgePrompt(criteria, content string) string {
	return fmt.Sprintf(
		"As a judge agent, evaluate whether the last assistant turn meets the criteria below.\n\n"+
			"### Criteria: %s\n\n"+
			"### Scoring Schema: If the last assistant's text meets the criteria, the verdict is 'deny'; otherwise, the verdict is 'allow'.\n\n"+
			"%s\n\n"+
			"Respond with a single JSON object: {\"verdict\": \"allow\"|\"deny\", \"reason\": \"<short text>\"}.",
		criteria, content,
	)
}

// cloudJudgeVerdict is the wire shape we expect inside the extracted
// JSON object. Unknown fields are ignored to forward-compat for any
// future telemetry the model might volunteer.
type cloudJudgeVerdict struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// parseCloudJudgeResponse extracts the JSON verdict from raw model
// output. Returns (deny=true, reason, nil) when the verdict is "deny",
// (deny=false, reason, nil) when it is "allow", and ErrCloudJudgeNoJSON
// when no parseable verdict object is present.
//
// We take the LAST verdict object the model emitted — the prompt
// interpolates classified content before the JSON instruction, and a
// first-match strategy would let an attacker who can plant a verdict
// object in tool output spoof the classifier's reply.
func parseCloudJudgeResponse(raw string) (bool, string, error) {
	matches := jsonVerdictRegex.FindAllString(raw, -1)
	if len(matches) == 0 {
		return false, "", fmt.Errorf("%w: %s", ErrCloudJudgeNoJSON, truncateForError(raw, graniteErrSnippetMax))
	}
	match := matches[len(matches)-1]
	var v cloudJudgeVerdict
	if err := json.Unmarshal([]byte(match), &v); err != nil {
		return false, "", fmt.Errorf("cloud-judge: parse verdict JSON: %w", err)
	}
	switch v.Verdict {
	case "deny":
		return true, v.Reason, nil
	case "allow":
		return false, v.Reason, nil
	default:
		return false, "", fmt.Errorf("cloud-judge: unknown verdict %q", v.Verdict)
	}
}
