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
	require.NoError(t, err, "Run")

	rec.mu.Lock()
	records := append([]types.TurnRecord(nil), rec.turnRecords...)
	turns := append([]types.TurnTrace(nil), rec.turns...)
	rec.mu.Unlock()
	require.GreaterOrEqual(t, len(records), 2, "turn records")
	require.GreaterOrEqual(t, len(prov.params), 2, "provider requests")

	for i, record := range records {
		t.Run(fmt.Sprintf("turn %d", i), func(t *testing.T) {
			sent := prov.params[i]
			got := record.ModelInput.Request
			require.NotNil(t, got, "ModelInput.Request")
			want := types.RequestParams{
				Temperature:      sent.Temperature,
				MaxTokens:        sent.MaxTokens,
				ReasoningEffort:  sent.ReasoningEffort,
				ReasoningSummary: sent.ReasoningSummary,
				ToolChoice:       sent.ToolChoice,
				CacheKey:         sent.CacheKey,
			}
			require.Equal(t, want, *got, "recorded params match the params sent")
			require.NotZero(t, got.MaxTokens, "maxTokens")
			require.NotEmpty(t, got.CacheKey, "cacheKey")
			require.NotNil(t, got.Temperature, "temperature")
			require.InDelta(t, temperature, *got.Temperature, 0, "temperature")
			require.Equal(t, "high", got.ReasoningEffort, "reasoningEffort")
			require.Equal(t, "off", got.ReasoningSummary, "reasoningSummary")
		})
	}

	require.Equal(t, types.ToolChoiceAuto, records[0].ModelInput.Request.ToolChoice, "first turn toolChoice")
	require.Equal(t, types.ToolChoiceRequired, records[1].ModelInput.Request.ToolChoice, "escalated turn toolChoice")

	require.Equal(t, "claude-sonnet-4-6-20260101", records[0].ResponseModel, "first TurnRecord.ResponseModel")
	require.Empty(t, records[1].ResponseModel, "second TurnRecord.ResponseModel")

	require.NotEmpty(t, turns, "turn traces")
	require.Equal(t, "claude-sonnet-4-6-20260101", turns[0].ResponseModel, "first TurnTrace.ResponseModel")
	if len(turns) > 1 {
		require.Empty(t, turns[1].ResponseModel, "second TurnTrace.ResponseModel")
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

	_, err := loop.Run(context.Background(), escalationConfig("planning"))
	require.NoError(t, err, "Run")

	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.NotEmpty(t, rec.turnRecords, "turn records")
	got := rec.turnRecords[0].ModelInput.Request
	require.NotNil(t, got, "recorded request")
	require.NotNil(t, got.Temperature, "recorded temperature")
	require.InDelta(t, defaultTemperature, *got.Temperature, 0, "defaulted temperature")
}

func TestRun_JSONLRecordingHoldsPromptVersionAndRequest(t *testing.T) {
	answer := noToolAnswer()
	answer[1].ResponseModel = "claude-sonnet-4-6-20260101"
	prov := &paramCapturingProvider{scripts: [][]types.StreamEvent{answer}}
	loop, _ := buildEscalationLoop(&sequencedProvider{scripts: [][]types.StreamEvent{noToolAnswer()}}, nil)
	loop.Provider = prov
	var buf bytes.Buffer
	loop.Trace = trace.NewJSONLTraceEmitter(&buf, false)

	_, err := loop.Run(context.Background(), escalationConfig("planning"))
	require.NoError(t, err, "Run")

	recording, err := tracereader.NewReader(&buf).ReadRecording()
	require.NoError(t, err, "ReadRecording")
	require.Len(t, prov.params, 1, "provider requests")
	sent := prov.params[0]
	require.Equal(t, sent.System, recording.SystemPrompt, "recorded system prompt")
	sum := sha256.Sum256([]byte(sent.System))
	require.Equal(t, hex.EncodeToString(sum[:]), recording.SystemPromptSHA256, "system prompt digest")
	require.Equal(t, version.Full(), recording.HarnessVersion, "harness version")
	require.Len(t, recording.Turns, 1, "recorded turns")
	require.NotNil(t, recording.Turns[0].ModelInput.Request, "recorded turn request params")
	require.Equal(t, "claude-sonnet-4-6-20260101", recording.Turns[0].ResponseModel, "recorded response model")
	require.Equal(t, sent.MaxTokens, recording.Turns[0].ModelInput.Request.MaxTokens, "recorded maxTokens")
}

func TestRequestParamsOf_NilTemperatureIsOmitted(t *testing.T) {
	got := requestParamsOf(types.StreamParams{MaxTokens: 1000})
	require.Nil(t, got.Temperature, "temperature must be omitted rather than recorded as 0")
	require.Equal(t, 1000, got.MaxTokens, "maxTokens")

	line, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(line), "temperature", "recorded request params %s", line)
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
			require.Equal(t, "claude-sonnet-4-6-20260101", got)
		}},
		{"oversized value is capped", strings.Repeat("m", 10*maxResponseModelBytes), func(t *testing.T, got string) {
			require.Len(t, got, maxResponseModelBytes)
		}},
		{"cap falls on a rune boundary", multibyte, func(t *testing.T, got string) {
			require.LessOrEqual(t, len(got), maxResponseModelBytes)
			require.True(t, utf8.ValidString(got), "capped value is valid UTF-8")
		}},
		{"secret-shaped value is scrubbed", "model-" + secret, func(t *testing.T, got string) {
			require.NotContains(t, got, secret)
			require.True(t, strings.HasPrefix(got, "model-"), "non-secret prefix is kept")
		}},
		{"secret straddling the cap is scrubbed", strings.Repeat("m", maxResponseModelBytes-10) + "-" + secret, func(t *testing.T, got string) {
			require.NotContains(t, got, "sk-ant")
			require.LessOrEqual(t, len(got), maxResponseModelBytes)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch := make(chan types.StreamEvent, 2)
			ch <- types.StreamEvent{Type: "message_complete", StopReason: "end_turn", ResponseModel: tc.model}
			close(ch)
			result, err := streamEventsToResult(context.Background(), ch, transport.NewNullTransport(), true, ignoreEmitFailure, nil)
			require.NoError(t, err, "streamEventsToResult")
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
			require.NoError(t, err, "Run")

			rec.mu.Lock()
			defer rec.mu.Unlock()
			require.NotEmpty(t, rec.turnRecords, "turn records")
			got := rec.turnRecords[0].ModelInput.Request
			require.NotNil(t, got, "recorded request")
			require.Equal(t, tc.want, got.ReasoningSummary, "recorded reasoningSummary")
		})
	}
}
