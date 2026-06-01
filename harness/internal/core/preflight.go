package core

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/executor/egressproxy"
	"github.com/rxbynerd/stirrup/harness/internal/mcp"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/workspaceexport"
	"github.com/rxbynerd/stirrup/types"
)

// Preflighter is the structural interface a component implements to opt
// into a dry-run reachability/authentication probe. Components satisfy it
// WITHOUT importing core — Preflight type-asserts each constructed
// component to this interface and calls Probe when present, or records a
// skip when absent. This keeps the dependency direction one-way (core ->
// components) and preserves the invariant that the agentic loop is a pure
// function of its interfaces: Preflight is a separate entry point that
// never touches loop.go.
//
// Probe must be side-effect-free with respect to the run: it may open a
// network connection or read a file, but it must not spend provider
// tokens, mutate the workspace, or register tools. A nil return means the
// component is reachable and authenticated; a non-nil error becomes a
// failed preflight step carrying the error text.
type Preflighter interface {
	Probe(ctx context.Context) error
}

// PreflightStatus is the outcome of a single preflight step.
type PreflightStatus string

const (
	// PreflightOK marks a step whose probe (or construction) succeeded.
	PreflightOK PreflightStatus = "ok"
	// PreflightSkip marks a step intentionally not run: a component that
	// implements no Probe, or one gated off by a --no-probe-* flag.
	PreflightSkip PreflightStatus = "skip"
	// PreflightFail marks a step whose construction or probe failed.
	PreflightFail PreflightStatus = "fail"
)

// PreflightStep is one line of the preflight report.
type PreflightStep struct {
	Name   string          `json:"name"`
	Status PreflightStatus `json:"status"`
	// Detail carries the success note or the failure's error text.
	Detail string `json:"detail,omitempty"`
	// Hint is an operator-facing remediation suggestion, set only on
	// failures where a concrete next step is known.
	Hint string `json:"hint,omitempty"`
}

// PreflightReport is the structured result of a dry-run. It is
// JSON-serialisable for --output=json. OK is true when no step failed
// (skips do not fail the run).
type PreflightReport struct {
	Steps []PreflightStep `json:"steps"`
	OK    bool            `json:"ok"`
}

// PreflightOptions gates which network-touching probes run. A true Skip*
// flag records the corresponding step as a skip instead of probing — used
// by the --no-probe-* CLI flags for cost-controlled or air-gapped
// environments. Timeout bounds the entire preflight wall-clock.
type PreflightOptions struct {
	SkipProvider bool
	SkipMCP      bool
	SkipTrace    bool
	SkipEgress   bool
	Timeout      time.Duration
}

// DefaultPreflightTimeout is the wall-clock budget for a dry-run when the
// caller does not set PreflightOptions.Timeout. Matches the 30s default
// documented for --dry-run-timeout.
const DefaultPreflightTimeout = 30 * time.Second

