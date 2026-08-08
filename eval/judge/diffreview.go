package judge

// The diff-review judge is documented in docs/eval.md.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
)

const (
	diffReviewDefaultModel = "claude-haiku-4-5-20251001"
	diffReviewMaxDiffBytes = 64 * 1024
	diffReviewAPIURL       = "https://api.anthropic.com/v1/messages"
	diffReviewAPIVersion   = "2023-06-01"
	diffReviewMaxTokens    = 1024
	diffReviewTimeout      = 30 * time.Second

	diffReviewSystemPrompt = `You are a code-review judge. Evaluate the supplied git diff against the natural-language criteria.

Respond with ONLY a JSON object in this exact format:
{"passed": true, "feedback": "brief explanation"}

- "passed" must be a boolean indicating whether the diff meets the criteria.
- "feedback" must be a short explanation citing the most decisive evidence from the diff.

Do not include any text outside the JSON object.`
)

// evaluateDiffReview runs `git diff` in the workspace and sends the diff plus
// criteria to the configured LLM, returning the parsed verdict.
func evaluateDiffReview(ctx context.Context, j types.EvalJudge, jctx JudgeContext) (eval.JudgeVerdict, error) {
	if j.Criteria == "" {
		return eval.JudgeVerdict{}, fmt.Errorf("diff-review judge requires a criteria string")
	}
	if jctx.WorkspaceDir == "" {
		return eval.JudgeVerdict{}, fmt.Errorf("diff-review judge requires a workspace dir")
	}

	apiKey := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	if apiKey == "" {
		return eval.JudgeVerdict{}, fmt.Errorf("diff-review judge: ANTHROPIC_API_KEY not set")
	}

	diff, err := captureDiff(ctx, jctx.WorkspaceDir)
	if err != nil {
		return eval.JudgeVerdict{}, fmt.Errorf("capturing git diff: %w", err)
	}
	if len(diff) > diffReviewMaxDiffBytes {
		// Mark the truncation in the prompt so the model does not silently
		// review an incomplete diff.
		diff = diff[:diffReviewMaxDiffBytes] + "\n\n[... diff truncated at " + fmt.Sprintf("%d", diffReviewMaxDiffBytes) + " bytes ...]\n"
	}

	model := diffReviewDefaultModel

	verdict, err := callDiffReviewModel(ctx, apiKey, model, j.Criteria, diff)
	if err != nil {
		return eval.JudgeVerdict{}, fmt.Errorf("diff-review: %w", err)
	}
	return verdict, nil
}

// captureDiff runs `git diff HEAD` inside dir and returns the resulting
// text. An empty diff is passed through so the model can decide whether "no
// change" is acceptable per the criteria.
func captureDiff(ctx context.Context, dir string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "diff", "HEAD")
	cmd.Dir = dir
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git diff HEAD: %v: %s", err, errOut.String())
	}
	return out.String(), nil
}

// anthropicRequest mirrors the subset of the /v1/messages request schema
// needed here; kept local so eval stays independent of harness/internal/.
type anthropicRequest struct {
	Model       string             `json:"model"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature float64            `json:"temperature"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// callDiffReviewModel posts the diff and criteria to
// api.anthropic.com/v1/messages and returns the parsed verdict.
func callDiffReviewModel(ctx context.Context, apiKey, model, criteria, diff string) (eval.JudgeVerdict, error) {
	body := anthropicRequest{
		Model:       model,
		System:      diffReviewSystemPrompt,
		MaxTokens:   diffReviewMaxTokens,
		Temperature: 0.0,
		Messages: []anthropicMessage{
			{
				Role:    "user",
				Content: "## Criteria\n\n" + criteria + "\n\n## Diff\n\n```diff\n" + diff + "\n```\n",
			},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return eval.JudgeVerdict{}, fmt.Errorf("marshal request: %w", err)
	}

	client := &http.Client{
		Timeout: diffReviewTimeout,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, diffReviewAPIURL, bytes.NewReader(payload))
	if err != nil {
		return eval.JudgeVerdict{}, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", diffReviewAPIVersion)
	req.Header.Set("x-api-key", apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return eval.JudgeVerdict{}, fmt.Errorf("api call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		var sb bytes.Buffer
		_, _ = sb.ReadFrom(resp.Body)
		return eval.JudgeVerdict{}, fmt.Errorf("api returned %d: %s", resp.StatusCode, sb.String())
	}

	var ar anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return eval.JudgeVerdict{}, fmt.Errorf("decode response: %w", err)
	}

	var text strings.Builder
	for _, blk := range ar.Content {
		if blk.Type == "text" {
			text.WriteString(blk.Text)
		}
	}
	return parseDiffReviewVerdict(text.String()), nil
}

// diffReviewResponse is the expected JSON shape from the model.
type diffReviewResponse struct {
	Passed   bool   `json:"passed"`
	Feedback string `json:"feedback"`
}

// parseDiffReviewVerdict translates the model's JSON response into a
// JudgeVerdict. A malformed response is a verdict FAILURE with the raw
// response as the reason.
func parseDiffReviewVerdict(response string) eval.JudgeVerdict {
	trimmed := strings.TrimSpace(response)
	var dr diffReviewResponse
	if err := json.Unmarshal([]byte(trimmed), &dr); err != nil {
		return eval.JudgeVerdict{
			Passed: false,
			Reason: fmt.Sprintf("diff-review model returned malformed JSON: %s (raw: %s)", err, trimmed),
		}
	}
	return eval.JudgeVerdict{
		Passed: dr.Passed,
		Reason: dr.Feedback,
	}
}
