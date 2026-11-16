package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

// toolErrorSecret is built from fragments so the literal does not trip
// secret scanning on push.
var toolErrorSecret = "ghp_" + strings.Repeat("Zq7", 12)

type erroringPermissions struct{ err error }

func (p erroringPermissions) Check(context.Context, types.ToolDefinition, json.RawMessage) (*permission.PermissionResult, error) {
	return nil, p.err
}

func secretErrorTool(build func(t *tool.Tool)) *tool.Tool {
	t := &tool.Tool{
		Name:        "leaky",
		Description: "test tool whose failures quote a credential",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}
	build(t)
	return t
}

func TestPlanAndDispatch_ScrubsToolErrorText(t *testing.T) {
	leak := errors.New("request failed: Authorization token " + toolErrorSecret + " rejected")
	cases := []struct {
		name  string
		tool  *tool.Tool
		perms permission.PermissionPolicy
	}{
		{
			name: "handler error",
			tool: secretErrorTool(func(t *tool.Tool) {
				t.Handler = func(context.Context, json.RawMessage) (string, error) { return "", leak }
			}),
		},
		{
			name: "structured handler error",
			tool: secretErrorTool(func(t *tool.Tool) {
				t.StructuredHandler = func(context.Context, json.RawMessage) (tool.StructuredResult, error) {
					return tool.StructuredResult{}, leak
				}
			}),
		},
		{
			name: "structured handler error result",
			tool: secretErrorTool(func(t *tool.Tool) {
				t.StructuredHandler = func(context.Context, json.RawMessage) (tool.StructuredResult, error) {
					return tool.StructuredResult{Text: leak.Error(), IsError: true}, nil
				}
			}),
		},
		{
			name: "async preflight error",
			tool: secretErrorTool(func(t *tool.Tool) {
				t.AsyncHandler = func(context.Context, json.RawMessage) (tool.AsyncDispatch, error) {
					return tool.AsyncDispatch{}, leak
				}
			}),
		},
		{
			name: "permission check error",
			tool: secretErrorTool(func(t *tool.Tool) {
				t.RequiresApproval = true
				t.Handler = func(context.Context, json.RawMessage) (string, error) { return "unreachable", nil }
			}),
			perms: erroringPermissions{err: leak},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			tr := newAsyncTestTransport()
			loop := buildAsyncTestLoop(t, tr, tc.tool)
			if tc.perms != nil {
				loop.Permissions = tc.perms
			}
			var secBuf bytes.Buffer
			loop.Security = security.NewSecurityLogger(&secBuf, "run-1")

			result, record := dispatchOne(t, loop, &types.RunConfig{Mode: "execution"}, "leaky")

			must.True(result.IsError, "the call must fail")
			check.NotContains(result.Content, toolErrorSecret, "the model must not see the credential")
			check.Contains(result.Content, "[REDACTED]", "the redaction must stay visible to the model")
			check.Contains(result.Content, "rejected", "the rest of the error must reach the model")
			check.Equal(result.Content, record.Output, "the turn record must carry the scrubbed text")
			check.Equal(result.Content, toolResultEventContent(t, tr), "the tool_result event must carry the scrubbed text")
			check.Contains(secBuf.String(), `"location":"core.tool_error"`, "the redaction must raise a security event")
			check.Contains(secBuf.String(), "github_pat", "the security event must name the pattern")
		})
	}
}

func TestPlanAndDispatch_LeavesSuccessfulToolTextUnscrubbed(t *testing.T) {
	must := require.New(t)
	text := "config holds " + toolErrorSecret
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, secretErrorTool(func(t *tool.Tool) {
		t.Handler = func(context.Context, json.RawMessage) (string, error) { return text, nil }
	}))

	result, _ := dispatchOne(t, loop, &types.RunConfig{Mode: "execution"}, "leaky")

	must.False(result.IsError, "the call must succeed")
	must.Equal(text, result.Content, "a successful result reaches the model as the tool returned it")
}

func TestPlanAndDispatch_DropsFailedStructuredPayloadHoldingASecret(t *testing.T) {
	must := require.New(t)
	payload, err := json.Marshal(map[string]string{"detail": "token " + toolErrorSecret})
	must.NoError(err)
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, secretErrorTool(func(t *tool.Tool) {
		t.StructuredHandler = func(context.Context, json.RawMessage) (tool.StructuredResult, error) {
			return tool.StructuredResult{Text: "lookup failed", Structured: payload, Kind: "command_result", IsError: true}, nil
		}
	}))
	var secBuf bytes.Buffer
	loop.Security = security.NewSecurityLogger(&secBuf, "run-1")

	result, record := dispatchOne(t, loop, &types.RunConfig{Mode: "execution"}, "leaky")

	must.True(result.IsError, "the call must fail")
	must.Equal("lookup failed", result.Content, "text without a match must pass unchanged")
	must.Nil(result.Structured, "a payload holding a match must not reach the model")
	must.Empty(result.Kind, "a dropped payload must take its kind with it")
	must.Nil(record.Structured, "the turn record must drop the payload too")
	must.Contains(secBuf.String(), `"location":"core.tool_error"`, "the redaction must raise a security event")
}

