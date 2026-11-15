package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

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
