package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestReader_AllAndLast(t *testing.T) {
	traces := []types.RunTrace{
		{ID: "first", Turns: 1},
		{ID: "second", Turns: 5},
	}

	var buf bytes.Buffer
	for _, tr := range traces {
		buf.Write(mustMarshal(t, tr))
		buf.WriteByte('\n')
	}
	body := buf.Bytes()

	r := NewReader(bytes.NewReader(body), WithLogger(discardLogger()))
	got, err := r.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(All) = %d, want 2", len(got))
	}
	if got[0].ID != "first" || got[1].ID != "second" {
		t.Errorf("All IDs = %q,%q want first,second", got[0].ID, got[1].ID)
	}

	r2 := NewReader(bytes.NewReader(body), WithLogger(discardLogger()))
	last, err := r2.Last()
	if err != nil {
		t.Fatalf("Last: %v", err)
	}
	if last.ID != "second" {
		t.Errorf("Last.ID = %q, want second", last.ID)
	}
}

func TestReader_SkipMalformed(t *testing.T) {
	good := mustMarshal(t, types.RunTrace{ID: "ok", Turns: 2})

	var buf bytes.Buffer
	buf.WriteString("not json at all\n")
	buf.WriteString("\n") // blank line tolerated, not malformed
	buf.Write(good)
	buf.WriteByte('\n')
	buf.WriteString("{still bad\n")

	r := NewReader(&buf, WithLogger(discardLogger()))
	got, err := r.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ok" {
		t.Fatalf("All = %+v, want one record with ID=ok", got)
	}
}

func TestReader_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := Open(path, WithLogger(discardLogger()))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = r.Close() }()

	if _, err := r.Last(); err == nil {
		t.Fatal("Last on empty file: expected error")
	}
}

func TestReader_OversizedLineSkipped(t *testing.T) {
	good := mustMarshal(t, types.RunTrace{ID: "ok"})
	oversized := bytes.Repeat([]byte("x"), MaxLineBytes+8)

	var buf bytes.Buffer
	buf.Write(oversized)
	buf.WriteByte('\n')
	buf.Write(good)
	buf.WriteByte('\n')

	r := NewReader(&buf, WithLogger(discardLogger()))
	got, err := r.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ok" {
		t.Fatalf("got %+v, want one record with ID=ok", got)
	}
}

func TestReader_NextReturnsEOF(t *testing.T) {
	r := NewReader(strings.NewReader(""), WithLogger(discardLogger()))
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next on empty: err = %v, want io.EOF", err)
	}
}

func TestOpen_StdinSentinel(t *testing.T) {
	r, err := Open("-", WithLogger(discardLogger()))
	if err != nil {
		t.Fatalf("Open '-': %v", err)
	}
	defer func() { _ = r.Close() }()
	if r.closer != nil {
		t.Error("Open('-') must not own stdin")
	}
}

func TestTail_OneShotConsumesAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.jsonl")
	var buf bytes.Buffer
	for i, id := range []string{"a", "b", "c"} {
		buf.Write(mustMarshal(t, types.RunTrace{ID: id, Turns: i + 1}))
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	var seen []string
	err := Tail(context.Background(), path, TailOptions{Logger: discardLogger()}, func(tr *types.RunTrace) error {
		seen = append(seen, tr.ID)
		return nil
	})
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if got, want := strings.Join(seen, ","), "a,b,c"; got != want {
		t.Errorf("Tail order = %q, want %q", got, want)
	}
}

func TestTail_FollowStreamsAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.jsonl")
	initial := mustMarshal(t, types.RunTrace{ID: "first"})
	if err := os.WriteFile(path, append(initial, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	seenCh := make(chan string, 8)
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- Tail(ctx, path, TailOptions{
			Follow:       true,
			PollInterval: 10 * time.Millisecond,
			Logger:       discardLogger(),
		}, func(tr *types.RunTrace) error {
			seenCh <- tr.ID
			return nil
		})
	}()

	expectID := func(want string) {
		t.Helper()
		select {
		case got := <-seenCh:
			if got != want {
				t.Fatalf("Tail saw %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for record %q", want)
		}
	}

	expectID("first")

	// Append a second record while Tail is following.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(mustMarshal(t, types.RunTrace{ID: "second"}), '\n')); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	expectID("second")

	cancel()
	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("Tail returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Tail did not exit after cancel")
	}
}

