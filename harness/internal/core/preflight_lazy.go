package core

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/rxbynerd/stirrup/harness/internal/commandoutput"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

// mcpDiscovery records the MCP tools a dry-run probe saw, by server name.
type mcpDiscovery struct {
	tools map[string][]string
	// unprobed names the servers whose tools are unknown: probe skipped or
	// no answer.
	unprobed []string
	// incomplete explains why the tool set may be missing MCP tools. Empty
	// when every configured server was probed.
	incomplete string
}

// unprobedTool reports whether name is an MCP tool name of a server whose
// tools the probe did not see.
func (d mcpDiscovery) unprobedTool(name string) bool {
	for _, server := range d.unprobed {
		if strings.HasPrefix(name, "mcp_"+server+"_") {
			return true
		}
	}
	return false
}

// containerToolCapabilities are the container executor capabilities that
// tool registration reads.
func containerToolCapabilities() executor.ExecutorCapabilities {
	return executor.ExecutorCapabilities{CanRead: true, CanWrite: true, CanExec: true}
}

// capabilityOnlyExecutor stands in for an executor a dry-run does not
// construct. Tool registration reads only Capabilities; no tool runs.
type capabilityOnlyExecutor struct {
	executor.Executor
	caps executor.ExecutorCapabilities
}

func (e capabilityOnlyExecutor) Capabilities() executor.ExecutorCapabilities {
	return e.caps
}

// preflightLazyLoader builds the lazy loader a real run builds for config:
// the same buildToolRegistry, spawn_agent registration, profile presenter
// and loader options as BuildLoop, over the MCP tools the probe saw.
func preflightLazyLoader(config *types.RunConfig, exec executor.Executor, mcp mcpDiscovery) (*tool.LazyLoader, error) {
	if isNilComponent(exec) {
		exec = capabilityOnlyExecutor{caps: containerToolCapabilities()}
	}
	var outputStore *commandoutput.Store
	if commandOutputStoreWanted(config.Tools) {
		outputStore = &commandoutput.Store{}
	}
	registry := buildToolRegistry(exec, buildEditStrategy(config.EditStrategy), config.Tools, outputStore)
	for _, server := range config.Tools.MCPServers {
		for _, name := range mcp.tools[server.Name] {
			registry.Register(&tool.Tool{Name: name, MCPServer: server.Name})
		}
	}
	registerSpawnAgent(registry, config.Tools, nil)

	profile, ok := tool.ProfileFor(config.Tools.Profile)
	if !ok {
		return nil, fmt.Errorf("tools.profile %q has no presentation table", config.Tools.Profile)
	}
	presenter, err := tool.NewPresenter(registry, profile)
	if err != nil {
		return nil, fmt.Errorf("build tool profile presenter: %w", err)
	}
	return tool.NewLazyLoader(presenter, config.Tools.LazyLoading, lazyLoaderOptions(config.Tools)), nil
}

// preflightLazyTools reports what tools.lazyLoading sends and defers, the
// tool search each routed provider and model uses, and every eager name
// that matches no tool. It never fails the report: a real run only warns
// about the same conditions.
func preflightLazyTools(
	ctx context.Context,
	config *types.RunConfig,
	exec executor.Executor,
	mcp mcpDiscovery,
	ok func(string, string),
	skip func(string, string),
	warn func(string, error),
) {
	const step = "lazy-tools"
	if !config.Tools.LazyLoading.Enabled {
		skip(step, "tools.lazyLoading.enabled is false")
		return
	}
	loader, err := preflightLazyLoader(config, exec, mcp)
	if err != nil {
		warn(step, err)
		return
	}

	counts := loader.GroupCounts()
	eager, deferred := 0, 0
	for _, c := range counts {
		eager += c.Eager
		deferred += c.Deferred
	}
	parts := []string{
		"strategy " + loader.Strategy(),
		fmt.Sprintf("%d eager, %d deferred", eager, deferred),
	}
	for _, c := range counts {
		parts = append(parts, fmt.Sprintf("%s: %d eager, %d deferred", c.Group.Label(), c.Eager, c.Deferred))
	}
	if mcp.incomplete != "" {
		parts = append(parts, "MCP tools are discovered at run start and not counted ("+mcp.incomplete+")")
	}
	switch {
	case loader.HasDeferred() || mcp.incomplete != "":
		if !loader.HasDeferred() {
			parts = append(parts, "whether any tool is deferred is decided at run start once MCP tools are known")
		}
		support := buildToolSearchSupport(config, quirks.DefaultRegistry(), slog.New(slog.DiscardHandler))
		for _, sel := range router.Candidates(ctx, buildRouter(config.ModelRouter, config.Provider.Type), config.Mode) {
			parts = append(parts, fmt.Sprintf("%s/%s searches with %s", sel.Provider, sel.Model, describeToolSearch(config, loader, support, sel)))
		}
	default:
		parts = append(parts, "nothing is deferred, so every tool is sent")
	}
	ok(step, strings.Join(parts, "; "))

	for _, name := range loader.UnknownEager() {
		if mcp.unprobedTool(name) {
			continue
		}
		warn(step+":eager:"+name, fmt.Errorf("tools.lazyLoading.eager names %q, which is not a registered tool; it is ignored", name))
	}
}

// describeToolSearch names the tool search a turn routed to sel offers
// under the loader's strategy, and why it is not the native one.
func describeToolSearch(config *types.RunConfig, loader *tool.LazyLoader, support ToolSearchSupport, sel router.ModelSelection) string {
	if loader.Strategy() == types.LazyToolStrategyHarness {
		return "the harness tool_search (strategy harness)"
	}
	capability := support(sel.Provider, sel.Model)
	if search := lazySearchFor(loader, capability); search.native {
		return "the native " + capability.Variant + " tool search"
	}
	switch {
	case !capability.Supported && hasCustomResponsesEndpoint(providerConfigFor(config, sel.Provider)):
		return "the harness tool_search (custom baseUrl)"
	case !capability.Supported:
		return "the harness tool_search (model not in the native tool search allow-list)"
	case !loader.HasDeferred():
		return "the native " + capability.Variant + " tool search once a tool is deferred"
	default:
		return "the harness tool_search (no eager tool besides tool_search)"
	}
}
