package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

const triageSchema = `{"type":"object","required":["title","labels"],"additionalProperties":false,"properties":{"title":{"type":"string","maxLength":72},"labels":{"type":"array","items":{"enum":["bug","feature"]}}}}`

// answerProvider answers each Stream call with the next scripted final
// answer (repeating the last) and records the messages it was sent.
type answerProvider struct {
	mu      sync.Mutex
	answers []string
	// stops overrides the "end_turn" stop reason per call when set.
	stops    []string
	requests [][]types.Message
	systems  []string
}

func (p *answerProvider) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := min(len(p.requests), len(p.answers)-1)
	p.requests = append(p.requests, append([]types.Message(nil), params.Messages...))
	p.systems = append(p.systems, params.System)
	stop := "end_turn"
	if idx < len(p.stops) {
		stop = p.stops[idx]
	}
	ch := make(chan types.StreamEvent, 2)
	ch <- types.StreamEvent{Type: "text_delta", Text: p.answers[idx]}
	ch <- types.StreamEvent{Type: "message_complete", StopReason: stop}
	close(ch)
	return ch, nil
}

func (p *answerProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

// recordingVerifier passes or fails per call and records the final
// assistant text of every history it was asked to verify.
type recordingVerifier struct {
	mu     sync.Mutex
	passes []bool
	seen   []string
}

func (v *recordingVerifier) Verify(_ context.Context, vc verifier.VerifyContext) (*types.VerificationResult, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seen = append(v.seen, finalAnswerText(vc.Messages))
	passed := true
	if len(v.passes) > 0 {
		passed, v.passes = v.passes[0], v.passes[1:]
	}
	return &types.VerificationResult{Passed: passed, Feedback: "the labels are wrong"}, nil
}

func structuredTestLoop(answers ...string) (*AgenticLoop, *answerProvider, *cancellableTransport) {
	prov := &answerProvider{answers: answers}
	loop := buildTestLoop(&mockProvider{})
	loop.Provider = prov
	tr := &cancellableTransport{}
	loop.Transport = tr
	return loop, prov, tr
}

func structuredTestConfig() *types.RunConfig {
	config := buildTestConfig()
	config.OutputSchema = json.RawMessage(triageSchema)
	return config
}

func TestRun_InvalidThenValidAnswerReturnsStructuredOutput(t *testing.T) {
	loop, prov, tr := structuredTestLoop(
		"Here is the triage: the title is Fix login.",
		"```json\n{\n  \"title\": \"Fix login\",\n  \"labels\": [\"bug\"]\n}\n```",
	)

	runTrace, err := loop.Run(context.Background(), structuredTestConfig())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success (error %q)", runTrace.Outcome, runTrace.Error)
	}
	const want = `{"title":"Fix login","labels":["bug"]}`
	if string(runTrace.StructuredOutput) != want {
		t.Errorf("StructuredOutput = %s, want %s", runTrace.StructuredOutput, want)
	}
	if prov.calls() != 2 {
		t.Fatalf("provider calls = %d, want 2", prov.calls())
	}

	correction := prov.requests[1][len(prov.requests[1])-1]
	if correction.Role != "user" || !correction.Synthetic {
		t.Fatalf("second request ends with %+v, want a synthetic user correction", correction)
	}
	if text := correction.Content[0].Text; !strings.Contains(text, "not a single JSON document") || !strings.Contains(text, "<output_schema>") {
		t.Errorf("correction text = %q, want the violation and the schema pointer", text)
	}
	if !strings.Contains(prov.systems[0], "<output_schema>") {
		t.Error("system prompt lacks the output schema section")
	}

	done := doneEvent(t, tr.Events())
	if done.Trace == nil || string(done.Trace.StructuredOutput) != want {
		t.Errorf("done trace structured output = %v, want %s", done.Trace, want)
	}
}

