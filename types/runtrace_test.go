package types

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

// TestToolCallSummary_StructuralParityWithTrace guards the
// ToolCallSummary(tc) struct-conversion cast: Go permits the
// conversion to silently ignore differing struct tags or a field added
// to one type but not the other. This reflection check fails loudly
// the moment the field set (name + type + json tag) diverges.
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
// and asserts ErrorCategory is either empty or a member of the bounded
// observability.ToolFailureCategory wire-string set. ErrorCategory is a
// plain string in types (types cannot import
// harness/internal/observability; see docs/architecture.md), so there
// is no compile-time guard on valid members — the set is hard-coded
// here and must be updated in lockstep if the enum gains a member.
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
// and TurnTrace.BatchID: streaming traces omit both fields, and batch
// traces carry the provider-assigned batch identifier.
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
// TurnTrace serialises without the mode/batchId keys.
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
// on TurnTrace.Mode: a zero-valued TurnTrace must not carry "mode":""
// on the wire.
func TestTurnTrace_EmptyModeOmittedFromJSON(t *testing.T) {
	tt := TurnTrace{}
	data, err := json.Marshal(tt)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), `"mode"`) {
		t.Errorf("empty Mode must be omitted; JSON = %s", data)
	}
}

// TestTurnTrace_IsBatch pins that empty and "streaming" resolve to
// false, and "batch" resolves to true.
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

// TestTurnTrace_LegacyJSON_DeserialisesToEmptyMode pins that a JSON
// document without the "mode" field deserialises to Mode: "" rather
// than erroring or coercing to a non-empty default.
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

// TestToolCallTrace_IDOmittedWhenEmpty pins the omitempty contract on
// the tool_use ID: providers without call identifiers keep the pre-ID
// wire shape, and a populated ID round-trips. The OTel content-capture
// path keys tool-span pairing on this field.
func TestToolCallTrace_IDOmittedWhenEmpty(t *testing.T) {
	bare, err := json.Marshal(ToolCallTrace{Name: "read_file", DurationMs: 1, Success: true})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(bare), `"id"`) {
		t.Errorf("empty ID must be omitted; JSON = %s", bare)
	}

	populated, err := json.Marshal(ToolCallSummary{ID: "tu-1", Name: "read_file", DurationMs: 1, Success: true})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(populated), `"id":"tu-1"`) {
		t.Errorf("populated ID must be emitted; JSON = %s", populated)
	}

	var round ToolCallTrace
	if err := json.Unmarshal(populated, &round); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if round.ID != "tu-1" {
		t.Errorf("round-trip ID = %q, want %q", round.ID, "tu-1")
	}
}

// TestTurnTrace_ModelOmittedWhenEmpty pins the omitempty contract on
// the per-turn model: legacy traces keep their wire shape, and the
// router's selection round-trips.
func TestTurnTrace_ModelOmittedWhenEmpty(t *testing.T) {
	bare, err := json.Marshal(TurnTrace{Turn: 1, StopReason: "end_turn"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(bare), `"model"`) {
		t.Errorf("empty Model must be omitted; JSON = %s", bare)
	}

	data, err := json.Marshal(TurnTrace{Turn: 1, Model: "claude-sonnet-4-6"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var round TurnTrace
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if round.Model != "claude-sonnet-4-6" {
		t.Errorf("round-trip Model = %q, want %q", round.Model, "claude-sonnet-4-6")
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

func TestTokenUsage_AddAccumulatesEveryField(t *testing.T) {
	total := TokenUsage{Input: 10, Output: 5, CacheRead: 2, CacheWrite: 3, Reasoning: 1}
	total.Add(TokenUsage{Input: 100, Output: 50, CacheRead: 20, CacheWrite: 30, Reasoning: 10})
	want := TokenUsage{Input: 110, Output: 55, CacheRead: 22, CacheWrite: 33, Reasoning: 11}
	if total != want {
		t.Errorf("Add = %+v, want %+v", total, want)
	}
}

func TestTokenUsage_AddSaturates(t *testing.T) {
	total := TokenUsage{Input: math.MaxInt - 5, Output: 1, CacheRead: math.MaxInt}
	total.Add(TokenUsage{Input: 100, Output: 2, CacheRead: 1})
	want := TokenUsage{Input: math.MaxInt, Output: 3, CacheRead: math.MaxInt}
	if total != want {
		t.Errorf("Add = %+v, want %+v", total, want)
	}
}

func TestTokenUsage_TotalSaturates(t *testing.T) {
	if got := (TokenUsage{Input: math.MaxInt, Output: 10}).Total(); got != math.MaxInt {
		t.Errorf("Total = %d, want math.MaxInt", got)
	}
	if got := (TokenUsage{Input: 1200, Output: 34, CacheRead: 1000}).Total(); got != 1234 {
		t.Errorf("Total = %d, want 1234 (cached input is already inside Input)", got)
	}
}

// TestTokenUsage_BreakdownOmittedWhenZero pins the wire shape for
// providers that report no cache or reasoning figures: only input and
// output appear.
func TestTokenUsage_BreakdownOmittedWhenZero(t *testing.T) {
	data, err := json.Marshal(TurnTrace{Tokens: TokenUsage{Input: 10, Output: 5}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, `"tokens":{"input":10,"output":5}`) {
		t.Errorf("tokens must carry only input/output when breakdown is zero: %s", s)
	}
	if strings.Contains(s, `"inputReported"`) {
		t.Errorf("an estimated turn must omit inputReported: %s", s)
	}
}

func TestTurnTrace_ReportedUsageRoundTrip(t *testing.T) {
	tt := TurnTrace{
		Turn:          1,
		Tokens:        TokenUsage{Input: 4000, Output: 300, CacheRead: 3000, CacheWrite: 900, Reasoning: 120},
		InputReported: true,
	}
	data, err := json.Marshal(tt)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)
	for _, want := range []string{
		`"tokens":{"input":4000,"output":300,"cacheRead":3000,"cacheWrite":900,"reasoning":120}`,
		`"inputReported":true`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("encoded JSON %s missing %s", s, want)
		}
	}
	var round TurnTrace
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if round.Tokens != tt.Tokens || !round.InputReported {
		t.Errorf("round-trip = %+v, want %+v", round, tt)
	}
}
