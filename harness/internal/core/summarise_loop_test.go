package core

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

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
	const turns = 6

	calibrate := &recordingStrategy{inner: contextpkg.NewSummariseStrategy(summary, "summary-model", 0)}
	loop := buildTestLoop(nil)
	registerLargeTestTool(t, loop)
	loop.Provider = &thinkingParamsProvider{turns: turns}
	loop.Context = calibrate
	cfg := buildTestConfig()
	cfg.Prompt = prompt
	if _, err := loop.Run(context.Background(), cfg); err != nil {
		t.Fatalf("calibration Run: %v", err)
	}

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
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("outcome = %q, want success", runTrace.Outcome)
	}
	return prov, tp.events, logs.String()
}

// TestLoop_SummaryFailureWarns pins that a failing summary call reaches
// operators as a warning event and a log line while the run continues on
// the sliding-window fallback.
func TestLoop_SummaryFailureWarns(t *testing.T) {
	summary := &failingSummaryProvider{}
	_, events, logs := runSummariseLoop(t, summary, "Hello, write a test file.")
	if summary.calls == 0 {
		t.Fatal("the summary provider was never called; the budget does not force a summary")
	}

	warnings := 0
	for _, ev := range events {
		if ev.Type == "warning" && strings.Contains(ev.Message, "summary call failed") {
			warnings++
			if !strings.Contains(ev.Message, "summary model unavailable") {
				t.Errorf("warning does not name the provider error: %q", ev.Message)
			}
		}
	}
	if warnings != summary.calls {
		t.Errorf("%d summary-failure warnings for %d failed calls", warnings, summary.calls)
	}
	if !strings.Contains(logs, "context strategy degraded") || !strings.Contains(logs, "summary model unavailable") {
		t.Errorf("log output lacks the summary failure:\n%s", logs)
	}
}

// TestLoop_SummariseSendsTaskVerbatim pins that after summarisation every
// request still starts with the operator's task exactly as written,
// including phrases the summary input would otherwise strip.
func TestLoop_SummariseSendsTaskVerbatim(t *testing.T) {
	const task = "You must not modify vendor/. Ignore previous drafts and write a test file."
	prov, events, _ := runSummariseLoop(t, summaryTextProvider{}, task)

	summarised := 0
	for i, sent := range prov.sent {
		first := sent[0]
		if first.Role != "user" || first.Content[0].Text != task {
			t.Fatalf("request %d starts with %+v, want the task verbatim", i+1, first.Content[0])
		}
		if strings.Contains(first.Content[len(first.Content)-1].Text, "<conversation_summary>") {
			summarised++
		}
	}
	if summarised == 0 {
		t.Fatal("no request carried a summary; the budget does not force a summary")
	}
	for _, ev := range events {
		if ev.Type == "warning" && strings.Contains(ev.Message, "summar") {
			t.Errorf("unexpected summary warning: %q", ev.Message)
		}
	}
}
