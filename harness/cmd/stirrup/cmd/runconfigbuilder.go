package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/rxbynerd/stirrup/types"
)

// ResolveMode controls the late-stage mutation BuildRunConfig applies to
// the merged document. The harness path uses ResolveAll so the loop
// receives a validated, prompt-resolved, mode-defaulted config; the
// run-config subcommand defaults to ResolveBase so intermediate
// pipeline stages can emit transformable documents without prematurely
// applying defaults a later stage might override.
type ResolveMode int

const (
	// ResolveBase merges sources (file or stdin + flag overrides + WIF
	// folding) and returns. No mode-default application, no validator
	// invocation, no prompt-resolution chain, no RunID generation. The
	// emitted document is intentionally minimally mutated so chained
	// `stirrup run-config | stirrup run-config | ...` stages remain
	// idempotent.
	ResolveBase ResolveMode = iota
	// ResolveAll applies the full harness-path mutation chain on top of
	// ResolveBase: applyModeDefaults, prompt resolution (--prompt >
	// positional > --prompt-file > STIRRUP_PROMPT > config.prompt),
	// STIRRUP_FOLLOWUP_GRACE env-var fallback, RunID generation when
	// the source omitted one, and types.ValidateRunConfig. This is what
	// `stirrup harness` needs before invoking the loop.
	ResolveAll
)

// RunConfigSources gathers the inputs BuildRunConfig consumes. Both the
// harness command and the new run-config command populate this struct
// and dispatch through the shared builder so flag overrides, base
// loading, WIF folding, and (optionally) prompt + validation cannot
// drift between the two surfaces.
type RunConfigSources struct {
	// Stdin is the reader treated as a piped base RunConfig. Nil means
	// "do not consider stdin". When non-nil, the builder checks whether
	// the underlying fd is a TTY (when Stdin is *os.File) and, if so,
	// ignores it. Pass a non-file reader (bytes.Reader, strings.Reader)
	// from tests to force the "piped" code path.
	Stdin io.Reader
	// ConfigPath is the --config flag value. Empty means no file; "-"
	// means stdin (in which case Stdin must be non-nil and reachable).
	ConfigPath string
	// Cmd is the cobra command whose flags carry the overrides. The
	// builder reads both Changed() and the resolved values via
	// applyOverrides / applyAnthropicWIFOverrides.
	Cmd *cobra.Command
	// Args is the positional argument slice (typically the prompt
	// positional). Only consulted when Resolve == ResolveAll and the
	// prompt is otherwise unresolved, mirroring runHarness's existing
	// chain.
	Args []string
	// Resolve picks between the chained-pipeline-friendly ResolveBase
	// and the loop-ready ResolveAll. See the ResolveMode docs.
	Resolve ResolveMode
}

