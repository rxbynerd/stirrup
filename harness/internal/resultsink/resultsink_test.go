package resultsink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// emittedPayload returns the JSON body that follows the stdout sentinel.
func emittedPayload(line string) string {
	return strings.TrimSpace(strings.TrimPrefix(line, StdoutResultSentinel))
}

// decodeEmittedResult decodes the RunResult carried by one emitted line.
func decodeEmittedResult(t *testing.T, line string) types.RunResult {
	t.Helper()
	payload := emittedPayload(line)
	var decoded types.RunResult
	require.NoError(t, json.Unmarshal([]byte(payload), &decoded), "payload=%q", payload)
	return decoded
}

func TestStdoutJSONSink_Emit_WritesSentinelAndJSON(t *testing.T) {
	var buf bytes.Buffer
	sink := NewStdoutJSONSinkTo(&buf)

	res := types.RunResult{
		SchemaVersion:      1,
		RunID:              "run-xyz",
		Outcome:            "success",
		Turns:              3,
		TokenUsage:         types.TokenUsage{Input: 100, Output: 50},
		DurationMs:         1234,
		FinalAssistantText: "done",
	}
	require.NoError(t, sink.Emit(context.Background(), res))

	out := buf.String()
	assert.True(t, strings.HasPrefix(out, StdoutResultSentinel), "output missing sentinel prefix: %q", out)
	assert.True(t, strings.HasSuffix(out, "\n"), "output missing trailing newline: %q", out)

	// Exact sentinel bytes must be "STIRRUP_RESULT " — pinned as a
	// contract so the smoke workflow can grep for it verbatim.
	assert.Equal(t, "STIRRUP_RESULT ", StdoutResultSentinel)

	decoded := decodeEmittedResult(t, out)
	assert.Equal(t, "run-xyz", decoded.RunID)
	assert.Equal(t, "success", decoded.Outcome)
	assert.Equal(t, 3, decoded.Turns)
}

// TestStdoutJSONSink_FinalAssistantText_OmitsKeyWhenEmpty pins the wire
// behaviour of the run's last assistant text through the sink: a populated
// value both appears in the emitted JSON and round-trips on decode, while an
// empty value is dropped entirely by the omitempty tag so a run with no
// assistant text emits no finalAssistantText key.
func TestStdoutJSONSink_FinalAssistantText_OmitsKeyWhenEmpty(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		wantKey bool
	}{
		{"populated", "the answer is 42", true},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			sink := NewStdoutJSONSinkTo(&buf)
			res := types.RunResult{
				SchemaVersion:      1,
				RunID:              "run-fat",
				Outcome:            "success",
				FinalAssistantText: tc.text,
			}
			require.NoError(t, sink.Emit(context.Background(), res))

			payload := emittedPayload(buf.String())
			hasKey := strings.Contains(payload, "finalAssistantText")
			require.Equal(t, tc.wantKey, hasKey, "finalAssistantText key present; payload=%q", payload)

			decoded := decodeEmittedResult(t, buf.String())
			require.Equal(t, tc.text, decoded.FinalAssistantText)
		})
	}
}

// TestStdoutJSONSink_FinalAssistantTextTruncated_OmitsKeyWhenFalse pins the
// wire behaviour of the truncation flag through the sink: for a payload
// well under maxEncodedResultBytes the sink is a pure passthrough
// (the upstream buildRunResult cap already ran), so a
// FinalAssistantTextTruncated=true round-trips through the emitted
// JSON and a false value is omitted by the omitempty tag.
func TestStdoutJSONSink_FinalAssistantTextTruncated_OmitsKeyWhenFalse(t *testing.T) {
	cases := []struct {
		name      string
		truncated bool
		wantKey   bool
	}{
		{"truncated", true, true},
		{"not truncated", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			sink := NewStdoutJSONSinkTo(&buf)
			res := types.RunResult{
				SchemaVersion:               1,
				RunID:                       "run-cap",
				Outcome:                     "success",
				FinalAssistantText:          "the answer is 42... [truncated by harness]",
				FinalAssistantTextTruncated: tc.truncated,
			}
			require.NoError(t, sink.Emit(context.Background(), res))

			payload := emittedPayload(buf.String())
			hasKey := strings.Contains(payload, "finalAssistantTextTruncated")
			require.Equal(t, tc.wantKey, hasKey, "finalAssistantTextTruncated key present; payload=%q", payload)

			decoded := decodeEmittedResult(t, buf.String())
			require.Equal(t, tc.truncated, decoded.FinalAssistantTextTruncated)
		})
	}
}

