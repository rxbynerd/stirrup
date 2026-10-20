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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	must := require.New(t)
	data, err := json.Marshal(v)
	must.NoError(err)
	return data
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestReader_AllAndLast_ReturnRecordsInOrder(t *testing.T) {
	must := require.New(t)
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

	got, err := NewReader(bytes.NewReader(body), WithLogger(discardLogger())).All()
	must.NoError(err)
	must.Len(got, 2)
	must.Equal("first", got[0].ID)
	must.Equal("second", got[1].ID)

	last, err := NewReader(bytes.NewReader(body), WithLogger(discardLogger())).Last()
	must.NoError(err)
	must.Equal("second", last.ID)
}

func TestReader_SkipMalformed(t *testing.T) {
	must := require.New(t)
	good := mustMarshal(t, types.RunTrace{ID: "ok", Turns: 2})

	var buf bytes.Buffer
	buf.WriteString("not json at all\n")
	buf.WriteString("\n") // blank line tolerated, not malformed
	buf.Write(good)
	buf.WriteByte('\n')
	buf.WriteString("{still bad\n")

	got, err := NewReader(&buf, WithLogger(discardLogger())).All()
	must.NoError(err)
	must.Len(got, 1)
	must.Equal("ok", got[0].ID)
}

func TestReader_EmptyFile(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "empty.jsonl")
	must.NoError(os.WriteFile(path, nil, 0o644))

	r, err := Open(path, WithLogger(discardLogger()))
	must.NoError(err)
	defer func() { _ = r.Close() }()

	_, err = r.Last()
	must.Error(err, "Last on an empty file must fail")
}

func TestReader_OversizedLineSkipped(t *testing.T) {
	must := require.New(t)
	good := mustMarshal(t, types.RunTrace{ID: "ok"})
	oversized := bytes.Repeat([]byte("x"), MaxLineBytes+8)

	var buf bytes.Buffer
	buf.Write(oversized)
	buf.WriteByte('\n')
	buf.Write(good)
	buf.WriteByte('\n')

	got, err := NewReader(&buf, WithLogger(discardLogger())).All()
	must.NoError(err)
	must.Len(got, 1)
	must.Equal("ok", got[0].ID)
}

func TestReader_Next_ReturnsEOFOnEmptyInput(t *testing.T) {
	must := require.New(t)
	_, err := NewReader(strings.NewReader(""), WithLogger(discardLogger())).Next()
	must.ErrorIs(err, io.EOF)
}

func TestOpen_StdinSentinel_DoesNotOwnStdin(t *testing.T) {
	must := require.New(t)
	r, err := Open("-", WithLogger(discardLogger()))
	must.NoError(err)
	defer func() { _ = r.Close() }()
	must.Nil(r.closer, "Open(\"-\") must not own stdin")
}

func TestTail_OneShot_ConsumesAllRecordsInOrder(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	var buf bytes.Buffer
	for i, id := range []string{"a", "b", "c"} {
		buf.Write(mustMarshal(t, types.RunTrace{ID: id, Turns: i + 1}))
		buf.WriteByte('\n')
	}
	must.NoError(os.WriteFile(path, buf.Bytes(), 0o644))

	var seen []string
	err := Tail(context.Background(), path, TailOptions{Logger: discardLogger()}, func(tr *types.RunTrace) error {
		seen = append(seen, tr.ID)
		return nil
	})
	must.NoError(err)
	must.Equal([]string{"a", "b", "c"}, seen)
}

func TestTail_Follow_StreamsAppendedRecords(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "live.jsonl")
	initial := mustMarshal(t, types.RunTrace{ID: "first"})
	must.NoError(os.WriteFile(path, append(initial, '\n'), 0o644))

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
			must.Equal(want, got)
		case <-time.After(2 * time.Second):
			must.FailNow("timeout waiting for record", "record %q", want)
		}
	}

	expectID("first")

	// Append a second record while Tail is following.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	must.NoError(err)
	_, err = f.Write(append(mustMarshal(t, types.RunTrace{ID: "second"}), '\n'))
	must.NoError(err)
	_ = f.Close()

	expectID("second")

	cancel()
	select {
	case err := <-doneCh:
		must.NoError(err)
	case <-time.After(2 * time.Second):
		must.FailNow("Tail did not exit after cancel")
	}
}

func TestTail_HandlerError_AbortsTail(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	must.NoError(os.WriteFile(path, append(mustMarshal(t, types.RunTrace{ID: "x"}), '\n'), 0o644))

	stop := errors.New("stop")
	err := Tail(context.Background(), path, TailOptions{Logger: discardLogger()}, func(*types.RunTrace) error {
		return stop
	})
	must.ErrorIs(err, stop)
}