func TestRun_StructuredOutputFailsAfterCorrectiveAttempts(t *testing.T) {
	loop, prov, tr := structuredTestLoop(
		`{"title":"Fix login"}`,
		`{"title":"Fix login","labels":["urgent"]}`,
		`{"title":"Fix login","labels":["bug"],"owner":"me"}`,
	)
	ver := &recordingVerifier{}
	loop.Verifier = ver

	runTrace, err := loop.Run(context.Background(), structuredTestConfig())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "structured_output_failed" {
		t.Fatalf("Outcome = %q, want structured_output_failed", runTrace.Outcome)
	}
	if got := prov.calls(); got != 1+maxStructuredOutputCorrections {
		t.Errorf("provider calls = %d, want %d", got, 1+maxStructuredOutputCorrections)
	}
	if !strings.HasPrefix(runTrace.Error, "structured output: the JSON document does not satisfy the schema:") ||
		!strings.Contains(runTrace.Error, "additional properties 'owner' not allowed") {
		t.Errorf("Error = %q, want the last validation error", runTrace.Error)
	}
	if strings.Contains(runTrace.Error, "missing property") {
		t.Errorf("Error = %q carries an earlier attempt's violation", runTrace.Error)
	}
	if runTrace.StructuredOutput != nil {
		t.Errorf("StructuredOutput = %s, want none for a failed run", runTrace.StructuredOutput)
	}
	if len(ver.seen) != 0 {
		t.Errorf("verifier ran %d times, want never for invalid answers", len(ver.seen))
	}
	if runTrace.StopDetails != nil {
		t.Errorf("StopDetails = %+v, want none once the outcome is reclassified", runTrace.StopDetails)
	}
	done := doneEvent(t, tr.Events())
	if done.StopReason != "structured_output_failed" || done.Trace == nil || done.Trace.Error != runTrace.Error {
		t.Errorf("done = %+v, want structured_output_failed with the run error", done)
	}
}

func TestRun_StructuredCorrectionsShareMaxTurns(t *testing.T) {
	const valid = `{"title":"Fix login","labels":["bug"]}`
	tests := []struct {
		name        string
		maxTurns    int
		answers     []string
		wantCalls   int
		wantOutcome string
	}{
		{name: "turn budget smaller than the correction budget", maxTurns: 2, answers: []string{"not json"}, wantCalls: 2, wantOutcome: "max_turns"},
		{name: "single turn leaves no room for a correction", maxTurns: 1, answers: []string{"not json"}, wantCalls: 1, wantOutcome: "max_turns"},
		{name: "valid answer on the last turn", maxTurns: 3, answers: []string{"not json", "still not json", valid}, wantCalls: 3, wantOutcome: "success"},
		{name: "correction budget exhausted before the turn budget", maxTurns: 20, answers: []string{"not json"}, wantCalls: 1 + maxStructuredOutputCorrections, wantOutcome: "structured_output_failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			loop, prov, _ := structuredTestLoop(tc.answers...)
			config := structuredTestConfig()
			config.MaxTurns = tc.maxTurns

			runTrace, err := loop.Run(context.Background(), config)
			if err != nil {
				t.Fatalf("Run() error: %v", err)
			}
			if got := prov.calls(); got != tc.wantCalls {
				t.Errorf("provider calls = %d, want %d", got, tc.wantCalls)
			}
			if runTrace.Turns != tc.wantCalls {
				t.Errorf("RunTrace.Turns = %d, want %d", runTrace.Turns, tc.wantCalls)
			}
			if runTrace.Outcome != tc.wantOutcome {
				t.Fatalf("Outcome = %q, want %q (error %q)", runTrace.Outcome, tc.wantOutcome, runTrace.Error)
			}
			if tc.wantOutcome == "max_turns" {
				if !strings.Contains(runTrace.Error, "before the final answer satisfied the output schema") ||
					!strings.Contains(runTrace.Error, "not a single JSON document") {
					t.Errorf("Error = %q, want the turn limit and the last violation", runTrace.Error)
				}
				last := prov.requests[len(prov.requests)-1]
				if corrections := countSyntheticUserTurns(last); corrections != tc.wantCalls-1 {
					t.Errorf("last request carries %d corrections, want %d", corrections, tc.wantCalls-1)
				}
			}
		})
	}
}

