package judge

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParseDiffReviewVerdict_HappyPath pins the model-side JSON
// contract: a well-formed {"passed": true|false, "feedback": "..."}
// round-trips into the expected JudgeVerdict shape.
func TestParseDiffReviewVerdict_HappyPath(t *testing.T) {
	cases := []struct {
		name       string
		response   string
		wantPassed bool
		wantReason string
	}{
		{
			name:       "passed",
			response:   `{"passed": true, "feedback": "looks good"}`,
			wantPassed: true,
			wantReason: "looks good",
		},
		{
			name:       "failed",
			response:   `{"passed": false, "feedback": "missing test"}`,
			wantPassed: false,
			wantReason: "missing test",
		},
		{
			name:       "whitespace-trimmed",
			response:   "  \n {\"passed\": true, \"feedback\": \"trimmed\"} \n ",
			wantPassed: true,
			wantReason: "trimmed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseDiffReviewVerdict(tc.response)
			if got.Passed != tc.wantPassed {
				t.Errorf("Passed = %v, want %v", got.Passed, tc.wantPassed)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
		})
	}
}

// TestParseDiffReviewVerdict_Malformed pins the malformed-response
// posture: a parsed-as-FAIL verdict with the raw response in the
// reason, never an error. This matches the verifier-side parser so
// the eval framework's caller sees a consistent surface across
// at-run-time and post-run judging.
func TestParseDiffReviewVerdict_Malformed(t *testing.T) {
	cases := []string{
		`not json at all`,
		`{"passed": "yes"}`,   // wrong type for passed
		`{"feedback": "..."}`, // missing passed
		``,
	}
	for _, response := range cases {
		got := parseDiffReviewVerdict(response)
		if response == `{"feedback": "..."}` {
			// Missing "passed" decodes as zero-value (false), so the
			// outer parser will return Passed=false with no error;
			// that's a valid "model misbehaved → fail" verdict.
			if got.Passed {
				t.Errorf("response %q: got Passed=true, want false", response)
			}
			continue
		}
		if got.Passed {
			t.Errorf("response %q: got Passed=true, want false", response)
		}
	}
}

// TestAnthropicRequestShape pins the wire shape the diff-review
// judge POSTs. A regression in field names would surface here
// before the live API rejects the request with a confusing 400.
func TestAnthropicRequestShape(t *testing.T) {
	req := anthropicRequest{
		Model:       "claude-haiku-4-5-20251001",
		System:      "judge",
		MaxTokens:   1024,
		Temperature: 0.0,
		Messages: []anthropicMessage{
			{Role: "user", Content: "criteria + diff"},
		},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(data)
	for _, key := range []string{`"model":`, `"system":`, `"messages":`, `"max_tokens":`, `"temperature":`, `"role":`, `"content":`} {
		if !strings.Contains(body, key) {
			t.Errorf("missing key %q in request body: %s", key, body)
		}
	}
	// Anthropic uses snake_case keys; a camelCase regression would
	// emit "maxTokens" or "stopReason" and the API would reject
	// the call. Anchor on the negative.
	for _, bad := range []string{`"maxTokens"`, `"stopReason"`} {
		if strings.Contains(body, bad) {
			t.Errorf("unexpected camelCase key %q in request body", bad)
		}
	}
}