// BuildRunConfig is the single resolution path for every flag-set or
// file/stdin-supplied RunConfig the CLI accepts. It replaces the two
// inline paths (`--config` + overrides vs flag-only) that used to live
// in runHarness so any future change to precedence, WIF folding, or
// prompt resolution lands in one place.
//
// Resolution order (lowest -> highest precedence):
//  1. Defaults supplied by flag DefValues.
//  2. Base RunConfig from --config <path>, --config -, the
//     STIRRUP_CONFIG env var (path or "-", consulted only when the
//     --config flag was not passed), or piped stdin auto-detected
//     when ConfigPath is empty and Stdin is non-TTY. The base
//     sources are mutually exclusive; combining a path-shaped source
//     (--config or STIRRUP_CONFIG=<path>) with piped stdin is a hard
//     error so silent precedence surprises cannot happen in scripted
//     pipelines.
//  3. Explicit flag overrides via applyOverrides (Changed()-gated, so
//     a flag at its default value never clobbers a base field).
//  4. applyAnthropicWIFOverrides (env-var + flag federation folding
//     plus the apiKeyRef mutual-exclusion guard from #117).
//
// When Resolve == ResolveAll the builder additionally:
//   - Calls applyModeDefaults so read-only modes get a non-empty Tools.
//     BuiltIn list and the deny-side-effects policy.
//   - Generates a RunID when the base omitted one.
//   - Folds STIRRUP_FOLLOWUP_GRACE when neither the flag nor the file
//     set FollowUpGrace.
//   - Walks the prompt resolution chain (--prompt > positional >
//     --prompt-file > STIRRUP_PROMPT > base.Prompt) and errors if the
//     result is empty.
//   - Calls types.ValidateRunConfig and returns its error verbatim.
//
// When Resolve == ResolveBase none of the above run; the document is
// emitted minimally mutated so a downstream `stirrup run-config` stage
// can layer one more override on top.
func BuildRunConfig(sources RunConfigSources) (*types.RunConfig, error) {
	cfg, err := loadBaseRunConfig(sources)
	if err != nil {
		return nil, err
	}

	if err := applyOverrides(sources.Cmd, cfg, sources.Args); err != nil {
		return nil, err
	}

	// Anthropic Workload Identity Federation overrides (issue #117).
	// Encapsulated for readability; the helper handles the four ID
	// fields, the inferred credential.type, the token-source inference
	// chain, and the apiKeyRef mutual-exclusion guard. The single call
	// site here keeps the slog.Warn diagnostics inside the helper from
	// firing twice per invocation.
	if err := applyAnthropicWIFOverrides(sources.Cmd, cfg); err != nil {
		return nil, err
	}

	if sources.Resolve != ResolveAll {
		return cfg, nil
	}

	applyModeDefaults(cfg)

	if cfg.RunID == "" {
		cfg.RunID = generateRunID()
	}

	if cfg.FollowUpGrace == nil {
		if v := os.Getenv("STIRRUP_FOLLOWUP_GRACE"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.FollowUpGrace = &n
			}
		}
	}

	if err := resolvePromptForRun(sources.Cmd, cfg); err != nil {
		return nil, err
	}

	if err := types.ValidateRunConfig(cfg); err != nil {
		return nil, validationError(fmt.Errorf("invalid config: %w", err))
	}
	return cfg, nil
}

// loadBaseRunConfig picks the right base — file, stdin, or
// flag-derived — and returns a RunConfig pre-population is layered on.
// The three sources are mutually exclusive: --config <path> with a
// non-TTY stdin is rejected with both sources cited, because the
// alternative (silent precedence) would surprise pipeline authors
// debugging which source landed which field.
//
// STIRRUP_CONFIG is a fourth-precedence fallback that fills the same
// slot as --config when the flag was not passed. It accepts the same
// shape as the flag: a filesystem path, or the literal "-" meaning
// stdin. Combining STIRRUP_CONFIG=<path> with piped stdin is rejected
// the same way --config <path> + piped stdin is, and the error cites
// the env var by name so the operator knows which source to remove.
// A STIRRUP_CONFIG=- + piped-stdin combination is fine because the
// env var is opting into the stdin path, not naming a separate base.
func loadBaseRunConfig(sources RunConfigSources) (*types.RunConfig, error) {
	stdinPiped := isStdinPiped(sources.Stdin)
	explicitStdin := sources.ConfigPath == "-"
	filePath := ""
	if sources.ConfigPath != "" && !explicitStdin {
		filePath = sources.ConfigPath
	}

	// STIRRUP_CONFIG fills the --config slot only when the flag was not
	// passed; an explicit --config always wins. Surrounding whitespace
	// is trimmed because shell env-var editors and CI secret stores
	// routinely smuggle in a stray newline or space — silently mapping
	// `STIRRUP_CONFIG=" /path"` to `open  /path: no such file` would be
	// a worse failure than treating the trimmed value as authoritative.
	envConfigSourced := false
	if sources.ConfigPath == "" {
		if envPath := strings.TrimSpace(os.Getenv("STIRRUP_CONFIG")); envPath != "" {
			envConfigSourced = true
			if envPath == "-" {
				explicitStdin = true
			} else {
				filePath = envPath
			}
		}
	}

	// The base-source preconditions below are configuration mistakes the
	// operator must resolve before any input is read — ambiguous sources,
	// or --config -/STIRRUP_CONFIG=- without a usable piped stdin. They
	// are validation-class (exit 1): nothing was parsed and no I/O
	// failed. Declaring the class explicitly keeps every validation-class
	// error in this package self-describing rather than relying on the
	// classifyExitCode default.
	if filePath != "" && stdinPiped {
		if envConfigSourced {
			return nil, validationError(fmt.Errorf("ambiguous config sources: both env var STIRRUP_CONFIG=%q and piped stdin specify a base config; pick one", filePath))
		}
		return nil, validationError(fmt.Errorf("ambiguous config sources: --config %q and piped stdin are both present; pick one", filePath))
	}

	switch {
	case explicitStdin:
		if !stdinPiped {
			if envConfigSourced {
				if sources.Stdin == nil {
					return nil, validationError(fmt.Errorf("STIRRUP_CONFIG=- requires piped stdin but no stdin reader was provided"))
				}
				return nil, validationError(fmt.Errorf("STIRRUP_CONFIG=- requires piped stdin but stdin is a terminal"))
			}
			return nil, validationError(fmt.Errorf("--config - requires piped stdin but stdin is a terminal"))
		}
		if envConfigSourced {
			slog.Debug("using STIRRUP_CONFIG as base RunConfig source", "path", "-")
		}
		return readRunConfigFromReader(sources.Stdin, "<stdin>")
	case stdinPiped && filePath == "":
		return readRunConfigFromReader(sources.Stdin, "<stdin>")
	case filePath != "":
		if envConfigSourced {
			slog.Debug("using STIRRUP_CONFIG as base RunConfig source", "path", filePath)
		}
		cfg, err := loadRunConfigFile(filePath)
		if err != nil && envConfigSourced {
			return nil, fmt.Errorf("STIRRUP_CONFIG: %w", err)
		}
		return cfg, err
	}

	return buildFlagOnlyRunConfig(sources.Cmd, sources.Args)
}

