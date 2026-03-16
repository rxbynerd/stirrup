package builtins

import (
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
)

// RegisterBuiltins registers all built-in tools with the given registry.
// The executor is used for filesystem and command execution tools; web_fetch
// manages its own HTTP client independently.
func RegisterBuiltins(registry *tool.Registry, exec executor.Executor) {
	registry.Register(ReadFileTool(exec))
	registry.Register(WriteFileTool(exec))
	registry.Register(ListDirectoryTool(exec))
	registry.Register(SearchFilesTool(exec))
	registry.Register(RunCommandTool(exec))
	registry.Register(WebFetchTool())
}
