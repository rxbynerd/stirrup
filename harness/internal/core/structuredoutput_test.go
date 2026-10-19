package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
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
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome, "error: %s", runTrace.Error)
	require.Equal(t, 2, prov.calls())

	const want = `{"title":"Fix login","labels":["bug"]}`
	assert.Equal(t, want, string(runTrace.StructuredOutput)) //nolint:testifylint // the compacted bytes are what is asserted

	correction := prov.requests[1][len(prov.requests[1])-1]
	require.Equal(t, "user", correction.Role)
	require.True(t, correction.Synthetic, "the second request should end with a synthetic user correction")
	assert.Contains(t, correction.Content[0].Text, "not a single JSON document")
	assert.Contains(t, correction.Content[0].Text, "<output_schema>")
	assert.Contains(t, prov.systems[0], "<output_schema>", "system prompt lacks the output schema section")

	done := doneEvent(t, tr.Events())
	require.NotNil(t, done.Trace)
	assert.Equal(t, want, string(done.Trace.StructuredOutput)) //nolint:testifylint // the compacted bytes are what is asserted
}

// TestRun_StructuredOutputCorrectionIsReportedBetweenTurns pins that the
// correction request is reported as the text appended to history, after
// the rejected answer commits and before the corrected turn opens.
func TestRun_StructuredOutputCorrectionIsReportedBetweenTurns(t *testing.T) {
	loop, prov, tr := structuredTestLoop("not json", `{"title":"Fix login","labels":["bug"]}`)

	_, err := loop.Run(context.Background(), structuredTestConfig())
	require.NoError(t, err)
	require.Equal(t, 2, prov.calls())
	appended := prov.requests[1][len(prov.requests[1])-1].Content[0].Text

	var order []string
	for _, ev := range tr.Events() {
		switch {
		case ev.Type == "turn_started":
			order = append(order, fmt.Sprintf("start:%d", *ev.Turn))
		case ev.Type == "turn_committed":
			order = append(order, fmt.Sprintf("commit:%d", *ev.Turn))
		case ev.Type == "user_message" && ev.Source == types.UserMessageSourceStructuredOutputCorrection:
			order = append(order, "correction")
			assert.Equal(t, appended, ev.Text, "the correction text reported as an event")
		}
	}
	assert.Equal(t, []string{"start:0", "commit:0", "correction", "start:1", "commit:1"}, order)
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
	require.NoError(t, err)
	require.Equal(t, "structured_output_failed", runTrace.Outcome)

	assert.Equal(t, 1+maxStructuredOutputCorrections, prov.calls())
	assert.True(t, strings.HasPrefix(runTrace.Error, "structured output: the JSON document does not satisfy the schema:"), "Error = %q", runTrace.Error)
	assert.Contains(t, runTrace.Error, "additional properties 'owner' not allowed")
	assert.NotContains(t, runTrace.Error, "missing property", "the error carries an earlier attempt's violation")
	assert.Nil(t, runTrace.StructuredOutput, "a failed run has no structured output")
	assert.Empty(t, ver.seen, "the verifier must not run on invalid answers")
	assert.Nil(t, runTrace.StopDetails, "reclassifying the outcome clears the stop details")

	done := doneEvent(t, tr.Events())
	require.NotNil(t, done.Trace)
	assert.Equal(t, "structured_output_failed", done.StopReason)
	assert.Equal(t, runTrace.Error, done.Trace.Error)
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
			require.NoError(t, err)
			require.Equal(t, tc.wantCalls, prov.calls())
			require.Equal(t, tc.wantCalls, runTrace.Turns)
			require.Equal(t, tc.wantOutcome, runTrace.Outcome, "error: %s", runTrace.Error)

			if tc.wantOutcome == "max_turns" {
				require.Contains(t, runTrace.Error, "before the final answer satisfied the output schema")
				require.Contains(t, runTrace.Error, "not a single JSON document")
				last := prov.requests[len(prov.requests)-1]
				require.Equal(t, tc.wantCalls-1, countSyntheticUserTurns(last), "corrections carried by the last request")
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
			require.NoError(t, err)
			require.Equal(t, tc.maxTurns, prov.callCount())
			require.Equal(t, tc.maxTurns, runTrace.Turns)
			require.Equal(t, tc.wantOutcome, runTrace.Outcome, "error: %s", runTrace.Error)
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
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome)
	require.Equal(t, []string{`{"title":"Fix login","labels":["bug"]}`}, ver.seen, "the verifier should see only the valid answer")
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
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome, "error: %s", runTrace.Error)

	assert.Equal(t, `{"title":"Fix login","labels":["bug"]}`, string(runTrace.StructuredOutput), "the answer that passed verification") //nolint:testifylint // the compacted bytes are what is asserted
	assert.Equal(t, 3, prov.calls())
	want := []string{`{"title":"Fix login","labels":["feature"]}`, `{"title":"Fix login","labels":["bug"]}`}
	assert.Equal(t, want, ver.seen)
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
			require.NoError(t, err)
			require.Equal(t, tc.wantOutcome, runTrace.Outcome)
			require.Nil(t, runTrace.StructuredOutput, "a failed run has no structured output")

			done := doneEvent(t, tr.Events())
			require.NotNil(t, done.Trace)
			require.Nil(t, done.Trace.StructuredOutput)
		})
	}
}