// isStdinPiped reports whether the reader looks like a piped or
// file-redirected stdin source the operator deliberately attached.
// The detection is deliberately narrow:
//
//   - A nil reader returns false (no stdin to consider).
//   - A non-*os.File reader (bytes.Reader, strings.Reader from tests)
//     returns true so unit tests can exercise the piped path without
//     mocking out os.Stdin.
//   - An *os.File whose fd is a TTY returns false — an interactive
//     shell must never silently read keystrokes as a JSON RunConfig.
//   - An *os.File backed by a named pipe (shell `|`) or a regular
//     file (`< config.json`) returns true.
//   - An *os.File backed by anything else — character devices
//     (including `< /dev/null` and the shape `go test` hands its
//     children), Unix sockets, and the no-redirect case — returns
//     false and the harness falls through to flag-only construction.
//
// This is narrower than the spec's "any non-TTY stdin is piped"
// because that rule would make `go test ./harness/...` trip the
// piped-base path for every existing harness_test.go fixture: the
// test runner inherits stdin as a non-TTY char device from the
// terminal. Limiting auto-detection to pipes and regular files
// covers the canonical pipeline example (`run-config | harness`)
// and explicit redirection from a config file (`harness <
// config.json`), and leaves the `< /dev/null` "non-TTY but empty"
// case to the explicit `--config -` form (which always errors on
// EOF input). The cost is that an operator who pipes `< /dev/null`
// gets the flag-only path's prompt-required error rather than a
// "stdin was empty" error — still loud, still actionable, just
// pointing at the more usual remediation.
func isStdinPiped(r io.Reader) bool {
	if r == nil {
		return false
	}
	f, ok := r.(*os.File)
	if !ok {
		return true
	}
	if term.IsTerminal(int(f.Fd())) {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	mode := info.Mode()
	if mode&os.ModeNamedPipe != 0 {
		return true
	}
	if mode.IsRegular() {
		return true
	}
	return false
}

// readRunConfigFromReader mirrors loadRunConfigFile but consumes any
// io.Reader. Used for both --config - and the auto-detected pipe path
// in BuildRunConfig. The 1 MiB cap matches the file path (a RunConfig
// is at most a few KB; megabytes mean a mistake), the
// DisallowUnknownFields setting matches the file path (typos in piped
// JSON should fail fast), and the empty-input error is what the spec
// requires for `stirrup harness < /dev/null`.
func readRunConfigFromReader(r io.Reader, source string) (*types.RunConfig, error) {
	// +1 lets us distinguish "exactly at the cap" from "larger than the
	// cap" so the operator-facing error is accurate. Same shape as the
	// readPromptFile helper above.
	data, err := io.ReadAll(io.LimitReader(r, maxConfigFileBytes+1))
	if err != nil {
		// Read failure on the stream itself (a broken pipe, a flaky
		// redirected file) is an I/O error (exit 3), not a parse error:
		// the bytes never reached the decoder.
		return nil, ioError(fmt.Errorf("reading config from %s: %w", source, err))
	}
	if int64(len(data)) > maxConfigFileBytes {
		return nil, ioError(fmt.Errorf("reading config from %s: exceeds %d byte cap", source, maxConfigFileBytes))
	}
	if len(data) == 0 {
		return nil, ioError(fmt.Errorf("reading config from %s: input is empty; pass --config <path> or remove the redirection", source))
	}
	var cfg types.RunConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		// Malformed JSON (syntax error, unknown field, type mismatch) is
		// a parse error (exit 2): the input was read but did not decode.
		return nil, parseError(fmt.Errorf("parsing config from %s: %w", source, err))
	}
	return &cfg, nil
}

