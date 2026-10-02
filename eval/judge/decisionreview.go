package judge

// The decision provider is documented in docs/eval.md.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
)

// diffReviewDecisionLayoutVersion identifies the request built by
// buildDecisionRequest and the budget constants below, and is part of the
// config hash; change it whenever either changes.
const diffReviewDecisionLayoutVersion = "diff-review-decision/v1"

// decisionQuestionKey names the single question of a diff-review call.
const decisionQuestionKey = "verdict"

const decisionQuestionText = "Does the change in `state` meet every criterion in `criteria`?"

// decisionStopReason is the JudgeRecord.StopReason of a decision call, which
// has no stop reason.
const decisionStopReason = "n/a"

// Input limits of the /v1/systemone protocol, counted in bytes: a byte-level
// tokenizer never emits more tokens than the text has bytes, so a request
// within a limit in bytes is within it in tokens.
const (
	// decisionStateTokenLimit bounds the state plus the longest question.
	decisionStateTokenLimit = 32_000

	// decisionRequestTokenLimit bounds the state plus every question.
	decisionRequestTokenLimit = 64_000

	// decisionTokenReserve is held back from both limits for request
	// framing and special tokens.
	decisionTokenReserve = 512

	// decisionNoteReserve bounds the truncation note, so the room left for
	// the diff does not depend on whether the note is present.
	decisionNoteReserve = 160
)

// decisionChoices describes the two options of the verdict question.
func decisionChoices() map[string]string {
	return map[string]string{
		types.JudgeStatusPass: "The change meets every criterion.",
		types.JudgeStatusFail: "The change misses at least one criterion, or the diff does not show that it meets it.",
	}
}

// decisionChoicesJSON is the canonical form of decisionChoices for the
// config hash.
func decisionChoicesJSON() string {
	data, _ := json.Marshal(decisionChoices())
	return string(data)
}

// decisionInstructions are the verdict question's structured instructions.
// They travel in the question, never in the agent-authored state.
type decisionInstructions struct {
	Criteria string `json:"criteria"`
	Boundary string `json:"boundary"`
	Note     string `json:"note,omitempty"`
	Question string `json:"question"`
}

// buildDecisionRequest maps a diff-review call onto one choice question: the
// fenced change summary and diff are the state, and the criteria, the fence
// notice and any truncation note are the question's instructions.
func buildDecisionRequest(criteria string, diff workspaceDiff, fence dataFence) decisionRequest {
	instructions := decisionInstructions{
		Criteria: criteria,
		Boundary: fence.notice(diffReviewFenceLabel),
		Question: decisionQuestionText,
	}
	if diff.Truncated {
		instructions.Note = fmt.Sprintf("The diff is %d bytes; only the first %d bytes are shown in `state`.", diff.Size, len(diff.Head))
	}
	return decisionRequest{
		State: fence.wrap(diffReviewFenceLabel, diffReviewFenceContent(diff)),
		Questions: map[string]decisionQuestion{
			decisionQuestionKey: {Type: "choice", Instructions: instructions, Criteria: decisionChoices()},
		},
	}
}

// decisionSizes returns the bytes of the state plus the longest question and
// of the state plus every question. Questions are measured in their JSON
// form, which is never shorter than the text a model reads.
func decisionSizes(req decisionRequest) (stateAndLongest, total int, err error) {
	longest := 0
	for _, q := range req.Questions {
		data, err := json.Marshal(q)
		if err != nil {
			return 0, 0, fmt.Errorf("encoding decision question: %w", err)
		}
		longest = max(longest, len(data))
		total += len(data)
	}
	return len(req.State) + longest, len(req.State) + total, nil
}

// checkDecisionLimits rejects a request over either protocol limit, naming
// the limit it exceeds.
func checkDecisionLimits(req decisionRequest) error {
	stateAndLongest, total, err := decisionSizes(req)
	if err != nil {
		return err
	}
	if budget := decisionStateTokenLimit - decisionTokenReserve; stateAndLongest > budget {
		return fmt.Errorf("decision request has %d bytes of state and longest question, over the %d-byte budget for the provider's %d-token limit on the state plus a question",
			stateAndLongest, budget, decisionStateTokenLimit)
	}
	if budget := decisionRequestTokenLimit - decisionTokenReserve; total > budget {
		return fmt.Errorf("decision request has %d bytes of state and questions, over the %d-byte budget for the provider's %d-token limit on a request",
			total, budget, decisionRequestTokenLimit)
	}
	return nil
}

// fitDecisionBudget cuts diff's head so that the request built from it fits
// the protocol's limits, marking the diff truncated when it cuts. The head
// is measured as the fence neutralises it, which can lengthen it.
func fitDecisionBudget(criteria string, diff workspaceDiff) (workspaceDiff, error) {
	var zero dataFence
	zero.nonce = strings.Repeat("0", 2*fenceNonceBytes)
	frame, _, err := decisionSizes(buildDecisionRequest(criteria, workspaceDiff{Stat: diff.Stat}, zero))
	if err != nil {
		return diff, err
	}
	room := decisionStateTokenLimit - decisionTokenReserve - decisionNoteReserve - frame
	if room <= 0 {
		return diff, fmt.Errorf("the criteria and change summary alone exceed the decision provider's %d-token input limit", decisionStateTokenLimit)
	}
	fits := func(n int) bool { return len(neutraliseFenceMarkers(diff.Head[:n])) <= room }
	if fits(len(diff.Head)) {
		return diff, nil
	}
	limit := min(len(diff.Head), room)
	n := sort.Search(limit+1, func(n int) bool { return !fits(n) }) - 1
	diff.Head = string(trimPartialRune([]byte(diff.Head[:n])))
	diff.Truncated = true
	return diff, nil
}

