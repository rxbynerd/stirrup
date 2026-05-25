package provider

import (
	"encoding/json"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// These tests pin the wire behaviour of the #222 reliability controls
// (parallel-tool-call policy and input examples) per provider, gated on the
// resolved quirks capability. They exercise the builders directly — no live
// calls — mirroring the existing *_builder_test.go contract pattern.

func toolWith222Example() types.ToolDefinition {
	return types.ToolDefinition{
		Name:        "demo",
		Description: "Demo tool. Example: {\"x\": \"hi\"}",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}`),
		Presentation: &types.ToolPresentation{
			InputExamples: []json.RawMessage{json.RawMessage(`{"x":"hi"}`)},
		},
	}
}

func userTurn() []types.Message {
	return []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "go"}}}}
}

// decodeObject unmarshals raw JSON into a string-keyed map, failing the test
// on error. Used to walk a wire body without committing to a typed shape.
func decodeObject(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode object: %v\nraw: %s", err, raw)
	}
	return m
}

func schemaHasExamples(t *testing.T, schema json.RawMessage) bool {
	t.Helper()
	_, ok := decodeObject(t, schema)["examples"]
	return ok
}

func TestOpenAIChat_222_ParallelAndExamples_NonStrict(t *testing.T) {
	disable := false
	params := types.StreamParams{
		Model:             "gpt-4o", // non-strict
		Messages:          userTurn(),
		Tools:             []types.ToolDefinition{toolWith222Example()},
		MaxTokens:         100,
		ParallelToolCalls: &disable,
	}
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	req, err := buildOpenAIRequest(params, true, q, nil)
	if err != nil {
		t.Fatalf("buildOpenAIRequest: %v", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	top := decodeObject(t, body)
	if string(top["parallel_tool_calls"]) != "false" {
		t.Errorf("parallel_tool_calls = %s, want false", top["parallel_tool_calls"])
	}
	// tools[0].function.parameters must carry the folded examples.
	var tools []json.RawMessage
	if err := json.Unmarshal(top["tools"], &tools); err != nil {
		t.Fatalf("tools: %v", err)
	}
	fn := decodeObject(t, decodeObject(t, tools[0])["function"])
	if !schemaHasExamples(t, fn["parameters"]) {
		t.Errorf("non-strict tool schema is missing the folded examples keyword: %s", fn["parameters"])
	}
}

func TestOpenAIChat_222_StrictModelOmitsExamples(t *testing.T) {
	params := types.StreamParams{
		Model:     "gpt-5", // strict-mode model
		Messages:  userTurn(),
		Tools:     []types.ToolDefinition{toolWith222Example()},
		MaxTokens: 100,
	}
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	req, err := buildOpenAIRequest(params, true, q, newStrictSchemaCache())
	if err != nil {
		t.Fatalf("buildOpenAIRequest: %v", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(decodeObject(t, body)["tools"], &tools); err != nil {
		t.Fatalf("tools: %v", err)
	}
	fn := decodeObject(t, tools[0])
	if string(fn["function"]) == "" {
		t.Fatalf("tool has no function entry: %s", tools[0])
	}
	fnObj := decodeObject(t, fn["function"])
	if string(fnObj["strict"]) != "true" {
		t.Errorf("strict model should emit strict:true, got %s", fnObj["strict"])
	}
	// The structured-outputs subset rejects `examples`, so a strict tool must
	// NOT carry it; the description text remains the example carrier.
	if schemaHasExamples(t, fnObj["parameters"]) {
		t.Errorf("strict tool schema must not carry examples: %s", fnObj["parameters"])
	}
}

func TestOpenAIChat_222_DefaultsOmitParallel(t *testing.T) {
	params := types.StreamParams{
		Model:     "gpt-4o",
		Messages:  userTurn(),
		Tools:     []types.ToolDefinition{toolWith222Example()},
		MaxTokens: 100,
		// ParallelToolCalls unset.
	}
	q := quirks.DefaultRegistry().Resolve("openai-compatible", params.Model)
	req, err := buildOpenAIRequest(params, true, q, nil)
	if err != nil {
		t.Fatalf("buildOpenAIRequest: %v", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, ok := decodeObject(t, body)["parallel_tool_calls"]; ok {
		t.Error("parallel_tool_calls must be omitted when the caller did not set it")
	}
}

func TestResponses_222_ParallelAndExamples(t *testing.T) {
	enable := true
	params := types.StreamParams{
		Model:             "gpt-4o",
		Messages:          userTurn(),
		Tools:             []types.ToolDefinition{toolWith222Example()},
		MaxTokens:         100,
		ParallelToolCalls: &enable,
	}
	q := quirks.DefaultRegistry().Resolve("openai-responses", params.Model)
	req, err := buildResponsesRequest(params, q, nil)
	if err != nil {
		t.Fatalf("buildResponsesRequest: %v", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	top := decodeObject(t, body)
	if string(top["parallel_tool_calls"]) != "true" {
		t.Errorf("parallel_tool_calls = %s, want true", top["parallel_tool_calls"])
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(top["tools"], &tools); err != nil {
		t.Fatalf("tools: %v", err)
	}
	// Responses tools are flat: parameters sits directly on the entry.
	if !schemaHasExamples(t, decodeObject(t, tools[0])["parameters"]) {
		t.Errorf("responses tool schema is missing folded examples: %s", tools[0])
	}
}

func TestAnthropic_222_ExamplesAndDisableParallel(t *testing.T) {
	disable := false
	params := types.StreamParams{
		Model:             "claude-sonnet-4-5",
		Messages:          userTurn(),
		Tools:             []types.ToolDefinition{toolWith222Example()},
		MaxTokens:         100,
		ParallelToolCalls: &disable, // request no-parallel
	}
	q := quirks.DefaultRegistry().Resolve("anthropic", params.Model)
	body, err := json.Marshal(buildAnthropicRequest(params, true, q))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	top := decodeObject(t, body)

	// disable_parallel_tool_use rides on a synthesised auto tool_choice.
	tc := decodeObject(t, top["tool_choice"])
	if string(tc["type"]) != `"auto"` {
		t.Errorf("tool_choice.type = %s, want \"auto\"", tc["type"])
	}
	if string(tc["disable_parallel_tool_use"]) != "true" {
		t.Errorf("tool_choice.disable_parallel_tool_use = %s, want true", tc["disable_parallel_tool_use"])
	}

	// examples fold into input_schema (Anthropic has no strict subset).
	var tools []json.RawMessage
	if err := json.Unmarshal(top["tools"], &tools); err != nil {
		t.Fatalf("tools: %v", err)
	}
	if !schemaHasExamples(t, decodeObject(t, tools[0])["input_schema"]) {
		t.Errorf("anthropic tool input_schema is missing folded examples: %s", tools[0])
	}
}

func TestAnthropic_222_ParallelEnabledIsNoOp(t *testing.T) {
	enable := true
	params := types.StreamParams{
		Model:             "claude-sonnet-4-5",
		Messages:          userTurn(),
		Tools:             []types.ToolDefinition{toolWith222Example()},
		MaxTokens:         100,
		ParallelToolCalls: &enable, // parallel is the default; nothing to emit
	}
	q := quirks.DefaultRegistry().Resolve("anthropic", params.Model)
	body, err := json.Marshal(buildAnthropicRequest(params, true, q))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, ok := decodeObject(t, body)["tool_choice"]; ok {
		t.Error("enabling parallel (the Anthropic default) must not synthesise a tool_choice object")
	}
}

func TestAnthropic_222_RequiredChoiceCarriesDisableParallel(t *testing.T) {
	disable := false
	params := types.StreamParams{
		Model:             "claude-sonnet-4-5",
		Messages:          userTurn(),
		Tools:             []types.ToolDefinition{toolWith222Example()},
		MaxTokens:         100,
		ToolChoice:        types.ToolChoiceRequired,
		ParallelToolCalls: &disable,
	}
	q := quirks.DefaultRegistry().Resolve("anthropic", params.Model)
	body, err := json.Marshal(buildAnthropicRequest(params, true, q))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	tc := decodeObject(t, decodeObject(t, body)["tool_choice"])
	if string(tc["type"]) != `"any"` {
		t.Errorf("tool_choice.type = %s, want \"any\" (required)", tc["type"])
	}
	if string(tc["disable_parallel_tool_use"]) != "true" {
		t.Errorf("required tool_choice should still carry disable_parallel_tool_use, got %s", tc["disable_parallel_tool_use"])
	}
}

func TestGemini_222_NoParallelNoExamples(t *testing.T) {
	disable := false
	params := types.StreamParams{
		Model:             "gemini-2.5-pro",
		Messages:          userTurn(),
		Tools:             []types.ToolDefinition{toolWith222Example()},
		MaxTokens:         100,
		ParallelToolCalls: &disable, // unsupported on Gemini → no-op
	}
	q := quirks.DefaultRegistry().Resolve("gemini", params.Model)
	body, _, err := BuildGenerateContentRequest(params, nil, q)
	if err != nil {
		t.Fatalf("BuildGenerateContentRequest: %v", err)
	}
	top := decodeObject(t, body)
	for _, k := range []string{"parallel_tool_calls", "parallelToolCalls"} {
		if _, ok := top[k]; ok {
			t.Errorf("Gemini request must not carry %q (no native control)", k)
		}
	}
	// Gemini's ToolExamples capability is zero, so examples are never folded
	// into the function-declaration schema; the description text carries them.
	var tools []json.RawMessage
	if err := json.Unmarshal(top["tools"], &tools); err != nil {
		t.Fatalf("tools: %v", err)
	}
	decls := decodeObject(t, tools[0])
	var fnDecls []json.RawMessage
	if err := json.Unmarshal(decls["functionDeclarations"], &fnDecls); err != nil {
		t.Fatalf("functionDeclarations: %v", err)
	}
	params0 := decodeObject(t, fnDecls[0])["parameters"]
	if schemaHasExamples(t, params0) {
		t.Errorf("Gemini tool schema must not carry examples: %s", params0)
	}
}
