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

// TestToolChoiceModeRoundTrip pins the MarshalJSON/UnmarshalJSON
// inverse: every defined member marshals to its lowercase string form
// and unmarshals back to the same value.
func TestToolChoiceModeRoundTrip(t *testing.T) {
	cases := []struct {
		mode ToolChoiceMode
		json string
	}{
		{ToolChoiceAuto, `"auto"`},
		{ToolChoiceRequired, `"required"`},
		{ToolChoiceNone, `"none"`},
		{ToolChoiceTool, `"tool"`},
	}
	for _, tc := range cases {
		b, err := json.Marshal(tc.mode)
		if err != nil {
			t.Fatalf("marshal %v: %v", tc.mode, err)
		}
		if string(b) != tc.json {
			t.Errorf("marshal %v = %s, want %s", tc.mode, b, tc.json)
		}
		var got ToolChoiceMode
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal %s: %v", b, err)
		}
		if got != tc.mode {
			t.Errorf("round-trip %s = %v, want %v", b, got, tc.mode)
		}
	}
}

// TestToolChoiceModeUnmarshalRejects guards the deserialisation
// hardening: an unknown string and an out-of-range integer-as-string
// both error rather than being silently coerced to ToolChoiceAuto.
func TestToolChoiceModeUnmarshalRejects(t *testing.T) {
	bad := []string{
		`"any"`,        // OpenAI/Anthropic native token, not a ToolChoiceMode form
		`"AUTO"`,       // case-sensitive
		`"unknown(9)"`, // the String() fallback form must not round-trip
		`4`,            // out-of-range integer
		`"4"`,          // out-of-range as a string
		`""`,           // empty
		`null`,         // not a defined form
	}
	for _, in := range bad {
		var m ToolChoiceMode
		if err := json.Unmarshal([]byte(in), &m); err == nil {
			t.Errorf("Unmarshal(%s) = nil error, want rejection", in)
		}
	}
}

// TestToolChoiceModeIsValid checks the predicate is true for defined
// members and false for out-of-range values.
func TestToolChoiceModeIsValid(t *testing.T) {
	for _, m := range []ToolChoiceMode{ToolChoiceAuto, ToolChoiceRequired, ToolChoiceNone, ToolChoiceTool} {
		if !m.IsValid() {
			t.Errorf("IsValid(%v) = false, want true", m)
		}
	}
	for _, m := range []ToolChoiceMode{-1, 4, 100} {
		if m.IsValid() {
			t.Errorf("IsValid(%d) = true, want false", int(m))
		}
	}
}

// TestToolChoiceModeMarshalRejects guards the serialisation hardening:
// an out-of-range value must surface a marshal error rather than emit
// the "unknown(N)" String() fallback onto a trace or recording.
func TestToolChoiceModeMarshalRejects(t *testing.T) {
	for _, m := range []ToolChoiceMode{99, -1} {
		if _, err := json.Marshal(m); err == nil {
			t.Errorf("Marshal(%d) = nil error, want rejection", int(m))
		}
	}
}
