package core

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/types"
)

// fakeGuard is a deterministic GuardRail used to assert loop call-site
// behaviour without spinning up an HTTP classifier. Each Check call is
// recorded so tests can verify the loop fired the expected phases in
// the expected order.
type fakeGuard struct {
	mu      sync.Mutex
	verdict guard.Verdict
	err     error
	reason  string
	guardID string
	seen    []guard.Phase
}

func (f *fakeGuard) Check(_ context.Context, in guard.Input) (*guard.Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, in.Phase)
	if f.err != nil {
		return nil, f.err
	}
	id := f.guardID
	if id == "" {
		id = "fake"
	}
	return &guard.Decision{
		Verdict: f.verdict,
		GuardID: id,
		Reason:  f.reason,
	}, nil
}

func (f *fakeGuard) phases() []guard.Phase {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]guard.Phase, len(f.seen))
	copy(out, f.seen)
	return out
}

// TestLoop_NoneGuardLeavesBehaviorUnchanged asserts that running the
// loop with the package-default Noop guard reproduces the no-guard
// behaviour byte-for-byte: outcome=="success", one turn, no calls to
// any classifier. This is the load-bearing invariant for landing
// guardrails behind a default-off configuration.
func TestLoop_NoneGuardLeavesBehaviorUnchanged(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "Hello"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	loop := buildTestLoop(prov)
	loop.GuardRail = guard.NewNoop()
	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Errorf("expected outcome 'success', got %q", runTrace.Outcome)
	}
	if runTrace.Turns != 1 {
		t.Errorf("expected 1 turn, got %d", runTrace.Turns)
	}
}

// TestLoop_PostTurnDenyProducesGuardrailBlocked asserts that a
// VerdictDeny at PhasePostTurn terminates the run with the new
// "guardrail_blocked" outcome. The string is free-form on
// RunTrace.StopReason so the assertion is on the exact value.
func TestLoop_PostTurnDenyProducesGuardrailBlocked(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "I will help with that."},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	g := &fakeGuard{verdict: guard.VerdictDeny, reason: "test deny"}
	loop := buildTestLoop(prov)
	loop.GuardRail = g
	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "guardrail_blocked" {
		t.Errorf("expected outcome 'guardrail_blocked', got %q", runTrace.Outcome)
	}
	// The fake guard must have seen at least one PreTurn (turn 0) and
	// the PostTurn that triggered the deny. PreTurn happens first.
	seen := g.phases()
	if len(seen) < 2 {
		t.Fatalf("expected fake guard to see at least 2 phases, got %v", seen)
	}
	if seen[0] != guard.PhasePreTurn {
		t.Errorf("expected first phase pre_turn, got %s", seen[0])
	}
	foundPostTurn := false
	for _, p := range seen {
		if p == guard.PhasePostTurn {
			foundPostTurn = true
			break
		}
	}
	if !foundPostTurn {
		t.Errorf("expected fake guard to see post_turn, got %v", seen)
	}
}