func TestRun_StructuredCorrectionsWithToolTurnsShareMaxTurns(t *testing.T) {
	tests := []struct {
		name        string
		maxTurns    int
		wantOutcome string
	}{
		{name: "turns run out at a correction boundary", maxTurns: 4, wantOutcome: "max_turns"},
		{name: "turns run out mid-correction", maxTurns: 5, wantOutcome: "max_turns"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prov := &attemptProvider{turnsPerAttempt: 2}
			loop, callTurns := buildRepairTestLoop(prov)
			config := structuredTestConfig()
			config.MaxTurns = tc.maxTurns

			runTrace, err := loop.Run(context.Background(), config)
			if err != nil {
				t.Fatalf("Run() error: %v", err)
			}
			if got := prov.callCount(); got != tc.maxTurns {
				t.Errorf("provider calls = %d, want %d", got, tc.maxTurns)
			}
			if runTrace.Turns != tc.maxTurns {
				t.Errorf("RunTrace.Turns = %d, want %d", runTrace.Turns, tc.maxTurns)
			}
			if runTrace.Outcome != tc.wantOutcome {
				t.Errorf("Outcome = %q, want %q (error %q)", runTrace.Outcome, tc.wantOutcome, runTrace.Error)
			}
			assertStrictlyIncreasing(t, "tool call turns", *callTurns)
		})
	}
}

func countSyntheticUserTurns(messages []types.Message) int {
	n := 0
	for _, m := range messages {
		if m.Role == "user" && m.Synthetic {
			n++
		}
	}
	return n
}

func TestRun_InvalidAnswerNeverReachesVerifier(t *testing.T) {
	loop, _, _ := structuredTestLoop(
		"not json",
		`{"title":"Fix login","labels":["bug"]}`,
	)
	ver := &recordingVerifier{}
	loop.Verifier = ver

	runTrace, err := loop.Run(context.Background(), structuredTestConfig())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", runTrace.Outcome)
	}
	if len(ver.seen) != 1 || ver.seen[0] != `{"title":"Fix login","labels":["bug"]}` {
		t.Errorf("verifier saw %q, want only the valid answer", ver.seen)
	}
}

func TestRun_AnswerAfterVerifierRetryIsValidatedAgain(t *testing.T) {
	loop, prov, _ := structuredTestLoop(
		`{"title":"Fix login","labels":["feature"]}`,
		`{"title":"Fix login","labels":"bug"}`,
		`{"title":"Fix login","labels":["bug"]}`,
	)
	ver := &recordingVerifier{passes: []bool{false, true}}
	loop.Verifier = ver

	runTrace, err := loop.Run(context.Background(), structuredTestConfig())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success (error %q)", runTrace.Outcome, runTrace.Error)
	}
	if string(runTrace.StructuredOutput) != `{"title":"Fix login","labels":["bug"]}` {
		t.Errorf("StructuredOutput = %s, want the answer that passed verification", runTrace.StructuredOutput)
	}
	if prov.calls() != 3 {
		t.Errorf("provider calls = %d, want 3", prov.calls())
	}
	want := []string{`{"title":"Fix login","labels":["feature"]}`, `{"title":"Fix login","labels":["bug"]}`}
	if strings.Join(ver.seen, "|") != strings.Join(want, "|") {
		t.Errorf("verifier saw %q, want %q", ver.seen, want)
	}
}

func TestRun_FailedOutcomeReportsNoStructuredOutput(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(*AgenticLoop)
		wantOutcome string
	}{
		{
			name:        "verification failed",
			setup:       func(l *AgenticLoop) { l.Verifier = &recordingVerifier{passes: []bool{false, false, false, false}} },
			wantOutcome: "verification_failed",
		},
		{
			name:        "post-run hook failed",
			setup:       func(l *AgenticLoop) { l.Hooks = &fakeHookRunner{postErr: errSentinelPostHookFailure} },
			wantOutcome: "hook_failed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			loop, _, tr := structuredTestLoop(`{"title":"Fix login","labels":["bug"]}`)
			tc.setup(loop)

			runTrace, err := loop.Run(context.Background(), structuredTestConfig())
			if err != nil {
				t.Fatalf("Run() error: %v", err)
			}
			if runTrace.Outcome != tc.wantOutcome {
				t.Fatalf("Outcome = %q, want %q", runTrace.Outcome, tc.wantOutcome)
			}
			if runTrace.StructuredOutput != nil {
				t.Errorf("StructuredOutput = %s, want none for a failed run", runTrace.StructuredOutput)
			}
			if done := doneEvent(t, tr.Events()); done.Trace == nil || done.Trace.StructuredOutput != nil {
				t.Errorf("done trace = %+v, want no structured output", done.Trace)
			}
		})
	}
}