func TestRun_RepairEndingWithoutAnAnswerReportsNoStructuredOutput(t *testing.T) {
	loop, prov, _ := structuredTestLoop(`{"title":"Fix login","labels":["bug"]}`, `{"title":"Fix`)
	prov.stops = []string{"end_turn", "max_tokens"}
	loop.Verifier = &recordingVerifier{passes: []bool{false}}

	runTrace, err := loop.Run(context.Background(), structuredTestConfig())
	require.NoError(t, err)
	require.Equal(t, "max_tokens", runTrace.Outcome)
	require.Nil(t, runTrace.StructuredOutput, "the run's last answer was cut off")
}

func TestRun_NoOutputSchemaKeepsFreeText(t *testing.T) {
	loop, prov, tr := structuredTestLoop("Plain prose answer.")
	ver := &recordingVerifier{}
	loop.Verifier = ver

	runTrace, err := loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	assert.Equal(t, "success", runTrace.Outcome)
	assert.Equal(t, "Plain prose answer.", runTrace.FinalAssistantText)
	assert.Nil(t, runTrace.StructuredOutput)
	assert.Equal(t, 1, prov.calls())
	assert.Len(t, ver.seen, 1)
	assert.NotContains(t, prov.systems[0], "output_schema", "system prompt mentions an output schema without one configured")

	done := doneEvent(t, tr.Events())
	require.NotNil(t, done.Trace)
	assert.Nil(t, done.Trace.StructuredOutput)
}

func TestRun_StructuredOutputIsScrubbed(t *testing.T) {
	loop, _, _ := structuredTestLoop(`{"title":"key AKIAIOSFODNN7EXAMPLE leaked","labels":["bug"]}`)

	runTrace, err := loop.Run(context.Background(), structuredTestConfig())
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome, "error: %s", runTrace.Error)
	assert.NotContains(t, string(runTrace.StructuredOutput), "AKIAIOSFODNN7EXAMPLE", "the credential should be scrubbed")
	assert.True(t, json.Valid(runTrace.StructuredOutput), "the structured output should stay valid JSON after scrubbing")
}

func TestRun_RedactionThatBreaksTheSchemaFailsWithoutCorrection(t *testing.T) {
	loop, prov, _ := structuredTestLoop(`{"key":"AKIAIOSFODNN7EXAMPLE"}`)
	config := structuredTestConfig()
	config.OutputSchema = json.RawMessage(`{"type":"object","required":["key"],"properties":{"key":{"type":"string","pattern":"^AKIA[A-Z0-9]{16}$"}}}`)

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.Equal(t, "structured_output_failed", runTrace.Outcome)
	assert.Equal(t, 1, prov.calls(), "the model cannot correct a redaction")
	assert.True(t, strings.HasPrefix(runTrace.Error, "structured output: scrubbing secrets from the JSON document broke it"), "Error = %q", runTrace.Error)
}

