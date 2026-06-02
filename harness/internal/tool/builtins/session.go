package builtins

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
)

// SessionController is the interface the start-and-detach session tools use
// to drive the run's SessionManager. It is declared here, returning JSON
// and primitives rather than core types, so the builtins package does not
// import core (which would be a cycle — core registers these tools). The
// factory wires a core-backed adapter (issue #71).
type SessionController interface {
	// Start dispatches a detached sub-agent and returns its session id.
	Start(ctx context.Context, prompt, mode string, maxTurns int) (sessionID string, err error)
	// Status returns a non-blocking snapshot of a session as JSON.
	Status(sessionID string) (json.RawMessage, error)
	// Wait blocks until the session is terminal, the timeout elapses, or
	// the context is cancelled, then returns the snapshot as JSON. A
	// non-positive timeout selects the configured default.
	Wait(ctx context.Context, sessionID string, timeoutSeconds int) (json.RawMessage, error)
	// Terminate stops a session and returns the resulting snapshot as JSON.
	Terminate(sessionID string) (json.RawMessage, error)
}

var startSessionSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"prompt": {
			"type": "string",
			"description": "The task for the detached sub-agent to perform. Be specific and self-contained — the sub-agent has no access to the parent conversation history."
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

var sessionIDSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"session_id": {
			"type": "string",
			"description": "The session id returned by start_session."
		}
	},
	"required": ["session_id"],
	"additionalProperties": false
}`)

var waitSessionSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"session_id": {
			"type": "string",
			"description": "The session id returned by start_session."
		},
		"timeout_seconds": {
			"type": "integer",
			"description": "Maximum seconds to block waiting for the session to finish. When the timeout elapses the session keeps running and the returned state is still \"running\"; call wait_session or check_session again later. Defaults to the run's configured session timeout.",
			"minimum": 1
		}
	},
	"required": ["session_id"],
	"additionalProperties": false
}`)

