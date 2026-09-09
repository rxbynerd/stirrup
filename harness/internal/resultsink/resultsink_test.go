package resultsink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

func TestStdoutJSONSink_EmitsSentinelAndJSON(t *testing.T) {
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
	if err := sink.Emit(context.Background(), res); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	out := buf.String()
	if !strings.HasPrefix(out, StdoutResultSentinel) {
		t.Errorf("output missing sentinel prefix: %q", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("output missing trailing newline: %q", out)
	}

	// Exact sentinel bytes must be "STIRRUP_RESULT " — pinned as a
	// contract so the smoke workflow can grep for it verbatim.
	if StdoutResultSentinel != "STIRRUP_RESULT " {
		t.Errorf("sentinel changed: got %q", StdoutResultSentinel)
	}

	payload := strings.TrimSpace(strings.TrimPrefix(out, StdoutResultSentinel))
	var decoded types.RunResult
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("unmarshal payload: %v\npayload=%q", err, payload)
	}
	if decoded.RunID != "run-xyz" {
		t.Errorf("decoded RunID: got %q, want run-xyz", decoded.RunID)
	}
	if decoded.Outcome != "success" {
		t.Errorf("decoded Outcome: got %q, want success", decoded.Outcome)
	}
	if decoded.Turns != 3 {
		t.Errorf("decoded Turns: got %d, want 3", decoded.Turns)
	}
}

// TestStdoutJSONSink_FinalAssistantText pins the wire behaviour of the
// run's last assistant text through the sink: a populated value both
// appears in the emitted JSON and round-trips on decode, while an empty
// value is dropped entirely by the omitempty tag so a run with no
// assistant text emits no finalAssistantText key.
func TestStdoutJSONSink_FinalAssistantText(t *testing.T) {
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
			if err := sink.Emit(context.Background(), res); err != nil {
				t.Fatalf("Emit: %v", err)
			}
			payload := strings.TrimSpace(strings.TrimPrefix(buf.String(), StdoutResultSentinel))
			hasKey := strings.Contains(payload, "finalAssistantText")
			if hasKey != tc.wantKey {
				t.Errorf("finalAssistantText key present = %v, want %v\npayload=%q", hasKey, tc.wantKey, payload)
			}
			var decoded types.RunResult
			if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
				t.Fatalf("unmarshal payload: %v\npayload=%q", err, payload)
			}
			if decoded.FinalAssistantText != tc.text {
				t.Errorf("decoded FinalAssistantText = %q, want %q", decoded.FinalAssistantText, tc.text)
			}
		})
	}
}