func TestRun_RepairEndingWithoutAnAnswerReportsNoStructuredOutput(t *testing.T) {
	loop, prov, _ := structuredTestLoop(`{"title":"Fix login","labels":["bug"]}`, `{"title":"Fix`)
	prov.stops = []string{"end_turn", "max_tokens"}
	loop.Verifier = &recordingVerifier{passes: []bool{false}}

	runTrace, err := loop.Run(context.Background(), structuredTestConfig())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "max_tokens" {
		t.Fatalf("Outcome = %q, want max_tokens", runTrace.Outcome)
	}
	if runTrace.StructuredOutput != nil {
		t.Errorf("StructuredOutput = %s, want none: the run's last answer was cut off", runTrace.StructuredOutput)
	}
}

func TestRun_NoOutputSchemaKeepsFreeText(t *testing.T) {
	loop, prov, tr := structuredTestLoop("Plain prose answer.")
	ver := &recordingVerifier{}
	loop.Verifier = ver

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" || runTrace.FinalAssistantText != "Plain prose answer." {
		t.Errorf("Outcome = %q, FinalAssistantText = %q; want success with the prose answer", runTrace.Outcome, runTrace.FinalAssistantText)
	}
	if runTrace.StructuredOutput != nil || prov.calls() != 1 || len(ver.seen) != 1 {
		t.Errorf("StructuredOutput = %s, provider calls = %d, verifier calls = %d; want none, 1, 1",
			runTrace.StructuredOutput, prov.calls(), len(ver.seen))
	}
	if strings.Contains(prov.systems[0], "output_schema") {
		t.Error("system prompt mentions an output schema without one configured")
	}
	if done := doneEvent(t, tr.Events()); done.Trace == nil || done.Trace.StructuredOutput != nil {
		t.Errorf("done trace = %+v, want no structured output", done.Trace)
	}
}

func TestRun_StructuredOutputIsScrubbed(t *testing.T) {
	loop, _, _ := structuredTestLoop(`{"title":"key AKIAIOSFODNN7EXAMPLE leaked","labels":["bug"]}`)

	runTrace, err := loop.Run(context.Background(), structuredTestConfig())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success (error %q)", runTrace.Outcome, runTrace.Error)
	}
	if strings.Contains(string(runTrace.StructuredOutput), "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("StructuredOutput = %s, want the credential scrubbed", runTrace.StructuredOutput)
	}
	if !json.Valid(runTrace.StructuredOutput) {
		t.Errorf("StructuredOutput = %s is not valid JSON after scrubbing", runTrace.StructuredOutput)
	}
}

func TestRun_RedactionThatBreaksTheSchemaFailsWithoutCorrection(t *testing.T) {
	loop, prov, _ := structuredTestLoop(`{"key":"AKIAIOSFODNN7EXAMPLE"}`)
	config := structuredTestConfig()
	config.OutputSchema = json.RawMessage(`{"type":"object","required":["key"],"properties":{"key":{"type":"string","pattern":"^AKIA[A-Z0-9]{16}$"}}}`)

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "structured_output_failed" {
		t.Fatalf("Outcome = %q, want structured_output_failed", runTrace.Outcome)
	}
	if prov.calls() != 1 {
		t.Errorf("provider calls = %d, want 1: the model cannot correct a redaction", prov.calls())
	}
	if !strings.HasPrefix(runTrace.Error, "structured output: scrubbing secrets from the JSON document broke it") {
		t.Errorf("Error = %q, want redaction named as the cause", runTrace.Error)
	}
}