// callDecisionModel asks a /v1/systemone model for a verdict on diff and
// fills in rec's call fields. The verdict is the higher-probability option;
// the reason states the probabilities, since the protocol returns no text.
func callDecisionModel(ctx context.Context, cfg types.JudgeLLMConfig, criteria string, diff workspaceDiff, rec *types.JudgeRecord) (eval.JudgeVerdict, error) {
	apiKey, err := resolveJudgeKey(cfg)
	if err != nil {
		return diffReviewError(rec, err)
	}
	client, err := newDecisionClient(newJudgeHTTPClient(cfg, apiKey), cfg.BaseURL, apiKey, cfg.Model)
	if err != nil {
		return diffReviewError(rec, redactError(err, apiKey))
	}
	fence, err := newDataFence(rand.Reader)
	if err != nil {
		return diffReviewError(rec, err)
	}
	req := buildDecisionRequest(criteria, diff, fence)
	if err := checkDecisionLimits(req); err != nil {
		return diffReviewError(rec, err)
	}

	start := time.Now()
	resp, err := client.Decide(ctx, req)
	rec.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		return diffReviewError(rec, redactError(fmt.Errorf("model call failed: %w", err), apiKey))
	}
	rec.ServedModel = redactSecret(resp.Model, apiKey)
	rec.InputTokens = resp.InputTokens
	rec.OutputTokens = resp.OutputTokens
	rec.StopReason = decisionStopReason

	verdict, err := parseDecisionAnswers(resp.Answers)
	if err != nil {
		rec.ParseStatus = types.JudgeParseSchemaViolation
		return diffReviewError(rec, redactError(err, apiKey))
	}
	rec.ParseStatus = types.JudgeParseOK
	verdict.Record = rec
	return verdict, nil
}

// decisionChoiceAnswer is a choice answer. Fields the protocol may add are
// ignored.
type decisionChoiceAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    *float64           `json:"confidence"`
}

// decisionProbabilitySlack is how far the probabilities may sum from 1.
const decisionProbabilitySlack = 0.01

// parseDecisionAnswers validates the answer to the verdict question: exactly
// that one answer, a choice of pass or fail whose probability is the highest,
// probabilities for both options that sum to 1, and a confidence.
func parseDecisionAnswers(answers map[string]json.RawMessage) (eval.JudgeVerdict, error) {
	raw, ok := answers[decisionQuestionKey]
	if !ok || len(answers) != 1 {
		return eval.JudgeVerdict{}, fmt.Errorf("decision response must answer exactly the question %q; got %d answers", decisionQuestionKey, len(answers))
	}
	var a decisionChoiceAnswer
	if err := json.Unmarshal(raw, &a); err != nil {
		return eval.JudgeVerdict{}, fmt.Errorf("decision answer is not a choice answer: %v (answer: %s)", err, excerpt(string(raw)))
	}
	if a.Type != "choice" {
		return eval.JudgeVerdict{}, fmt.Errorf("decision answer type %s is not \"choice\"", excerpt(a.Type))
	}
	if a.Choice != types.JudgeStatusPass && a.Choice != types.JudgeStatusFail {
		return eval.JudgeVerdict{}, fmt.Errorf("decision choice %s is neither \"pass\" nor \"fail\"", excerpt(a.Choice))
	}
	if len(a.Probabilities) != 2 {
		return eval.JudgeVerdict{}, fmt.Errorf("decision answer carries %d probabilities, want one for each of pass and fail", len(a.Probabilities))
	}
	sum := 0.0
	for _, option := range []string{types.JudgeStatusPass, types.JudgeStatusFail} {
		p, ok := a.Probabilities[option]
		if !ok || !unitInterval(p) {
			return eval.JudgeVerdict{}, fmt.Errorf("decision probability for %q is missing or outside [0, 1]", option)
		}
		sum += p
	}
	if math.Abs(sum-1) > decisionProbabilitySlack {
		return eval.JudgeVerdict{}, fmt.Errorf("decision probabilities sum to %.4f, not 1", sum)
	}
	pPass, pFail := a.Probabilities[types.JudgeStatusPass], a.Probabilities[types.JudgeStatusFail]
	if (a.Choice == types.JudgeStatusPass && pPass < pFail) || (a.Choice == types.JudgeStatusFail && pFail < pPass) {
		return eval.JudgeVerdict{}, fmt.Errorf("decision choice %q is not the higher-probability option (p(pass)=%.4f)", a.Choice, pPass)
	}
	if a.Confidence == nil || !unitInterval(*a.Confidence) {
		return eval.JudgeVerdict{}, errors.New("decision confidence is missing or outside [0, 1]")
	}
	return eval.JudgeVerdict{
		Passed: a.Choice == types.JudgeStatusPass,
		Status: a.Choice,
		Reason: fmt.Sprintf("decision model: p(pass)=%.2f confidence=%.2f", pPass, *a.Confidence),
	}, nil
}

func unitInterval(v float64) bool {
	return !math.IsNaN(v) && v >= 0 && v <= 1
}