// buildFlagOnlyRunConfig reads the flag values off cmd and constructs
// the RunConfig the flag-only base used to start from. This is the
// no-stdin, no-file branch of BuildRunConfig and corresponds to
// the second `runHarness` path before the refactor.
//
// applyOverrides will run against the returned config too, but most of
// its overrides are no-ops on a flag-only base because the values are
// already in place. Running it unconditionally keeps the cross-field
// inferences (--gcp-credentials-file implying gcp-service-account
// credential.type, --azure-tenant-id implying azure-workload-identity,
// --permission-policy-file implying policy-engine) reachable through
// the single resolution path.
func buildFlagOnlyRunConfig(cmd *cobra.Command, args []string) (*types.RunConfig, error) {
	f := cmd.Flags()

	prompt, _ := f.GetString("prompt")
	if prompt == "" && len(args) > 0 {
		prompt = args[0]
	}

	mode, _ := f.GetString("mode")
	sessionName, _ := f.GetString("name")
	model, _ := f.GetString("model")
	providerType, _ := f.GetString("provider")
	apiKeyRef, _ := f.GetString("api-key-ref")
	baseURL, _ := f.GetString("base-url")
	apiKeyHeader, _ := f.GetString("api-key-header")
	queryParamRaw, _ := f.GetStringArray("query-param")
	compatProfile, _ := f.GetString("provider-compat-profile")
	toolsProfile, _ := f.GetString("tools-profile")
	gcpProject, _ := f.GetString("gcp-project")
	gcpLocation, _ := f.GetString("gcp-location")
	gcpCredentialsFile, _ := f.GetString("gcp-credentials-file")
	anthropicFederationRuleID, _ := f.GetString("anthropic-federation-rule-id")
	anthropicOrganizationID, _ := f.GetString("anthropic-organization-id")
	anthropicServiceAccountID, _ := f.GetString("anthropic-service-account-id")
	anthropicWorkspaceID, _ := f.GetString("anthropic-workspace-id")
	anthropicFromGitHubActions, _ := f.GetBool("anthropic-from-github-actions")
	azureTenantID, _ := f.GetString("azure-tenant-id")
	azureClientID, _ := f.GetString("azure-client-id")
	azureScope, _ := f.GetString("azure-scope")
	workspace, _ := f.GetString("workspace")
	maxTurns, _ := f.GetInt("max-turns")
	timeout, _ := f.GetInt("timeout")
	tracePath, _ := f.GetString("trace")
	transportType, _ := f.GetString("transport")
	transportAddr, _ := f.GetString("transport-addr")
	followUpGrace, _ := f.GetInt("followup-grace")
	logLevel, _ := f.GetString("log-level")
	executorType, _ := f.GetString("executor")
	editStrategyType, _ := f.GetString("edit-strategy")
	verifierType, _ := f.GetString("verifier")
	gitStrategyType, _ := f.GetString("git-strategy")
	traceEmitterType, _ := f.GetString("trace-emitter")
	otelEndpoint, _ := f.GetString("otel-endpoint")
	otelProtocol, _ := f.GetString("otel-protocol")
	containerRuntime, _ := f.GetString("container-runtime")
	k8sNamespace, _ := f.GetString("k8s-namespace")
	k8sKubeconfig, _ := f.GetString("k8s-kubeconfig")
	k8sServiceAccount, _ := f.GetString("k8s-service-account")
	k8sEgressProxyURL, _ := f.GetString("k8s-egress-proxy-url")
	k8sNodeSelectorRaw, _ := f.GetStringArray("k8s-node-selector")
	permissionPolicyFile, _ := f.GetString("permission-policy-file")
	codeScannerType, _ := f.GetString("code-scanner")
	guardRailType, _ := f.GetString("guardrail")
	guardRailEndpoint, _ := f.GetString("guardrail-endpoint")
	guardRailModel, _ := f.GetString("guardrail-model")
	guardRailFailOpen, _ := f.GetBool("guardrail-fail-open")
	guardRailTimeout, _ := f.GetDuration("guardrail-timeout")
	deploymentEnvironment, _ := f.GetString("deployment-environment")
	serviceNamespace, _ := f.GetString("service-namespace")
	logExport, _ := f.GetString("log-export")
	// OTEL_EXPORTER_OTLP_LOGS_ENDPOINT is the OTel-standard per-signal
	// override for the logs endpoint. When set it pins the log exporter's
	// endpoint regardless of --otel-endpoint; left empty, the factory falls
	// back to the trace emitter's endpoint. Read here (in the CLI builder)
	// rather than in the factory so the agentic loop's "no direct env reads"
	// invariant is preserved.
	logExportEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT")
	providerRetryMaxAttempts, _ := f.GetInt("provider-retry-max-attempts")
	providerRetryInitialDelay, _ := f.GetDuration("provider-retry-initial-delay")
	providerRetryMaxDelay, _ := f.GetDuration("provider-retry-max-delay")
	providerRetryWallClockBudget, _ := f.GetDuration("provider-retry-wall-clock")
	workspaceExportTo, _ := f.GetString("export-workspace-to")
	toolDispatchMaxParallel, _ := f.GetInt("max-tool-parallel")
	escalateToolChoice, _ := f.GetBool("escalate-tool-choice")
	// Read the retry cap only when the operator set it explicitly. Mirrors
	// the applyOverrides Changed() discipline: forwarding the flag's
	// default (0) unconditionally would clobber a base-config MaxRetries
	// with 0. The clobber is benign today (0 resolves to the library
	// default) but a future non-zero-default knob would silently override
	// a file value otherwise.
	escalateToolChoiceMaxRetries := 0
	if f.Changed("escalate-tool-choice-max-retries") {
		escalateToolChoiceMaxRetries, _ = f.GetInt("escalate-tool-choice-max-retries")
	}
	batch, _ := f.GetBool("batch")

	var queryParams map[string]string
	for _, entry := range queryParamRaw {
		k, v, err := parseQueryParam(entry)
		if err != nil {
			return nil, fmt.Errorf("--query-param %q: %w", entry, err)
		}
		if queryParams == nil {
			queryParams = map[string]string{}
		}
		queryParams[k] = v
	}

	var k8sNodeSelector map[string]string
	for _, entry := range k8sNodeSelectorRaw {
		k, v, err := parseQueryParam(entry)
		if err != nil {
			return nil, fmt.Errorf("--k8s-node-selector %q: %w", entry, err)
		}
		if k8sNodeSelector == nil {
			k8sNodeSelector = map[string]string{}
		}
		k8sNodeSelector[k] = v
	}

	temperature := optionalFloat64Flag(cmd, "temperature")

	// buildHarnessRunConfigCore (not buildHarnessRunConfig) is what runs
	// here so the trailing applyModeDefaults stays gated by Resolve ==
	// ResolveAll up in BuildRunConfig. A run-config stage that uses
	// ResolveBase should not silently materialise the read-only mode's
	// Tools.BuiltIn list because a later stage may pivot to --mode
	// execution where the read-only defaults do not apply.
	return buildHarnessRunConfigCore(harnessCLIOptions{
		// RunID intentionally left empty; ResolveAll generates one when
		// the base did not supply one, and ResolveBase leaves it empty
		// so chained stages do not each mint a fresh ID.
		Mode:                         mode,
		SessionName:                  sessionName,
		Prompt:                       prompt,
		ProviderType:                 providerType,
		BaseURL:                      baseURL,
		APIKeyHeader:                 apiKeyHeader,
		QueryParams:                  queryParams,
		CompatProfile:                compatProfile,
		ToolsProfile:                 toolsProfile,
		APIKeyRef:                    apiKeyRef,
		GCPProject:                   gcpProject,
		GCPLocation:                  gcpLocation,
		GCPCredentialsFile:           gcpCredentialsFile,
		AnthropicFederationRuleID:    anthropicFederationRuleID,
		AnthropicOrganizationID:      anthropicOrganizationID,
		AnthropicServiceAccountID:    anthropicServiceAccountID,
		AnthropicWorkspaceID:         anthropicWorkspaceID,
		AnthropicFromGitHubActions:   anthropicFromGitHubActions,
		AzureTenantID:                azureTenantID,
		AzureClientID:                azureClientID,
		AzureScope:                   azureScope,
		Model:                        model,
		Workspace:                    workspace,
		MaxTurns:                     maxTurns,
		Timeout:                      timeout,
		TracePath:                    tracePath,
		TransportType:                transportType,
		TransportAddr:                transportAddr,
		FollowUpGrace:                followUpGrace,
		Temperature:                  temperature,
		LogLevel:                     logLevel,
		ExecutorType:                 executorType,
		EditStrategyType:             editStrategyType,
		VerifierType:                 verifierType,
		GitStrategyType:              gitStrategyType,
		TraceEmitterType:             traceEmitterType,
		OTelEndpoint:                 otelEndpoint,
		OTelProtocol:                 otelProtocol,
		ContainerRuntime:             containerRuntime,
		K8sNamespace:                 k8sNamespace,
		K8sKubeconfig:                k8sKubeconfig,
		K8sServiceAccount:            k8sServiceAccount,
		K8sEgressProxyURL:            k8sEgressProxyURL,
		K8sNodeSelector:              k8sNodeSelector,
		PermissionPolicyFile:         permissionPolicyFile,
		CodeScannerType:              codeScannerType,
		GuardRailType:                guardRailType,
		GuardRailEndpoint:            guardRailEndpoint,
		GuardRailModel:               guardRailModel,
		GuardRailFailOpen:            guardRailFailOpen,
		GuardRailTimeout:             guardRailTimeout,
		DeploymentEnvironment:        deploymentEnvironment,
		ServiceNamespace:             serviceNamespace,
		LogExport:                    logExport,
		LogExportEndpoint:            logExportEndpoint,
		ProviderRetryMaxAttempts:     providerRetryMaxAttempts,
		ProviderRetryInitialDelay:    providerRetryInitialDelay,
		ProviderRetryMaxDelay:        providerRetryMaxDelay,
		ProviderRetryWallClockBudget: providerRetryWallClockBudget,
		WorkspaceExportTo:            workspaceExportTo,
		ToolDispatchMaxParallel:      toolDispatchMaxParallel,
		EscalateToolChoice:           escalateToolChoice,
		EscalateToolChoiceMaxRetries: escalateToolChoiceMaxRetries,
		Batch:                        batch,
	})
}

