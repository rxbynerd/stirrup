package builtins

import (
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
)

// RegisterBuiltins registers all built-in tools with the given registry.
// The executor is used for filesystem and command execution tools; web_fetch
// manages its own HTTP client independently. The legacy search_files tool
// is deliberately absent: callers must use grep_files (regex content) or
// find_files (glob filename) instead.
func RegisterBuiltins(registry *tool.Registry, exec executor.Executor) {
	registry.Register(ReadFileTool(exec))
	registry.Register(WriteFileTool(exec))
	registry.Register(ListDirectoryTool(exec))
	registry.Register(GrepFilesTool(exec))
	registry.Register(FindFilesTool(exec))
	registry.Register(RunCommandTool(exec))
	registry.Register(WebFetchTool())
}
