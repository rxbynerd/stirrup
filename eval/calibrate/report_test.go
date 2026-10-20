package calibrate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	must := require.New(t)
	must.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	must.NoError(os.WriteFile(path, []byte(content), 0o644))
}

func sampleJudgments() []Judgment {
	r := func() *types.JudgeRecord {
		return &types.JudgeRecord{ConfigHash: strings.Repeat("ab", 32), InputTokens: 1000, OutputTokens: 100, LatencyMs: 250}
	}
	return []Judgment{
		{Case: "ok", Label: "pass", Verdict: "pass", Record: r()},
		{Case: "ok", Sample: 1, Label: "pass", Verdict: "fail", Record: r()},
		{Case: "neg", Label: "fail", Verdict: "fail", Record: r()},
		{Case: "neg", Sample: 1, Label: "fail", Verdict: "fail", Record: r()},
		{Case: "adv", Label: "fail", Verdict: "pass", Adversarial: true, InjectionTarget: "pass", Record: r()},
		{Case: "adv", Sample: 1, Label: "fail", Verdict: "error", Adversarial: true, InjectionTarget: "pass", Reason: "model call failed: provider returned HTTP 529", Record: r()},
	}
}

func TestNewReport_RendersTextAndRoundTripsJSON(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	cfg := types.JudgeLLMConfig{Provider: "openai-compatible", Model: "judge-m", BaseURL: "https://user:pw@gw.example/v1?token=abc#frag", APIKeyRef: "secret://GW_KEY"}
	r := NewReport("golden.json", "seed", 3, cfg, 2, sampleJudgments(), &Prices{InputPerMTok: 1, OutputPerMTok: 4})

	must.Equal("https://gw.example/v1", r.Judge.BaseURL)
	must.NotEmpty(r.Judge.ConfigHash)
	must.Equal("openai-compatible", r.Judge.Provider)
	must.Equal([]string{"ok"}, r.UnstableCases)

	var text bytes.Buffer
	must.NoError(WriteText(&text, r))
	out := text.String()
	for _, want := range []string{
		"Judge calibration: seed (3 cases x 2 repeats = 6 judgments)",
		"Judge: openai-compatible judge-m at https://gw.example/v1 (config abababababab)",
		"Decided 5 of 6 judgments; 1 error",
		"TPR (pass judged pass)", "50.0%",
		"Cohen's kappa",
		"Adversarial flip rate", "100.0%",
		"Error rate", "16.7%",
		"Adversarial judgments that errored (not in the flip rate): 1",
		"Latency: mean 250 ms, p95 250 ms over 5 model calls",
		"Tokens: 6000 input, 600 output",
		"Estimated cost: $0.0084 (input $0.0060 + output $0.0024 at $1/$4 per million tokens)",
		"Unstable across repeats: ok",
		"ok #1: label pass, verdict fail",
		"adv #0: label fail, verdict pass (followed the injection)",
		"adv #1: model call failed: provider returned HTTP 529",
	} {
		check.Contains(out, want)
	}
	for _, leak := range []string{"secret://GW_KEY", "token=abc", "user:pw"} {
		check.NotContains(out, leak, "text report")
	}

	data, err := json.Marshal(r)
	must.NoError(err)
	for _, leak := range []string{"GW_KEY", "token=abc", "user:pw"} {
		check.NotContains(string(data), leak, "JSON report")
	}

	var back Report
	must.NoError(json.Unmarshal(data, &back))
	must.Equal(ReportSchemaVersion, back.SchemaVersion)
	must.Equal(r.Metrics.Confusion, back.Metrics.Confusion)
	must.Len(back.Judgments, 6)
	must.NotNil(back.Metrics.Cost)
}

func TestWriteText_AllErrorsLeavesMetricsUndefined(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	js := []Judgment{{Case: "a", Label: "pass", Verdict: "error", Reason: "boom"}}
	var text bytes.Buffer
	must.NoError(WriteText(&text, NewReport("g.json", "", 1, types.JudgeLLMConfig{Model: "m"}, 1, js, nil)))
	out := text.String()
	for _, want := range []string{"Judge calibration: g.json (1 case x 1 repeat = 1 judgment)", "Decided 0 of 1 judgment; 1 error", "Judge: anthropic m\n", "undefined", "Latency: no live model calls", "a: boom"} {
		check.Contains(out, want)
	}
	check.NotContains(out, "Estimated cost")
	check.NotContains(out, "a #0")
}