func TestRun_OversizedStructuredOutputIsAFailureNotTruncated(t *testing.T) {
	big := `{"title":"` + strings.Repeat("x", 60) + `","labels":["bug","bug","bug","bug","bug","bug"]}`
	loop, prov, _ := structuredTestLoop(big)
	config := structuredTestConfig()
	config.ResultSink = &types.ResultSinkConfig{Type: "none", MaxFinalAssistantTextBytes: 64}

	runTrace, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.Equal(t, "structured_output_failed", runTrace.Outcome)
	assert.Contains(t, runTrace.Error, "over the 64-byte limit")
	assert.Equal(t, 1+maxStructuredOutputCorrections, prov.calls(), "every corrective attempt is spent")
	assert.Nil(t, runTrace.StructuredOutput)
}

func TestSpawnSubAgent_ChildDoesNotInheritOutputSchema(t *testing.T) {
	parent, _, _ := structuredTestLoop(`{"title":"parent","labels":["bug"]}`)
	parent.Tools.(*tool.Registry).Register(&tool.Tool{
		Name:        "read_file",
		Description: "stub read_file",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "", nil
		},
	})
	child := &answerProvider{answers: []string{"child prose answer"}}
	parent.Provider = child
	parentConfig := structuredTestConfig()

	result, err := SpawnSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "look around", Mode: "research"})
	require.NoError(t, err)
	require.Equal(t, "success", result.Outcome, "a prose answer should succeed")
	assert.NotContains(t, child.systems[0], "output_schema", "the child system prompt carries the parent's output schema")
	assert.NotEmpty(t, parentConfig.OutputSchema, "spawning a child cleared the parent's output schema")
}

func TestExtractStructuredOutput_UnwrapsFencesAndRejectsProse(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		want    string
		wantErr string
	}{
		{name: "bare object", text: `{"a":1}`, want: `{"a":1}`},
		{name: "bare object padded", text: "\n  {\"a\":1}  \n", want: `{"a":1}`},
		{name: "bare array", text: `[1,2]`, want: `[1,2]`},
		{name: "bare string", text: `"done"`, want: `"done"`},
		{name: "fenced json", text: "```json\n{\"a\":1}\n```", want: `{"a":1}`},
		{name: "fenced upper tag", text: "```JSON\n{\"a\":1}\n```", want: `{"a":1}`},
		{name: "fenced untagged", text: "```\n{\"a\":1}\n```", want: `{"a":1}`},
		{name: "empty", text: "  \n", wantErr: "has no text"},
		{name: "prose before fence", text: "Here it is:\n```json\n{\"a\":1}\n```", wantErr: "send only the JSON document"},
		{name: "prose after fence", text: "```json\n{\"a\":1}\n```\nHope that helps.", wantErr: "nothing after its closing fence"},
		{name: "two fences", text: "```json\n{\"a\":1}\n```\n```json\n{\"b\":2}\n```", wantErr: "not a single JSON document"},
		{name: "other language", text: "```yaml\na: 1\n```", wantErr: `tagged "yaml"`},
		{name: "unclosed fence", text: "```json\n{\"a\":1}", wantErr: "nothing after its closing fence"},
		{name: "fence without body", text: "```json", wantErr: "no content"},
		{name: "trailing prose", text: `{"a":1} done`, wantErr: "after top-level value"},
		{name: "two documents", text: `{"a":1} {"b":2}`, wantErr: "not a single JSON document"},
		{name: "prose", text: "The answer is yes.", wantErr: "send only the JSON document"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractStructuredOutput(tc.text)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, string(got))
		})
	}
}

