package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

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
	must := require.New(t)
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
	_, err := loop.Run(context.Background(), config)
	must.NoError(err, "Run")

	rec.mu.Lock()
	records := append([]types.TurnRecord(nil), rec.turnRecords...)
	turns := append([]types.TurnTrace(nil), rec.turns...)
	rec.mu.Unlock()
	must.GreaterOrEqual(len(records), 2, "turn records")
	must.GreaterOrEqual(len(prov.params), 2, "provider requests")

	for i, record := range records {
		t.Run(fmt.Sprintf("turn %d", i), func(t *testing.T) {
			must := require.New(t)
			sent := prov.params[i]
			got := record.ModelInput.Request
			must.NotNil(got, "ModelInput.Request")
			want := types.RequestParams{
				Temperature:      sent.Temperature,
				MaxTokens:        sent.MaxTokens,
				ReasoningEffort:  sent.ReasoningEffort,
				ReasoningSummary: sent.ReasoningSummary,
				ToolChoice:       sent.ToolChoice,
				CacheKey:         sent.CacheKey,
			}
			must.Equal(want, *got, "recorded params match the params sent")
			must.NotZero(got.MaxTokens, "maxTokens")
			must.NotEmpty(got.CacheKey, "cacheKey")
			must.NotNil(got.Temperature, "temperature")
			must.InDelta(temperature, *got.Temperature, 0, "temperature")
			must.Equal("high", got.ReasoningEffort, "reasoningEffort")
			must.Equal("off", got.ReasoningSummary, "reasoningSummary")
		})
	}

	must.Equal(types.ToolChoiceAuto, records[0].ModelInput.Request.ToolChoice, "first turn toolChoice")
	must.Equal(types.ToolChoiceRequired, records[1].ModelInput.Request.ToolChoice, "escalated turn toolChoice")

	must.Equal("claude-sonnet-4-6-20260101", records[0].ResponseModel, "first TurnRecord.ResponseModel")
	must.Empty(records[1].ResponseModel, "second TurnRecord.ResponseModel")

	must.NotEmpty(turns, "turn traces")
	must.Equal("claude-sonnet-4-6-20260101", turns[0].ResponseModel, "first TurnTrace.ResponseModel")
	if len(turns) > 1 {
		must.Empty(turns[1].ResponseModel, "second TurnTrace.ResponseModel")
	}
}

func TestRun_DefaultsTemperatureInRecordedRequest(t *testing.T) {
	must := require.New(t)
	prov := &paramCapturingProvider{scripts: [][]types.StreamEvent{
		{{Type: "text_delta", Text: "hi"}, {Type: "message_complete", StopReason: "end_turn"}},
	}}
	loop, _ := buildEscalationLoop(&sequencedProvider{scripts: [][]types.StreamEvent{noToolAnswer()}}, nil)
	loop.Provider = prov
	rec := &recordingTraceEmitter{}
	loop.Trace = rec

	_, err := loop.Run(context.Background(), escalationConfig("planning"))
	must.NoError(err, "Run")

	rec.mu.Lock()
	defer rec.mu.Unlock()
	must.NotEmpty(rec.turnRecords, "turn records")
	got := rec.turnRecords[0].ModelInput.Request
	must.NotNil(got, "recorded request")
	must.NotNil(got.Temperature, "recorded temperature")
	must.InDelta(defaultTemperature, *got.Temperature, 0, "defaulted temperature")
}