// errPromptHintRequested is a sentinel returned by resolvePromptForRun
// when a bare `stirrup harness` reaches the prompt-required gate on an
// interactive terminal (issue #249). It is not an operator-facing error
// — runHarness recognises it, prints the grouped usage hint to stderr,
// and exits 0. Scripted (non-TTY) invocations never see it: they get the
// opaque errPromptRequired message and a non-zero exit so log
// aggregators are not flooded with a multi-line template.
var errPromptHintRequested = errors.New("stirrup: interactive prompt hint requested")

// errPromptRequired is the opaque "no prompt anywhere" error surfaced to
// scripted (non-TTY) callers. Kept as a sentinel so the message lives in
// one place; the verbatim text matches the pre-#249 paths so
// harness_test.go's existing fixtures continue to match.
var errPromptRequired = errors.New("prompt is required: pass via --prompt flag, as a positional argument, --prompt-file, STIRRUP_PROMPT env var, or the prompt field in --config")

// stderrIsInteractive reports whether the prompt-required gate should
// show the grouped hint instead of the opaque error. It is a package
// variable, not a direct os.Stderr probe, so tests can pin both branches
// without allocating a PTY — mirroring how the trace tests inject a
// colorMode rather than a real terminal. Production points it at
// os.Stderr's fd.
var stderrIsInteractive = func() bool {
	return term.IsTerminal(int(os.Stderr.Fd()))
}

