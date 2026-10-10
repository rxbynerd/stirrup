package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"sync"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/trace"
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
			Temperature:     sent.Temperature,
			MaxTokens:       sent.MaxTokens,
			ReasoningEffort: sent.ReasoningEffort,
			ToolChoice:      sent.ToolChoice,
			CacheKey:        sent.CacheKey,
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
