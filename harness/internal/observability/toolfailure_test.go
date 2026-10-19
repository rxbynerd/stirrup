package observability

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestToolFailureCategory_KnownValuesAreValid pins the closed-enum
// invariant: every exported ToolFailure* constant must be present in
// allToolFailureCategories. A new constant added without registering
// it would silently IsValid()=false at runtime and metric emission
// sites that gate on IsValid would drop the observation.
//
// The list mirrors the categories declared in toolfailure.go. If a
// new constant is added, this list and the map both need updates;
// missing either ends with a failing test, which is the goal.
func TestToolFailureCategory_KnownValuesAreValid(t *testing.T) {
	known := []ToolFailureCategory{
		ToolFailureUnknownTool,
		ToolFailureInvalidArguments,
		ToolFailureSchemaValidation,
		ToolFailureSecurityGuard,
		ToolFailurePermissionDenied,
		ToolFailurePermissionError,
		ToolFailureGuardrailDenied,
		ToolFailureHandlerError,
		ToolFailureHandlerMissing,
		ToolFailureAsyncPreflight,
		ToolFailureAsyncTransport,
		ToolFailureAsyncTimeout,
		ToolFailureAsyncCancelled,
		ToolFailureAsyncUpstreamError,
		ToolFailureAsyncPanic,
		ToolFailureAsyncInternal,
		ToolFailureProviderRequest,
		ToolFailureProviderStream,
		ToolFailureStallRepeated,
		ToolFailureStallConsecutiveFailures,
		ToolFailureNoToolWhenRequired,
	}
	for _, c := range known {
		t.Run(string(c), func(t *testing.T) {
			require.True(t, c.IsValid(), "category is not registered in allToolFailureCategories")
			require.NotEmpty(t, c.String(), "category has an empty wire string")
		})
	}

	// A typo in the registry (an orphaned entry, or a known constant
	// left out of it) changes the count, so the sizes must match.
	require.Len(t, allToolFailureCategories, len(known), "registry entries diverge from known constants")
}

// TestToolFailureCategory_BogusValuesRejected pins the other half of
// the bound: any value not in the registry must IsValid()=false.
// Metric emission sites should reject these so free-form strings
// cannot widen the cardinality of the tool_failures series.
func TestToolFailureCategory_BogusValuesRejected(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "upper-case constant name", value: "UNKNOWN_TOOL"},
		{name: "spaced words", value: "permission denied"},
		{name: "camel case", value: "PermissionDenied"},
		{name: "truncated wire value", value: "handler_err"},
		{name: "trailing space", value: "unknown_tool "},
		{name: "nul byte", value: "\x00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.False(t, ToolFailureCategory(tt.value).IsValid(), "value %q must not validate", tt.value)
		})
	}
}

// TestToolFailureCategory_StringReturnsWireValue confirms String() returns
// the underlying wire form unchanged so reflective consumers (tests,
// dashboards) read the same value they would see on the OTLP wire.
func TestToolFailureCategory_StringReturnsWireValue(t *testing.T) {
	require.Equal(t, "handler_error", ToolFailureHandlerError.String())
}
