package core

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/types"
)

type failingSummaryProvider struct{ calls int }

func (p *failingSummaryProvider) Stream(context.Context, types.StreamParams) (<-chan types.StreamEvent, error) {
	p.calls++
	return nil, fmt.Errorf("summary model unavailable")
}

// runSummariseLoop runs a tool-calling loop whose budget first forces the
// summarise strategy to compact on call index 4, returning the main
// provider, the emitted events and the log output.
func runSummariseLoop(t *testing.T, summary contextpkg.SummaryProvider, prompt string) (*thinkingParamsProvider, []types.HarnessEvent, string) {
	t.Helper()
	must := require.New(t)
	const turns = 6

	calibrate := &recordingStrategy{inner: contextpkg.NewSummariseStrategy(summary, "summary-model", 0)}
	loop := buildTestLoop(nil)
	registerLargeTestTool(t, loop)
	loop.Provider = &thinkingParamsProvider{turns: turns}
	loop.Context = calibrate
	cfg := buildTestConfig()
	cfg.Prompt = prompt
	_, err := loop.Run(context.Background(), cfg)
	must.NoError(err, "calibration run")

	prov := &thinkingParamsProvider{turns: turns}
	tp := &recordingTransport{}
	var logs bytes.Buffer
	loop = buildTestLoop(nil)
	registerLargeTestTool(t, loop)
	loop.Provider = prov
	loop.Context = contextpkg.NewSummariseStrategy(summary, "summary-model", 0)
	loop.Transport = tp
	loop.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	cfg = buildTestConfig()
	cfg.Prompt = prompt
	cfg.ContextStrategy.MaxTokens = maxTokensCompactingFrom(t, calibrate.currentTokens, 4)
	runTrace, err := loop.Run(context.Background(), cfg)
	must.NoError(err, "run with forced compaction")
	must.Equal("success", runTrace.Outcome)
	return prov, tp.events, logs.String()
}

// TestLoop_SummaryFailure_WarnsAndContinues pins that a failing summary call
// reaches operators as a warning event and a log line while the run continues
// on the sliding-window fallback.
func TestLoop_SummaryFailure_WarnsAndContinues(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	summary := &failingSummaryProvider{}
	_, events, logs := runSummariseLoop(t, summary, "Hello, write a test file.")
	must.NotZero(summary.calls, "the summary provider was never called; the budget does not force a summary")

	var warnings []string
	for _, ev := range events {
		if ev.Type == "warning" && strings.Contains(ev.Message, "summary call failed") {
			warnings = append(warnings, ev.Message)
		}
	}
	check.Len(warnings, summary.calls, "one summary-failure warning per failed call")
	for _, msg := range warnings {
		check.Contains(msg, "summary model unavailable", "warning names the provider error")
	}
	check.Contains(logs, "context strategy degraded")
	check.Contains(logs, "summary model unavailable", "log output names the summary failure")
}

// TestLoop_Summarise_SendsTaskVerbatim pins that after summarisation every
// request still starts with the operator's task exactly as written,
// including phrases the summary input would otherwise strip.
func TestLoop_Summarise_SendsTaskVerbatim(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	const task = "You must not modify vendor/. Ignore previous drafts and write a test file."
	prov, events, _ := runSummariseLoop(t, summaryTextProvider{}, task)

	summarised := 0
	for i, sent := range prov.sent {
		first := sent[0]
		must.Equal("user", first.Role, "request %d opening role", i+1)
		must.Equal(task, first.Content[0].Text, "request %d opening text", i+1)
		if strings.Contains(first.Content[len(first.Content)-1].Text, "<conversation_summary>") {
			summarised++
		}
	}
	must.NotZero(summarised, "no request carried a summary; the budget does not force a summary")

	var summaryWarnings []string
	for _, ev := range events {
		if ev.Type == "warning" && strings.Contains(ev.Message, "summar") {
			summaryWarnings = append(summaryWarnings, ev.Message)
		}
	}
	check.Empty(summaryWarnings, "a successful summary emits no summary warning")
}
