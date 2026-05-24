package observability

import "testing"

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
	}
	for _, c := range known {
		if !c.IsValid() {
			t.Errorf("category %q not registered in allToolFailureCategories", c)
		}
		if c.String() == "" {
			t.Errorf("category %v has empty wire string", c)
		}
	}
	// Cross-check: every entry in the registry maps to one of the
	// known constants. A typo in the map (e.g. duplicated key or
	// orphaned entry) would surface here without needing per-key
	// assertions.
	if len(allToolFailureCategories) != len(known) {
		t.Errorf("registry size = %d, known constants = %d (entries diverge)",
			len(allToolFailureCategories), len(known))
	}
}

// TestToolFailureCategory_BogusValuesRejected pins the other half of
// the bound: any value not in the registry must IsValid()=false.
// Metric emission sites should reject these so free-form strings
// cannot widen the cardinality of the tool_failures series.
func TestToolFailureCategory_BogusValuesRejected(t *testing.T) {
	for _, s := range []string{
		"",
		"UNKNOWN_TOOL",
		"permission denied",
		"PermissionDenied",
		"handler_err",
		"unknown_tool ",
		"\x00",
	} {
		if ToolFailureCategory(s).IsValid() {
			t.Errorf("bogus value %q must not validate", s)
		}
	}
}

// TestToolFailureCategory_StringRoundTrip confirms String() returns
// the underlying wire form unchanged so reflective consumers (tests,
// dashboards) read the same value they would see on the OTLP wire.
func TestToolFailureCategory_StringRoundTrip(t *testing.T) {
	c := ToolFailureHandlerError
	if got := c.String(); got != "handler_error" {
		t.Errorf("String() = %q, want handler_error", got)
	}
}
