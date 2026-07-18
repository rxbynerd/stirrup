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
// the merged document. ResolveAll produces a loop-ready config;
// ResolveBase leaves the document minimally mutated for chained
// `stirrup run-config` pipeline stages.
type ResolveMode int

const (
	// ResolveBase merges sources and returns, without mode defaults,
	// validation, prompt resolution, or RunID generation.
	ResolveBase ResolveMode = iota
	// ResolveAll additionally applies mode defaults, prompt resolution,
	// FollowUpGrace/RunID fallbacks, and types.ValidateRunConfig.
	ResolveAll
)

// RunConfigSources gathers the inputs BuildRunConfig consumes. Both the
// harness command and the run-config command populate this struct and
// dispatch through the shared builder so the two surfaces cannot drift.
type RunConfigSources struct {
	// Stdin is the reader treated as a piped base RunConfig; nil means
	// "do not consider stdin". A non-*os.File reader (from tests) is
	// always treated as piped.
	Stdin io.Reader
	// ConfigPath is the --config flag value. Empty means no file; "-"
	// means stdin.
	ConfigPath string
	// Cmd carries the flag overrides.
	Cmd *cobra.Command
	// Args is the positional argument slice, consulted only under
	// ResolveAll when the prompt is otherwise unresolved.
	Args []string
	// Resolve picks between ResolveBase and ResolveAll.
	Resolve ResolveMode
}

