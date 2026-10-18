package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
	tracereader "github.com/rxbynerd/stirrup/types/trace"
	"github.com/rxbynerd/stirrup/types/version"
)

// paramCapturingProvider replays scripted streams and keeps the
// StreamParams of every request it receives.
type paramCapturingProvider struct {
	mu      sync.Mutex
	scripts [][]types.StreamEvent
	params  []types.StreamParams
}

func (p *paramCapturingProvider) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := len(p.params)
	if idx >= len(p.scripts) {
		idx = len(p.scripts) - 1
	}
	p.params = append(p.params, params)
	events := p.scripts[idx]
	ch := make(chan types.StreamEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func TestRun_RecordsRequestParamsSentToProvider(t *testing.T) {
	temperature := 0.35
	prov := &paramCapturingProvider{scripts: [][]types.StreamEvent{
		{
			{Type: "text_delta", Text: "answer without tools"},
			{Type: "message_complete", StopReason: "end_turn", ResponseModel: "claude-sonnet-4-6-20260101"},
		},
		toolCallTurn(),
		{{Type: "text_delta", Text: "done"}, {Type: "message_complete", StopReason: "end_turn"}},
	}}
	loop, _ := buildEscalationLoop(&sequencedProvider{scripts: [][]types.StreamEvent{noToolAnswer()}}, newDefaultEscalationPolicy(1, nativeCaps))
	loop.Provider = prov
	rec := &recordingTraceEmitter{}
	loop.Trace = rec

	config := escalationConfig("execution")
	config.Temperature = &temperature
	config.ReasoningEffort = "high"
	config.ReasoningSummary = "off"
	if _, err := loop.Run(context.Background(), config); err != nil {
		t.Fatalf("Run: %v", err)
	}

	rec.mu.Lock()
	records := append([]types.TurnRecord(nil), rec.turnRecords...)
	turns := append([]types.TurnTrace(nil), rec.turns...)
	rec.mu.Unlock()
	if len(records) < 2 || len(prov.params) < 2 {
		t.Fatalf("got %d turn records and %d requests, want at least 2 of each", len(records), len(prov.params))
	}
	for i, record := range records {
		sent := prov.params[i]
		got := record.ModelInput.Request
		if got == nil {
			t.Fatalf("turn %d: ModelInput.Request is nil", i)
		}
		want := types.RequestParams{
			Temperature:      sent.Temperature,
			MaxTokens:        sent.MaxTokens,
			ReasoningEffort:  sent.ReasoningEffort,
			ReasoningSummary: sent.ReasoningSummary,
			ToolChoice:       sent.ToolChoice,
			CacheKey:         sent.CacheKey,
		}
		if !reflect.DeepEqual(*got, want) {
			t.Errorf("turn %d: recorded %+v, sent %+v", i, *got, want)
		}
		if got.MaxTokens == 0 || got.CacheKey == "" {
			t.Errorf("turn %d: recorded params are empty: %+v", i, *got)
		}
		if got.Temperature == nil || *got.Temperature != temperature {
			t.Errorf("turn %d: temperature = %v, want %v", i, got.Temperature, temperature)
		}
		if got.ReasoningEffort != "high" {
			t.Errorf("turn %d: reasoningEffort = %q, want high", i, got.ReasoningEffort)
		}
		if got.ReasoningSummary != "off" {
			t.Errorf("turn %d: reasoningSummary = %q, want off", i, got.ReasoningSummary)
		}
	}

	if got := records[0].ModelInput.Request.ToolChoice; got != types.ToolChoiceAuto {
		t.Errorf("first turn toolChoice = %v, want auto", got)
	}
	if got := records[1].ModelInput.Request.ToolChoice; got != types.ToolChoiceRequired {
		t.Errorf("escalated turn toolChoice = %v, want required", got)
	}

	if records[0].ResponseModel != "claude-sonnet-4-6-20260101" || records[1].ResponseModel != "" {
		t.Errorf("TurnRecord.ResponseModel = %q, %q; want the reported model then empty", records[0].ResponseModel, records[1].ResponseModel)
	}
	if len(turns) == 0 || turns[0].ResponseModel != "claude-sonnet-4-6-20260101" {
		t.Errorf("first TurnTrace.ResponseModel = %+v, want the provider-reported model", turns)
	}
	if len(turns) > 1 && turns[1].ResponseModel != "" {
		t.Errorf("second TurnTrace.ResponseModel = %q, want empty when the provider reports none", turns[1].ResponseModel)
	}
}

func TestRun_DefaultsTemperatureInRecordedRequest(t *testing.T) {
	prov := &paramCapturingProvider{scripts: [][]types.StreamEvent{
		{{Type: "text_delta", Text: "hi"}, {Type: "message_complete", StopReason: "end_turn"}},
	}}
	loop, _ := buildEscalationLoop(&sequencedProvider{scripts: [][]types.StreamEvent{noToolAnswer()}}, nil)
	loop.Provider = prov
	rec := &recordingTraceEmitter{}
	loop.Trace = rec

	if _, err := loop.Run(context.Background(), escalationConfig("planning")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.turnRecords) == 0 {
		t.Fatal("no turn records")
	}
	got := rec.turnRecords[0].ModelInput.Request
	if got == nil || got.Temperature == nil || *got.Temperature != defaultTemperature {
		t.Errorf("recorded temperature = %+v, want the defaulted %v", got, defaultTemperature)
	}
}

func TestRun_JSONLRecordingHoldsPromptVersionAndRequest(t *testing.T) {
	answer := noToolAnswer()
	answer[1].ResponseModel = "claude-sonnet-4-6-20260101"
	prov := &paramCapturingProvider{scripts: [][]types.StreamEvent{answer}}
	loop, _ := buildEscalationLoop(&sequencedProvider{scripts: [][]types.StreamEvent{noToolAnswer()}}, nil)
	loop.Provider = prov
	var buf bytes.Buffer
	loop.Trace = trace.NewJSONLTraceEmitter(&buf, false)

	if _, err := loop.Run(context.Background(), escalationConfig("planning")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	recording, err := tracereader.NewReader(&buf).ReadRecording()
	if err != nil {
		t.Fatalf("ReadRecording: %v", err)
	}
	if len(prov.params) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(prov.params))
	}
	sent := prov.params[0]
	if recording.SystemPrompt != sent.System {
		t.Errorf("recorded system prompt differs from the one sent:\n got %q\nwant %q", recording.SystemPrompt, sent.System)
	}
	sum := sha256.Sum256([]byte(sent.System))
	if want := hex.EncodeToString(sum[:]); recording.SystemPromptSHA256 != want {
		t.Errorf("system prompt digest = %q, want %q", recording.SystemPromptSHA256, want)
	}
	if recording.HarnessVersion != version.Full() {
		t.Errorf("harness version = %q, want %q", recording.HarnessVersion, version.Full())
	}
	if len(recording.Turns) != 1 || recording.Turns[0].ModelInput.Request == nil {
		t.Fatalf("turns = %+v, want one turn with request params", recording.Turns)
	}
	if got := recording.Turns[0].ResponseModel; got != "claude-sonnet-4-6-20260101" {
		t.Errorf("recorded response model = %q", got)
	}
	if got := recording.Turns[0].ModelInput.Request.MaxTokens; got != sent.MaxTokens {
		t.Errorf("recorded maxTokens = %d, sent %d", got, sent.MaxTokens)
	}
}