func TestRun_JSONLRecordingHoldsPromptVersionAndRequest(t *testing.T) {
	must := require.New(t)
	answer := noToolAnswer()
	answer[1].ResponseModel = "claude-sonnet-4-6-20260101"
	prov := &paramCapturingProvider{scripts: [][]types.StreamEvent{answer}}
	loop, _ := buildEscalationLoop(&sequencedProvider{scripts: [][]types.StreamEvent{noToolAnswer()}}, nil)
	loop.Provider = prov
	var buf bytes.Buffer
	loop.Trace = trace.NewJSONLTraceEmitter(&buf, false)

	_, err := loop.Run(context.Background(), escalationConfig("planning"))
	must.NoError(err, "Run")

	recording, err := tracereader.NewReader(&buf).ReadRecording()
	must.NoError(err, "ReadRecording")
	must.Len(prov.params, 1, "provider requests")
	sent := prov.params[0]
	must.Equal(sent.System, recording.SystemPrompt, "recorded system prompt")
	sum := sha256.Sum256([]byte(sent.System))
	must.Equal(hex.EncodeToString(sum[:]), recording.SystemPromptSHA256, "system prompt digest")
	must.Equal(version.Full(), recording.HarnessVersion, "harness version")
	must.Len(recording.Turns, 1, "recorded turns")
	must.NotNil(recording.Turns[0].ModelInput.Request, "recorded turn request params")
	must.Equal("claude-sonnet-4-6-20260101", recording.Turns[0].ResponseModel, "recorded response model")
	must.Equal(sent.MaxTokens, recording.Turns[0].ModelInput.Request.MaxTokens, "recorded maxTokens")
}

func TestRequestParamsOf_NilTemperatureIsOmitted(t *testing.T) {
	must := require.New(t)
	got := requestParamsOf(types.StreamParams{MaxTokens: 1000})
	must.Nil(got.Temperature, "temperature must be omitted rather than recorded as 0")
	must.Equal(1000, got.MaxTokens, "maxTokens")

	line, err := json.Marshal(got)
	must.NoError(err)
	must.NotContains(string(line), "temperature", "recorded request params %s", line)
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
			must := require.New(t)
			must.Equal("claude-sonnet-4-6-20260101", got)
		}},
		{"oversized value is capped", strings.Repeat("m", 10*maxResponseModelBytes), func(t *testing.T, got string) {
			must := require.New(t)
			must.Len(got, maxResponseModelBytes)
		}},
		{"cap falls on a rune boundary", multibyte, func(t *testing.T, got string) {
			must := require.New(t)
			must.LessOrEqual(len(got), maxResponseModelBytes)
			must.True(utf8.ValidString(got), "capped value is valid UTF-8")
		}},
		{"secret-shaped value is scrubbed", "model-" + secret, func(t *testing.T, got string) {
			must := require.New(t)
			must.NotContains(got, secret)
			must.True(strings.HasPrefix(got, "model-"), "non-secret prefix is kept")
		}},
		{"secret straddling the cap is scrubbed", strings.Repeat("m", maxResponseModelBytes-10) + "-" + secret, func(t *testing.T, got string) {
			must := require.New(t)
			must.NotContains(got, "sk-ant")
			must.LessOrEqual(len(got), maxResponseModelBytes)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			ch := make(chan types.StreamEvent, 2)
			ch <- types.StreamEvent{Type: "message_complete", StopReason: "end_turn", ResponseModel: tc.model}
			close(ch)
			result, err := streamEventsToResult(context.Background(), ch, transport.NewNullTransport(), true, ignoreEmitFailure, nil)
			must.NoError(err, "streamEventsToResult")
			tc.check(t, result.ResponseModel)
		})
	}
}

// TestRun_RecordsResolvedReasoningSummary pins that the recorded request
// carries the reasoningSummary the selected adapter applies, so a trace
// shows whether a summary was requested.
func TestRun_RecordsResolvedReasoningSummary(t *testing.T) {
	cases := []struct {
		name, providerType, configured, want string
	}{
		{"anthropic unset summary resolves to auto", "anthropic", "", types.ReasoningSummaryAuto},
		{"openai responses unset summary resolves to off", "openai-responses", "", types.ReasoningSummaryOff},
		{"openai responses keeps configured auto", "openai-responses", types.ReasoningSummaryAuto, types.ReasoningSummaryAuto},
		{"anthropic keeps configured off", "anthropic", types.ReasoningSummaryOff, types.ReasoningSummaryOff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
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

			_, err := loop.Run(context.Background(), config)
			must.NoError(err, "Run")

			rec.mu.Lock()
			defer rec.mu.Unlock()
			must.NotEmpty(rec.turnRecords, "turn records")
			got := rec.turnRecords[0].ModelInput.Request
			must.NotNil(got, "recorded request")
			must.Equal(tc.want, got.ReasoningSummary, "recorded reasoningSummary")
		})
	}
}
