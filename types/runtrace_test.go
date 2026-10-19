package types

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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

	require.Equal(t, trace, summary,
		"ToolCallSummary and ToolCallTrace field sets diverged; the "+
			"types.ToolCallSummary(tc) cast would silently drop or misalign data. "+
			"Add the new field to BOTH structs (same name, type, json tag, and order).")
}

// TestToolCallTrace_ErrorCategoryInBoundedSet decodes JSONL trace records
// and asserts ErrorCategory is either empty or a member of the bounded
// observability.ToolFailureCategory wire-string set. ErrorCategory is a
// plain string in types (types cannot import
// harness/internal/observability; see docs/architecture.md), so there
// is no compile-time guard on valid members — the set is hard-coded
// here and must be updated in lockstep if the enum gains a member.
func TestToolCallTrace_ErrorCategoryInBoundedSet(t *testing.T) {
	// Mirror of observability.ToolFailureCategory wire strings. NOT
	// imported — types must not depend on harness/internal/observability.
	validCategories := map[string]struct{}{
		"unknown_tool":                {},
		"invalid_arguments":           {},
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
		require.NoError(t, json.Unmarshal([]byte(line), &tc), "line %d", i)
		if tc.ErrorCategory == "" {
			continue
		}
		require.Contains(t, validCategories, tc.ErrorCategory, "line %d", i)
	}

	// A category outside the bounded set must be flagged by the same
	// guard, proving the assertion is load-bearing rather than vacuous.
	var unlisted ToolCallTrace
	require.NoError(t, json.Unmarshal([]byte(`{"name":"x","success":false,"errorCategory":"made_up_category"}`), &unlisted))
	require.NotContains(t, validCategories, unlisted.ErrorCategory,
		"test premise broken: an unlisted category must not be in the valid set")
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
	require.NoError(t, err)
	s := string(data)
	require.Contains(t, s, `"mode":"batch"`)
	require.Contains(t, s, `"batchId":"msgbatch_xyz"`)

	var round TurnTrace
	require.NoError(t, json.Unmarshal(data, &round))
	require.Equal(t, "batch", round.Mode, "round-trip Mode")
	require.Equal(t, "msgbatch_xyz", round.BatchID, "round-trip BatchID")
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
	require.NoError(t, err)
	s := string(data)
	require.NotContains(t, s, `"batchId"`)
	require.Contains(t, s, `"mode":"streaming"`)
}

// TestTurnTrace_EmptyModeOmittedFromJSON pins the omitempty contract
// on TurnTrace.Mode: a zero-valued TurnTrace must not carry "mode":""
// on the wire.
func TestTurnTrace_EmptyModeOmittedFromJSON(t *testing.T) {
	data, err := json.Marshal(TurnTrace{})
	require.NoError(t, err)
	require.NotContains(t, string(data), `"mode"`)
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
			require.Equal(t, tc.want, tt.IsBatch())
		})
	}
}

// TestTurnTrace_LegacyJSON_DeserialisesToEmptyMode pins that a JSON
// document without the "mode" field deserialises to Mode: "" rather
// than erroring or coercing to a non-empty default.
func TestTurnTrace_LegacyJSON_DeserialisesToEmptyMode(t *testing.T) {
	legacy := `{"turn":2,"tokens":{"input":0,"output":0},"toolCalls":0,"stopReason":"end_turn","durationMs":500}`
	var tt TurnTrace
	require.NoError(t, json.Unmarshal([]byte(legacy), &tt))
	require.Empty(t, tt.Mode, "legacy trace Mode")
	require.Empty(t, tt.BatchID, "legacy trace BatchID")
	require.Equal(t, 2, tt.Turn, "legacy trace Turn")
	require.Equal(t, int64(500), tt.DurationMs, "legacy trace DurationMs")
	require.Equal(t, "end_turn", tt.StopReason, "legacy trace StopReason")
}

// TestToolCallTrace_IDOmittedWhenEmpty pins the omitempty contract on
// the tool_use ID: providers without call identifiers keep the pre-ID
// wire shape, and a populated ID round-trips. The OTel content-capture
// path keys tool-span pairing on this field.
func TestToolCallTrace_IDOmittedWhenEmpty(t *testing.T) {
	bare, err := json.Marshal(ToolCallTrace{Name: "read_file", DurationMs: 1, Success: true})
	require.NoError(t, err)
	require.NotContains(t, string(bare), `"id"`)

	populated, err := json.Marshal(ToolCallSummary{ID: "tu-1", Name: "read_file", DurationMs: 1, Success: true})
	require.NoError(t, err)
	require.Contains(t, string(populated), `"id":"tu-1"`)

	var round ToolCallTrace
	require.NoError(t, json.Unmarshal(populated, &round))
	require.Equal(t, "tu-1", round.ID)
}

// TestTurnTrace_ModelOmittedWhenEmpty pins the omitempty contract on
// the per-turn model: legacy traces keep their wire shape, and the
// router's selection round-trips.
func TestTurnTrace_ModelOmittedWhenEmpty(t *testing.T) {
	bare, err := json.Marshal(TurnTrace{Turn: 1, StopReason: "end_turn"})
	require.NoError(t, err)
	require.NotContains(t, string(bare), `"model"`)

	data, err := json.Marshal(TurnTrace{Turn: 1, Model: "claude-sonnet-4-6"})
	require.NoError(t, err)
	var round TurnTrace
	require.NoError(t, json.Unmarshal(data, &round))
	require.Equal(t, "claude-sonnet-4-6", round.Model)
}

