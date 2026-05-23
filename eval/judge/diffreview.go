package judge

// diff-review judge: feed the workspace's working diff into an LLM
// (cheap model — defaults to Claude Haiku) and ask the model whether
// the diff meets the natural-language criteria. The output JSON
// shape mirrors verifier/llmjudge.go so a control-plane gate can
// route either at-run-time (verifier) or post-run (this judge)
// verdicts through one parser.
//
// Implementation notes:
//
//   - eval/* cannot import harness/internal/* (CLAUDE.md
//     boundary), so the API client is implemented here directly
//     against api.anthropic.com using stdlib net/http. Per the
//     project's "hand-rolled HTTP over SDKs" invariant, no vendor
//     module is added.
//   - The API key is sourced from ANTHROPIC_API_KEY at evaluation
//     time. Mining suites that bundle a diff-review judge MUST NOT
//     embed the key in the suite HCL — the secret stays in the
//     operator's environment.
//   - The judge invokes the model NON-streaming: we want the whole
//     verdict before deciding, and the JSON envelope is small.
//   - A network failure (timeout, 5xx) surfaces as a judge ERROR
//     (returned error from Evaluate). A model that returns
//     malformed JSON is a verdict FAILURE with diagnostics — same
//     posture as the verifier-side parser.

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
	diffReviewDefaultModel = "claude-3-5-haiku-latest"
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

// evaluateDiffReview is the judge.Evaluate handler for the
// diff-review type. Runs `git diff` in the workspace, sends the
// captured diff + criteria to the configured LLM, parses the JSON
// verdict.
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
		// Truncate but mark in the prompt; the model is told the
		// trailing portion is missing so it does not silently
		// review an incomplete diff.
		diff = diff[:diffReviewMaxDiffBytes] + "\n\n[... diff truncated at " + fmt.Sprintf("%d", diffReviewMaxDiffBytes) + " bytes ...]\n"
	}

	model := diffReviewDefaultModel
	// EvalJudge has no dedicated model field; the criteria string
	// is the user-facing surface. Future iterations may add a
	// j.Model attribute (and an HCL `model = "..."` line); for now
	// the default cheap model is hard-coded.

	verdict, err := callDiffReviewModel(ctx, apiKey, model, j.Criteria, diff)
	if err != nil {
		return eval.JudgeVerdict{}, fmt.Errorf("diff-review: %w", err)
	}
	return verdict, nil
}

// captureDiff runs `git diff HEAD` inside dir and returns the
// resulting text. An empty working tree (no diff) is the dominant
// "I added nothing" case for an execution-mode harness run; the
// judge handles it gracefully by passing the empty diff through —
// the model can then decide whether "no change" is acceptable per
// the criteria.
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

// anthropicRequest mirrors the subset of the /v1/messages request
// schema the diff-review judge needs. Kept local rather than
// imported from the harness provider so the eval module stays
// independent of harness/internal/.
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
// The HTTP client has an explicit 30-second timeout per the
// project's "no http.DefaultClient" invariant.
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
// JudgeVerdict. A malformed response is a verdict FAILURE with the
// raw response as the reason — matching the verifier's posture so
// the eval framework's caller sees a consistent failure surface
// whether the misbehaviour was at-run-time (verifier) or post-run
// (this judge).
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