// TestStdoutJSONSink_Emit_EncodedCapExactBoundary pins the
// less-than-or-equal semantics at the exact byte: a line encoding to
// exactly maxEncodedResultBytes passes through untouched, while one
// byte more triggers truncation and never exceeds the cap.
func TestStdoutJSONSink_Emit_EncodedCapExactBoundary(t *testing.T) {
	base := types.RunResult{SchemaVersion: 1, RunID: "run-exact", Outcome: "success"}

	probe := base
	probe.FinalAssistantText = "x"
	probeLine, err := marshalResultLine(probe)
	require.NoError(t, err, "marshalResultLine")
	// Every further "x" byte is unescaped ASCII, so the line grows by
	// exactly one byte per character added to FinalAssistantText.
	fixedOverhead := len(probeLine) - 1

	fitsExactly := base
	fitsExactly.FinalAssistantText = strings.Repeat("x", maxEncodedResultBytes-fixedOverhead)

	var buf bytes.Buffer
	sink := NewStdoutJSONSinkTo(&buf)
	require.NoError(t, sink.Emit(context.Background(), fitsExactly))
	require.Equal(t, maxEncodedResultBytes, buf.Len(), "exact-fit line length")

	decoded := decodeEmittedResult(t, buf.String())
	require.False(t, decoded.FinalAssistantTextTruncated, "exact-fit payload should not be marked truncated")
	require.Equal(t, fitsExactly.FinalAssistantText, decoded.FinalAssistantText, "exact-fit payload's FinalAssistantText was altered")

	oneOver := base
	oneOver.FinalAssistantText = strings.Repeat("x", maxEncodedResultBytes-fixedOverhead+1)

	buf.Reset()
	require.NoError(t, sink.Emit(context.Background(), oneOver))
	require.LessOrEqual(t, buf.Len(), maxEncodedResultBytes, "one-byte-over line length")

	decoded = decodeEmittedResult(t, buf.String())
	require.True(t, decoded.FinalAssistantTextTruncated, "one-byte-over payload should be marked truncated")
}

// TestStdoutJSONSink_Emit_EncodedCapCatchesLineSeparatorInflation covers the
// residual expansion documented on Emit: Go's encoder always escapes
// U+2028/U+2029 to a 6-byte \uXXXX sequence regardless of SetEscapeHTML, so a
// payload whose raw byte size is comfortably under maxEncodedResultBytes can
// still encode past it.
func TestStdoutJSONSink_Emit_EncodedCapCatchesLineSeparatorInflation(t *testing.T) {
	text := strings.Repeat(" ", 50000) // 150,000 raw bytes, 300,000 encoded bytes
	require.Less(t, len(text), maxEncodedResultBytes, "test fixture raw size should be under the cap")
	result := types.RunResult{SchemaVersion: 1, RunID: "run-u2028", Outcome: "success", FinalAssistantText: text}

	var buf bytes.Buffer
	sink := NewStdoutJSONSinkTo(&buf)
	require.NoError(t, sink.Emit(context.Background(), result))
	require.LessOrEqual(t, buf.Len(), maxEncodedResultBytes, "emitted line length")

	decoded := decodeEmittedResult(t, buf.String())
	require.True(t, decoded.FinalAssistantTextTruncated, "U+2028-heavy payload under the raw cap but over the encoded cap should be truncated")
}