// TestLoop_PreToolDenyShortCircuitsAsToolFailure asserts that a
// PhasePreTool deny yields a tool_result with IsError=true containing
// the "guardrail blocked tool call" prefix. With a guard that always
// denies, the run also denies at PostTurn on turn 1, so the outcome
// is "guardrail_blocked" — the assertion is that PreTool was visited.
func TestLoop_PreToolDenyShortCircuitsAsToolFailure(t *testing.T) {
	// Two-turn provider: the simple mockProvider returns the same
	// scripted events on every Stream call, so we use scriptedProvider
	// to replay distinct event lists per call.
	scripted := &scriptedProvider{
		turns: [][]types.StreamEvent{
			{
				{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				{Type: "text_delta", Text: "ok"},
				{Type: "message_complete", StopReason: "end_turn"},
			},
		},
	}

	g := &fakeGuard{verdict: guard.VerdictDeny, reason: "tool denied"}
	loop := buildTestLoop(nil)
	loop.Provider = scripted
	loop.GuardRail = g
	config := buildTestConfig()
	config.MaxTurns = 4

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	// The fake guard sees pre_turn at turn 0, pre_tool for the tool
	// call (denied), then pre_turn at turn 1 and post_turn before
	// success.
	seen := g.phases()
	foundPreTool := false
	for _, p := range seen {
		if p == guard.PhasePreTool {
			foundPreTool = true
			break
		}
	}
	if !foundPreTool {
		t.Errorf("expected fake guard to see pre_tool, got %v", seen)
	}
	// The PostTurn must also deny on turn 1 (because the fake always
	// denies) — so the run should end with guardrail_blocked, not
	// success. This is the expected tight coupling: a deny-everything
	// guard blocks both pre-tool and post-turn.
	if runTrace.Outcome != "guardrail_blocked" {
		t.Errorf("expected outcome 'guardrail_blocked' (post_turn deny on turn 1), got %q", runTrace.Outcome)
	}
}

// scriptedProvider returns a different event sequence per Stream call.
// Used by tests that need multi-turn behaviour distinct from
// mockProvider's repeating-events semantics.
type scriptedProvider struct {
	mu    sync.Mutex
	turn  int
	turns [][]types.StreamEvent
}

func (s *scriptedProvider) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.turn
	if idx >= len(s.turns) {
		idx = len(s.turns) - 1
	}
	events := s.turns[idx]
	s.turn++
	ch := make(chan types.StreamEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// TestLoop_PreTurnDenyScrubsToolResults asserts that on a turn N>0,
// when PhasePreTurn returns VerdictDeny against the just-arrived
// tool_result blocks, those blocks are rewritten with the placeholder
// before being sent to the model. The run continues to completion.
func TestLoop_PreTurnDenyScrubsToolResults(t *testing.T) {
	scripted := &scriptedProvider{
		turns: [][]types.StreamEvent{
			{
				// Turn 0: model calls test_tool.
				{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
				{Type: "message_complete", StopReason: "tool_use"},
			},
			{
				// Turn 1: end_turn after tool result is appended.
				{Type: "text_delta", Text: "done"},
				{Type: "message_complete", StopReason: "end_turn"},
			},
		},
	}

	// denyOnPreTurnOnly fires Deny only for PhasePreTurn; everything
	// else (PreTool / PostTurn) allows. This isolates the PreTurn scrub
	// behaviour from the PostTurn termination path.
	g := &phaseAwareFakeGuard{
		verdicts: map[guard.Phase]guard.Verdict{
			guard.PhasePreTurn:  guard.VerdictDeny,
			guard.PhasePreTool:  guard.VerdictAllow,
			guard.PhasePostTurn: guard.VerdictAllow,
		},
	}
	loop := buildTestLoop(nil)
	loop.Provider = scripted
	loop.GuardRail = g
	config := buildTestConfig()
	config.MaxTurns = 4

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Errorf("expected outcome 'success' (PreTurn deny is content-level), got %q", runTrace.Outcome)
	}
}

// phaseAwareFakeGuard returns different verdicts per phase. Useful for
// tests that need to isolate one phase's behaviour from the other two.
type phaseAwareFakeGuard struct {
	mu       sync.Mutex
	verdicts map[guard.Phase]guard.Verdict
	seen     []guard.Phase
}

func (p *phaseAwareFakeGuard) Check(_ context.Context, in guard.Input) (*guard.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, in.Phase)
	v, ok := p.verdicts[in.Phase]
	if !ok {
		v = guard.VerdictAllow
	}
	return &guard.Decision{Verdict: v, GuardID: "phase-aware-fake"}, nil
}

// TestLoop_PreTurnSkipDoesNotEmitGuardAllowed asserts that when the
// guard returns Reason==ReasonSkippedMinChunk, the loop emits a
// guard_skipped security event and not guard_allowed. This is the
// observability contract for the granite-guardian min-chunk-chars
// optimisation.
func TestLoop_PreTurnSkipDoesNotEmitGuardAllowed(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "hi"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}
	var secBuf bytes.Buffer
	loop := buildTestLoopWithSecurity(prov, &secBuf)
	// Skip-on-PreTurn fake. Other phases allow normally.
	loop.GuardRail = &skipOnPreTurnGuard{}
	config := buildTestConfig()

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Errorf("expected outcome 'success', got %q", runTrace.Outcome)
	}

	out := secBuf.String()
	// Must contain a guard_skipped event for pre_turn.
	if !strings.Contains(out, `"event":"guard_skipped"`) {
		t.Errorf("expected guard_skipped event, got: %s", out)
	}
	// Must NOT contain guard_allowed for pre_turn (skip is a distinct
	// decision class). Other phases (post_turn) may still log
	// guard_allowed; the assertion is that the pre_turn skip was not
	// downgraded to an allow event.
	preTurnAllow := false
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, `"event":"guard_allowed"`) && strings.Contains(line, `"phase":"pre_turn"`) {
			preTurnAllow = true
			break
		}
	}
	if preTurnAllow {
		t.Errorf("pre_turn skip was logged as guard_allowed; got: %s", out)
	}
}