// StartSessionTool returns the start_session tool: it dispatches a sub-agent
// the same way spawn_agent does but returns immediately with a session id
// instead of blocking on the result. Use it to fan out investigative
// threads ("whilst I'm here" work) and collect their findings later with
// wait_session / check_session, or to kick off a thread and forget it.
func StartSessionTool(ctrl SessionController) *tool.Tool {
	return &tool.Tool{
		Name: "start_session",
		Description: "Start a detached sub-agent session and return immediately with a session id, instead of blocking on the result like spawn_agent does. " +
			"Use this to kick off independent investigative threads, parallel exploration, or 'whilst I'm here' work that you can collect later — call wait_session to block for a result, check_session to poll without blocking, or terminate_session to stop it. You may also simply forget a session you no longer care about. " +
			"The sub-agent shares the workspace and inherits the parent's permission policy, security GuardRail, and code scanner — detaching is a high-privilege delegation, not a sandbox. " +
			"The prompt must be specific and self-contained because the sub-agent cannot see the parent's history. mode defaults to the parent's mode; max_turns bounds the sub-agent at 1-20 turns (default 10). " +
			"Returns {\"sessionId\": \"...\", \"state\": \"running\"}. " +
			"Example: {\"prompt\": \"Audit every call site of RunConfig.Redact and report any that log the raw config.\", \"mode\": \"research\", \"max_turns\": 8}",
		InputExamples:     []json.RawMessage{json.RawMessage(`{"prompt": "Audit every call site of RunConfig.Redact and report any that log the raw config.", "mode": "research", "max_turns": 8}`)},
		InputSchema:       startSessionSchema,
		WorkspaceMutating: false,
		// start_session is the budget-consuming operation in the session
		// family (it spawns), so it carries the same upstream-approval gate
		// as spawn_agent. check/wait/terminate manage an already-approved
		// session and are not separately gated.
		RequiresApproval: true,
		Handler: func(ctx context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Prompt   string `json:"prompt"`
				Mode     string `json:"mode"`
				MaxTurns int    `json:"max_turns"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("parse start_session input: %w", err)
			}
			id, err := ctrl.Start(ctx, params.Prompt, params.Mode, params.MaxTurns)
			if err != nil {
				return "", err
			}
			out, err := json.Marshal(struct {
				SessionID string `json:"sessionId"`
				State     string `json:"state"`
			}{SessionID: id, State: "running"})
			if err != nil {
				return "", fmt.Errorf("marshal start_session result: %w", err)
			}
			return string(out), nil
		},
	}
}

// CheckSessionTool returns the check_session tool: a non-blocking poll of a
// session's state. Returns running while in flight, or a terminal state
// (done / error / terminated) with the sub-agent's result once finished.
func CheckSessionTool(ctrl SessionController) *tool.Tool {
	return &tool.Tool{
		Name: "check_session",
		Description: "Check the status of a detached session started with start_session, without blocking. " +
			"Returns the current state — \"running\" while the sub-agent is still working, or a terminal state (\"done\", \"error\", \"terminated\") with the sub-agent's result once it has finished. An unknown session id returns state \"not_found\". " +
			"Use this to poll progress while you do other work; use wait_session when you are ready to block for the result. " +
			"Example: {\"session_id\": \"session-3\"}",
		InputExamples:     []json.RawMessage{json.RawMessage(`{"session_id": "session-3"}`)},
		InputSchema:       sessionIDSchema,
		WorkspaceMutating: false,
		RequiresApproval:  false,
		Handler: func(_ context.Context, input json.RawMessage) (string, error) {
			id, err := parseSessionID(input, "check_session")
			if err != nil {
				return "", err
			}
			status, err := ctrl.Status(id)
			if err != nil {
				return "", err
			}
			return string(status), nil
		},
	}
}

// WaitSessionTool returns the wait_session tool: block until a session
// finishes (or the timeout elapses) and return its result.
func WaitSessionTool(ctrl SessionController) *tool.Tool {
	return &tool.Tool{
		Name: "wait_session",
		Description: "Block until a detached session started with start_session reaches a terminal state, then return its result. " +
			"If the optional timeout_seconds elapses first, the session keeps running and the returned state is still \"running\" — call wait_session again to keep waiting, or move on. An unknown session id returns state \"not_found\". " +
			"Use this to collect a result you previously kicked off; use spawn_agent instead when you want to delegate and block in a single step. " +
			"Example: {\"session_id\": \"session-3\", \"timeout_seconds\": 120}",
		InputExamples:     []json.RawMessage{json.RawMessage(`{"session_id": "session-3", "timeout_seconds": 120}`)},
		InputSchema:       waitSessionSchema,
		WorkspaceMutating: false,
		RequiresApproval:  false,
		Handler: func(ctx context.Context, input json.RawMessage) (string, error) {
			var params struct {
				SessionID      string `json:"session_id"`
				TimeoutSeconds int    `json:"timeout_seconds"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("parse wait_session input: %w", err)
			}
			if params.SessionID == "" {
				return "", fmt.Errorf("wait_session requires a session_id")
			}
			status, err := ctrl.Wait(ctx, params.SessionID, params.TimeoutSeconds)
			if err != nil {
				return "", err
			}
			return string(status), nil
		},
	}
}

// TerminateSessionTool returns the terminate_session tool: stop a detached
// session and discard its in-flight work.
func TerminateSessionTool(ctrl SessionController) *tool.Tool {
	return &tool.Tool{
		Name: "terminate_session",
		Description: "Terminate a detached session started with start_session, stopping the sub-agent and discarding its in-flight work. " +
			"Returns the session with state \"terminated\". An unknown session id is an error. " +
			"Use this to cancel a thread you no longer need; a session you simply stop referencing is also fine to leave running until the run ends. " +
			"Example: {\"session_id\": \"session-3\"}",
		InputExamples:     []json.RawMessage{json.RawMessage(`{"session_id": "session-3"}`)},
		InputSchema:       sessionIDSchema,
		WorkspaceMutating: false,
		RequiresApproval:  false,
		Handler: func(_ context.Context, input json.RawMessage) (string, error) {
			id, err := parseSessionID(input, "terminate_session")
			if err != nil {
				return "", err
			}
			status, err := ctrl.Terminate(id)
			if err != nil {
				return "", err
			}
			return string(status), nil
		},
	}
}

// parseSessionID unmarshals the {session_id} input shared by check_session
// and terminate_session.
func parseSessionID(input json.RawMessage, toolName string) (string, error) {
	var params struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("parse %s input: %w", toolName, err)
	}
	if params.SessionID == "" {
		return "", fmt.Errorf("%s requires a session_id", toolName)
	}
	return params.SessionID, nil
}