// TestStdoutJSONSink_FinalAssistantTextTruncated pins the wire
// behaviour of the truncation flag through the sink: for a payload
// well under maxEncodedResultBytes the sink is a pure passthrough
// (the upstream buildRunResult cap already ran), so a
// FinalAssistantTextTruncated=true round-trips through the emitted
// JSON and a false value is omitted by the omitempty tag.
func TestStdoutJSONSink_FinalAssistantTextTruncated(t *testing.T) {
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
			if err := sink.Emit(context.Background(), res); err != nil {
				t.Fatalf("Emit: %v", err)
			}
			payload := strings.TrimSpace(strings.TrimPrefix(buf.String(), StdoutResultSentinel))
			hasKey := strings.Contains(payload, "finalAssistantTextTruncated")
			if hasKey != tc.wantKey {
				t.Errorf("finalAssistantTextTruncated key present = %v, want %v\npayload=%q", hasKey, tc.wantKey, payload)
			}
			var decoded types.RunResult
			if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
				t.Fatalf("unmarshal payload: %v\npayload=%q", err, payload)
			}
			if decoded.FinalAssistantTextTruncated != tc.truncated {
				t.Errorf("decoded FinalAssistantTextTruncated = %v, want %v", decoded.FinalAssistantTextTruncated, tc.truncated)
			}
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
	if err != nil {
		t.Fatalf("marshalResultLine: %v", err)
	}
	// Every further "x" byte is unescaped ASCII, so the line grows by
	// exactly one byte per character added to FinalAssistantText.
	fixedOverhead := len(probeLine) - 1

	fitsExactly := base
	fitsExactly.FinalAssistantText = strings.Repeat("x", maxEncodedResultBytes-fixedOverhead)

	var buf bytes.Buffer
	sink := NewStdoutJSONSinkTo(&buf)
	if err := sink.Emit(context.Background(), fitsExactly); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if got := buf.Len(); got != maxEncodedResultBytes {
		t.Fatalf("exact-fit line length = %d, want %d", got, maxEncodedResultBytes)
	}
	var decoded types.RunResult
	if err := json.Unmarshal(bytes.TrimPrefix(bytes.TrimSpace(buf.Bytes()), []byte(StdoutResultSentinel)), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.FinalAssistantTextTruncated {
		t.Error("exact-fit payload should not be marked truncated")
	}
	if decoded.FinalAssistantText != fitsExactly.FinalAssistantText {
		t.Error("exact-fit payload's FinalAssistantText was altered")
	}

	oneOver := base
	oneOver.FinalAssistantText = strings.Repeat("x", maxEncodedResultBytes-fixedOverhead+1)

	buf.Reset()
	if err := sink.Emit(context.Background(), oneOver); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if got := buf.Len(); got > maxEncodedResultBytes {
		t.Fatalf("one-byte-over line length = %d, want <= %d", got, maxEncodedResultBytes)
	}
	decoded = types.RunResult{}
	if err := json.Unmarshal(bytes.TrimPrefix(bytes.TrimSpace(buf.Bytes()), []byte(StdoutResultSentinel)), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !decoded.FinalAssistantTextTruncated {
		t.Error("one-byte-over payload should be marked truncated")
	}
}

// TestStdoutJSONSink_Emit_EncodedCapCatchesLineSeparatorInflation
// covers the residual expansion documented on Emit: Go's encoder
// always escapes U+2028/U+2029 to a 6-byte \uXXXX sequence regardless
// of SetEscapeHTML, so a payload whose raw byte size is comfortably
// under maxEncodedResultBytes can still encode past it.
func TestStdoutJSONSink_Emit_EncodedCapCatchesLineSeparatorInflation(t *testing.T) {
	text := strings.Repeat("\u2028", 50000) // 150,000 raw bytes, 300,000 encoded bytes
	if len(text) >= maxEncodedResultBytes {
		t.Fatalf("test fixture raw size %d should be under the cap %d", len(text), maxEncodedResultBytes)
	}
	result := types.RunResult{SchemaVersion: 1, RunID: "run-u2028", Outcome: "success", FinalAssistantText: text}

	var buf bytes.Buffer
	sink := NewStdoutJSONSinkTo(&buf)
	if err := sink.Emit(context.Background(), result); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if got := buf.Len(); got > maxEncodedResultBytes {
		t.Fatalf("emitted line length = %d, want <= %d", got, maxEncodedResultBytes)
	}

	var decoded types.RunResult
	if err := json.Unmarshal(bytes.TrimPrefix(bytes.TrimSpace(buf.Bytes()), []byte(StdoutResultSentinel)), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !decoded.FinalAssistantTextTruncated {
		t.Error("U+2028-heavy payload under the raw cap but over the encoded cap should be truncated")
	}
}

// TestStdoutJSONSink_Emit_HTMLCharactersNotEscaped pins that
// disabling SetEscapeHTML means "<", ">", and "&" encode 1:1 instead
// of expanding 6x, so a payload full of them that fits under
// maxEncodedResultBytes is emitted whole rather than needlessly
// truncated by escaping inflation.
func TestStdoutJSONSink_Emit_HTMLCharactersNotEscaped(t *testing.T) {
	text := strings.Repeat("<a>&", maxEncodedResultBytes/8) // well under the cap even unescaped
	result := types.RunResult{SchemaVersion: 1, RunID: "run-html", Outcome: "success", FinalAssistantText: text}

	var buf bytes.Buffer
	sink := NewStdoutJSONSinkTo(&buf)
	if err := sink.Emit(context.Background(), result); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	payload := strings.TrimSpace(strings.TrimPrefix(buf.String(), StdoutResultSentinel))
	if !strings.Contains(payload, `<a>&`) {
		preview := payload
		if len(preview) > 200 {
			preview = preview[:200]
		}
		t.Errorf("HTML characters were escaped despite SetEscapeHTML(false): %q", preview)
	}

	var decoded types.RunResult
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.FinalAssistantTextTruncated {
		t.Error("HTML-heavy payload under the cap should not be truncated")
	}
	if decoded.FinalAssistantText != text {
		t.Error("FinalAssistantText was altered despite fitting under the cap")
	}
}

// TestStdoutJSONSink_Emit_ShrinksFeedbackBeforeFinalAssistantText pins
// the truncation priority: VerifierVerdict.Feedback is sacrificed
// before FinalAssistantText, since the run's own answer is more
// valuable than a judge's secondary verdict. A large Feedback should
// be truncated while a modest FinalAssistantText is preserved intact.
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
	if err := sink.Emit(context.Background(), result); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if got := buf.Len(); got > maxEncodedResultBytes {
		t.Fatalf("emitted line length = %d, want <= %d", got, maxEncodedResultBytes)
	}

	var decoded types.RunResult
	if err := json.Unmarshal(bytes.TrimPrefix(bytes.TrimSpace(buf.Bytes()), []byte(StdoutResultSentinel)), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.FinalAssistantText != answer {
		t.Errorf("FinalAssistantText was truncated even though Feedback should absorb the cut first: got %q, want %q", decoded.FinalAssistantText, answer)
	}
	if decoded.FinalAssistantTextTruncated {
		t.Error("FinalAssistantTextTruncated should stay false when Feedback alone can absorb the cut")
	}
	if decoded.VerifierVerdict == nil || len(decoded.VerifierVerdict.Feedback) >= len(result.VerifierVerdict.Feedback) {
		gotLen := -1
		if decoded.VerifierVerdict != nil {
			gotLen = len(decoded.VerifierVerdict.Feedback)
		}
		t.Errorf("VerifierVerdict.Feedback was not truncated: got length %d, original length %d", gotLen, len(result.VerifierVerdict.Feedback))
	}
}

// TestStdoutJSONSink_Emit_EnvelopeAloneOverCap covers the fallback
// path: when VerifierVerdict.Feedback alone (with FinalAssistantText
// already empty) exceeds the cap even fully truncated, Emit does not
// hang, still emits a decodable line, and logs a warning naming the
// encoded size and the cap rather than failing silently.
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
	if err := sink.Emit(context.Background(), result); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	if got := buf.Len(); got <= maxEncodedResultBytes {
		t.Fatalf("expected the fixture to exceed the cap even after truncation (got %d, cap %d) — test no longer exercises the fallback path", got, maxEncodedResultBytes)
	}

	var decoded types.RunResult
	if err := json.Unmarshal(bytes.TrimPrefix(bytes.TrimSpace(buf.Bytes()), []byte(StdoutResultSentinel)), &decoded); err != nil {
		t.Fatalf("emitted line does not decode: %v\nline=%q", err, buf.String())
	}
	if decoded.RunID != result.RunID {
		t.Errorf("decoded RunID = %q, want %q", decoded.RunID, result.RunID)
	}

	if !strings.Contains(logBuf.String(), "encoded line cap") {
		t.Errorf("expected a warning naming the encoded line cap, got log output: %s", logBuf.String())
	}
	wantCap := fmt.Sprintf(`"cap":%d`, maxEncodedResultBytes)
	if !strings.Contains(logBuf.String(), wantCap) {
		t.Errorf("expected the warning to name the cap (%s), got: %s", wantCap, logBuf.String())
	}
}

// TestStdoutJSONSink_Emit_AlreadyTruncatedInput covers a
// FinalAssistantText that already carries the upstream truncation
// marker (buildRunResult's raw-size cap already fired) and is still
// over the encoded cap. Emit must not duplicate the marker and must
// leave the truncated flag set.
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
	if err := sink.Emit(context.Background(), result); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if got := buf.Len(); got > maxEncodedResultBytes {
		t.Fatalf("emitted line length = %d, want <= %d", got, maxEncodedResultBytes)
	}

	var decoded types.RunResult
	if err := json.Unmarshal(bytes.TrimPrefix(bytes.TrimSpace(buf.Bytes()), []byte(StdoutResultSentinel)), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !decoded.FinalAssistantTextTruncated {
		t.Error("FinalAssistantTextTruncated should remain true")
	}
	if count := strings.Count(decoded.FinalAssistantText, marker); count != 1 {
		t.Errorf("expected exactly one truncation marker, got %d in %q", count, decoded.FinalAssistantText)
	}
}

func TestNoneSink_NoOp(t *testing.T) {
	sink := NoneSink{}
	if err := sink.Emit(context.Background(), types.RunResult{}); err != nil {
		t.Errorf("NoneSink.Emit should not error: %v", err)
	}
}

func TestNewResultSink_Defaults(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *types.ResultSinkConfig
		wantTyp string
	}{
		{"nil", nil, "NoneSink"},
		{"empty type", &types.ResultSinkConfig{Type: ""}, "NoneSink"},
		{"none", &types.ResultSinkConfig{Type: "none"}, "NoneSink"},
		{"stdout-json", &types.ResultSinkConfig{Type: "stdout-json"}, "*StdoutJSONSink"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink, err := NewResultSink(tc.cfg)
			if err != nil {
				t.Fatalf("NewResultSink: %v", err)
			}
			gotTyp := ""
			switch sink.(type) {
			case NoneSink:
				gotTyp = "NoneSink"
			case *StdoutJSONSink:
				gotTyp = "*StdoutJSONSink"
			default:
				t.Fatalf("unexpected sink type %T", sink)
			}
			if gotTyp != tc.wantTyp {
				t.Errorf("type: got %q, want %q", gotTyp, tc.wantTyp)
			}
		})
	}
}

func TestNewResultSink_ReservedTypesRejected(t *testing.T) {
	for _, typ := range []string{"gcp-pubsub", "gcs"} {
		t.Run(typ, func(t *testing.T) {
			_, err := NewResultSink(&types.ResultSinkConfig{Type: typ})
			if err == nil {
				t.Fatalf("want error for type=%q, got nil", typ)
			}
			if !strings.Contains(err.Error(), "not yet implemented") {
				t.Errorf("error should mention not-implemented, got %v", err)
			}
		})
	}
}

func TestNewResultSink_UnknownTypeRejected(t *testing.T) {
	_, err := NewResultSink(&types.ResultSinkConfig{Type: "kinesis"})
	if err == nil {
		t.Fatal("want error for unknown type")
	}
	if !strings.Contains(err.Error(), "unsupported resultSink.type") {
		t.Errorf("error message: got %v", err)
	}
}