func TestRun_OversizedStructuredOutputIsAFailureNotTruncated(t *testing.T) {
	big := `{"title":"` + strings.Repeat("x", 60) + `","labels":["bug","bug","bug","bug","bug","bug"]}`
	loop, prov, _ := structuredTestLoop(big)
	config := structuredTestConfig()
	config.ResultSink = &types.ResultSinkConfig{Type: "none", MaxFinalAssistantTextBytes: 64}

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "structured_output_failed" {
		t.Fatalf("Outcome = %q, want structured_output_failed", runTrace.Outcome)
	}
	if !strings.Contains(runTrace.Error, "over the 64-byte limit") {
		t.Errorf("Error = %q, want the size limit named", runTrace.Error)
	}
	if prov.calls() != 1+maxStructuredOutputCorrections {
		t.Errorf("provider calls = %d, want every corrective attempt spent", prov.calls())
	}
	if runTrace.StructuredOutput != nil {
		t.Errorf("StructuredOutput = %s, want none", runTrace.StructuredOutput)
	}
}

func TestSpawnSubAgent_ChildDoesNotInheritOutputSchema(t *testing.T) {
	parent, _, _ := structuredTestLoop(`{"title":"parent","labels":["bug"]}`)
	child := &answerProvider{answers: []string{"child prose answer"}}
	parent.Provider = child
	parentConfig := structuredTestConfig()

	result, err := SpawnSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "look around", Mode: "research"})
	if err != nil {
		t.Fatalf("SpawnSubAgent: %v", err)
	}
	if result.Outcome != "success" {
		t.Fatalf("child outcome = %q, want success for a prose answer", result.Outcome)
	}
	if strings.Contains(child.systems[0], "output_schema") {
		t.Error("child system prompt carries the parent's output schema")
	}
	if len(parentConfig.OutputSchema) == 0 {
		t.Error("spawning a child cleared the parent's output schema")
	}
}

