package core

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/executor/egressproxy"
	"github.com/rxbynerd/stirrup/harness/internal/mcp"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/workspaceexport"
	"github.com/rxbynerd/stirrup/types"
)

// Preflighter is the structural interface a component implements to opt
// into a dry-run reachability/authentication probe. Components satisfy it
// WITHOUT importing core — Preflight type-asserts each constructed
// component to this interface and calls Probe when present, or records a
// skip when absent.
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
	// SkipExecutor gates the container-engine probe (--no-probe-executor).
	// No effect on local/api executors, which expose no Probe regardless.
	SkipExecutor bool
	Timeout      time.Duration
}

// DefaultPreflightTimeout is the wall-clock budget for a dry-run when the
// caller does not set PreflightOptions.Timeout. Matches the 30s default
// documented for --dry-run-timeout.
const DefaultPreflightTimeout = 30 * time.Second

// Preflight runs every initialisation step short of the first agentic turn
// and returns a per-step report. It validates the config, constructs each
// probe-eligible component individually (so one failure is isolated to its
// own step rather than aborting the whole report), then probes each via
// builtComponents.probeSteps — the same enumeration BuildLoop's components
// satisfy, which is what keeps a dry-run structurally in parity with a real
// run (see docs/architecture.md).
//
// The whole sequence runs under a context.WithTimeout(ctx, opts.Timeout).
// Owned resources (trace exporters) are closed before returning.
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

	secrets, err := security.NewAutoSecretStore(ctx, config)
	if err != nil {
		fail("secret-store", err, "check the secret backend (env/file/SSM) for the referenced secret:// paths")
		report.finalise()
		return report, nil
	}
	ok("secret-store", "secret store constructed")

	// Executor construction is read-only here (nil for container, built for
	// local/api); its construction step is emitted by buildComponents below.
	execResult := preflightExecutorConstruct(ctx, config, secrets, secLogger)

	// Resolved up front so a secret:// resolution failure surfaces as its
	// own dedicated step rather than an opaque trace-emitter construction
	// error inside buildComponents.
	resolvedHeaders, hdrErr := observability.ResolveHeaders(ctx, secrets, config.TraceEmitter.Headers)
	if hdrErr != nil {
		fail("trace-emitter", hdrErr, "resolve secret:// references in traceEmitter.headers")
		report.finalise()
		return report, nil
	}

	// buildComponents is the SAME constructor BuildLoopWithTransport calls —
	// this is what makes the parity structural. A construction failure
	// stops the dry-run at the first error; the sink has already recorded
	// the step.
	//
	// An empty registry and nil transport match the dry-run intent for the
	// permission policy: construction (and any Cedar parse) is what is
	// validated, not the run's tool set or upstream approval channel.
	sink := &componentStepSink{ok: ok, failStep: fail}
	bc, err := buildComponents(ctx, config, secrets, secLogger, tool.NewRegistry(), nil, execResult, resolvedHeaders, resourceOptionsFromConfig(config), sink)
	if err != nil {
		// The sink already recorded the failed construction step; the
		// wrapped error is for internal logging only and does not become a
		// second report step.
		report.finalise()
		return report, nil
	}
	if closer, isCloser := bc.traceEmitter.(interface{ Close() error }); isCloser {
		defer func() { _ = closer.Close() }()
	}

	for _, step := range bc.probeSteps() {
		runProbeStep(ctx, config, step, opts, ok, skip, fail)
	}

	preflightMCP(ctx, config, secrets, opts, ok, skip, fail)

	preflightEgress(ctx, config, opts, ok, skip, fail)
	// No dedicated --no-probe gate: the probe is a read-only
	// bucket-metadata GET that spends nothing, so it always runs when an
	// export destination is set.
	preflightWorkspaceExport(ctx, config, ok, skip, fail)

	report.finalise()
	return report, nil
}