func TestTail_HandlerErrorAborts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.jsonl")
	if err := os.WriteFile(path, append(mustMarshal(t, types.RunTrace{ID: "x"}), '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	stop := errors.New("stop")
	err := Tail(context.Background(), path, TailOptions{Logger: discardLogger()}, func(*types.RunTrace) error {
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("Tail err = %v, want %v", err, stop)
	}
}

// streamingTrace renders a synthetic streaming-event JSONL file: one
// run_started, len(turns) turn_records, and one run_finished (when
// finalOutcome.ID != ""). Used to drive the reader's streaming-format
// tests without coupling them to harness/internal/trace.
func streamingTrace(t *testing.T, runID string, config types.RunConfig, turns []types.TurnRecord, finalOutcome types.RunTrace) []byte {
	t.Helper()
	var buf bytes.Buffer
	started := map[string]any{
		"kind":          "run_started",
		"schemaVersion": "1",
		"runId":         runID,
		"startedAt":     time.Now().UTC(),
		"config":        config,
	}
	buf.Write(mustMarshal(t, started))
	buf.WriteByte('\n')
	for _, tr := range turns {
		ev := map[string]any{
			"kind":        "turn_record",
			"turn":        tr.Turn,
			"modelInput":  tr.ModelInput,
			"modelOutput": tr.ModelOutput,
			"toolCalls":   tr.ToolCalls,
		}
		buf.Write(mustMarshal(t, ev))
		buf.WriteByte('\n')
	}
	if finalOutcome.ID != "" {
		ev := map[string]any{
			"kind":        "run_finished",
			"completedAt": time.Now().UTC(),
			"trace":       finalOutcome,
		}
		buf.Write(mustMarshal(t, ev))
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// TestReader_StreamingFormat_NextYieldsRunFinishedTrace pins that on
// the new streaming wire shape, Reader.Next surfaces the trace
// embedded in the run_finished event and silently skips the
// run_started / turn_record / tool_call_record events. Legacy
// consumers (eval runner's parseTraceFile) keep working unchanged.
func TestReader_StreamingFormat_NextYieldsRunFinishedTrace(t *testing.T) {
	body := streamingTrace(t,
		"run-stream",
		types.RunConfig{RunID: "run-stream", Mode: "execution"},
		[]types.TurnRecord{
			{Turn: 1, ModelOutput: []types.ContentBlock{{Type: "text", Text: "hello"}}},
		},
		types.RunTrace{ID: "run-stream", Turns: 1, Outcome: "success"},
	)
	r := NewReader(bytes.NewReader(body), WithLogger(discardLogger()))
	got, err := r.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d traces, want 1", len(got))
	}
	if got[0].ID != "run-stream" || got[0].Outcome != "success" {
		t.Errorf("trace = %+v, want ID=run-stream Outcome=success", got[0])
	}
}

// TestReader_LegacyAndStreamingParseEquivalent pins that a legacy
// single-blob trace and a streaming trace describing the same run
// surface identical *types.RunTrace values via Reader.Next. This is
// the backward-compatibility AC of #270.
func TestReader_LegacyAndStreamingParseEquivalent(t *testing.T) {
	finalOutcome := types.RunTrace{
		ID:       "run-shared",
		Turns:    2,
		Outcome:  "success",
		Config:   types.RunConfig{RunID: "run-shared"},
	}

	legacy := append(mustMarshal(t, finalOutcome), '\n')
	streaming := streamingTrace(t,
		"run-shared",
		types.RunConfig{RunID: "run-shared"},
		[]types.TurnRecord{
			{Turn: 1, ModelOutput: []types.ContentBlock{{Type: "text", Text: "first"}}},
			{Turn: 2, ModelOutput: []types.ContentBlock{{Type: "text", Text: "second"}}},
		},
		finalOutcome,
	)

	rLegacy := NewReader(bytes.NewReader(legacy), WithLogger(discardLogger()))
	gotLegacy, err := rLegacy.All()
	if err != nil {
		t.Fatalf("legacy All: %v", err)
	}
	rStreaming := NewReader(bytes.NewReader(streaming), WithLogger(discardLogger()))
	gotStreaming, err := rStreaming.All()
	if err != nil {
		t.Fatalf("streaming All: %v", err)
	}
	if len(gotLegacy) != 1 || len(gotStreaming) != 1 {
		t.Fatalf("len: legacy=%d streaming=%d, want 1/1", len(gotLegacy), len(gotStreaming))
	}
	if gotLegacy[0].ID != gotStreaming[0].ID {
		t.Errorf("ID differs: legacy=%q streaming=%q", gotLegacy[0].ID, gotStreaming[0].ID)
	}
	if gotLegacy[0].Outcome != gotStreaming[0].Outcome {
		t.Errorf("Outcome differs: legacy=%q streaming=%q", gotLegacy[0].Outcome, gotStreaming[0].Outcome)
	}
	if gotLegacy[0].Turns != gotStreaming[0].Turns {
		t.Errorf("Turns differs: legacy=%d streaming=%d", gotLegacy[0].Turns, gotStreaming[0].Turns)
	}
}

// TestReadRecording_StreamingFormat pins that ReadRecording walks
// run_started + turn_record* + run_finished and returns a complete
// *types.RunRecording with transcripts in order.
func TestReadRecording_StreamingFormat(t *testing.T) {
	turns := []types.TurnRecord{
		{
			Turn: 1,
			ModelInput: types.ModelInput{Model: "claude-3-5"},
			ModelOutput: []types.ContentBlock{{Type: "text", Text: "first"}},
		},
		{
			Turn: 2,
			ModelOutput: []types.ContentBlock{{Type: "text", Text: "second"}},
		},
	}
	body := streamingTrace(t,
		"run-rec",
		types.RunConfig{RunID: "run-rec", Mode: "execution"},
		turns,
		types.RunTrace{ID: "run-rec", Turns: 2, Outcome: "success"},
	)

	r := NewReader(bytes.NewReader(body), WithLogger(discardLogger()))
	rec, err := r.ReadRecording()
	if err != nil {
		t.Fatalf("ReadRecording: %v", err)
	}
	if rec.RunID != "run-rec" {
		t.Errorf("RunID: got %q, want run-rec", rec.RunID)
	}
	if rec.Config.Mode != "execution" {
		t.Errorf("Config.Mode: got %q, want execution", rec.Config.Mode)
	}
	if len(rec.Turns) != 2 {
		t.Fatalf("Turns: got %d, want 2", len(rec.Turns))
	}
	if rec.Turns[0].Turn != 1 || rec.Turns[1].Turn != 2 {
		t.Errorf("turn order: got %d, %d, want 1, 2", rec.Turns[0].Turn, rec.Turns[1].Turn)
	}
	if rec.FinalOutcome.Outcome != "success" {
		t.Errorf("FinalOutcome.Outcome: got %q, want success", rec.FinalOutcome.Outcome)
	}
}

// TestReadRecording_PartialStream pins that a stream interrupted
// before run_finished still reassembles a recording with whatever
// turns it observed; FinalOutcome.ID stays empty so callers can
// detect the truncation.
func TestReadRecording_PartialStream(t *testing.T) {
	body := streamingTrace(t,
		"run-partial",
		types.RunConfig{RunID: "run-partial"},
		[]types.TurnRecord{
			{Turn: 1, ModelOutput: []types.ContentBlock{{Type: "text", Text: "only"}}},
		},
		types.RunTrace{}, // no final outcome — streamingTrace skips emit
	)
	r := NewReader(bytes.NewReader(body), WithLogger(discardLogger()))
	rec, err := r.ReadRecording()
	if err != nil {
		t.Fatalf("ReadRecording: %v", err)
	}
	if rec.RunID != "run-partial" {
		t.Errorf("RunID: got %q, want run-partial", rec.RunID)
	}
	if len(rec.Turns) != 1 {
		t.Errorf("Turns: got %d, want 1", len(rec.Turns))
	}
	if rec.FinalOutcome.ID != "" {
		t.Errorf("FinalOutcome.ID: got %q, want \"\" (truncated stream)", rec.FinalOutcome.ID)
	}
}

// TestReadRecording_LegacyFormat pins that a legacy single-blob trace
// is surfaced through ReadRecording with the embedded RunTrace as
// FinalOutcome and Turns nil. This lets a single consumer accept
// both wire shapes.
func TestReadRecording_LegacyFormat(t *testing.T) {
	finalOutcome := types.RunTrace{ID: "run-legacy", Turns: 3, Outcome: "success"}
	body := append(mustMarshal(t, finalOutcome), '\n')
	r := NewReader(bytes.NewReader(body), WithLogger(discardLogger()))
	rec, err := r.ReadRecording()
	if err != nil {
		t.Fatalf("ReadRecording: %v", err)
	}
	if rec.RunID != "run-legacy" {
		t.Errorf("RunID: got %q, want run-legacy", rec.RunID)
	}
	if len(rec.Turns) != 0 {
		t.Errorf("Turns: got %d, want 0 (legacy has no transcript)", len(rec.Turns))
	}
	if rec.FinalOutcome.ID != "run-legacy" {
		t.Errorf("FinalOutcome.ID: got %q, want run-legacy", rec.FinalOutcome.ID)
	}
}