func TestCheckStructuredOutput_CompactsAndRejectsViolations(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"},"note":{"type":"string"}},"additionalProperties":false}`)

	t.Run("compacts and keeps key order and numbers", func(t *testing.T) {
		got, err := checkStructuredOutput("{\n \"note\": \"a<b\",\n \"n\": 12345678901234567890\n}", json.RawMessage(`{"type":"object"}`), 1024)
		require.NoError(t, err)
		require.Equal(t, `{"note":"a<b","n":12345678901234567890}`, string(got)) //nolint:testifylint // asserts exact bytes, including a number beyond float64 precision
	})

	t.Run("rejects prototype pollution keys", func(t *testing.T) {
		for _, doc := range []string{`{"n":1,"__proto__":{"admin":true}}`, `{"n":1,"note":"x","nested":[{"constructor":"Foo"}]}`} {
			_, err := checkStructuredOutput(doc, json.RawMessage(`{"type":"object"}`), 1024)
			require.ErrorContains(t, err, "reserved key", "document: %s", doc)
			require.NotErrorIs(t, err, errStructuredOutputRedacted, "document: %s", doc)
		}
	})

	t.Run("names the reserved key a schema requires", func(t *testing.T) {
		requires := json.RawMessage(`{"type":"object","required":["constructor"]}`)
		_, err := checkStructuredOutput(`{"constructor":"Foo"}`, requires, 1024)
		require.ErrorContains(t, err, `reserved key "constructor"`)
	})

	t.Run("rejects a repeated key", func(t *testing.T) {
		_, err := checkStructuredOutput(`{"n":1,"note":"a","note":"b"}`, schema, 1024)
		require.ErrorContains(t, err, `repeats the key "note"`)
		require.NotErrorIs(t, err, errStructuredOutputRedacted)
	})

	t.Run("redaction that breaks the schema is not correctable", func(t *testing.T) {
		pattern := json.RawMessage(`{"type":"object","properties":{"key":{"type":"string","pattern":"^AKIA[A-Z0-9]{16}$"}}}`)
		_, err := checkStructuredOutput(`{"key":"AKIAIOSFODNN7EXAMPLE"}`, pattern, 1024)
		require.ErrorIs(t, err, errStructuredOutputRedacted)
		require.NotContains(t, err.Error(), "AKIAIOSFODNN7EXAMPLE", "the error must not carry the credential")
	})

	t.Run("keys that collide after redaction are not correctable", func(t *testing.T) {
		_, err := checkStructuredOutput(`{"AKIAIOSFODNN7EXAMPLE":1,"AKIAIOSFODNN7EXAMPL2":2}`, json.RawMessage(`{"type":"object"}`), 1024)
		require.ErrorIs(t, err, errStructuredOutputRedacted)
		require.ErrorContains(t, err, "two keys became")
	})

	t.Run("violation lists pointers without header", func(t *testing.T) {
		_, err := checkStructuredOutput(`{"n":"one","extra":true}`, schema, 1024)
		require.Error(t, err)
		msg := err.Error()
		require.True(t, strings.HasPrefix(msg, "the JSON document does not satisfy the schema:\n- at "), "violation = %q", msg)
		require.NotContains(t, msg, "urn:stirrup")
		require.NotContains(t, msg, "jsonschema validation failed")
	})

	t.Run("size limit applies to the compact document", func(t *testing.T) {
		_, err := checkStructuredOutput("{\n\n\n\"n\": 1\n\n\n}", schema, 7)
		require.NoError(t, err, "the 7-byte compact document should be accepted")
		_, err = checkStructuredOutput(`{"n":1}`, schema, 6)
		require.ErrorContains(t, err, "over the 6-byte limit")
	})
}