// BuildRunConfig is the single resolution path for every flag-set or
// file/stdin-supplied RunConfig the CLI accepts. Precedence order is
// documented in docs/configuration.md#precedence; WIF override folding
// runs Anthropic before OpenAI so a conflicting-type combination errors
// on the second call rather than letting one silently win.
func BuildRunConfig(sources RunConfigSources) (*types.RunConfig, error) {
	cfg, err := loadBaseRunConfig(sources)
	if err != nil {
		return nil, err
	}

	if err := applyOverrides(sources.Cmd, cfg, sources.Args); err != nil {
		return nil, err
	}

	if err := applyAnthropicWIFOverrides(sources.Cmd, cfg); err != nil {
		return nil, err
	}

	if err := applyOpenAIWIFOverrides(sources.Cmd, cfg); err != nil {
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

// loadBaseRunConfig picks the base RunConfig source — file, stdin, or
// flag-derived — per the precedence documented in
// docs/configuration.md#precedence. The sources are mutually exclusive.
func loadBaseRunConfig(sources RunConfigSources) (*types.RunConfig, error) {
	stdinPiped := isStdinPiped(sources.Stdin)
	explicitStdin := sources.ConfigPath == "-"
	filePath := ""
	if sources.ConfigPath != "" && !explicitStdin {
		filePath = sources.ConfigPath
	}

	// Whitespace is trimmed: CI secret stores routinely smuggle in a
	// stray newline around the env var value.
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

	// An auto-detected pipe is only a real source when it carries bytes;
	// an empty read (e.g. an empty anonymous pipe attached by a
	// non-interactive runtime) is downgraded to "no piped stdin" rather
	// than erroring. --config -/STIRRUP_CONFIG=- are exempt: there the
	// operator named stdin explicitly, so an empty read stays an error.
	var pipedConfig []byte
	if stdinPiped && !explicitStdin {
		data, err := readCappedStdin(sources.Stdin, "<stdin>")
		if err != nil {
			return nil, err
		}
		if len(data) == 0 {
			stdinPiped = false
		} else {
			pipedConfig = data
		}
	}

	// Ambiguous or malformed base-source combinations are validation-class
	// (exit 1): nothing was parsed and no I/O failed.
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
		// pipedConfig is guaranteed non-empty here.
		return decodeRunConfig(pipedConfig, "<stdin>")
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
// file-redirected stdin source the operator deliberately attached: a
// named pipe or regular file, but not a TTY or a character device
// (see docs/configuration.md#reading-from-stdin for why the detection
// excludes char devices). A non-*os.File reader is always treated as
// piped so tests can exercise the path without mocking os.Stdin.
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

// readCappedStdin reads up to the config-size cap from r. Emptiness is
// deliberately NOT an error here — the caller decides whether an empty
// read is fatal or benign (see loadBaseRunConfig).
func readCappedStdin(r io.Reader, source string) ([]byte, error) {
	// +1 distinguishes "exactly at the cap" from "larger than the cap".
	data, err := io.ReadAll(io.LimitReader(r, maxConfigFileBytes+1))
	if err != nil {
		return nil, ioError(fmt.Errorf("reading config from %s: %w", source, err))
	}
	if int64(len(data)) > maxConfigFileBytes {
		return nil, ioError(fmt.Errorf("reading config from %s: exceeds %d byte cap", source, maxConfigFileBytes))
	}
	return data, nil
}

// decodeRunConfig parses already-read config bytes with
// DisallowUnknownFields strictness. Callers that treat an empty read as
// benign must gate on len(data) themselves before calling this.
func decodeRunConfig(data []byte, source string) (*types.RunConfig, error) {
	if len(data) == 0 {
		return nil, ioError(fmt.Errorf("reading config from %s: input is empty; pass --config <path> or remove the redirection", source))
	}
	var cfg types.RunConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, parseError(fmt.Errorf("parsing config from %s: %w", source, err))
	}
	return &cfg, nil
}

// readRunConfigFromReader mirrors loadRunConfigFile but consumes any
// io.Reader. Used for the explicit --config - path, where an empty read
// is an error.
func readRunConfigFromReader(r io.Reader, source string) (*types.RunConfig, error) {
	data, err := readCappedStdin(r, source)
	if err != nil {
		return nil, err
	}
	return decodeRunConfig(data, source)
}

// buildFlagOnlyRunConfig reads the flag values off cmd and constructs
// the no-stdin, no-file base RunConfig. applyOverrides still runs
// against the result so cross-field inferences (e.g.
// --gcp-credentials-file implying gcp-service-account credential.type)
// stay reachable through the single resolution path.
func buildFlagOnlyRunConfig(cmd *cobra.Command, args []string) (*types.RunConfig, error) {
	f := cmd.Flags()

	prompt, _ := f.GetString("prompt")
	if prompt == "" && len(args) > 0 {
		prompt = args[0]
	}

	mode, _ := f.GetString("mode")
	sessionName, _ := f.GetString("name")
	model, _ := f.GetString("model")
	promptModel, _ := f.GetString("prompt-model")
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
	otelHeaderRaw, _ := f.GetStringArray("otel-header")
	otelMetricsEndpoint, _ := f.GetString("otel-metrics-endpoint")
	otelCaptureContent, _ := f.GetBool("otel-capture-content")
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
	deploymentEnvironment, _ := f.GetString("deployment-environment")
	serviceNamespace, _ := f.GetString("service-namespace")
	logExport, _ := f.GetString("log-export")
	// Read here, not in the factory, to preserve the agentic loop's "no
	// direct env reads" invariant.
	logExportEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT")
	providerRetryMaxAttempts, _ := f.GetInt("provider-retry-max-attempts")
	providerRetryInitialDelay, _ := f.GetDuration("provider-retry-initial-delay")
	providerRetryMaxDelay, _ := f.GetDuration("provider-retry-max-delay")
	providerRetryWallClockBudget, _ := f.GetDuration("provider-retry-wall-clock")
	workspaceExportTo, _ := f.GetString("export-workspace-to")
	toolDispatchMaxParallel, _ := f.GetInt("max-tool-parallel")
	escalateToolChoice, _ := f.GetBool("escalate-tool-choice")
	// Read only when set explicitly, mirroring the Changed() discipline
	// elsewhere, so an unset flag never clobbers a base-config value.
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

	var otelHeaders map[string]string
	for _, entry := range otelHeaderRaw {
		k, v, err := parseQueryParam(entry)
		if err != nil {
			return nil, fmt.Errorf("--otel-header %q: %w", entry, err)
		}
		if otelHeaders == nil {
			otelHeaders = map[string]string{}
		}
		otelHeaders[k] = v
	}

	temperature := optionalFloat64Flag(cmd, "temperature")

	// buildHarnessRunConfigCore (not buildHarnessRunConfig) keeps
	// applyModeDefaults gated by Resolve == ResolveAll up in
	// BuildRunConfig, so a ResolveBase stage does not materialise the
	// read-only mode's defaults before a later stage might pivot to
	// --mode execution.
	return buildHarnessRunConfigCore(harnessCLIOptions{
		// RunID intentionally left empty; ResolveAll generates one when
		// needed, so chained ResolveBase stages do not each mint a fresh ID.
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
		PromptModel:                  promptModel,
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
		OTelHeaders:                  otelHeaders,
		OTelMetricsEndpoint:          otelMetricsEndpoint,
		OTelCaptureContent:           otelCaptureContent,
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

// errPromptHintRequested is returned by resolvePromptForRun when an
// interactive terminal reaches the prompt-required gate. It is not an
// operator-facing error: runHarness prints the grouped usage hint and
// exits 0. Scripted (non-TTY) callers never see it — they get
// errPromptRequired instead.
var errPromptHintRequested = errors.New("stirrup: interactive prompt hint requested")

// errPromptRequired is the "no prompt anywhere" error surfaced to
// scripted (non-TTY) callers.
var errPromptRequired = errors.New("prompt is required: pass via --prompt flag, as a positional argument, --prompt-file, STIRRUP_PROMPT env var, or the prompt field in --config")

// stderrIsInteractive reports whether the prompt-required gate should
// show the grouped hint instead of the opaque error. A package variable
// so tests can pin both branches without a real TTY.
var stderrIsInteractive = func() bool {
	return term.IsTerminal(int(os.Stderr.Fd()))
}

// resolvePromptForRun fills cfg.Prompt from the lower-precedence sources
// (--prompt-file, STIRRUP_PROMPT) when higher-precedence ones left it
// empty. When no source supplies a prompt, an interactive terminal gets
// errPromptHintRequested; a non-TTY caller gets errPromptRequired.
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
			// Returned bare (unwrapped): wrapping it in an exitError would
			// let it reach classifyExitCode as a non-zero failure on what
			// is actually the interactive success path.
			return errPromptHintRequested
		}
		// A scripted (non-TTY) run with no prompt anywhere is validation-class.
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
