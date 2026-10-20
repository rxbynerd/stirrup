package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// fakeBatchAdapter satisfies batchModeAdapter by returning a fixed
// LastBatchID, without pulling in the real BatchAdapter's
// BatchClient/BatchProviderConfig wiring.
type fakeBatchAdapter struct {
	batchID string
	events  []types.StreamEvent
}

func (f *fakeBatchAdapter) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	ch := make(chan types.StreamEvent, len(f.events))
	for _, e := range f.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func (f *fakeBatchAdapter) LastBatchID() string { return f.batchID }

// TestTurnModeInfo_StreamingDefault: a vanilla ProviderAdapter (no
// LastBatchID method) resolves to (TurnModeStreaming, "").
func TestTurnModeInfo_StreamingDefault(t *testing.T) {
	check := assert.New(t)
	mode, batchID := turnModeInfo(&mockProvider{})
	check.Equal(types.TurnModeStreaming, mode, "mode")
	check.Empty(batchID, "batchID")
}

// TestTurnModeInfo_BatchAdapterPopulatesBatchID: any adapter implementing
// LastBatchID() yields mode=batch and the surfaced identifier.
func TestTurnModeInfo_BatchAdapterPopulatesBatchID(t *testing.T) {
	check := assert.New(t)
	mode, batchID := turnModeInfo(&fakeBatchAdapter{batchID: "msgbatch_xyz"})
	check.Equal(types.TurnModeBatch, mode, "mode")
	check.Equal("msgbatch_xyz", batchID, "batchID")
}

// TestTurnModeInfo_NilSelectedProvider: turnModeInfo(nil) returns the
// streaming defaults rather than panicking.
func TestTurnModeInfo_NilSelectedProvider(t *testing.T) {
	check := assert.New(t)
	mode, batchID := turnModeInfo(nil)
	check.Equal(types.TurnModeStreaming, mode, "mode")
	check.Empty(batchID, "batchID")
}

