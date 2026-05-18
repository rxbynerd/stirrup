package resultsink

import (
	"bytes"
	"context"
	"encoding/json"
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