// Preflight runs every initialisation step short of the first agentic
// turn and returns a per-step report. It mirrors BuildLoop's construction
// sequence but stops before assembling the AgenticLoop: it validates the
// config, constructs each component (a construction failure becomes a
// failed step naming the component — this is where credential resolution,
// which BuildLoop performs inline during provider construction, surfaces),
// then probes each constructed component that implements Preflighter and
// is not gated off by opts.
//
// MAINTENANCE INVARIANT: this construction sequence intentionally mirrors
// BuildLoopWithTransport in factory.go and MUST be updated in lockstep
// with it. There is no shared construction helper today (a refactor to
// extract one is a deferred follow-up), so a new component added to
// BuildLoop that resolves credentials or validates connectivity will be
// silently ABSENT from the dry-run — giving a false "all OK" — unless a
// corresponding step is added here. See factory.go:BuildLoopWithTransport.
//
// The whole sequence runs under a context.WithTimeout(ctx, opts.Timeout)
// so a wedged endpoint cannot hang the dry-run past the operator's budget.
// Owned resources (the executor's container, trace exporters) are closed
// before returning so a dry-run leaves nothing running.
//
// Preflight returns a non-nil error only for an internal failure that
// prevents producing a report at all; a report with failed steps is
// returned with a nil error and report.OK == false so the caller can
// render every step and map the aggregate to an exit code.
func Preflight(ctx context.Context, config *types.RunConfig, opts PreflightOptions) (*PreflightReport, error) {
	if config == nil {
		return nil, fmt.Errorf("preflight: nil RunConfig")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultPreflightTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	report := &PreflightReport{}
	add := func(step PreflightStep) { report.Steps = append(report.Steps, step) }
	ok := func(name, detail string) { add(PreflightStep{Name: name, Status: PreflightOK, Detail: detail}) }
	skip := func(name, detail string) { add(PreflightStep{Name: name, Status: PreflightSkip, Detail: detail}) }
	fail := func(name string, err error, hint string) {
		add(PreflightStep{Name: name, Status: PreflightFail, Detail: err.Error(), Hint: hint})
	}

	// 1. Config validation — the same fast invariant check BuildLoop runs.
	if err := types.ValidateRunConfig(config); err != nil {
		fail("config-validation", err, "fix the RunConfig shape; see `stirrup run-config --validate`")
		report.finalise()
		return report, nil
	}
	ok("config-validation", "RunConfig invariants satisfied")

	// Preflight constructs components that take a SecurityLogger, but a
	// dry-run must not emit security events to the operator's stderr (those
	// belong to a real run's audit trail). Discard them.
	secLogger := security.NewSecurityLogger(io.Discard, config.RunID)

	// 2. Secret store.
	secrets, err := security.NewAutoSecretStore(ctx, config)
	if err != nil {
		fail("secret-store", err, "check the secret backend (env/file/SSM) for the referenced secret:// paths")
		report.finalise()
		return report, nil
	}
	ok("secret-store", "secret store constructed")

	// 3. Providers + credential resolution. buildProviders resolves the
	// credential source for every provider (default + named) inline, so a
	// failure here is the credential/auth-config surface. Reported as a
	// "credentials" step to match the issue's step naming. Provider
	// reachability probes follow only if construction succeeded.
	prov, providers, err := buildProviders(ctx, config, secrets)
	if err != nil {
		fail("credentials", err, "verify the credential source resolves (env var set, federation rule valid, key present)")
	} else {
		ok("credentials", "all provider credentials resolved")
		preflightProviders(ctx, config, providers, opts, ok, skip, fail)
	}

	// 4. Executor.
	preflightExecutor(ctx, config, secrets, secLogger, ok, skip, fail)

	// 5. Permission policy. The policy-engine arm loads + parses the Cedar
	// file at construction, so a malformed policy fails here; its Probe
	// then smoke-tests evaluation. Other policy types implement no Probe.
	pp, err := buildPermissionPolicy(config, tool.NewRegistry(), nil, secLogger)
	if err != nil {
		fail("permission-policy", err, "check permissionPolicy.policyFile parses as Cedar")
	} else {
		ok("permission-policy", fmt.Sprintf("%s policy constructed", config.PermissionPolicy.Type))
		probeComponent(ctx, "permission-policy-probe", pp, false, "", ok, skip, fail)
	}

	// 6. Trace emitter. otel/gcs probe network reachability; jsonl opens
	// its file at construction and implements no Probe (skip). Gated by
	// --no-probe-trace. Built via the same buildTraceEmitter path BuildLoop
	// uses so the preflight exercises the real construction (secret://
	// header resolution included).
	resolvedHeaders, hdrErr := observability.ResolveHeaders(ctx, secrets, config.TraceEmitter.Headers)
	if hdrErr != nil {
		fail("trace-emitter", hdrErr, "resolve secret:// references in traceEmitter.headers")
	} else {
		te, err := buildTraceEmitter(ctx, config.TraceEmitter, resolvedHeaders, resourceOptionsFromConfig(config))
		if err != nil {
			fail("trace-emitter", err, traceHint(config.TraceEmitter.Type))
		} else {
			ok("trace-emitter", fmt.Sprintf("%s trace emitter constructed", traceTypeName(config.TraceEmitter.Type)))
			probeComponent(ctx, "trace-emitter-probe", te, opts.SkipTrace, "--no-probe-trace set", ok, skip, fail)
			if closer, isCloser := te.(interface{ Close() error }); isCloser {
				defer func() { _ = closer.Close() }()
			}
		}
	}

	// 7. MCP servers. Probed per-server with a tools/list handshake that
	// does not register tools. Gated by --no-probe-mcp.
	preflightMCP(ctx, config, secrets, opts, ok, skip, fail)

	// 8. Egress allowlist. Only relevant for the container executor in
	// allowlist network mode. Gated by --no-probe-egress.
	preflightEgress(ctx, config, opts, ok, skip, fail)

	// 9. Workspace export destination. Only when a gs:// export URI is
	// configured. It has no dedicated --no-probe gate: the probe is a
	// read-only bucket-metadata GET that spends nothing, so it always runs
	// when an export destination is set.
	preflightWorkspaceExport(ctx, config, ok, skip, fail)

	// prov is the default provider adapter; it was probed via the
	// providers map above (which keys the default by its type), so the
	// value is not needed again here. Retained from buildProviders' return
	// for parity with BuildLoop's signature.
	_ = prov

	report.finalise()
	return report, nil
}

// finalise sets report.OK to true iff no step failed.
func (r *PreflightReport) finalise() {
	r.OK = true
	for _, s := range r.Steps {
		if s.Status == PreflightFail {
			r.OK = false
			return
		}
	}
}

// probeComponent type-asserts component to Preflighter and records the
// outcome. A component implementing no Probe records a skip with a
// generic note; a gated component (skipGate true) records a skip with
// gateNote. The name is the step name (distinct from the construction
// step so the report shows both construction and probe outcomes).
func probeComponent(
	ctx context.Context,
	name string,
	component any,
	skipGate bool,
	gateNote string,
	ok func(string, string),
	skip func(string, string),
	fail func(string, error, string),
) {
	probe, isProbe := component.(Preflighter)
	if !isProbe {
		skip(name, "component exposes no probe")
		return
	}
	if skipGate {
		skip(name, gateNote)
		return
	}
	if err := probe.Probe(ctx); err != nil {
		fail(name, err, "")
		return
	}
	ok(name, "probe succeeded")
}

// preflightExecutor checks the configured executor. The container path is
// deliberately NOT routed through buildExecutor: constructing a
// ContainerExecutor creates and STARTS a real container (and the egress
// proxy in allowlist mode) as a side effect, which contradicts the
// read-only intent of a dry-run (issue #245 step 7). Instead it calls
// executor.ProbeContainerEngine, which only pings the engine socket and
// inspects the requested image (no pull, no container, no proxy). local
// and api executors are cheap to construct and have no live side effects,
// so they keep the construction path; neither implements a Probe, so the
// probe step records a skip.
func preflightExecutor(
	ctx context.Context,
	config *types.RunConfig,
	secrets security.SecretStore,
	secLogger *security.SecurityLogger,
	ok func(string, string),
	skip func(string, string),
	fail func(string, error, string),
) {
	if config.Executor.Type == "container" {
		if err := executor.ProbeContainerEngine(ctx, containerProbeConfig(config.Executor)); err != nil {
			fail("executor", err, executorHint("container"))
			return
		}
		ok("executor", "container engine reachable and image present")
		return
	}

	exec, err := buildExecutor(ctx, config.Executor, secrets, secLogger)
	if err != nil {
		fail("executor", err, executorHint(config.Executor.Type))
		return
	}
	ok("executor", fmt.Sprintf("%s executor constructed", executorTypeName(config.Executor.Type)))
	probeComponent(ctx, "executor-probe", exec, false, "", ok, skip, fail)
	if closer, isCloser := exec.(interface{ Close() error }); isCloser {
		_ = closer.Close()
	}
}

// containerProbeConfig projects the RunConfig's executor block onto the
// subset ProbeContainerEngine needs. The socket is left empty so
// ProbeContainerEngine auto-detects it (RunConfig has no socket override;
// that field exists only on the test-facing ContainerExecutorConfig).
func containerProbeConfig(cfg types.ExecutorConfig) executor.ContainerExecutorConfig {
	return executor.ContainerExecutorConfig{
		Image:             cfg.Image,
		Network:           cfg.Network,
		Runtime:           cfg.Runtime,
		RegistryAllowlist: cfg.RegistryAllowlist,
	}
}

// preflightProviders probes the default and every named provider adapter
// for metadata-endpoint reachability. Gated by --no-probe-provider. Each
// adapter is the raw streaming adapter (the normalizer/batch wrappers are
// not applied in preflight), so the Probe methods on the concrete adapter
// types are reached directly. Provider names are sorted so the report's
// step order is deterministic for a multi-provider config (reproducible
// --output=json).
func preflightProviders(
	ctx context.Context,
	config *types.RunConfig,
	providers map[string]provider.ProviderAdapter,
	opts PreflightOptions,
	ok func(string, string),
	skip func(string, string),
	fail func(string, error, string),
) {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		p := providers[name]
		step := "provider-probe:" + name
		if opts.SkipProvider {
			skip(step, "--no-probe-provider set")
			continue
		}
		probe, isProbe := p.(Preflighter)
		if !isProbe {
			skip(step, "provider exposes no probe")
			continue
		}
		if err := probe.Probe(ctx); err != nil {
			fail(step, err, "verify the API key/credential and base URL for this provider")
			continue
		}
		ok(step, providerProbeDetail(providerTypeFor(config, name)))
	}
}

