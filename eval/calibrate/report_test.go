package calibrate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
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

func TestNewReportAndWriteText(t *testing.T) {
	cfg := types.JudgeLLMConfig{Provider: "openai-compatible", Model: "judge-m", BaseURL: "https://user:pw@gw.example/v1?token=abc#frag", APIKeyRef: "secret://GW_KEY"}
	r := NewReport("golden.json", "seed", 3, cfg, 2, sampleJudgments(), &Prices{InputPerMTok: 1, OutputPerMTok: 4})

	if r.Judge.BaseURL != "https://gw.example/v1" || r.Judge.ConfigHash == "" || r.Judge.Provider != "openai-compatible" {
		t.Errorf("judge identity = %+v", r.Judge)
	}
	if len(r.UnstableCases) != 1 || r.UnstableCases[0] != "ok" {
		t.Errorf("unstable cases = %v, want [ok]", r.UnstableCases)
	}

	var text bytes.Buffer
	if err := WriteText(&text, r); err != nil {
		t.Fatal(err)
	}
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
		if !strings.Contains(out, want) {
			t.Errorf("text report is missing %q:\n%s", want, out)
		}
	}
	for _, leak := range []string{"secret://GW_KEY", "token=abc", "user:pw"} {
		if strings.Contains(out, leak) {
			t.Errorf("text report carries %q", leak)
		}
	}

	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"GW_KEY", "token=abc", "user:pw"} {
		if strings.Contains(string(data), leak) {
			t.Errorf("JSON report carries %q", leak)
		}
	}
	var back Report
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.SchemaVersion != ReportSchemaVersion || back.Metrics.Confusion != r.Metrics.Confusion || len(back.Judgments) != 6 || back.Metrics.Cost == nil {
		t.Errorf("round-tripped report = %+v", back)
	}
}

func TestWriteText_UndefinedMetrics(t *testing.T) {
	js := []Judgment{{Case: "a", Label: "pass", Verdict: "error", Reason: "boom"}}
	var text bytes.Buffer
	if err := WriteText(&text, NewReport("g.json", "", 1, types.JudgeLLMConfig{Model: "m"}, 1, js, nil)); err != nil {
		t.Fatal(err)
	}
	out := text.String()
	for _, want := range []string{"Judge calibration: g.json (1 case x 1 repeat = 1 judgment)", "Decided 0 of 1 judgment; 1 error", "Judge: anthropic m\n", "undefined", "Latency: no live model calls", "a: boom"} {
		if !strings.Contains(out, want) {
			t.Errorf("text report is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Estimated cost") || strings.Contains(out, "a #0") {
		t.Errorf("unexpected cost or sample numbering:\n%s", out)
	}
}
