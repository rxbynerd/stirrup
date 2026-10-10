// Package prompt defines the PromptBuilder interface and implementations for
// constructing system prompts from mode templates and dynamic context.
package prompt

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
)

// WorkspaceReader is the view of the run's executor that the workspace
// fragments read through, so the file listing and git status come from the
// sandbox rather than the harness host. executor.Executor satisfies it.
type WorkspaceReader interface {
	ListDirectory(ctx context.Context, path string) ([]string, error)
	Exec(ctx context.Context, command string, timeout time.Duration) (*executor.ExecResult, error)
	Capabilities() executor.ExecutorCapabilities
}

// PromptContext provides the information a prompt builder needs.
type PromptContext struct {
	Mode string
	// Workspace is the configured workspace path. Only
	// WorkspacePathFragment uses it; it never names a host directory to
	// read.
	Workspace string
	// Executor is the run's executor. When nil, as for `stirrup prompt
	// render`, the workspace tree and git status fragments render nothing.
	Executor WorkspaceReader
	MaxTurns int
	// Model is the resolved prompt model (RunConfig.EffectivePromptModel):
	// the model identity the system prompt templates render against. It may
	// differ from the wire model when promptBuilder.promptModel is set for
	// a prompt/model comparison run.
	Model          string
	DynamicContext map[string]string
	// OutputSchema is RunConfig.OutputSchema: the JSON Schema the final
	// answer must satisfy. Empty when the run returns free text.
	OutputSchema json.RawMessage

	// RepositoryGuidance is the run's loaded repositoryGuidance files, in
	// render order. Empty when the run configures none.
	RepositoryGuidance []GuidanceDocument
}

// PromptBuilder constructs a system prompt from a PromptContext.
type PromptBuilder interface {
	Build(ctx context.Context, pc PromptContext) (string, error)
}