// providerTypeFor resolves the provider TYPE for a providers-map key. The
// default provider is keyed by its type; a named entry in
// config.Providers is keyed by its map name but carries its own Type.
func providerTypeFor(config *types.RunConfig, name string) string {
	if cfg, ok := config.Providers[name]; ok {
		return cfg.Type
	}
	return name
}

// providerProbeDetail returns the success detail for a provider probe.
// Bedrock is special-cased: its probe resolves the AWS credential chain
// but contacts no endpoint (bedrockruntime has no zero-cost reachability
// op), so claiming "metadata endpoint reachable" would mislead an operator
// diagnosing a Bedrock network/VPC issue.
func providerProbeDetail(providerType string) string {
	if providerType == "bedrock" {
		return "AWS credentials resolved (network reachability not probed — no zero-cost Bedrock metadata endpoint)"
	}
	return "metadata endpoint reachable"
}

// preflightMCP probes every configured MCP server. Gated by
// --no-probe-mcp. An MCP probe failure is recorded as a fail (consistent
// with every other probe), which fails the dry-run. The issue notes MCP
// failures are non-fatal unless --strict is set; --strict is out of scope
// for this change, so the simpler consistent behaviour is chosen and
// documented: a configured MCP server that does not answer the
// tools/list handshake fails the dry-run, and an operator who expects an
// MCP server to be down can suppress the probe with --no-probe-mcp.
func preflightMCP(
	ctx context.Context,
	config *types.RunConfig,
	secrets security.SecretStore,
	opts PreflightOptions,
	ok func(string, string),
	skip func(string, string),
	fail func(string, error, string),
) {
	if len(config.Tools.MCPServers) == 0 {
		skip("mcp", "no MCP servers configured")
		return
	}
	client := mcp.NewClient(tool.NewRegistry(), nil)
	for _, srv := range config.Tools.MCPServers {
		step := "mcp:" + srv.Name
		if opts.SkipMCP {
			skip(step, "--no-probe-mcp set")
			continue
		}
		if err := client.Probe(ctx, srv, secrets); err != nil {
			fail(step, err, "check the MCP server URI is reachable and the auth ref resolves (or pass --no-probe-mcp)")
			continue
		}
		ok(step, "initialize/tools/list handshake succeeded")
	}
}

