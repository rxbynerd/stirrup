package builtins

import (
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
)

// registerAllForTest registers every built-in tool constructor with the
// given registry, so this package's own tests can build a "has everything"
// registry without hand-listing constructors.
//
// This is NOT production wiring — the factory's buildToolRegistry
// (harness/internal/core/factory.go) gates each tool on
// toolEnabled(cfg.Tools.BuiltIn, ...) and the executor's capabilities.
// Living in a _test.go file makes divergence between the two structurally
// impossible: this function cannot be referenced from production code.
//
// Cross-references inside tool descriptions (e.g. grep_files mentioning
// find_files) use the canonical registry tool name; presenter aliases must
// not rename these references, since descriptions resolve via the
// canonical name regardless of which alias the provider surface exposes.
func registerAllForTest(registry *tool.Registry, exec executor.Executor) {
	registry.Register(ReadFileTool(exec))
	registry.Register(WriteFileTool(exec))
	registry.Register(ListDirectoryTool(exec))
	registry.Register(GrepFilesTool(exec))
	registry.Register(FindFilesTool(exec))
	registry.Register(RunCommandTool(exec))
	registry.Register(GitStatusTool(exec))
	registry.Register(GitChangedFilesTool(exec))
	registry.Register(GitDiffTool(exec))
	registry.Register(GitShowTool(exec))
	registry.Register(WebFetchTool())
}