func TestPlanAndDispatch_KeepsFailedStructuredPayloadWithoutASecret(t *testing.T) {
	must := require.New(t)
	payload := json.RawMessage(`{"exit_code":null,"detail":"lookup failed"}`)
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, secretErrorTool(func(t *tool.Tool) {
		t.StructuredHandler = func(context.Context, json.RawMessage) (tool.StructuredResult, error) {
			return tool.StructuredResult{Text: "failed", Structured: payload, Kind: "command_result", IsError: true}, nil
		}
	}))

	result, _ := dispatchOne(t, loop, &types.RunConfig{Mode: "execution"}, "leaky")

	must.True(result.IsError, "the call must fail")
	must.JSONEq(string(payload), string(result.Structured), "a clean payload must reach the model")
	must.Equal("command_result", result.Kind)
}

func TestPlanAndDispatch_AsyncFailuresRaiseRedactionEvents(t *testing.T) {
	t.Run("upstream error", func(t *testing.T) {
		must := require.New(t)
		tr := newAsyncTestTransport()
		loop := buildAsyncTestLoop(t, tr, asyncEchoTool())
		var secBuf bytes.Buffer
		loop.Security = security.NewSecurityLogger(&secBuf, "run-1")
		go func() {
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if id := tr.lastRequestID("tool_result_request"); id != "" {
					yes := true
					tr.FireControl(types.ControlEvent{
						Type:      "tool_result_response",
						RequestID: id,
						Content:   "auth failed for " + toolErrorSecret,
						IsError:   &yes,
					})
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()

		result, _ := dispatchOne(t, loop, &types.RunConfig{Mode: "execution"}, "async_echo")

		must.True(result.IsError, "the call must fail")
		must.NotContains(result.Content, toolErrorSecret, "the model must not see the credential")
		must.Contains(secBuf.String(), `"location":"core.tool_error"`, "the redaction must raise a security event")
	})

	t.Run("panic", func(t *testing.T) {
		must := require.New(t)
		tr := newAsyncTestTransport()
		loop := buildAsyncTestLoop(t, tr, &tool.Tool{
			Name:        "async_panic",
			Description: "test async tool that panics",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			AsyncHandler: func(context.Context, json.RawMessage) (tool.AsyncDispatch, error) {
				panic("lost " + toolErrorSecret)
			},
		})
		var secBuf bytes.Buffer
		loop.Security = security.NewSecurityLogger(&secBuf, "run-1")

		result, _ := dispatchOne(t, loop, &types.RunConfig{Mode: "execution"}, "async_panic")

		must.True(result.IsError, "the call must fail")
		must.Contains(result.Content, "panic", "the result must say the tool panicked")
		must.NotContains(result.Content, toolErrorSecret, "the model must not see the credential")
		must.Contains(secBuf.String(), `"location":"core.tool_error"`, "the redaction must raise a security event")
	})
}

func TestPlanAndDispatch_DropsFailedStructuredPayloadWhenTextHeldASecret(t *testing.T) {
	must := require.New(t)
	payload := json.RawMessage(`{"exit_code":null,"detail":"lookup failed"}`)
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, secretErrorTool(func(t *tool.Tool) {
		t.StructuredHandler = func(context.Context, json.RawMessage) (tool.StructuredResult, error) {
			return tool.StructuredResult{Text: "failed: token " + toolErrorSecret, Structured: payload, Kind: "command_result", IsError: true}, nil
		}
	}))

	result, record := dispatchOne(t, loop, &types.RunConfig{Mode: "execution"}, "leaky")

	must.NotContains(result.Content, toolErrorSecret)
	must.Nil(result.Structured, "a payload that accompanies text holding a match must not reach the model")
	must.Nil(record.Structured, "the turn record must drop the payload too")
}

func TestPlanAndDispatch_RaisesOneRedactionEventPerSecretInTextAndPayload(t *testing.T) {
	must := require.New(t)
	payload, err := json.Marshal(map[string]string{"detail": "token " + toolErrorSecret})
	must.NoError(err)
	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, secretErrorTool(func(t *tool.Tool) {
		t.StructuredHandler = func(context.Context, json.RawMessage) (tool.StructuredResult, error) {
			return tool.StructuredResult{Text: "failed: token " + toolErrorSecret, Structured: payload, Kind: "command_result", IsError: true}, nil
		}
	}))
	var secBuf bytes.Buffer
	loop.Security = security.NewSecurityLogger(&secBuf, "run-1")

	dispatchOne(t, loop, &types.RunConfig{Mode: "execution"}, "leaky")

	must.Equal(1, strings.Count(secBuf.String(), `"location":"core.tool_error"`), "one secret must raise one event")
}

func TestSpawnSubAgent_SetupErrorTextIsScrubbed(t *testing.T) {
	must := require.New(t)
	parentLoop := buildSubAgentTestLoop(&mockProvider{})
	parentLoop.Prompt = &failingPromptBuilder{err: errors.New("template read failed: token " + toolErrorSecret)}

	result, err := SpawnSubAgent(context.Background(), parentLoop, buildTestConfig(), SubAgentConfig{Prompt: "Do a subtask"})

	must.NoError(err)
	must.Equal("error", result.Outcome)
	must.NotContains(result.Output, toolErrorSecret, "the parent model must not see the credential")
	must.Contains(result.Output, "[REDACTED]", "the redaction must stay visible")
}
