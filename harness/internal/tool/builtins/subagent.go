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

// SubAgentSpawner is the interface that SpawnAgentTool uses to run sub-agents.
// This decouples the tool from the core package, avoiding a circular import.
// The core.SpawnSubAgent function satisfies this interface shape via a closure
// in the factory.
type SubAgentSpawner func(ctx context.Context, prompt, mode string, maxTurns int) (json.RawMessage, error)

// SpawnAgentTool returns a tool that spawns a sub-agent to handle a subtask.
// The spawner function is provided by the factory and encapsulates the
// reference to the parent AgenticLoop and RunConfig.
func SpawnAgentTool(spawner SubAgentSpawner) *tool.Tool {
	return &tool.Tool{
		Name:        "spawn_agent",
		Description: "Spawn a sub-agent to handle a subtask. The sub-agent has access to the same workspace and tools but runs independently with its own conversation history. Use for tasks that benefit from a fresh context or that can be performed independently.",
		InputSchema: spawnAgentSchema,
		// The spawn_agent tool itself does not mutate the workspace —
		// the sub-agent it launches is gated by its own permission
		// policy. We do however require upstream approval because
		// spawning an agent consumes additional model budget and the
		// operator may want to opt in to that.
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
