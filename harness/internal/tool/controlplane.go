package tool

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// ControlPlaneTool builds the async Tool for one tools.controlPlane entry.
// The preflight does no work: the loop emits tool_result_request with the
// validated input and the control plane supplies the result. The tool is
// never WorkspaceMutating, so read-only modes admit it.
func ControlPlaneTool(cfg types.ControlPlaneToolConfig) *Tool {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	return &Tool{
		Name:             cfg.Name,
		Description:      cfg.Description,
		InputSchema:      cfg.InputSchema,
		RequiresApproval: cfg.RequiresApproval,
		AsyncHandler: func(_ context.Context, _ json.RawMessage) (AsyncDispatch, error) {
			return AsyncDispatch{Timeout: timeout}, nil
		},
	}
}
