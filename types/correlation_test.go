package types

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func fullCorrelation() *CorrelationConfig {
	return &CorrelationConfig{
		SessionID: "session-42",
		TaskID:    "task-7",
		AttemptID: "attempt-3",
		UserID:    "user-pseudonym-9f2c",
		URL:       "https://cp.example.com/runs/42?tab=trace",
		Labels:    map[string]string{"agent_profile": "reviewer", "execution_mode": "batch"},
	}
}

func TestValidateRunConfig_CorrelationAccepted(t *testing.T) {
	labels := make(map[string]string, MaxCorrelationLabels)
	for i := range MaxCorrelationLabels {
		labels[fmt.Sprintf("k%d", i)] = "v"
	}
	cases := map[string]*CorrelationConfig{
		"absent":                nil,
		"empty block":           {},
		"fully populated":       fullCorrelation(),
		"199-char session ID":   {SessionID: strings.Repeat("s", MaxCorrelationSessionIDLength)},
		"session ID with space": {SessionID: "nightly sweep 1"},
		"255-byte task ID":      {TaskID: strings.Repeat("t", MaxCorrelationIDLength)},
		"unicode user ID":       {UserID: "café-文字"},
		"http URL":              {URL: "http://localhost:8080/runs/1"},
		"uppercase scheme":      {URL: "HTTPS://cp.example.com/runs/1"},
		"1500-byte URL":         {URL: "https://cp.example.com/" + strings.Repeat("a", MaxCorrelationURLLength-len("https://cp.example.com/"))},
		"sixteen labels":        {Labels: labels},
		"64-char label key":     {Labels: map[string]string{"a" + strings.Repeat("b", 63): "v"}},
	}
	for name, corr := range cases {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			c.Correlation = corr
			if err := ValidateRunConfig(c); err != nil {
				t.Fatalf("expected valid correlation, got: %v", err)
			}
		})
	}
}

func TestValidateRunConfig_CorrelationRejected(t *testing.T) {
	seventeen := make(map[string]string, MaxCorrelationLabels+1)
	for i := range MaxCorrelationLabels + 1 {
		seventeen[fmt.Sprintf("k%d", i)] = "v"
	}
	cases := []struct {
		name string
		corr CorrelationConfig
		want string
	}{
		{"200-char session ID", CorrelationConfig{SessionID: strings.Repeat("s", 200)}, "correlation.sessionId must be <= 199"},
		{"non-ASCII session ID", CorrelationConfig{SessionID: "café"}, "correlation.sessionId must be printable US-ASCII"},
		{"newline in session ID", CorrelationConfig{SessionID: "a\nb"}, "correlation.sessionId must be printable US-ASCII"},
		{"over-long task ID", CorrelationConfig{TaskID: strings.Repeat("t", 256)}, "correlation.taskId must be <= 255 bytes"},
		{"over-long attempt ID", CorrelationConfig{AttemptID: strings.Repeat("a", 256)}, "correlation.attemptId must be <= 255 bytes"},
		{"over-long user ID", CorrelationConfig{UserID: strings.Repeat("u", 256)}, "correlation.userId must be <= 255 bytes"},
		{"control char in user ID", CorrelationConfig{UserID: "u\x1b[31m"}, "correlation.userId contains non-printable"},
		{"invalid UTF-8 task ID", CorrelationConfig{TaskID: "bad\xff"}, "correlation.taskId must be valid UTF-8"},
		{"javascript URL", CorrelationConfig{URL: "javascript:alert(1)"}, "correlation.url must be an absolute http or https URL"},
		{"relative URL", CorrelationConfig{URL: "/runs/42"}, "correlation.url must be an absolute http or https URL"},
		{"ftp URL", CorrelationConfig{URL: "ftp://cp.example.com/runs/42"}, "correlation.url must be an absolute http or https URL"},
		{"URL without host", CorrelationConfig{URL: "https:///runs/42"}, "correlation.url must include a host"},
		{"URL with userinfo", CorrelationConfig{URL: "https://alice:hunter2@cp.example.com/runs/42"}, "correlation.url must not include userinfo"},
		{"over-long URL", CorrelationConfig{URL: "https://cp.example.com/" + strings.Repeat("a", MaxCorrelationURLLength)}, "correlation.url must be <= 1500 bytes"},
		{"seventeenth label", CorrelationConfig{Labels: seventeen}, "correlation.labels must have at most 16 entries"},
		{"__proto__ label key", CorrelationConfig{Labels: map[string]string{"__proto__": "v"}}, `correlation.labels key "__proto__"`},
		{"uppercase label key", CorrelationConfig{Labels: map[string]string{"Agent": "v"}}, `correlation.labels key "Agent"`},
		{"dotted label key", CorrelationConfig{Labels: map[string]string{"agent.profile": "v"}}, `correlation.labels key "agent.profile"`},
		{"65-char label key", CorrelationConfig{Labels: map[string]string{"a" + strings.Repeat("b", 64): "v"}}, "correlation.labels key"},
		{"over-long label value", CorrelationConfig{Labels: map[string]string{"k": strings.Repeat("v", 256)}}, "correlation.labels[k] must be <= 255 bytes"},
		{"control char in label value", CorrelationConfig{Labels: map[string]string{"k": "a\tb"}}, "correlation.labels[k] contains non-printable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			corr := tc.corr
			c.Correlation = &corr
			err := ValidateRunConfig(c)
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should contain %q, got: %v", tc.want, err)
			}
		})
	}
}

func TestValidateRunConfig_CorrelationURLErrorOmitsUserinfo(t *testing.T) {
	c := validConfig()
	c.Correlation = &CorrelationConfig{URL: "https://alice:hunter2@cp.example.com/runs/42"}
	err := ValidateRunConfig(c)
	if err == nil {
		t.Fatal("expected a userinfo error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error must not echo the URL's credentials: %v", err)
	}
}

func TestRedact_CorrelationPreserved(t *testing.T) {
	rc := RunConfig{
		Provider:    ProviderConfig{APIKeyRef: "secret://k"},
		Correlation: fullCorrelation(),
	}
	if got := rc.Redact().Correlation; !reflect.DeepEqual(got, fullCorrelation()) {
		t.Errorf("Correlation should survive Redact unchanged, got %+v", got)
	}
}

func TestCorrelationConfig_JSONFieldNames(t *testing.T) {
	raw, err := json.Marshal(RunConfig{Correlation: fullCorrelation()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Correlation map[string]json.RawMessage `json:"correlation"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"sessionId", "taskId", "attemptId", "userId", "url", "labels"} {
		if _, ok := decoded.Correlation[key]; !ok {
			t.Errorf("correlation JSON lacks %q: %s", key, raw)
		}
	}

	bare, err := json.Marshal(RunConfig{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(bare), `"correlation"`) {
		t.Errorf("an absent block must be omitted: %s", bare)
	}
}
