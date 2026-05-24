package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestValidateToolChoiceName exercises the character-set and length bound
// the adapters enforce before emitting a named-tool choice. The grammar
// is `^[a-zA-Z0-9_-]{1,64}$` — the intersection of the three providers'
// documented function-name grammars.
func TestValidateToolChoiceName(t *testing.T) {
	valid := []string{
		"read_file",
		"a",
		"Tool-Name_1",
		"ABCDEFGHIJ0123456789",
		strings.Repeat("a", 64), // boundary: exactly 64
	}
	for _, name := range valid {
		if err := ValidateToolChoiceName(name); err != nil {
			t.Errorf("ValidateToolChoiceName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"",                      // empty
		"bad name!",             // space + punctuation
		"with.dot",              // dot is not in the intersection grammar
		"slash/name",            // path separator
		"emoji😀",                // non-ASCII
		strings.Repeat("a", 65), // boundary: one over the limit
	}
	for _, name := range invalid {
		if err := ValidateToolChoiceName(name); err == nil {
			t.Errorf("ValidateToolChoiceName(%q) = nil, want error", name)
		}
	}
}

// TestStreamParamsToolChoiceOmitempty pins the zero-value/omitempty
// coupling the R1 compile-time guard protects at runtime: a StreamParams
// with ToolChoice unset (ToolChoiceAuto) must marshal without a
// toolChoice key, while a non-auto value emits it. This is the JSON
// round-trip half of the invariant the array-index guard enforces at
// compile time.
func TestStreamParamsToolChoiceOmitempty(t *testing.T) {
	auto := StreamParams{Model: "m"}
	b, err := json.Marshal(auto)
	if err != nil {
		t.Fatalf("marshal auto: %v", err)
	}
	if strings.Contains(string(b), "toolChoice") {
		t.Errorf("ToolChoiceAuto must be omitted from JSON, got: %s", b)
	}

	required := StreamParams{Model: "m", ToolChoice: ToolChoiceRequired}
	b, err = json.Marshal(required)
	if err != nil {
		t.Fatalf("marshal required: %v", err)
	}
	if !strings.Contains(string(b), "toolChoice") {
		t.Errorf("non-auto ToolChoice must appear in JSON, got: %s", b)
	}
}
