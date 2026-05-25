package types

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestToolCallSummary_StructuralParityWithTrace guards the four
// types.ToolCallSummary(tc) conversions the harness performs on a
// ToolCallTrace. The cast is a struct conversion: it only compiles while
// the two types have identical underlying field shapes, but Go permits
// the conversion to silently ignore differing struct *tags* and, more
// dangerously, a future field added to ToolCallTrace without a matching
// addition to ToolCallSummary would either break the conversion (caught
// at build) or — if added to ToolCallSummary first — leave the trace
// struct missing data with no signal. This reflection check fails loudly
// the moment the field set (name + type + json tag) diverges, so the
// next person editing one struct is forced to mirror the other (#314).
func TestToolCallSummary_StructuralParityWithTrace(t *testing.T) {
	type fieldShape struct {
		Name string
		Type string
		Tag  string
	}
	fields := func(rt reflect.Type) []fieldShape {
		out := make([]fieldShape, 0, rt.NumField())
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			out = append(out, fieldShape{
				Name: f.Name,
				Type: f.Type.String(),
				Tag:  f.Tag.Get("json"),
			})
		}
		return out
	}

	summary := fields(reflect.TypeOf(ToolCallSummary{}))
	trace := fields(reflect.TypeOf(ToolCallTrace{}))

	if !reflect.DeepEqual(summary, trace) {
		t.Errorf("ToolCallSummary and ToolCallTrace field sets diverged; the "+
			"types.ToolCallSummary(tc) cast would silently drop or misalign data.\n"+
			"ToolCallSummary: %+v\nToolCallTrace:   %+v\n"+
			"Add the new field to BOTH structs (same name, type, json tag, and order).",
			summary, trace)
	}
}

// TestToolCallTrace_ErrorCategoryRoundTrip decodes a JSONL trace record
// and asserts the ErrorCategory field is either empty or a member of the
// bounded observability.ToolFailureCategory enum's wire-string set.
// ErrorCategory is a plain string in types because types cannot import
// harness/internal/observability (the layering is intentional — see
// docs/architecture.md), so there is no compile-time guard that a write
// site used a valid member. The valid set is hard-coded here to preserve
// that layering boundary: if the enum gains a member, this list must be
// updated in lockstep, which is the explicit reminder the test exists to
// provide.
func TestToolCallTrace_ErrorCategoryRoundTrip(t *testing.T) {
	// Mirror of observability.ToolFailureCategory wire strings. NOT
	// imported — types must not depend on harness/internal/observability.
	validCategories := map[string]struct{}{
		"unknown_tool":                {},
		"schema_validation_failed":    {},
		"security_guard_denied":       {},
		"permission_denied":           {},
		"permission_error":            {},
		"guardrail_denied":            {},
		"handler_error":               {},
		"handler_missing":             {},
		"async_preflight_error":       {},
		"async_transport_unavailable": {},
		"async_timeout":               {},
		"async_cancelled":             {},
		"async_upstream_error":        {},
		"async_panic":                 {},
		"async_internal_error":        {},
		"provider_request_failed":     {},
		"provider_stream_failed":      {},
		"stall_repeated_calls":        {},
		"stall_consecutive_failures":  {},
		"no_tool_when_required":       {},
	}

	// A JSONL fixture: one successful call (no category) and one failed
	// call carrying a category from the bounded set.
	const fixture = `{"name":"read_file","durationMs":12,"success":true}
{"name":"missing_tool","durationMs":3,"success":false,"errorReason":"no such tool","errorCategory":"unknown_tool"}`

	for i, line := range strings.Split(strings.TrimSpace(fixture), "\n") {
		var tc ToolCallTrace
		if err := json.Unmarshal([]byte(line), &tc); err != nil {
			t.Fatalf("line %d: Unmarshal: %v", i, err)
		}
		if tc.ErrorCategory == "" {
			continue
		}
		if _, ok := validCategories[tc.ErrorCategory]; !ok {
			t.Errorf("line %d: ErrorCategory %q is not a member of the bounded enum wire-string set", i, tc.ErrorCategory)
		}
	}

	// A category outside the bounded set must be flagged by the same
	// guard, proving the assertion is load-bearing rather than vacuous.
	var bogus ToolCallTrace
	if err := json.Unmarshal([]byte(`{"name":"x","success":false,"errorCategory":"made_up_category"}`), &bogus); err != nil {
		t.Fatalf("Unmarshal bogus: %v", err)
	}
	if _, ok := validCategories[bogus.ErrorCategory]; ok {
		t.Fatalf("test premise broken: %q must not be in the valid set", bogus.ErrorCategory)
	}
}

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