// streamingTrace renders a synthetic streaming-event JSONL file: one
// run_started, len(turns) turn_records, and one run_finished (when
// finalOutcome.ID != "").
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
		if tr.OutputLimitRecovery {
			ev["outputLimitRecovery"] = true
		}
		if tr.StallNudge {
			ev["stallNudge"] = true
		}
		if tr.WindDown != "" {
			ev["windDown"] = tr.WindDown
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
// the streaming wire shape, Reader.Next surfaces the trace embedded
// in the run_finished event and skips the other event kinds.
func TestReader_StreamingFormat_NextYieldsRunFinishedTrace(t *testing.T) {
	must := require.New(t)
	body := streamingTrace(t,
		"run-stream",
		types.RunConfig{RunID: "run-stream", Mode: "execution"},
		[]types.TurnRecord{
			{Turn: 1, ModelOutput: []types.ContentBlock{{Type: "text", Text: "hello"}}},
		},
		types.RunTrace{ID: "run-stream", Turns: 1, Outcome: "success"},
	)
	got, err := NewReader(bytes.NewReader(body), WithLogger(discardLogger())).All()
	must.NoError(err)
	must.Len(got, 1)
	must.Equal("run-stream", got[0].ID)
	must.Equal("success", got[0].Outcome)
}

// TestReader_LegacyAndStreaming_ParseEquivalent pins that a legacy
// single-blob trace and a streaming trace describing the same run
// surface identical *types.RunTrace values via Reader.Next.
func TestReader_LegacyAndStreaming_ParseEquivalent(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	finalOutcome := types.RunTrace{
		ID:      "run-shared",
		Turns:   2,
		Outcome: "success",
		Config:  types.RunConfig{RunID: "run-shared"},
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

	gotLegacy, err := NewReader(bytes.NewReader(legacy), WithLogger(discardLogger())).All()
	must.NoError(err, "legacy All")
	gotStreaming, err := NewReader(bytes.NewReader(streaming), WithLogger(discardLogger())).All()
	must.NoError(err, "streaming All")
	must.Len(gotLegacy, 1)
	must.Len(gotStreaming, 1)

	check.Equal(gotLegacy[0].ID, gotStreaming[0].ID, "ID differs between legacy and streaming")
	check.Equal(gotLegacy[0].Outcome, gotStreaming[0].Outcome, "Outcome differs between legacy and streaming")
	check.Equal(gotLegacy[0].Turns, gotStreaming[0].Turns, "Turns differs between legacy and streaming")
}

// TestReadRecording_StreamingFormat pins that ReadRecording walks
// run_started + turn_record* + run_finished and returns a complete
// *types.RunRecording with transcripts in order.
func TestReadRecording_StreamingFormat(t *testing.T) {
	must := require.New(t)
	turns := []types.TurnRecord{
		{
			Turn:        1,
			ModelInput:  types.ModelInput{Model: "claude-3-5"},
			ModelOutput: []types.ContentBlock{{Type: "text", Text: "first"}},
		},
		{
			Turn:        2,
			ModelOutput: []types.ContentBlock{{Type: "text", Text: "second"}},
		},
	}
	body := streamingTrace(t,
		"run-rec",
		types.RunConfig{RunID: "run-rec", Mode: "execution"},
		turns,
		types.RunTrace{ID: "run-rec", Turns: 2, Outcome: "success"},
	)

	rec, err := NewReader(bytes.NewReader(body), WithLogger(discardLogger())).ReadRecording()
	must.NoError(err)
	must.Equal("run-rec", rec.RunID)
	must.Equal("execution", rec.Config.Mode)
	must.Len(rec.Turns, 2)
	must.Equal(1, rec.Turns[0].Turn)
	must.Equal(2, rec.Turns[1].Turn)
	must.Equal("success", rec.FinalOutcome.Outcome)
}

// TestReadRecording_PreservesOutputLimitRecovery pins that the per-turn
// output-limit recovery flag survives the turn_record round trip.
func TestReadRecording_PreservesOutputLimitRecovery(t *testing.T) {
	must := require.New(t)
	turns := []types.TurnRecord{
		{Turn: 0, OutputLimitRecovery: true},
		{Turn: 1},
	}
	body := streamingTrace(t, "run-olr", types.RunConfig{RunID: "run-olr"}, turns,
		types.RunTrace{ID: "run-olr", Turns: 2, Outcome: "success"})

	rec, err := NewReader(bytes.NewReader(body), WithLogger(discardLogger())).ReadRecording()
	must.NoError(err)
	must.Len(rec.Turns, 2)
	must.True(rec.Turns[0].OutputLimitRecovery)
	must.False(rec.Turns[1].OutputLimitRecovery)
}

// TestReadRecording_PreservesStallNudge pins that the per-turn stall
// nudge flag survives the turn_record round trip.
func TestReadRecording_PreservesStallNudge(t *testing.T) {
	must := require.New(t)
	turns := []types.TurnRecord{
		{Turn: 0},
		{Turn: 1, StallNudge: true},
	}
	body := streamingTrace(t, "run-nudge", types.RunConfig{RunID: "run-nudge"}, turns,
		types.RunTrace{ID: "run-nudge", Turns: 2, Outcome: "success"})

	rec, err := NewReader(bytes.NewReader(body), WithLogger(discardLogger())).ReadRecording()
	must.NoError(err)
	must.Len(rec.Turns, 2)
	must.False(rec.Turns[0].StallNudge)
	must.True(rec.Turns[1].StallNudge)
}

// TestReadRecording_PreservesWindDown pins that the per-turn wind-down
// trigger survives the turn_record round trip.
func TestReadRecording_PreservesWindDown(t *testing.T) {
	must := require.New(t)
	turns := []types.TurnRecord{
		{Turn: 0},
		{Turn: 1, WindDown: "turns"},
	}
	body := streamingTrace(t, "run-wind-down", types.RunConfig{RunID: "run-wind-down"}, turns,
		types.RunTrace{ID: "run-wind-down", Turns: 2, Outcome: "success"})

	rec, err := NewReader(bytes.NewReader(body), WithLogger(discardLogger())).ReadRecording()
	must.NoError(err)
	must.Len(rec.Turns, 2)
	must.Empty(rec.Turns[0].WindDown)
	must.Equal("turns", rec.Turns[1].WindDown)
}

// TestReadRecording_PartialStream pins that a stream interrupted
// before run_finished still reassembles a recording with whatever
// turns it observed; FinalOutcome.ID stays empty so callers can
// detect the truncation.
func TestReadRecording_PartialStream(t *testing.T) {
	must := require.New(t)
	body := streamingTrace(t,
		"run-partial",
		types.RunConfig{RunID: "run-partial"},
		[]types.TurnRecord{
			{Turn: 1, ModelOutput: []types.ContentBlock{{Type: "text", Text: "only"}}},
		},
		types.RunTrace{}, // no final outcome — streamingTrace skips emit
	)
	rec, err := NewReader(bytes.NewReader(body), WithLogger(discardLogger())).ReadRecording()
	must.NoError(err)
	must.Equal("run-partial", rec.RunID)
	must.Len(rec.Turns, 1)
	must.Empty(rec.FinalOutcome.ID, "a truncated stream has no final outcome")
}

// TestReadRecording_LegacyFormat pins that a legacy single-blob trace
// is surfaced through ReadRecording with the embedded RunTrace as
// FinalOutcome and Turns nil. This lets a single consumer accept
// both wire shapes.
func TestReadRecording_LegacyFormat(t *testing.T) {
	must := require.New(t)
	finalOutcome := types.RunTrace{ID: "run-legacy", Turns: 3, Outcome: "success"}
	body := append(mustMarshal(t, finalOutcome), '\n')
	rec, err := NewReader(bytes.NewReader(body), WithLogger(discardLogger())).ReadRecording()
	must.NoError(err)
	must.Equal("run-legacy", rec.RunID)
	must.Empty(rec.Turns, "legacy trace has no transcript")
	must.Equal("run-legacy", rec.FinalOutcome.ID)
}

// oversizedEvent returns a turn_record line longer than MaxLineBytes.
func oversizedEvent(t *testing.T, turn int) []byte {
	t.Helper()
	return mustMarshal(t, map[string]any{
		"kind": "turn_record",
		"turn": turn,
		"modelOutput": []types.ContentBlock{
			{Type: "text", Text: strings.Repeat("x", MaxLineBytes+8)},
		},
	})
}

func turnEvent(t *testing.T, turn int) []byte {
	t.Helper()
	return mustMarshal(t, map[string]any{"kind": "turn_record", "turn": turn})
}

func joinLines(trailingNewline bool, lines ...[]byte) []byte {
	out := bytes.Join(lines, []byte("\n"))
	if trailingNewline {
		out = append(out, '\n')
	}
	return out
}

// TestReadRecording_OversizedLineKeepsRecording pins that an oversized
// line is dropped without discarding the records read before or after
// it, and that the drop is counted.
func TestReadRecording_OversizedLineKeepsRecording(t *testing.T) {
	started := mustMarshal(t, map[string]any{
		"kind":   "run_started",
		"runId":  "run-big",
		"config": types.RunConfig{RunID: "run-big", Mode: "execution"},
	})
	finished := mustMarshal(t, map[string]any{
		"kind":  "run_finished",
		"trace": types.RunTrace{ID: "run-big", Outcome: "success"},
	})

	tests := []struct {
		name      string
		input     []byte
		wantTurns []int
		wantSkips int
		wantDone  bool
	}{
		{
			name: "oversized line between turns",
			input: joinLines(true, started, turnEvent(t, 0), oversizedEvent(t, 1),
				turnEvent(t, 2), finished),
			wantTurns: []int{0, 2},
			wantSkips: 1,
			wantDone:  true,
		},
		{
			name: "two oversized lines separated by a valid line",
			input: joinLines(true, started, oversizedEvent(t, 0), turnEvent(t, 1),
				oversizedEvent(t, 2), turnEvent(t, 3), finished),
			wantTurns: []int{1, 3},
			wantSkips: 2,
			wantDone:  true,
		},
		{
			name:      "oversized last line with trailing newline",
			input:     joinLines(true, started, turnEvent(t, 0), oversizedEvent(t, 1)),
			wantTurns: []int{0},
			wantSkips: 1,
		},
		{
			name:      "oversized last line without trailing newline",
			input:     joinLines(false, started, turnEvent(t, 0), oversizedEvent(t, 1)),
			wantTurns: []int{0},
			wantSkips: 1,
		},
		{
			name:      "no oversized line",
			input:     joinLines(true, started, turnEvent(t, 0), turnEvent(t, 1), finished),
			wantTurns: []int{0, 1},
			wantDone:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			r := NewReader(bytes.NewReader(tc.input), WithLogger(discardLogger()))
			rec, err := r.ReadRecording()
			must.NoError(err)
			must.Equal("run-big", rec.RunID, "run_started lost")
			must.Equal("execution", rec.Config.Mode, "run_started lost")

			var gotTurns []int
			for _, tr := range rec.Turns {
				gotTurns = append(gotTurns, tr.Turn)
			}
			must.Equal(tc.wantTurns, gotTurns)
			must.Equal(tc.wantSkips, rec.SkippedLines)
			must.Equal(tc.wantSkips, r.SkippedLines(), "Reader.SkippedLines")
			must.Equal(tc.wantDone, rec.FinalOutcome.ID != "", "run_finished seen")
		})
	}
}

// TestReadRecording_OversizedLineWarns pins that a non-zero skipped
// count is logged at warn level with the count.
func TestReadRecording_OversizedLineWarns(t *testing.T) {
	must := require.New(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	input := joinLines(true, turnEvent(t, 0), oversizedEvent(t, 1))

	_, err := NewReader(bytes.NewReader(input), WithLogger(logger)).ReadRecording()
	must.NoError(err)
	out := logs.String()
	must.Contains(out, "level=WARN")
	must.Contains(out, "skippedLines=1")
}

// TestReadRecording_CleanFile_NoWarnNoSkippedLines pins that a clean file
// neither warns nor sets SkippedLines, which stays out of the serialised
// recording.
func TestReadRecording_CleanFile_NoWarnNoSkippedLines(t *testing.T) {
	must := require.New(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	rec, err := NewReader(bytes.NewReader(joinLines(true, turnEvent(t, 0))), WithLogger(logger)).ReadRecording()
	must.NoError(err)
	must.Empty(logs.String(), "clean file must not log")
	must.Zero(rec.SkippedLines)
	must.NotContains(string(mustMarshal(t, rec)), "skippedLines", "skippedLines serialised for a complete recording")
}

// TestReader_OversizedTailIsNotReadAsRecord pins that the bytes of an
// oversized line past the cap are discarded rather than parsed as a
// record: the tail here is itself a valid trace record.
func TestReader_OversizedTailIsNotReadAsRecord(t *testing.T) {
	must := require.New(t)
	tail := mustMarshal(t, types.RunTrace{ID: "tail-fragment"})
	var line bytes.Buffer
	line.Write(bytes.Repeat([]byte(" "), MaxLineBytes+16))
	line.Write(tail)
	good := mustMarshal(t, types.RunTrace{ID: "after"})

	r := NewReader(bytes.NewReader(joinLines(true, line.Bytes(), good)), WithLogger(discardLogger()))
	got, err := r.All()
	must.NoError(err)
	must.Len(got, 1, "only the record after the oversized line is read")
	must.Equal("after", got[0].ID)
	must.Equal(1, r.SkippedLines())
}

// TestReader_LineAtCapIsKept pins the boundary: a line of
// MaxLineBytes-1 bytes plus its newline is read, not skipped.
func TestReader_LineAtCapIsKept(t *testing.T) {
	must := require.New(t)
	base := mustMarshal(t, types.RunTrace{ID: "edge"})
	pad := bytes.Repeat([]byte(" "), MaxLineBytes-1-len(base))
	line := append(append([]byte{}, base...), pad...)
	must.Len(line, MaxLineBytes-1, "test line length")

	r := NewReader(bytes.NewReader(joinLines(true, line)), WithLogger(discardLogger()))
	got, err := r.All()
	must.NoError(err)
	must.Len(got, 1)
	must.Equal("edge", got[0].ID)
	must.Zero(r.SkippedLines(), "a line at the cap must not be skipped")
}

func TestReadRecording_ReadsRequestMetadata(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	lines := strings.Join([]string{
		`{"kind":"run_started","schemaVersion":"2","runId":"r1","harnessVersion":"v1.2.3 (abc1234)"}`,
		`{"kind":"system_prompt","runId":"r1","systemPrompt":"be helpful","systemPromptSha256":"deadbeef"}`,
		`{"kind":"turn_record","runId":"r1","turn":0,"modelInput":{"messages":[],"tools":[],"model":"m","request":{"temperature":0.2,"maxTokens":8000,"reasoningEffort":"low","toolChoice":"required","cacheKey":"k"}},"responseModel":"m-20260101"}`,
		`{"kind":"run_finished","trace":{"id":"r1"}}`,
	}, "\n") + "\n"

	rec, err := NewReader(strings.NewReader(lines), WithLogger(discardLogger())).ReadRecording()
	must.NoError(err)
	check.Equal("v1.2.3 (abc1234)", rec.HarnessVersion)
	check.Equal("be helpful", rec.SystemPrompt)
	check.Equal("deadbeef", rec.SystemPromptSHA256)

	must.Len(rec.Turns, 1)
	check.Equal("m-20260101", rec.Turns[0].ResponseModel)

	req := rec.Turns[0].ModelInput.Request
	must.NotNil(req, "request params not read")
	must.NotNil(req.Temperature)
	check.InDelta(0.2, *req.Temperature, 1e-9)
	check.Equal(8000, req.MaxTokens)
	check.Equal("low", req.ReasoningEffort)
	check.Equal(types.ToolChoiceRequired, req.ToolChoice)
	check.Equal("k", req.CacheKey)
}

func TestReadRecording_TracesWithoutRequestMetadataStillParse(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	lines := strings.Join([]string{
		`{"kind":"run_started","schemaVersion":"1","runId":"old"}`,
		`{"kind":"turn_record","turn":0,"modelInput":{"messages":[],"tools":[],"model":"m"}}`,
		`{"kind":"run_finished","trace":{"id":"old"}}`,
	}, "\n") + "\n"

	rec, err := NewReader(strings.NewReader(lines), WithLogger(discardLogger())).ReadRecording()
	must.NoError(err)
	must.Len(rec.Turns, 1)
	must.Nil(rec.Turns[0].ModelInput.Request, "legacy turn has no request params")
	check.Empty(rec.HarnessVersion, "legacy trace gained a harness version")
	check.Empty(rec.SystemPrompt, "legacy trace gained a system prompt")
	check.Empty(rec.SystemPromptSHA256, "legacy trace gained a system prompt hash")
}

func TestReadRecording_SubRunSystemPromptDoesNotReplaceParents(t *testing.T) {
	must := require.New(t)
	lines := strings.Join([]string{
		`{"kind":"run_started","runId":"p"}`,
		`{"kind":"system_prompt","runId":"p","systemPrompt":"parent prompt"}`,
		`{"kind":"system_prompt","runId":"p/sub-1","parentRunId":"p","systemPrompt":"child prompt"}`,
	}, "\n") + "\n"

	rec, err := NewReader(strings.NewReader(lines), WithLogger(discardLogger())).ReadRecording()
	must.NoError(err)
	must.Equal("parent prompt", rec.SystemPrompt, "a sub-run prompt must not replace the parent's")
}
