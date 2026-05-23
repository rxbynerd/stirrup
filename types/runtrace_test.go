package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTurnTrace_MarshalRoundTrip pins the wire shape of TurnTrace.Mode
// and TurnTrace.BatchID added in #138. The omitempty contract is
// load-bearing: streaming traces continue to omit both fields, and
// batch traces carry the provider-assigned batch identifier so an
// operator can cross-reference a TurnTrace with the provider's console.
func TestTurnTrace_MarshalRoundTrip(t *testing.T) {
	tt := TurnTrace{
		Turn:       3,
		Tokens:     TokenUsage{Input: 100, Output: 50},
		ToolCalls:  2,
		StopReason: "end_turn",
		DurationMs: 1234,
		Mode:       "batch",
		BatchID:    "msgbatch_xyz",
	}

	data, err := json.Marshal(tt)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)
	for _, want := range []string{`"mode":"batch"`, `"batchId":"msgbatch_xyz"`} {
		if !strings.Contains(s, want) {
			t.Errorf("encoded JSON %q missing %q", s, want)
		}
	}

	var round TurnTrace
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if round.Mode != "batch" {
		t.Errorf("round-trip Mode = %q, want %q", round.Mode, "batch")
	}
	if round.BatchID != "msgbatch_xyz" {
		t.Errorf("round-trip BatchID = %q, want %q", round.BatchID, "msgbatch_xyz")
	}
}

// TestTurnTrace_StreamingOmitsBatchFields confirms that a streaming
// TurnTrace serialises without the mode/batchId keys, so legacy
// pipelines that parse the JSON in a non-strict mode (or schema-match
// on key presence) keep the same byte sequence they had pre-#138.
func TestTurnTrace_StreamingOmitsBatchFields(t *testing.T) {
	tt := TurnTrace{
		Turn:       1,
		Tokens:     TokenUsage{Input: 10, Output: 5},
		StopReason: "end_turn",
		DurationMs: 200,
		Mode:       "streaming",
	}
	data, err := json.Marshal(tt)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)
	if strings.Contains(s, `"batchId"`) {
		t.Errorf("streaming trace must omit batchId: %s", s)
	}
	if !strings.Contains(s, `"mode":"streaming"`) {
		t.Errorf("streaming trace must include mode=streaming: %s", s)
	}
}

// TestTurnTrace_EmptyModeOmittedFromJSON pins the omitempty contract
// on TurnTrace.Mode directly. The companion
// TestTurnTrace_LegacyJSON_DeserialisesToEmptyMode test exercises
// the reader side; this test exercises the writer side. Without
// omitempty, a freshly-zeroed TurnTrace would carry "mode":"" on
// the wire and any consumer schema-matching on key presence would
// treat the new field as load-bearing for every trace, regressing
// the pre-#138 wire shape.
func TestTurnTrace_EmptyModeOmittedFromJSON(t *testing.T) {
	tt := TurnTrace{} // Mode is the empty string by default.
	data, err := json.Marshal(tt)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), `"mode"`) {
		t.Errorf("empty Mode must be omitted; JSON = %s", data)
	}
}

// TestTurnTrace_IsBatch pins the empty-mode-means-streaming contract
// as a method rather than a per-call-site string compare. Empty must
// resolve to false (legacy / pre-resolution failure path), "streaming"
// to false, and "batch" to true. A future third mode that consumers
// must distinguish from batch lands here so the helper can grow a
// switch rather than every consumer learning a new literal.
func TestTurnTrace_IsBatch(t *testing.T) {
	cases := []struct {
		name string
		mode string
		want bool
	}{
		{"empty-mode", "", false},
		{"streaming", TurnModeStreaming, false},
		{"batch", TurnModeBatch, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tt := TurnTrace{Mode: tc.mode}
			if got := tt.IsBatch(); got != tc.want {
				t.Errorf("IsBatch() = %v, want %v (mode=%q)", got, tc.want, tc.mode)
			}
		})
	}
}

// TestTurnTrace_LegacyJSON_DeserialisesToEmptyMode pins the backward-
// compatibility contract for traces written before #138: a JSON
// document without the "mode" field must deserialise to Mode: "",
// not error or coerce to a non-empty default. Downstream consumers
// (lakehouse bucketing, mine-failures filter) treat empty mode as
// streaming so legacy traces continue to flow through unchanged.
func TestTurnTrace_LegacyJSON_DeserialisesToEmptyMode(t *testing.T) {
	legacy := `{"turn":2,"tokens":{"input":0,"output":0},"toolCalls":0,"stopReason":"end_turn","durationMs":500}`
	var tt TurnTrace
	if err := json.Unmarshal([]byte(legacy), &tt); err != nil {
		t.Fatalf("Unmarshal legacy: %v", err)
	}
	if tt.Mode != "" {
		t.Errorf("legacy trace Mode = %q, want empty", tt.Mode)
	}
	if tt.BatchID != "" {
		t.Errorf("legacy trace BatchID = %q, want empty", tt.BatchID)
	}
	if tt.Turn != 2 || tt.DurationMs != 500 || tt.StopReason != "end_turn" {
		t.Errorf("legacy trace decode lost data: %+v", tt)
	}
}

func TestRunTracePermissionDenialsJSONCompatibility(t *testing.T) {
	var oldTrace RunTrace
	if err := json.Unmarshal([]byte(`{"id":"run-1","turns":2}`), &oldTrace); err != nil {
		t.Fatalf("unmarshal old RunTrace shape: %v", err)
	}
	if oldTrace.PermissionDenials != 0 {
		t.Errorf("missing permissionDenials should decode as zero, got %d", oldTrace.PermissionDenials)
	}

	zeroBytes, err := json.Marshal(RunTrace{ID: "run-1"})
	if err != nil {
		t.Fatalf("marshal zero-denial RunTrace: %v", err)
	}
	if strings.Contains(string(zeroBytes), "permissionDenials") {
		t.Errorf("zero permissionDenials should be omitted for compatibility, got %s", zeroBytes)
	}

	nonZeroBytes, err := json.Marshal(RunTrace{ID: "run-1", PermissionDenials: 2})
	if err != nil {
		t.Fatalf("marshal non-zero-denial RunTrace: %v", err)
	}
	if !strings.Contains(string(nonZeroBytes), `"permissionDenials":2`) {
		t.Errorf("non-zero permissionDenials should be emitted, got %s", nonZeroBytes)
	}
}