// runProbeStep runs one probe-eligible component's probe, applying the
// per-component gating and (for the executor) the read-only container-engine
// probe that replaces a Preflighter assertion. The step name comes from
// builtComponents.probeSteps, so this dispatch is keyed on it.
func runProbeStep(
	ctx context.Context,
	config *types.RunConfig,
	step probeComponentStep,
	opts PreflightOptions,
	ok func(string, string),
	skip func(string, string),
	fail func(string, error, string),
) {
	switch {
	case step.name == "executor-probe":
		preflightExecutorProbe(ctx, config, step.component, opts, ok, skip, fail)
	case step.name == "trace-emitter-probe":
		probeComponent(ctx, step.name, step.component, opts.SkipTrace, "--no-probe-trace set", ok, skip, fail)
	case strings.HasPrefix(step.name, "provider-probe:"):
		if step.component == nil {
			skip(step.name, "provider not constructed")
			return
		}
		if opts.SkipProvider {
			skip(step.name, "--no-probe-provider set")
			return
		}
		probe, isProbe := step.component.(Preflighter)
		if !isProbe {
			skip(step.name, "provider exposes no probe")
			return
		}
		name := strings.TrimPrefix(step.name, "provider-probe:")
		if err := probe.Probe(ctx); err != nil {
			fail(step.name, err, "verify the API key/credential and base URL for this provider")
			return
		}
		ok(step.name, providerProbeDetail(providerTypeFor(config, name)))
	default:
		probeComponent(ctx, step.name, step.component, false, "", ok, skip, fail)
	}
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
// outcome. A nil component (one whose construction failed above) records a
// skip with "not constructed" — its construction step already failed the
// report, so a second failure here would double-count. A component
// implementing no Probe records a skip with a generic note; a gated
// component (skipGate true) records a skip with gateNote. The name is the
// step name (distinct from the construction step so the report shows both
// construction and probe outcomes).
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
	if isNilComponent(component) {
		skip(name, "component not constructed")
		return
	}
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

// isNilComponent reports whether an `any` holding a probe-eligible component
// is effectively nil — either an untyped nil or a typed nil interface value
// (e.g. a builtComponents field left unset after a construction failure).
// The plain `== nil` check misses the typed-nil case, so reflect is used.
func isNilComponent(component any) bool {
	if component == nil {
		return true
	}
	v := reflect.ValueOf(component)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return v.IsNil()
	default:
		return false
	}
}

// preflightExecutorConstruct decides the dry-run executor and returns it as
// an executorBuildResult for buildComponents to thread onto the component set
// and emit as the "executor" construction step; it does not emit the step
// itself.
//
// The container path is deliberately NOT routed through buildExecutor:
// constructing a ContainerExecutor creates and STARTS a real container (and
// the egress proxy in allowlist mode), which contradicts the read-only
// intent of a dry-run. For container it returns a nil executor, checked
// read-only in preflightExecutorProbe instead. local and api executors are
// cheap and side-effect-free, so they are built here for the probe phase.
func preflightExecutorConstruct(
	ctx context.Context,
	config *types.RunConfig,
	secrets security.SecretStore,
	secLogger *security.SecurityLogger,
) executorBuildResult {
	if config.Executor.Type == "container" {
		return executorBuildResult{
			detail: "container executor not constructed in dry-run; engine checked in executor-probe",
		}
	}

	exec, err := buildExecutor(ctx, config.Executor, secrets, secLogger)
	if err != nil {
		return executorBuildResult{err: err, hint: executorHint(config.Executor.Type)}
	}
	return executorBuildResult{
		exec:   exec,
		detail: fmt.Sprintf("%s executor constructed", executorTypeName(config.Executor.Type)),
	}
}

// preflightExecutorProbe runs the "executor-probe" step. For the container
// executor this is the read-only engine probe (socket ping + image-present,
// no pull, no container, no proxy) gated by --no-probe-executor. For
// local/api it delegates to the generic probeComponent against the executor
// constructed in preflightExecutorConstruct (neither implements a Probe, so
// the step records a skip; a nil executor — construction failed — likewise
// skips).
func preflightExecutorProbe(
	ctx context.Context,
	config *types.RunConfig,
	exec any,
	opts PreflightOptions,
	ok func(string, string),
	skip func(string, string),
	fail func(string, error, string),
) {
	if config.Executor.Type == "container" {
		if opts.SkipExecutor {
			skip("executor-probe", "--no-probe-executor set")
			return
		}
		if err := executor.ProbeContainerEngine(ctx, containerProbeConfig(config.Executor)); err != nil {
			fail("executor-probe", err, executorHint("container"))
			return
		}
		ok("executor-probe", "container engine reachable and image present")
		return
	}
	probeComponent(ctx, "executor-probe", exec, false, "", ok, skip, fail)
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
// --no-probe-mcp. A configured server that does not answer the
// tools/list handshake fails the dry-run; an operator who expects a
// server to be down can suppress the probe with --no-probe-mcp.
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
