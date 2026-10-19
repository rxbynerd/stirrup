package types

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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

func TestValidateRunConfig_Correlation_AcceptsValidBlocks(t *testing.T) {
	labels := make(map[string]string, MaxCorrelationLabels)
	for i := range MaxCorrelationLabels {
		labels[fmt.Sprintf("k%d", i)] = "v"
	}
	cases := map[string]*CorrelationConfig{
		"absent":                nil,
		"empty block":           {},
		"fully populated":       fullCorrelation(),
		"199-char session id":   {SessionID: strings.Repeat("s", MaxCorrelationSessionIDLength)},
		"session id with space": {SessionID: "nightly sweep 1"},
		"255-byte task id":      {TaskID: strings.Repeat("t", MaxCorrelationIDLength)},
		"unicode user id":       {UserID: "café-文字"},
		"http url":              {URL: "http://localhost:8080/runs/1"},
		"url with query":        {URL: "https://cp.example.com/runs?id=42&tab=trace"},
		"uppercase scheme":      {URL: "HTTPS://cp.example.com/runs/1"},
		"1500-byte url":         {URL: "https://cp.example.com/" + strings.Repeat("a", MaxCorrelationURLLength-len("https://cp.example.com/"))},
		"sixteen labels":        {Labels: labels},
		"64-char label key":     {Labels: map[string]string{"a" + strings.Repeat("b", 63): "v"}},
	}
	for name, corr := range cases {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			c.Correlation = corr
			require.NoError(t, ValidateRunConfig(c))
		})
	}
}

func TestValidateRunConfig_Correlation_RejectsInvalidBlocks(t *testing.T) {
	seventeen := make(map[string]string, MaxCorrelationLabels+1)
	for i := range MaxCorrelationLabels + 1 {
		seventeen[fmt.Sprintf("k%d", i)] = "v"
	}
	cases := []struct {
		name string
		corr CorrelationConfig
		want string
	}{
		{"200-char session id", CorrelationConfig{SessionID: strings.Repeat("s", 200)}, "correlation.sessionId must be <= 199 bytes"},
		{"non-ascii session id", CorrelationConfig{SessionID: "café"}, "correlation.sessionId must be printable US-ASCII"},
		{"newline in session id", CorrelationConfig{SessionID: "a\nb"}, "correlation.sessionId must be printable US-ASCII"},
		{"over-long task id", CorrelationConfig{TaskID: strings.Repeat("t", 256)}, "correlation.taskId must be <= 255 bytes"},
		{"over-long attempt id", CorrelationConfig{AttemptID: strings.Repeat("a", 256)}, "correlation.attemptId must be <= 255 bytes"},
		{"over-long user id", CorrelationConfig{UserID: strings.Repeat("u", 256)}, "correlation.userId must be <= 255 bytes"},
		{"control char in user id", CorrelationConfig{UserID: "u\x1b[31m"}, "correlation.userId contains non-printable"},
		{"invalid utf-8 task id", CorrelationConfig{TaskID: "bad\xff"}, "correlation.taskId must be valid UTF-8"},
		{"javascript url", CorrelationConfig{URL: "javascript:alert(1)"}, "correlation.url must be an absolute http or https URL"},
		{"relative url", CorrelationConfig{URL: "/runs/42"}, "correlation.url must be an absolute http or https URL"},
		{"ftp url", CorrelationConfig{URL: "ftp://cp.example.com/runs/42"}, "correlation.url must be an absolute http or https URL"},
		{"url without host", CorrelationConfig{URL: "https:///runs/42"}, "correlation.url must include a host"},
		{"port-only url", CorrelationConfig{URL: "http://:80/"}, "correlation.url must include a host"},
		{"url with fragment", CorrelationConfig{URL: "https://cp.example.com/runs/42#access_token=abc"}, "correlation.url must not include a fragment"},
		{"url with empty fragment", CorrelationConfig{URL: "https://cp.example.com/runs/42#"}, "correlation.url must not include a fragment"},
		{"url with userinfo", CorrelationConfig{URL: "https://alice:hunter2@cp.example.com/runs/42"}, "correlation.url must not include userinfo"},
		{"over-long url", CorrelationConfig{URL: "https://cp.example.com/" + strings.Repeat("a", MaxCorrelationURLLength)}, "correlation.url must be <= 1500 bytes"},
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
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestValidateRunConfig_Correlation_URLErrorOmitsUserinfo(t *testing.T) {
	c := validConfig()
	c.Correlation = &CorrelationConfig{URL: "https://alice:hunter2@cp.example.com/runs/42"}
	err := ValidateRunConfig(c)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "hunter2", "error must not echo the URL's credentials")
}

func TestValidateRunConfig_Correlation_LabelKeyEchoIsCapped(t *testing.T) {
	c := validConfig()
	long := "Bad" + strings.Repeat("x", 500)
	c.Correlation = &CorrelationConfig{Labels: map[string]string{long: "v"}}
	err := ValidateRunConfig(c)
	require.Error(t, err)
	t.Run("does not echo more than 64 bytes of the key", func(t *testing.T) {
		require.NotContains(t, err.Error(), long[:100])
	})
	t.Run("echoes the first 64 bytes of the key and an ellipsis", func(t *testing.T) {
		require.Contains(t, err.Error(), long[:64]+`"…`)
	})
}

func TestRedact_Correlation_PreservedAsDeepCopy(t *testing.T) {
	rc := RunConfig{
		Provider:    ProviderConfig{APIKeyRef: "secret://k"},
		Correlation: fullCorrelation(),
	}
	redacted := rc.Redact()
	require.Equal(t, fullCorrelation(), redacted.Correlation, "Correlation should survive Redact unchanged")
	redacted.Correlation.URL = "mutated"
	redacted.Correlation.Labels["agent_profile"] = "mutated"
	require.Equal(t, fullCorrelation(), rc.Correlation, "mutating the redacted copy reached the live config")
}

func TestCorrelationConfig_JSON_FieldNames(t *testing.T) {
	raw, err := json.Marshal(RunConfig{Correlation: fullCorrelation()})
	require.NoError(t, err)
	var decoded struct {
		Correlation map[string]json.RawMessage `json:"correlation"`
	}
	require.NoError(t, json.Unmarshal(raw, &decoded))
	for _, key := range []string{"sessionId", "taskId", "attemptId", "userId", "url", "labels"} {
		t.Run(key, func(t *testing.T) {
			require.Contains(t, decoded.Correlation, key, "correlation JSON: %s", raw)
		})
	}

	bare, err := json.Marshal(RunConfig{})
	require.NoError(t, err)
	require.NotContains(t, string(bare), `"correlation"`, "an absent block must be omitted: %s", bare)
}