// buildBatchTestLoop is buildTestLoop's twin, taking a *fakeBatchAdapter
// directly since buildTestLoop only accepts *mockProvider.
func buildBatchTestLoop(prov *fakeBatchAdapter, recorder *recordingTraceEmitter) *AgenticLoop {
	var transportBuf bytes.Buffer
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:              "test_tool",
		Description:       "A test tool",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{}}`),
		WorkspaceMutating: false,
		RequiresApproval:  false,
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "tool result", nil
		},
	})

	return &AgenticLoop{
		Provider:    prov,
		Router:      router.NewStaticRouter("anthropic", "claude-sonnet-4-6"),
		Prompt:      prompt.NewDefaultPromptBuilder(),
		Context:     contextpkg.NewSlidingWindowStrategy(),
		Tools:       registry,
		Executor:    nil,
		Edit:        edit.NewWholeFileStrategy(),
		Verifier:    verifier.NewNoneVerifier(),
		Permissions: permission.NewAllowAll(),
		Git:         git.NewNoneGitStrategy(),
		Transport:   transport.NewStdioTransport(&transportBuf, &bytes.Buffer{}),
		Trace:       recorder,
		Tracer:      noop.NewTracerProvider().Tracer(""),
		Metrics:     observability.NewNoopMetrics(),
		Logger:      slog.Default(),
	}
}

// TestLoop_BatchAdapter_RecordsBatchMode: when the loop's selectedProvider
// implements LastBatchID(), the emitted TurnTrace carries Mode="batch"
// and the batch identifier.
func TestLoop_BatchAdapter_RecordsBatchMode(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &fakeBatchAdapter{
		batchID: "msgbatch_test123",
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "ok"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}
	recorder := &recordingTraceEmitter{}
	loop := buildBatchTestLoop(prov, recorder)

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	turns, _ := recorder.snapshot()
	must.Len(turns, 1)
	check.Equal(types.TurnModeBatch, turns[0].Mode, "turn[0].Mode")
	check.Equal("msgbatch_test123", turns[0].BatchID, "turn[0].BatchID")
}

// TestLoop_StreamingProvider_RecordsStreamingMode: a vanilla mockProvider
// produces TurnTrace.Mode="streaming" on every successful turn.
func TestLoop_StreamingProvider_RecordsStreamingMode(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "ok"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}
	recorder := &recordingTraceEmitter{}
	loop := buildTestLoop(prov)
	loop.Trace = recorder

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	turns, _ := recorder.snapshot()
	must.Len(turns, 1)
	check.Equal(types.TurnModeStreaming, turns[0].Mode, "turn[0].Mode")
	check.Empty(turns[0].BatchID, "turn[0].BatchID")
}

// failingBatchAdapter returns a stream-time error so the loop hits the
// streamErr branch, while still recording the batch ID needed to find
// the upstream receipt.
type failingBatchAdapter struct{ batchID string }

func (f *failingBatchAdapter) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	ch := make(chan types.StreamEvent, 1)
	ch <- types.StreamEvent{Type: "error", Error: errors.New("synthetic stream failure")}
	close(ch)
	return ch, nil
}

func (f *failingBatchAdapter) LastBatchID() string { return f.batchID }

// TestLoop_ProviderNotFound_RecordsEmptyMode: the pre-resolution error path
// (router selects a provider absent from l.Providers) must record
// Mode="", not Mode="streaming".
func TestLoop_ProviderNotFound_RecordsEmptyMode(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	recorder := &recordingTraceEmitter{}
	loop := &AgenticLoop{
		// l.Provider is intentionally nil; Providers has no "anthropic"
		// entry, so the !ok branch fires before any provider is resolved.
		Providers: map[string]provider.ProviderAdapter{
			"other": &mockProvider{},
		},
		Router:      router.NewStaticRouter("anthropic", "claude-sonnet-4-6"),
		Prompt:      prompt.NewDefaultPromptBuilder(),
		Context:     contextpkg.NewSlidingWindowStrategy(),
		Tools:       tool.NewRegistry(),
		Executor:    nil,
		Edit:        edit.NewWholeFileStrategy(),
		Verifier:    verifier.NewNoneVerifier(),
		Permissions: permission.NewAllowAll(),
		Git:         git.NewNoneGitStrategy(),
		Transport:   transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}),
		Trace:       recorder,
		Tracer:      noop.NewTracerProvider().Tracer(""),
		Metrics:     observability.NewNoopMetrics(),
		Logger:      slog.Default(),
	}

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	turns, _ := recorder.snapshot()
	must.Len(turns, 1)
	check.Equal("error", turns[0].StopReason, "turn[0].StopReason")
	check.Empty(turns[0].Mode, "pre-resolution error records no mode")
}

func TestLoop_BatchAdapter_StreamError_StillRecordsBatchID(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prov := &failingBatchAdapter{batchID: "msgbatch_failed_run"}
	recorder := &recordingTraceEmitter{}
	loop := &AgenticLoop{
		Provider:    prov,
		Router:      router.NewStaticRouter("anthropic", "claude-sonnet-4-6"),
		Prompt:      prompt.NewDefaultPromptBuilder(),
		Context:     contextpkg.NewSlidingWindowStrategy(),
		Tools:       tool.NewRegistry(),
		Executor:    nil,
		Edit:        edit.NewWholeFileStrategy(),
		Verifier:    verifier.NewNoneVerifier(),
		Permissions: permission.NewAllowAll(),
		Git:         git.NewNoneGitStrategy(),
		Transport:   transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}),
		Trace:       recorder,
		Tracer:      noop.NewTracerProvider().Tracer(""),
		Metrics:     observability.NewNoopMetrics(),
		Logger:      slog.Default(),
	}

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	turns, _ := recorder.snapshot()
	must.Len(turns, 1)
	check.Equal("error", turns[0].StopReason, "turn[0].StopReason")
	check.Equal(types.TurnModeBatch, turns[0].Mode, "turn[0].Mode")
	check.Equal("msgbatch_failed_run", turns[0].BatchID, "failure paths must retain the batch id")
}