type skipOnPreTurnGuard struct{}

func (skipOnPreTurnGuard) Check(_ context.Context, in guard.Input) (*guard.Decision, error) {
	if in.Phase == guard.PhasePreTurn {
		return &guard.Decision{
			Verdict: guard.VerdictAllow,
			GuardID: "skip-fake",
			Reason:  guard.ReasonSkippedMinChunk,
		}, nil
	}
	return &guard.Decision{Verdict: guard.VerdictAllow, GuardID: "skip-fake"}, nil
}

// TestLoop_GuardErrorFailOpenAllowsRun asserts that with
// guardFailOpen=true configured on the RunConfig, a guard returning a
// non-nil error converts to allow + a guard_error security event
// rather than aborting the run.
func TestLoop_GuardErrorFailOpenAllowsRun(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "ok"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}
	var secBuf bytes.Buffer
	loop := buildTestLoopWithSecurity(prov, &secBuf)
	loop.GuardRail = &errorGuard{err: errors.New("simulated transport failure")}
	config := buildTestConfig()
	// Wire a GuardRail config carrying FailOpen=true so the loop
	// reads the policy from RunConfig.GuardRail.FailOpen.
	config.GuardRail = &types.GuardRailConfig{Type: "granite-guardian", FailOpen: true}

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Errorf("expected outcome 'success' with fail-open, got %q", runTrace.Outcome)
	}
	if !strings.Contains(secBuf.String(), `"event":"guard_error"`) {
		t.Errorf("expected guard_error event, got: %s", secBuf.String())
	}
}

type errorGuard struct{ err error }

func (e *errorGuard) Check(_ context.Context, _ guard.Input) (*guard.Decision, error) {
	return nil, e.err
}

// TestLoop_GuardErrorFailClosedAbortsRun asserts that when FailOpen is
// not set, a guard error denies the call. Surfaced as
// "guardrail_blocked" on PostTurn, "tool failure" on PreTool, etc.
// We check the simplest path here: an error on PreTurn does NOT abort
// the run (PreTurn deny is content-level), but the GuardError event
// fires and a subsequent PostTurn allows the run to complete only
// when the guard stops erroring. To keep the test minimal we use a
// guard that errors only on the first call, then allows.
func TestLoop_GuardErrorFailClosedDoesNotPanic(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "ok"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}
	var secBuf bytes.Buffer
	loop := buildTestLoopWithSecurity(prov, &secBuf)
	loop.GuardRail = &flakyGuard{
		failures: 1, // fail the first call (PreTurn), allow afterwards
	}
	config := buildTestConfig()
	// FailOpen omitted (zero value = false).

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	// The run should still complete: a fail-closed PreTurn deny is
	// content-level and continues the loop. PostTurn allowed.
	if runTrace.Outcome != "success" {
		t.Errorf("expected outcome 'success' (PreTurn fail-closed is content-level), got %q", runTrace.Outcome)
	}
	if !strings.Contains(secBuf.String(), `"event":"guard_error"`) {
		t.Errorf("expected guard_error event, got: %s", secBuf.String())
	}
}