// preflightEgress probes the egress allowlist by resolving each
// destination via DNS. Only relevant when the container executor is in
// allowlist network mode. Gated by --no-probe-egress.
func preflightEgress(
	ctx context.Context,
	config *types.RunConfig,
	opts PreflightOptions,
	ok func(string, string),
	skip func(string, string),
	fail func(string, error, string),
) {
	net := config.Executor.Network
	if config.Executor.Type != "container" || net == nil || net.Mode != "allowlist" {
		skip("egress", "executor is not a container in allowlist network mode")
		return
	}
	if opts.SkipEgress {
		skip("egress", "--no-probe-egress set")
		return
	}
	if err := egressproxy.ProbeAllowlist(ctx, net.Allowlist, nil); err != nil {
		fail("egress", err, "check the allowlist entries resolve via DNS (or pass --no-probe-egress)")
		return
	}
	ok("egress", "all allowlist destinations resolve")
}

// preflightWorkspaceExport probes the gs:// workspace-export destination
// when one is configured. It constructs a GCSExporter with the default
// workload-identity credential source (matching the CLI's
// newWorkspaceExporter default) and performs a read-only bucket check.
func preflightWorkspaceExport(
	ctx context.Context,
	config *types.RunConfig,
	ok func(string, string),
	skip func(string, string),
	fail func(string, error, string),
) {
	dest := config.Executor.WorkspaceExportTo
	if dest == "" {
		skip("workspace-export", "no workspace export destination configured")
		return
	}
	exporter, err := workspaceexport.NewGCSExporter(workspaceexport.GCSExporterOptions{
		CredentialSource: credential.NewGoogleWorkloadIdentitySource(),
	})
	if err != nil {
		fail("workspace-export", err, "")
		return
	}
	if err := exporter.Probe(ctx, dest); err != nil {
		fail("workspace-export", err, "verify the export bucket exists and the run's credential can access it")
		return
	}
	ok("workspace-export", "export bucket accessible")
}

// executorTypeName renders the configured executor type for the report,
// defaulting an empty type to "local" to match buildExecutor's default.
func executorTypeName(t string) string {
	if t == "" {
		return "local"
	}
	return t
}

// executorHint returns a remediation hint for an executor construction
// failure, tailored to the container path where the common causes
// (daemon down, image absent) have concrete operator actions.
func executorHint(t string) string {
	if t == "container" {
		return "check the Docker/Podman daemon is running and the image is present (pull it first)"
	}
	return ""
}

// traceTypeName renders the configured trace emitter type, defaulting an
// empty type to "jsonl" to match buildTraceEmitter's default.
func traceTypeName(t string) string {
	if t == "" {
		return "jsonl"
	}
	return t
}

// traceHint returns a remediation hint for a trace-emitter construction
// failure keyed on the configured type.
func traceHint(t string) string {
	switch t {
	case "otel":
		return "check traceEmitter.endpoint and protocol point at a reachable OTLP collector"
	case "gcs":
		return "check traceEmitter.bucket exists and the credential can access it"
	case "jsonl", "":
		return "check traceEmitter.filePath is in a writable directory"
	default:
		return ""
	}
}
