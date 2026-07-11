package builtins

import (
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
)

// registerAllForTest registers every built-in tool constructor with the
// given registry. It exists solely so this package's own tests (e.g.
// TestBuiltinDescriptions_EnrichedShape, which walks every tool's
// description) can build a "has everything" registry without hand-listing
// constructors.
//
// This is NOT production wiring. The factory's buildToolRegistry
// (harness/internal/core/factory.go) is what the agentic loop actually
// uses: it gates each tool on toolEnabled(cfg.Tools.BuiltIn, ...) and the
// executor's capabilities. This helper used to be exported as
// RegisterBuiltins and called only from tests, which let it silently drift
// out of sync with buildToolRegistry — the git_* tools were wired here but
// never reached the live registry (#448). Living in a _test.go file now
// makes that divergence structurally impossible: this function cannot be
// referenced from production code because it does not exist in a
// non-test build.
//
// Cross-references inside tool descriptions (e.g. grep_files mentioning
// find_files, list_directory mentioning find_files, edit_file mentioning
// write_file) use the canonical registry tool name. Wave 5 #234 aliases
// must NOT rename these references — descriptions resolve via the
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