func TestDuplicateKey_ReportsRepeatedObjectKeys(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		wantKey string
	}{
		{name: "same key in different objects", doc: `{"a":1,"b":{"a":2},"c":[{"a":3},{"a":4}]}`},
		{name: "repeated top-level key", doc: `{"a":1,"b":2,"a":3}`, wantKey: "a"},
		{name: "repeated key in nested object", doc: `{"o":{"x":[1,{"y":1}],"x":true}}`, wantKey: "x"},
		{name: "repeated strings in an array", doc: `["a","a",{"k":"a"}]`},
		{name: "bare string", doc: `"a"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key, ok := duplicateKey([]byte(tc.doc))
			require.Equal(t, tc.wantKey, key)
			require.Equal(t, tc.wantKey != "", ok)
		})
	}
}

func TestScrubJSONStrings_RedactsCredentialsKeepsLiterals(t *testing.T) {
	in := []byte(`{"key":"AKIAIOSFODNN7EXAMPLE","esc":"quote \" and \\ back","list":["AKIAIOSFODNN7EXAMPLE",1],"AKIAIOSFODNN7EXAMPLE":true}`)
	got := scrubJSONStrings(in)
	assert.NotContains(t, string(got), "AKIAIOSFODNN7EXAMPLE", "every credential occurrence should be redacted")
	require.True(t, json.Valid(got), "scrubbed = %s is not valid JSON", got)
	assert.Contains(t, string(got), `"esc":"quote \" and \\ back"`, "unchanged literals should be kept byte for byte")

	clean := []byte(`{"a":"plain","b":[true,null,1.50]}`)
	assert.Equal(t, string(clean), string(scrubJSONStrings(clean)), "a clean document should be unchanged")
}

func TestStructuredOutputViolation_IsScrubbedAndCapped(t *testing.T) {
	got := structuredOutputViolation(errors.New("bad value AKIAIOSFODNN7EXAMPLE " + strings.Repeat("y", 4096)))
	assert.NotContains(t, got, "AKIAIOSFODNN7EXAMPLE", "the violation must not carry the credential")
	assert.LessOrEqual(t, len(got), maxStructuredOutputViolationBytes+64, "the violation should be capped near the limit")
}

func TestStructuredOutputLimit_DefaultsAndCapsOperatorLimit(t *testing.T) {
	tests := []struct {
		name string
		sink *types.ResultSinkConfig
		want int
	}{
		{name: "default", sink: nil, want: types.DefaultMaxFinalAssistantTextBytes},
		{name: "operator limit below the cap", sink: &types.ResultSinkConfig{Type: "stdout-json", MaxFinalAssistantTextBytes: 4096}, want: 4096},
		{name: "operator limit above the cap", sink: &types.ResultSinkConfig{Type: "none", MaxFinalAssistantTextBytes: 1 << 20}, want: maxStructuredOutputBytes},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			config := structuredTestConfig()
			config.ResultSink = tc.sink
			require.Equal(t, tc.want, structuredOutputLimit(config))
		})
	}
}

func TestValidateOutputSchema_AcceptsCompilableAndRejectsUncompilable(t *testing.T) {
	t.Run("no schema", func(t *testing.T) {
		require.NoError(t, ValidateOutputSchema(buildTestConfig()))
	})
	t.Run("valid schema", func(t *testing.T) {
		require.NoError(t, ValidateOutputSchema(structuredTestConfig()))
	})
	t.Run("uncompilable schema", func(t *testing.T) {
		config := buildTestConfig()
		config.OutputSchema = json.RawMessage(`{"type":"strng"}`)
		err := ValidateOutputSchema(config)
		require.Error(t, err)
		require.True(t, strings.HasPrefix(err.Error(), "outputSchema: failed to compile schema"), "error = %v", err)
	})
}

func TestBuildLoopWithTransport_RejectsUncompilableOutputSchema(t *testing.T) {
	config := buildTestConfig()
	config.Executor.Workspace = t.TempDir()
	config.OutputSchema = json.RawMessage(`{"type":"object","required":"title"}`)

	_, err := BuildLoopWithTransport(context.Background(), config, nil)
	require.ErrorContains(t, err, "config validation: outputSchema: failed to compile schema")
}

func TestPreflight_UncompilableOutputSchemaFailsValidation(t *testing.T) {
	cfg := preflightTestConfig(t, "http://127.0.0.1:1/v1")
	cfg.OutputSchema = json.RawMessage(`{"type":"strng"}`)

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	require.NoError(t, err)
	st, _ := stepStatus(report, "config-validation")
	require.Equal(t, PreflightFail, st)
	require.False(t, report.OK)
}