func TestExtractStructuredOutput(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		want    string
		wantErr string
	}{
		{name: "bare_object", text: `{"a":1}`, want: `{"a":1}`},
		{name: "bare_object_padded", text: "\n  {\"a\":1}  \n", want: `{"a":1}`},
		{name: "bare_array", text: `[1,2]`, want: `[1,2]`},
		{name: "bare_string", text: `"done"`, want: `"done"`},
		{name: "fenced_json", text: "```json\n{\"a\":1}\n```", want: `{"a":1}`},
		{name: "fenced_upper_tag", text: "```JSON\n{\"a\":1}\n```", want: `{"a":1}`},
		{name: "fenced_untagged", text: "```\n{\"a\":1}\n```", want: `{"a":1}`},
		{name: "empty", text: "  \n", wantErr: "has no text"},
		{name: "prose_before_fence", text: "Here it is:\n```json\n{\"a\":1}\n```", wantErr: "send only the JSON document"},
		{name: "prose_after_fence", text: "```json\n{\"a\":1}\n```\nHope that helps.", wantErr: "nothing after its closing fence"},
		{name: "two_fences", text: "```json\n{\"a\":1}\n```\n```json\n{\"b\":2}\n```", wantErr: "not a single JSON document"},
		{name: "other_language", text: "```yaml\na: 1\n```", wantErr: `tagged "yaml"`},
		{name: "unclosed_fence", text: "```json\n{\"a\":1}", wantErr: "nothing after its closing fence"},
		{name: "fence_without_body", text: "```json", wantErr: "no content"},
		{name: "trailing_prose", text: `{"a":1} done`, wantErr: "after top-level value"},
		{name: "two_documents", text: `{"a":1} {"b":2}`, wantErr: "not a single JSON document"},
		{name: "prose", text: "The answer is yes.", wantErr: "send only the JSON document"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractStructuredOutput(tc.text)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			if string(got) != tc.want {
				t.Errorf("document = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestCheckStructuredOutput(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"},"note":{"type":"string"}},"additionalProperties":false}`)

	t.Run("compacts_and_keeps_key_order_and_numbers", func(t *testing.T) {
		got, err := checkStructuredOutput("{\n \"note\": \"a<b\",\n \"n\": 12345678901234567890\n}", json.RawMessage(`{"type":"object"}`), 1024)
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if string(got) != `{"note":"a<b","n":12345678901234567890}` {
			t.Errorf("document = %s", got)
		}
	})

	t.Run("rejects_prototype_pollution_keys", func(t *testing.T) {
		for _, doc := range []string{`{"n":1,"__proto__":{"admin":true}}`, `{"n":1,"note":"x","nested":[{"constructor":"Foo"}]}`} {
			_, err := checkStructuredOutput(doc, json.RawMessage(`{"type":"object"}`), 1024)
			if err == nil || !strings.Contains(err.Error(), "reserved key") || errors.Is(err, errStructuredOutputRedacted) {
				t.Errorf("%s: error = %v, want a correctable reserved-key violation", doc, err)
			}
		}
	})

	t.Run("names_the_reserved_key_a_schema_requires", func(t *testing.T) {
		requires := json.RawMessage(`{"type":"object","required":["constructor"]}`)
		_, err := checkStructuredOutput(`{"constructor":"Foo"}`, requires, 1024)
		if err == nil || !strings.Contains(err.Error(), `reserved key "constructor"`) {
			t.Errorf("error = %v, want the reserved key named", err)
		}
	})

	t.Run("rejects_a_repeated_key", func(t *testing.T) {
		_, err := checkStructuredOutput(`{"n":1,"note":"a","note":"b"}`, schema, 1024)
		if err == nil || !strings.Contains(err.Error(), `repeats the key "note"`) || errors.Is(err, errStructuredOutputRedacted) {
			t.Errorf("error = %v, want a correctable repeated-key violation", err)
		}
	})

	t.Run("redaction_that_breaks_the_schema_is_not_correctable", func(t *testing.T) {
		pattern := json.RawMessage(`{"type":"object","properties":{"key":{"type":"string","pattern":"^AKIA[A-Z0-9]{16}$"}}}`)
		_, err := checkStructuredOutput(`{"key":"AKIAIOSFODNN7EXAMPLE"}`, pattern, 1024)
		if !errors.Is(err, errStructuredOutputRedacted) {
			t.Fatalf("error = %v, want errStructuredOutputRedacted", err)
		}
		if strings.Contains(err.Error(), "AKIAIOSFODNN7EXAMPLE") {
			t.Errorf("error = %q carries the credential", err)
		}
	})

	t.Run("keys_that_collide_after_redaction_are_not_correctable", func(t *testing.T) {
		_, err := checkStructuredOutput(`{"AKIAIOSFODNN7EXAMPLE":1,"AKIAIOSFODNN7EXAMPL2":2}`, json.RawMessage(`{"type":"object"}`), 1024)
		if !errors.Is(err, errStructuredOutputRedacted) || !strings.Contains(err.Error(), "two keys became") {
			t.Errorf("error = %v, want a redaction collision", err)
		}
	})

	t.Run("violation_lists_pointers_without_header", func(t *testing.T) {
		_, err := checkStructuredOutput(`{"n":"one","extra":true}`, schema, 1024)
		if err == nil {
			t.Fatal("error = nil, want a violation")
		}
		msg := err.Error()
		if !strings.HasPrefix(msg, "the JSON document does not satisfy the schema:\n- at ") {
			t.Errorf("violation = %q, want one line per violation after the lead-in", msg)
		}
		if strings.Contains(msg, "urn:stirrup") || strings.Contains(msg, "jsonschema validation failed") {
			t.Errorf("violation = %q carries the validator header", msg)
		}
	})

	t.Run("size_limit_applies_to_the_compact_document", func(t *testing.T) {
		if _, err := checkStructuredOutput("{\n\n\n\"n\": 1\n\n\n}", schema, 7); err != nil {
			t.Errorf("error = %v, want the 7-byte compact document accepted", err)
		}
		if _, err := checkStructuredOutput(`{"n":1}`, schema, 6); err == nil || !strings.Contains(err.Error(), "over the 6-byte limit") {
			t.Errorf("error = %v, want the size limit", err)
		}
	})
}

func TestDuplicateKey(t *testing.T) {
	tests := []struct {
		doc     string
		wantKey string
	}{
		{doc: `{"a":1,"b":{"a":2},"c":[{"a":3},{"a":4}]}`},
		{doc: `{"a":1,"b":2,"a":3}`, wantKey: "a"},
		{doc: `{"o":{"x":[1,{"y":1}],"x":true}}`, wantKey: "x"},
		{doc: `["a","a",{"k":"a"}]`},
		{doc: `"a"`},
	}
	for _, tc := range tests {
		key, ok := duplicateKey([]byte(tc.doc))
		if ok != (tc.wantKey != "") || key != tc.wantKey {
			t.Errorf("duplicateKey(%s) = %q, %v; want %q", tc.doc, key, ok, tc.wantKey)
		}
	}
}

func TestScrubJSONStrings(t *testing.T) {
	in := []byte(`{"key":"AKIAIOSFODNN7EXAMPLE","esc":"quote \" and \\ back","list":["AKIAIOSFODNN7EXAMPLE",1],"AKIAIOSFODNN7EXAMPLE":true}`)
	got := scrubJSONStrings(in)
	if strings.Contains(string(got), "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("scrubbed = %s, want every credential occurrence redacted", got)
	}
	if !json.Valid(got) {
		t.Fatalf("scrubbed = %s is not valid JSON", got)
	}
	if !strings.Contains(string(got), `"esc":"quote \" and \\ back"`) {
		t.Errorf("scrubbed = %s, want unchanged literals kept byte for byte", got)
	}

	clean := []byte(`{"a":"plain","b":[true,null,1.50]}`)
	if got := scrubJSONStrings(clean); string(got) != string(clean) {
		t.Errorf("scrubbed = %s, want a clean document unchanged", got)
	}
}

func TestStructuredOutputViolationIsScrubbedAndCapped(t *testing.T) {
	got := structuredOutputViolation(errors.New("bad value AKIAIOSFODNN7EXAMPLE " + strings.Repeat("y", 4096)))
	if strings.Contains(got, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("violation carries the credential: %q", got[:64])
	}
	if len(got) > maxStructuredOutputViolationBytes+64 {
		t.Errorf("violation length = %d, want it capped near %d", len(got), maxStructuredOutputViolationBytes)
	}
}

func TestValidateOutputSchema(t *testing.T) {
	if err := ValidateOutputSchema(buildTestConfig()); err != nil {
		t.Errorf("no schema: error = %v, want nil", err)
	}
	if err := ValidateOutputSchema(structuredTestConfig()); err != nil {
		t.Errorf("valid schema: error = %v, want nil", err)
	}
	config := buildTestConfig()
	config.OutputSchema = json.RawMessage(`{"type":"strng"}`)
	if err := ValidateOutputSchema(config); err == nil || !strings.HasPrefix(err.Error(), "outputSchema: failed to compile schema") {
		t.Errorf("uncompilable schema: error = %v, want a compile failure", err)
	}
}

func TestBuildLoopWithTransport_RejectsUncompilableOutputSchema(t *testing.T) {
	config := buildTestConfig()
	config.Executor.Workspace = t.TempDir()
	config.OutputSchema = json.RawMessage(`{"type":"object","required":"title"}`)

	_, err := BuildLoopWithTransport(context.Background(), config, nil)
	if err == nil || !strings.Contains(err.Error(), "config validation: outputSchema: failed to compile schema") {
		t.Fatalf("BuildLoopWithTransport error = %v, want the schema rejected at validation", err)
	}
}

func TestPreflight_UncompilableOutputSchemaFailsValidation(t *testing.T) {
	cfg := preflightTestConfig(t, "http://127.0.0.1:1/v1")
	cfg.OutputSchema = json.RawMessage(`{"type":"strng"}`)

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if st, _ := stepStatus(report, "config-validation"); st != PreflightFail || report.OK {
		t.Errorf("config-validation = %v, OK = %v; want a failed validation step", st, report.OK)
	}
}