// resolvePromptForRun fills cfg.Prompt from the lower-precedence sources
// (--prompt-file, STIRRUP_PROMPT) when the higher-precedence ones
// (--prompt, positional, base RunConfig.prompt) left it empty. When no
// source supplies a prompt the function distinguishes two callers:
// reaching the gate on an interactive terminal returns
// errPromptHintRequested (runHarness then prints the grouped hint and
// exits 0); a non-TTY caller gets errPromptRequired verbatim so
// harness_test.go's existing fixtures and scripted automation continue
// to match the pre-#249 message.
func resolvePromptForRun(cmd *cobra.Command, cfg *types.RunConfig) error {
	if cfg.Prompt != "" {
		return nil
	}
	if promptFile, _ := cmd.Flags().GetString("prompt-file"); promptFile != "" {
		p, err := readPromptFile(promptFile)
		if err != nil {
			return err
		}
		cfg.Prompt = p
	}
	if cfg.Prompt == "" {
		if v := os.Getenv("STIRRUP_PROMPT"); v != "" {
			cfg.Prompt = v
		}
	}
	if cfg.Prompt == "" {
		if stderrIsInteractive() {
			// errPromptHintRequested is intercepted by runHarness (prints
			// the grouped hint, exits 0) and by run-config (remapped to
			// the plain errPromptRequired). It is NEVER classified here:
			// wrapping it in an exitError would let it reach
			// classifyExitCode as a non-zero failure on the interactive
			// success path. Return the bare sentinel.
			return errPromptHintRequested
		}
		// A scripted (non-TTY) run with no prompt anywhere is a
		// precondition / validation-class failure (exit 1, issue #253).
		// errors.Is still matches errPromptRequired through the exitError
		// wrapper (the wrapper is transparent via Unwrap), so the
		// run-config remap and existing fixtures are unaffected. Note this
		// wraps errPromptRequired, NOT the errPromptHintRequested sentinel
		// above — that one is returned bare on purpose.
		return validationError(errPromptRequired)
	}
	return nil
}

// writeRunConfigJSON marshals cfg to w with a trailing newline. The
// pretty form (2-space indent) is the default because the primary
// consumers are humans inspecting captured configs and `diff` output
// between pipeline stages; --compact on run-config flips to a single
// line for tooling that prefers terse JSON.
func writeRunConfigJSON(w io.Writer, cfg *types.RunConfig, compact bool) error {
	var (
		data []byte
		err  error
	)
	if compact {
		data, err = json.Marshal(cfg)
	} else {
		data, err = json.MarshalIndent(cfg, "", "  ")
	}
	if err != nil {
		// A RunConfig that fails to marshal is an internal bug, not an
		// operator-facing I/O or input failure; leave it untyped so it
		// takes the default exit 1 rather than masquerading as exit 3.
		return fmt.Errorf("marshal RunConfig: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return ioError(fmt.Errorf("write RunConfig: %w", err))
	}
	if _, err := w.Write([]byte("\n")); err != nil {
		return ioError(fmt.Errorf("write RunConfig: %w", err))
	}
	return nil
}
