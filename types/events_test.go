package types

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidateToolChoiceName_EnforcesGrammarAndLength covers the character-set
// and length bound the adapters enforce before emitting a named-tool choice.
// The grammar is `^[a-zA-Z0-9_-]{1,64}$`, the intersection of the three
// providers' documented function-name grammars.
func TestValidateToolChoiceName_EnforcesGrammarAndLength(t *testing.T) {
	valid := []struct {
		name  string
		input string
	}{
		{"accepts snake_case name", "read_file"},
		{"accepts single character", "a"},
		{"accepts hyphen, underscore and digits", "Tool-Name_1"},
		{"accepts mixed-case alphanumerics", "ABCDEFGHIJ0123456789"},
		{"accepts exactly 64 characters", strings.Repeat("a", 64)},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.NoError(ValidateToolChoiceName(tc.input))
		})
	}

	invalid := []struct {
		name  string
		input string
	}{
		{"rejects empty name", ""},
		{"rejects space and punctuation", "bad name!"},
		{"rejects dot", "with.dot"},
		{"rejects path separator", "slash/name"},
		{"rejects non-ASCII", "emoji😀"},
		{"rejects 65 characters", strings.Repeat("a", 65)},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Error(ValidateToolChoiceName(tc.input))
		})
	}
}

// A ToolChoice left at its zero value (ToolChoiceAuto) must be omitted from
// the JSON, and any other value must marshal to its string form.
func TestStreamParams_MarshalOmitsAutoToolChoice(t *testing.T) {
	t.Run("auto is omitted", func(t *testing.T) {
		must := require.New(t)
		b, err := json.Marshal(StreamParams{Model: "m"})
		must.NoError(err)
		must.NotContains(string(b), "toolChoice")
	})

	t.Run("non-auto marshals to its string form", func(t *testing.T) {
		must := require.New(t)
		b, err := json.Marshal(StreamParams{Model: "m", ToolChoice: ToolChoiceRequired})
		must.NoError(err)
		must.Contains(string(b), `"toolChoice":"required"`)
	})
}

// TestToolChoiceMode_JSONRoundTrip pins the MarshalJSON/UnmarshalJSON
// inverse: every defined member marshals to its lowercase string form
// and unmarshals back to the same value.
func TestToolChoiceMode_JSONRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		mode ToolChoiceMode
		wire string
	}{
		{"auto", ToolChoiceAuto, `"auto"`},
		{"required", ToolChoiceRequired, `"required"`},
		{"none", ToolChoiceNone, `"none"`},
		{"tool", ToolChoiceTool, `"tool"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			b, err := json.Marshal(tc.mode)
			must.NoError(err)
			must.Equal(tc.wire, string(b))

			var got ToolChoiceMode
			must.NoError(json.Unmarshal(b, &got))
			must.Equal(tc.mode, got)
		})
	}
}

// TestToolChoiceMode_StringHandlesOutOfRange documents that String stays
// usable on out-of-range values: unlike MarshalJSON (which rejects them),
// String never errors so it remains safe in fmt verbs and log lines. The
// default branch renders "unknown(N)".
func TestToolChoiceMode_StringHandlesOutOfRange(t *testing.T) {
	cases := []struct {
		name string
		mode ToolChoiceMode
		want string
	}{
		{"auto", ToolChoiceAuto, "auto"},
		{"required", ToolChoiceRequired, "required"},
		{"none", ToolChoiceNone, "none"},
		{"tool", ToolChoiceTool, "tool"},
		{"renders above-range value as unknown", ToolChoiceMode(99), "unknown(99)"},
		{"renders negative value as unknown", ToolChoiceMode(-1), "unknown(-1)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, tc.mode.String())
		})
	}
}

// TestToolChoiceMode_UnmarshalRejectsInvalidForms guards the deserialisation
// hardening: an unknown string and an out-of-range integer-as-string
// both error rather than being silently coerced to ToolChoiceAuto.
func TestToolChoiceMode_UnmarshalRejectsInvalidForms(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"rejects provider-native token", `"any"`},
		{"rejects uppercase name", `"AUTO"`},
		{"rejects String fallback form", `"unknown(9)"`},
		{"rejects out-of-range integer", `4`},
		{"rejects out-of-range string", `"4"`},
		{"rejects empty string", `""`},
		{"rejects null", `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var m ToolChoiceMode
			must.Error(json.Unmarshal([]byte(tc.input), &m))
		})
	}
}

// TestToolChoiceMode_IsValidDistinguishesRange checks the predicate is true
// for defined members and false for out-of-range values.
func TestToolChoiceMode_IsValidDistinguishesRange(t *testing.T) {
	cases := []struct {
		name string
		mode ToolChoiceMode
		want bool
	}{
		{"auto is valid", ToolChoiceAuto, true},
		{"required is valid", ToolChoiceRequired, true},
		{"none is valid", ToolChoiceNone, true},
		{"tool is valid", ToolChoiceTool, true},
		{"negative value is invalid", -1, false},
		{"just above range is invalid", 4, false},
		{"far above range is invalid", 100, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, tc.mode.IsValid())
		})
	}
}

// TestToolChoiceMode_MarshalRejectsOutOfRange guards the serialisation
// hardening: an out-of-range value must surface a marshal error rather than
// emit the "unknown(N)" String() fallback onto a trace or recording.
func TestToolChoiceMode_MarshalRejectsOutOfRange(t *testing.T) {
	cases := []struct {
		name string
		mode ToolChoiceMode
	}{
		{"rejects above-range value", 99},
		{"rejects negative value", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, err := json.Marshal(tc.mode)
			must.Error(err)
		})
	}
}

// TestToolChoiceMode_UnmarshalErrorTruncatesLongInput pins the bounded,
// escaped rendering of hostile input: an unknown value far longer than the
// 64-byte cap surfaces a quoted, ellipsis-terminated excerpt rather than
// the full raw payload, so the error stays log-safe and bounded.
func TestToolChoiceMode_UnmarshalErrorTruncatesLongInput(t *testing.T) {
	must := require.New(t)
	long := `"` + strings.Repeat("x", 200) + `"`
	var m ToolChoiceMode
	err := json.Unmarshal([]byte(long), &m)
	must.Error(err)

	msg := err.Error()
	must.Contains(msg, "…", "error must mark truncation with an ellipsis")
	must.LessOrEqual(len(msg), 120, "error message must stay bounded: %s", msg)
}

func TestHarnessCapabilities_AdvertisesFeaturesAndIsolatesSlice(t *testing.T) {
	must := require.New(t)
	want := []string{"live_transcript", "reasoning_summary", "follow_up_history", "turn_abandoned", "lazy_tool_loading"}
	got := HarnessCapabilities()
	must.Equal(want, got)

	got[0] = "mutated"
	again := HarnessCapabilities()
	must.Equal(want[0], again[0], "mutating the returned slice must not change later calls")
}