func TestTurnTrace_StopDetailsOmittedWhenNil(t *testing.T) {
	bare, err := json.Marshal(TurnTrace{Turn: 1, StopReason: "end_turn"})
	require.NoError(t, err)
	require.NotContains(t, string(bare), `"stopDetails"`)

	want := StopDetails{Type: "refusal"}
	data, err := json.Marshal(TurnTrace{Turn: 1, StopReason: "refusal", StopDetails: &want})
	require.NoError(t, err)
	require.Contains(t, string(data), `"stopDetails":{"type":"refusal"}`)

	var round TurnTrace
	require.NoError(t, json.Unmarshal(data, &round))
	require.NotNil(t, round.StopDetails)
	require.Equal(t, want, *round.StopDetails)
}

// TestRunTrace_PermissionDenialsJSONCompatibility pins that a RunTrace
// written before permissionDenials existed decodes with a zero count,
// and that the field is omitted when zero and emitted when set.
func TestRunTrace_PermissionDenialsJSONCompatibility(t *testing.T) {
	var oldTrace RunTrace
	require.NoError(t, json.Unmarshal([]byte(`{"id":"run-1","turns":2}`), &oldTrace))
	require.Zero(t, oldTrace.PermissionDenials)

	zeroBytes, err := json.Marshal(RunTrace{ID: "run-1"})
	require.NoError(t, err)
	require.NotContains(t, string(zeroBytes), "permissionDenials")

	nonZeroBytes, err := json.Marshal(RunTrace{ID: "run-1", PermissionDenials: 2})
	require.NoError(t, err)
	require.Contains(t, string(nonZeroBytes), `"permissionDenials":2`)
}

func TestTokenUsage_AddAccumulatesEveryField(t *testing.T) {
	total := TokenUsage{Input: 10, Output: 5, CacheRead: 2, CacheWrite: 3, Reasoning: 1}
	total.Add(TokenUsage{Input: 100, Output: 50, CacheRead: 20, CacheWrite: 30, Reasoning: 10})
	want := TokenUsage{Input: 110, Output: 55, CacheRead: 22, CacheWrite: 33, Reasoning: 11}
	require.Equal(t, want, total)
}

func TestTokenUsage_AddSaturates(t *testing.T) {
	total := TokenUsage{Input: math.MaxInt - 5, Output: 1, CacheRead: math.MaxInt}
	total.Add(TokenUsage{Input: 100, Output: 2, CacheRead: 1})
	want := TokenUsage{Input: math.MaxInt, Output: 3, CacheRead: math.MaxInt}
	require.Equal(t, want, total)
}

func TestTokenUsage_TotalSaturates(t *testing.T) {
	saturated := TokenUsage{Input: math.MaxInt, Output: 10}
	require.Equal(t, math.MaxInt, saturated.Total())

	withCache := TokenUsage{Input: 1200, Output: 34, CacheRead: 1000}
	require.Equal(t, 1234, withCache.Total(), "cached input is already inside Input")
}

// TestTokenUsage_BreakdownOmittedWhenZero pins the wire shape for
// providers that report no cache or reasoning figures: only input and
// output appear.
func TestTokenUsage_BreakdownOmittedWhenZero(t *testing.T) {
	data, err := json.Marshal(TurnTrace{Tokens: TokenUsage{Input: 10, Output: 5}})
	require.NoError(t, err)
	s := string(data)
	require.Contains(t, s, `"tokens":{"input":10,"output":5}`)
	require.NotContains(t, s, `"inputReported"`, "an estimated turn must omit inputReported")
}

func TestTurnTrace_ReportedUsageRoundTrip(t *testing.T) {
	tt := TurnTrace{
		Turn:          1,
		Tokens:        TokenUsage{Input: 4000, Output: 300, CacheRead: 3000, CacheWrite: 900, Reasoning: 120},
		InputReported: true,
	}
	data, err := json.Marshal(tt)
	require.NoError(t, err)
	s := string(data)
	require.Contains(t, s, `"tokens":{"input":4000,"output":300,"cacheRead":3000,"cacheWrite":900,"reasoning":120}`)
	require.Contains(t, s, `"inputReported":true`)

	var round TurnTrace
	require.NoError(t, json.Unmarshal(data, &round))
	require.Equal(t, tt.Tokens, round.Tokens)
	require.True(t, round.InputReported)
}

// TestTurnTrace_RetriesWireShape pins that a turn with no retries omits
// the key and a retried turn round-trips its count.
func TestTurnTrace_RetriesWireShape(t *testing.T) {
	data, err := json.Marshal(TurnTrace{Turn: 1, StopReason: "end_turn"})
	require.NoError(t, err)
	require.NotContains(t, string(data), `"retries"`)

	data, err = json.Marshal(TurnTrace{Turn: 1, StopReason: "end_turn", Retries: 1})
	require.NoError(t, err)
	require.Contains(t, string(data), `"retries":1`)

	var round TurnTrace
	require.NoError(t, json.Unmarshal(data, &round))
	require.Equal(t, 1, round.Retries)
}