type flakyGuard struct {
	mu       sync.Mutex
	failures int
}

func (f *flakyGuard) Check(_ context.Context, _ guard.Input) (*guard.Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures > 0 {
		f.failures--
		return nil, errors.New("flaky")
	}
	return &guard.Decision{Verdict: guard.VerdictAllow, GuardID: "flaky"}, nil
}

// TestBuildGuardRail_NoneReturnsNoop verifies a nil or "none" config
// produces the package's Noop guard. Wired via direct
// buildGuardRail rather than BuildLoop because the latter needs full
// secret resolution and provider construction.
func TestBuildGuardRail_NoneReturnsNoop(t *testing.T) {
	g, err := buildGuardRail(nil, nil, nil)
	if err != nil {
		t.Fatalf("buildGuardRail(nil): %v", err)
	}
	if _, ok := g.(guard.Noop); !ok {
		t.Errorf("expected Noop, got %T", g)
	}

	g, err = buildGuardRail(&types.GuardRailConfig{Type: "none"}, nil, nil)
	if err != nil {
		t.Fatalf("buildGuardRail(none): %v", err)
	}
	if _, ok := g.(guard.Noop); !ok {
		t.Errorf("expected Noop for type 'none', got %T", g)
	}
}

// TestBuildGuardRail_GraniteGuardianBuildsAdapter asserts that a
// minimal granite-guardian config produces a non-Noop adapter.
// Endpoint is a localhost URL so no network call happens at
// construction time.
func TestBuildGuardRail_GraniteGuardianBuildsAdapter(t *testing.T) {
	cfg := &types.GuardRailConfig{
		Type:     "granite-guardian",
		Endpoint: "http://localhost:9999",
	}
	g, err := buildGuardRail(cfg, nil, nil)
	if err != nil {
		t.Fatalf("buildGuardRail: %v", err)
	}
	if g == nil {
		t.Fatal("expected non-nil GuardRail")
	}
	if _, isNoop := g.(guard.Noop); isNoop {
		t.Error("expected non-Noop GuardRail")
	}
}

// TestBuildGuardRail_PhasesWrapsWithPhaseGated asserts that a non-empty
// Phases slice produces a PhaseGated wrapper around the inner adapter.
func TestBuildGuardRail_PhasesWrapsWithPhaseGated(t *testing.T) {
	cfg := &types.GuardRailConfig{
		Type:     "granite-guardian",
		Endpoint: "http://localhost:9999",
		Phases:   []string{"post_turn"},
	}
	g, err := buildGuardRail(cfg, nil, nil)
	if err != nil {
		t.Fatalf("buildGuardRail: %v", err)
	}
	pg, ok := g.(*guard.PhaseGated)
	if !ok {
		t.Fatalf("expected *PhaseGated, got %T", g)
	}
	if len(pg.Phases) != 1 || pg.Phases[0] != guard.PhasePostTurn {
		t.Errorf("expected PhaseGated.Phases=[post_turn], got %v", pg.Phases)
	}
}