// TestStdoutJSONSink_Emit_HTMLCharactersNotEscaped pins that disabling
// SetEscapeHTML means "<", ">", and "&" encode 1:1 instead of expanding 6x,
// so a payload full of them that fits under maxEncodedResultBytes is emitted
// whole rather than needlessly truncated by escaping inflation.
func TestStdoutJSONSink_Emit_HTMLCharactersNotEscaped(t *testing.T) {
	text := strings.Repeat("<a>&", maxEncodedResultBytes/8) // well under the cap even unescaped
	result := types.RunResult{SchemaVersion: 1, RunID: "run-html", Outcome: "success", FinalAssistantText: text}

	var buf bytes.Buffer
	sink := NewStdoutJSONSinkTo(&buf)
	require.NoError(t, sink.Emit(context.Background(), result))

	payload := emittedPayload(buf.String())
	preview := payload
	if len(preview) > 200 {
		preview = preview[:200]
	}
	assert.Contains(t, payload, `<a>&`, "HTML characters were escaped despite SetEscapeHTML(false): %q", preview)

	decoded := decodeEmittedResult(t, buf.String())
	assert.False(t, decoded.FinalAssistantTextTruncated, "HTML-heavy payload under the cap should not be truncated")
	assert.Equal(t, text, decoded.FinalAssistantText, "FinalAssistantText was altered despite fitting under the cap")
}

// TestStdoutJSONSink_Emit_ShrinksFeedbackBeforeFinalAssistantText pins the
// truncation priority: VerifierVerdict.Feedback is sacrificed before
// FinalAssistantText, since the run's own answer is more valuable than a
// judge's secondary verdict. A large Feedback should be truncated while a
// modest FinalAssistantText is preserved intact.
func TestStdoutJSONSink_Emit_ShrinksFeedbackBeforeFinalAssistantText(t *testing.T) {
	answer := "the final answer is 42"
	result := types.RunResult{
		SchemaVersion:      1,
		RunID:              "run-priority",
		Outcome:            "success",
		FinalAssistantText: answer,
		VerifierVerdict: &types.VerifierResult{
			Passed:   false,
			Feedback: strings.Repeat("x", maxEncodedResultBytes),
		},
	}

	var buf bytes.Buffer
	sink := NewStdoutJSONSinkTo(&buf)
	require.NoError(t, sink.Emit(context.Background(), result))
	require.LessOrEqual(t, buf.Len(), maxEncodedResultBytes, "emitted line length")

	decoded := decodeEmittedResult(t, buf.String())
	assert.Equal(t, answer, decoded.FinalAssistantText, "FinalAssistantText was truncated even though Feedback should absorb the cut first")
	assert.False(t, decoded.FinalAssistantTextTruncated, "FinalAssistantTextTruncated should stay false when Feedback alone can absorb the cut")
	require.NotNil(t, decoded.VerifierVerdict, "VerifierVerdict was dropped")
	assert.Less(t, len(decoded.VerifierVerdict.Feedback), len(result.VerifierVerdict.Feedback), "VerifierVerdict.Feedback was not truncated")
}

