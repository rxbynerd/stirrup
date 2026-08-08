package builtins

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
)

// spawnAgentSchema is the JSON Schema for the spawn_agent tool input.
var spawnAgentSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"prompt": {
			"type": "string",
			"description": "The task for the sub-agent to perform. Be specific and self-contained — the sub-agent has no access to the parent conversation history."
		},
		"mode": {
			"type": "string",
			"description": "Run mode for the sub-agent. Defaults to the parent's mode.",
			"enum": ["execution", "planning", "review", "research", "toil"]
		},
		"max_turns": {
			"type": "integer",
			"description": "Maximum turns for the sub-agent (1-20, defaults to 10).",
			"minimum": 1,
			"maximum": 20
		}
	},
	"required": ["prompt"],
	"additionalProperties": false
}`)

// SubAgentSpawner runs a sub-agent. It decouples the tool from the core
// package, avoiding a circular import; core.SpawnSubAgent satisfies it via
// a closure in the factory.
type SubAgentSpawner func(ctx context.Context, prompt, mode string, maxTurns int) (json.RawMessage, error)

// SpawnAgentTool returns a tool that spawns a sub-agent to handle a subtask.
// The spawner function is provided by the factory and encapsulates the
// reference to the parent AgenticLoop and RunConfig.
func SpawnAgentTool(spawner SubAgentSpawner) *tool.Tool {
	return &tool.Tool{
		Name: "spawn_agent",
		Description: "Delegate a self-contained subtask to a fresh sub-agent. The sub-agent shares the workspace and tools but runs with its own conversation history and returns a single JSON-encoded result. " +
			"The sub-agent inherits the parent's permission policy, security GuardRail, and code scanner — spawning is a high-privilege delegation, not a sandbox. " +
			"Use this for work that benefits from a clean context (broad exploration, parallel-feel investigation, a bounded refactor) or that would otherwise pollute the parent conversation. " +
			"Do not use for trivial one-tool-call tasks — the spawn overhead outweighs the benefit. The prompt must be specific and self-contained because the sub-agent cannot see the parent's history. " +
			"mode defaults to the parent's mode; max_turns bounds the sub-agent at 1-20 turns (default 10). " +
			"Example: {\"prompt\": \"Find every call site of harness.RunConfig.Redact and list them as path:line.\", \"mode\": \"research\", \"max_turns\": 8}",
		InputExamples: []json.RawMessage{json.RawMessage(`{"prompt": "Find every call site of harness.RunConfig.Redact and list them as path:line.", "mode": "research", "max_turns": 8}`)},
		InputSchema:   spawnAgentSchema,
		// The sub-agent is gated by its own permission policy; approval
		// is required here because spawning consumes additional budget.
		WorkspaceMutating: false,
		RequiresApproval:  true,
		Handler: func(ctx context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Prompt   string `json:"prompt"`
				Mode     string `json:"mode"`
				MaxTurns int    `json:"max_turns"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("parse spawn_agent input: %w", err)
			}

			resultJSON, err := spawner(ctx, params.Prompt, params.Mode, params.MaxTurns)
			if err != nil {
				return "", err
			}
			return string(resultJSON), nil
		},
	}
}