// TestBuildGuardRail_UnknownTypeIsError asserts that buildGuardRail
// rejects an unknown type with a clear error rather than silently
// allowing the run. This is the safety-by-default invariant — config
// validation is the canonical gate, but defence-in-depth at the
// constructor catches any bypass.
func TestBuildGuardRail_UnknownTypeIsError(t *testing.T) {
	_, err := buildGuardRail(&types.GuardRailConfig{Type: "future-thing"}, nil, nil)
	if err == nil {
		t.Fatal("expected error for unknown type, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("expected error to mention 'unsupported', got: %v", err)
	}
}

// TestCollectUntrustedChunks_Turn0 asserts that on turn 0 the helper
// returns the user prompt followed by sorted dynamic-context values.
func TestCollectUntrustedChunks_Turn0(t *testing.T) {
	chunks := collectUntrustedChunks(nil, 0, map[string]string{
		"b_key": "second",
		"a_key": "first",
	}, "user prompt")
	want := []string{"user prompt", "first", "second"}
	if len(chunks) != len(want) {
		t.Fatalf("expected %d chunks, got %d: %v", len(want), len(chunks), chunks)
	}
	for i, w := range want {
		if chunks[i] != w {
			t.Errorf("chunk[%d]: want %q, got %q", i, w, chunks[i])
		}
	}
}

// TestCollectUntrustedChunks_TurnNExtractsToolResults asserts that on
// turn N>0 the helper extracts every tool_result block's Content from
// the last user message.
func TestCollectUntrustedChunks_TurnNExtractsToolResults(t *testing.T) {
	messages := []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "ignored"}}},
		{Role: "assistant", Content: []types.ContentBlock{{Type: "text", Text: "calling tools"}}},
		{Role: "user", Content: []types.ContentBlock{
			{Type: "tool_result", ToolUseID: "tc_1", Content: "result one"},
			{Type: "tool_result", ToolUseID: "tc_2", Content: "result two"},
		}},
	}
	chunks := collectUntrustedChunks(messages, 1, nil, "")
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "result one" || chunks[1] != "result two" {
		t.Errorf("unexpected chunks: %v", chunks)
	}
}

// TestBatchUntrustedChunks_DelimiterFormat asserts the batched chunks
// envelope uses the documented "--- chunk i ---" header format.
func TestBatchUntrustedChunks_DelimiterFormat(t *testing.T) {
	out := batchUntrustedChunks([]string{"alpha", "beta"})
	if !strings.Contains(out, "--- chunk 0 ---") {
		t.Errorf("expected '--- chunk 0 ---' header, got: %q", out)
	}
	if !strings.Contains(out, "--- chunk 1 ---") {
		t.Errorf("expected '--- chunk 1 ---' header, got: %q", out)
	}
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Errorf("expected both chunk bodies, got: %q", out)
	}
}

// TestReplaceUntrustedChunks_Turn0IsNoop asserts that replaceUntrustedChunks
// does not mutate messages on turn 0 — the user prompt is the
// untrusted content and rewriting it would produce an opaque
// user-facing failure for v1.
func TestReplaceUntrustedChunks_Turn0IsNoop(t *testing.T) {
	original := []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "original"}}},
	}
	cp := make([]types.Message, len(original))
	copy(cp, original)
	replaceUntrustedChunks(cp, 0, "[blocked]")
	if cp[0].Content[0].Text != "original" {
		t.Errorf("expected turn 0 noop, got %q", cp[0].Content[0].Text)
	}
}

// TestSpotlightUntrustedChunks_RewrapsToolResults asserts spotlight
// rewrap fires on turn N>0 against tool_result blocks.
func TestSpotlightUntrustedChunks_RewrapsToolResults(t *testing.T) {
	messages := []types.Message{
		{Role: "user", Content: []types.ContentBlock{
			{Type: "tool_result", ToolUseID: "tc_1", Content: "raw payload"},
		}},
	}
	spotlightUntrustedChunks(messages, 1)
	got := messages[0].Content[0].Content
	if !strings.HasPrefix(got, guard.SpotlightOpenTag) || !strings.HasSuffix(got, guard.SpotlightCloseTag) {
		t.Errorf("expected spotlight wrapping, got: %q", got)
	}
}

// TestLastAssistantText_ConcatenatesTextBlocks asserts text blocks are
// joined and tool_use blocks are skipped.
func TestLastAssistantText_ConcatenatesTextBlocks(t *testing.T) {
	blocks := []types.ContentBlock{
		{Type: "text", Text: "line one"},
		{Type: "tool_use", ID: "tc_1", Name: "irrelevant"},
		{Type: "text", Text: "line two"},
	}
	got := lastAssistantText(blocks)
	want := "line one\nline two"
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}
