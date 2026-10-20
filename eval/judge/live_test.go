package judge

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// Live judge tests call real providers and run only with
// STIRRUP_EVAL_LIVE_JUDGE=1. Each sends one small diff-review in
// json_schema mode and checks that the provider accepts the per-call
// nonce schema and that the model copies the nonce back.

func TestLiveDiffReview_Anthropic(t *testing.T) {
	runLiveDiffReview(t, "ANTHROPIC_API_KEY", types.JudgeLLMConfig{
		Provider:  types.JudgeProviderAnthropic,
		Model:     liveModel("STIRRUP_EVAL_LIVE_ANTHROPIC_MODEL", DefaultLLMModel),
		APIKeyRef: "secret://ANTHROPIC_API_KEY",
	})
}

func TestLiveDiffReview_OpenRouter(t *testing.T) {
	runLiveDiffReview(t, "OPENROUTER_API_KEY", types.JudgeLLMConfig{
		Provider:  types.JudgeProviderOpenAICompatible,
		Model:     liveModel("STIRRUP_EVAL_LIVE_OPENROUTER_MODEL", "openai/gpt-6-luna"),
		BaseURL:   "https://openrouter.ai/api/v1",
		APIKeyRef: "secret://OPENROUTER_API_KEY",
	})
}

func liveModel(envVar, fallback string) string {
	if m := os.Getenv(envVar); m != "" {
		return m
	}
	return fallback
}

func runLiveDiffReview(t *testing.T, keyEnv string, llm types.JudgeLLMConfig) {
	check := assert.New(t)
	must := require.New(t)
	if os.Getenv("STIRRUP_EVAL_LIVE_JUDGE") != "1" {
		t.Skip("set STIRRUP_EVAL_LIVE_JUDGE=1 to call a live judge provider")
	}
	key := os.Getenv(keyEnv)
	if key == "" {
		t.Skipf("%s is not set", keyEnv)
	}
	llm.TimeoutSeconds = 60
	j := diffReviewJudge()
	j.LLM = &llm

	ctx := context.Background()
	task := types.EvalTask{ID: "live", Judge: j}
	must.NoError(PreflightSuite(ctx, []types.EvalTask{task}, Options{}), "preflight")

	jctx := changedJudgeContext(t, Options{})
	verdict, err := Evaluate(ctx, j, jctx)
	must.NoError(err, "Evaluate")
	rec := verdict.Record
	must.NotNil(rec, "verdict has no record")
	t.Logf("verdict=%s parse=%s stop=%s served=%s tokens=%d/%d latency=%dms reason=%q",
		verdict.Status, rec.ParseStatus, rec.StopReason, rec.ServedModel,
		rec.InputTokens, rec.OutputTokens, rec.LatencyMs, verdict.Reason)

	check.Contains([]string{types.JudgeStatusPass, types.JudgeStatusFail}, verdict.Status,
		"verdict status should be pass or fail")
	check.Contains([]string{types.JudgeParseOK, types.JudgeParseLastMatch}, rec.ParseStatus,
		"the nonce-bound verdict should parse")
	check.NotEmpty(rec.ServedModel, "record lacks provider metadata: %+v", rec)
	check.NotZero(rec.InputTokens, "record lacks provider metadata: %+v", rec)
	// A plain bool assertion keeps the key out of failure output, which NotContains would print.
	check.False(strings.Contains(verdict.Reason, key), "verdict reason contains the API key") //nolint:testifylint // NotContains prints the secret on failure
}