func TestRequestParamsOf_NilTemperatureIsOmitted(t *testing.T) {
	got := requestParamsOf(types.StreamParams{MaxTokens: 1000})
	if got.Temperature != nil {
		t.Errorf("Temperature = %v, want nil so the recording omits it rather than recording 0", *got.Temperature)
	}
	if got.MaxTokens != 1000 {
		t.Errorf("MaxTokens = %d, want 1000", got.MaxTokens)
	}
	line, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(line), "temperature") {
		t.Errorf("recorded request params %s carry a temperature, want it omitted", line)
	}
}

func TestStreamEventsToResult_BoundsAndScrubsResponseModel(t *testing.T) {
	const secret = "sk-ant-api03-modelleak0123456789abcdef"
	multibyte := strings.Repeat("é", maxResponseModelBytes)

	for _, tc := range []struct {
		name  string
		model string
		check func(t *testing.T, got string)
	}{
		{"ordinary id is unchanged", "claude-sonnet-4-6-20260101", func(t *testing.T, got string) {
			if got != "claude-sonnet-4-6-20260101" {
				t.Errorf("got %q", got)
			}
		}},
		{"oversized value is capped", strings.Repeat("m", 10*maxResponseModelBytes), func(t *testing.T, got string) {
			if len(got) != maxResponseModelBytes {
				t.Errorf("len = %d, want %d", len(got), maxResponseModelBytes)
			}
		}},
		{"cap falls on a rune boundary", multibyte, func(t *testing.T, got string) {
			if len(got) > maxResponseModelBytes || !utf8.ValidString(got) {
				t.Errorf("len = %d, valid = %v", len(got), utf8.ValidString(got))
			}
		}},
		{"secret-shaped value is scrubbed", "model-" + secret, func(t *testing.T, got string) {
			if strings.Contains(got, secret) || !strings.HasPrefix(got, "model-") {
				t.Errorf("got %q, want the secret removed", got)
			}
		}},
		{"secret straddling the cap is scrubbed", strings.Repeat("m", maxResponseModelBytes-10) + "-" + secret, func(t *testing.T, got string) {
			if strings.Contains(got, "sk-ant") || len(got) > maxResponseModelBytes {
				t.Errorf("got %q", got)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch := make(chan types.StreamEvent, 2)
			ch <- types.StreamEvent{Type: "message_complete", StopReason: "end_turn", ResponseModel: tc.model}
			close(ch)
			result, err := streamEventsToResult(context.Background(), ch, transport.NewNullTransport(), true, ignoreEmitFailure, nil)
			if err != nil {
				t.Fatalf("streamEventsToResult: %v", err)
			}
			tc.check(t, result.ResponseModel)
		})
	}
}

// TestRun_RecordsResolvedReasoningSummary pins that the recorded request
// carries the reasoningSummary the selected adapter applies, so a trace
// shows whether a summary was requested.
func TestRun_RecordsResolvedReasoningSummary(t *testing.T) {
	cases := []struct {
		providerType, configured, want string
	}{
		{"anthropic", "", types.ReasoningSummaryAuto},
		{"openai-responses", "", types.ReasoningSummaryOff},
		{"openai-responses", types.ReasoningSummaryAuto, types.ReasoningSummaryAuto},
		{"anthropic", types.ReasoningSummaryOff, types.ReasoningSummaryOff},
	}
	for _, tc := range cases {
		t.Run(tc.providerType+"/"+tc.configured, func(t *testing.T) {
			prov := &paramCapturingProvider{scripts: [][]types.StreamEvent{
				{{Type: "text_delta", Text: "hi"}, {Type: "message_complete", StopReason: "end_turn"}},
			}}
			loop, _ := buildEscalationLoop(&sequencedProvider{scripts: [][]types.StreamEvent{noToolAnswer()}}, nil)
			loop.Provider = prov
			loop.Router = router.NewStaticRouter(tc.providerType, "some-model")
			rec := &recordingTraceEmitter{}
			loop.Trace = rec
			config := escalationConfig("planning")
			config.Provider.Type = tc.providerType
			config.ReasoningSummary = tc.configured

			if _, err := loop.Run(context.Background(), config); err != nil {
				t.Fatalf("Run: %v", err)
			}
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if len(rec.turnRecords) == 0 {
				t.Fatal("no turn records")
			}
			if got := rec.turnRecords[0].ModelInput.Request.ReasoningSummary; got != tc.want {
				t.Errorf("recorded reasoningSummary = %q, want %q", got, tc.want)
			}
		})
	}
}