// TestStdoutJSONSink_Emit_EnvelopeAloneOverCap covers the fallback path: when
// VerifierVerdict.Feedback alone (with FinalAssistantText already empty)
// exceeds the cap even fully truncated, Emit does not hang, still emits a
// decodable line, and logs a warning naming the encoded size and the cap
// rather than failing silently.
func TestStdoutJSONSink_Emit_EnvelopeAloneOverCap(t *testing.T) {
	// Error is neither Feedback nor FinalAssistantText, so it is not
	// truncated by either search stage: an oversized Error is the
	// simplest way to construct the "nothing left to shrink" case.
	result := types.RunResult{
		SchemaVersion: 1,
		RunID:         "run-envelope-over-cap",
		Outcome:       "internal-error",
		Error:         strings.Repeat("e", 300*1024),
	}

	prevDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevDefault) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	var buf bytes.Buffer
	sink := NewStdoutJSONSinkTo(&buf)
	require.NoError(t, sink.Emit(context.Background(), result))
	require.Greater(t, buf.Len(), maxEncodedResultBytes,
		"the fixture must exceed the cap even after truncation, or the test no longer exercises the fallback path")

	decoded := decodeEmittedResult(t, buf.String())
	assert.Equal(t, result.RunID, decoded.RunID)

	assert.Contains(t, logBuf.String(), "encoded line cap", "expected a warning naming the encoded line cap")
	wantCap := fmt.Sprintf(`"cap":%d`, maxEncodedResultBytes)
	assert.Contains(t, logBuf.String(), wantCap, "expected the warning to name the cap")
}

// TestStdoutJSONSink_Emit_AlreadyTruncatedInput covers a FinalAssistantText
// that already carries the upstream truncation marker (buildRunResult's
// raw-size cap already fired) and is still over the encoded cap. Emit must
// not duplicate the marker and must leave the truncated flag set.
func TestStdoutJSONSink_Emit_AlreadyTruncatedInput(t *testing.T) {
	const marker = "... [truncated by harness]"
	// The run of "x" is far longer than the marker, so the binary
	// search's cut point lands deep inside it rather than inside the
	// marker itself, clear of the edge case where a cut inside a
	// pre-existing marker would nest one marker inside another.
	text := strings.Repeat("x", 300*1024) + marker
	result := types.RunResult{
		SchemaVersion:               1,
		RunID:                       "run-already-truncated",
		Outcome:                     "success",
		FinalAssistantText:          text,
		FinalAssistantTextTruncated: true,
	}

	var buf bytes.Buffer
	sink := NewStdoutJSONSinkTo(&buf)
	require.NoError(t, sink.Emit(context.Background(), result))
	require.LessOrEqual(t, buf.Len(), maxEncodedResultBytes, "emitted line length")

	decoded := decodeEmittedResult(t, buf.String())
	assert.True(t, decoded.FinalAssistantTextTruncated, "FinalAssistantTextTruncated should remain true")
	assert.Equal(t, 1, strings.Count(decoded.FinalAssistantText, marker), "expected exactly one truncation marker")
}

func TestNoneSink_EmitIsNoOp(t *testing.T) {
	sink := NoneSink{}
	require.NoError(t, sink.Emit(context.Background(), types.RunResult{}))
}

func TestNewResultSink_SelectsSinkByType(t *testing.T) {
	cases := []struct {
		name     string
		cfg      *types.ResultSinkConfig
		wantType any
	}{
		{"nil", nil, NoneSink{}},
		{"empty type", &types.ResultSinkConfig{Type: ""}, NoneSink{}},
		{"none", &types.ResultSinkConfig{Type: "none"}, NoneSink{}},
		{"stdout-json", &types.ResultSinkConfig{Type: "stdout-json"}, (*StdoutJSONSink)(nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink, err := NewResultSink(tc.cfg)
			require.NoError(t, err)
			require.IsType(t, tc.wantType, sink)
		})
	}
}

func TestNewResultSink_ReservedTypesRejected(t *testing.T) {
	for _, typ := range []string{"gcp-pubsub", "gcs"} {
		t.Run(typ, func(t *testing.T) {
			_, err := NewResultSink(&types.ResultSinkConfig{Type: typ})
			require.ErrorContains(t, err, "not yet implemented")
		})
	}
}

func TestNewResultSink_UnknownTypeRejected(t *testing.T) {
	_, err := NewResultSink(&types.ResultSinkConfig{Type: "kinesis"})
	require.ErrorContains(t, err, "unsupported resultSink.type")
}
