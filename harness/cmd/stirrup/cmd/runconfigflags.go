package cmd

import (
	"github.com/spf13/cobra"

	"github.com/rxbynerd/stirrup/types"
)

// addRunConfigFlags registers every flag that maps to a RunConfig field
// (or to the prompt-resolution chain that lands on RunConfig.Prompt).
// Both `stirrup harness` and `stirrup run-config` call this so the two
// subcommands cannot drift on default values, help text, or supported
// fields. Pure CLI-behaviour flags that do not round-trip through
// RunConfig (--export-workspace-required, --output-runconfig) live on
// the harness command directly; the run-config subcommand adds its own
// --validate / --compact / --redact afterwards.
func addRunConfigFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("config", "", "Path to a JSON RunConfig file (mirrors proto/harness/v1/harness.proto). Use \"-\" to read from stdin. Explicit flags still override individual fields; unset flags do not.")
	f.StringP("mode", "m", "planning", "Run mode: execution, planning, review, research, toil. Default is the read-only `planning` mode (no edits, no shell, deny-side-effects policy); pass `--mode execution` for editable runs with shell access.")
	f.String("model", "claude-sonnet-4-6", "Model to use (sets ModelRouter.Model; for dynamic/per-mode routers in --config files this only sets the default-model field, not the cheap/expensive override)")
	f.String("prompt-model", "", "Render the shipped system prompt templates as if for this model (sets promptBuilder.promptModel). The model called on the wire is unchanged — combine with --model to compare a prompt tuned for one model against another. Empty derives the prompt model from --model.")
	f.String("provider", "anthropic", "Provider type: anthropic, bedrock, gemini (Vertex AI), openai-compatible (Chat Completions), openai-responses (Responses API). The two OpenAI variants speak different wire formats and must be selected explicitly.")
	f.String("api-key-ref", "secret://ANTHROPIC_API_KEY", "Secret reference for API key")
	f.String("base-url", "", "API base URL for openai-compatible / openai-responses providers (e.g. https://<resource>.openai.azure.com/openai/v1)")
	f.String("api-key-header", "", "Header name for sending the API key. Empty = Authorization: Bearer (default). Set to \"api-key\" for Azure OpenAI key auth.")
	f.StringArray("query-param", nil, "Repeatable key=value query parameter appended to every provider request URL (e.g. api-version=preview). Used by the openai-* adapters.")
	f.String("provider-compat-profile", "", "Optional compatibility profile selecting a pre-defined provider-quirks rule. Closed set; current legal values: \"\" (no profile), \"zai-glm\" (Z.ai GLM tool_stream + legacy max_tokens).")
	f.String("tools-profile", "", "Model-facing toolset profile (sets tools.profile). Closed set; current legal values: \"\"/\"default\" (no aliasing, internal tool names), \"coding-classic\" (terse coding-CLI aliases: grep, find, bash). Dispatch identities are unchanged regardless of profile.")
	f.String("gcp-project", "", "GCP project ID hosting the Vertex AI usage. Required when --provider=gemini.")
	f.String("gcp-location", "global", "Vertex AI location: \"global\" or a region (e.g. us-central1). Determines the URL host and project location segment.")
	f.String("gcp-credentials-file", "", "Path to a Google service account JSON key file. When set, implies credential.type=gcp-service-account.")

	f.String("anthropic-federation-rule-id", "", "Anthropic federation rule ID (`fdrl_...`). Implies `credential.type=anthropic-wif` when set. Env fallback: ANTHROPIC_FEDERATION_RULE_ID.")
	f.String("anthropic-organization-id", "", "Anthropic organization UUID. Required with WIF. Env fallback: ANTHROPIC_ORGANIZATION_ID.")
	f.String("anthropic-service-account-id", "", "Anthropic service account ID (`svac_...`). Required with WIF. Env fallback: ANTHROPIC_SERVICE_ACCOUNT_ID.")
	f.String("anthropic-workspace-id", "", "Anthropic workspace ID (`wrkspc_...`) or `default`. Conditional. Env fallback: ANTHROPIC_WORKSPACE_ID.")
	f.Bool("anthropic-from-github-actions", false, "Enable GitHub Actions OIDC token source for Anthropic WIF. Reads ACTIONS_ID_TOKEN_REQUEST_URL and ACTIONS_ID_TOKEN_REQUEST_TOKEN from the runner environment. Ignored (with a warning) if `--config` already sets `credential.tokenSource`.")

	f.String("azure-tenant-id", "", "Azure AD tenant UUID hosting the App Registration. When set, implies credential.type=azure-workload-identity. Use with --provider=openai-compatible or openai-responses against Azure OpenAI / Foundry.")
	f.String("azure-client-id", "", "App Registration / federated identity credential client ID (UUID). Required with --azure-tenant-id.")
	f.String("azure-scope", "https://cognitiveservices.azure.com/.default", "OAuth2 scope for the Entra access token. Override only for non-default Azure audiences (custom AAD app registrations, sovereign clouds).")

	f.String("openai-identity-provider-id", "", "OpenAI Workload Identity Federation identity provider ID. Implies credential.type=openai-wif when set. Use with --provider=openai-compatible or openai-responses against the OpenAI API. Env fallback: OPENAI_IDENTITY_PROVIDER_ID.")
	f.String("openai-service-account-id", "", "OpenAI WIF service account ID. Required with --openai-identity-provider-id. Env fallback: OPENAI_SERVICE_ACCOUNT_ID.")
	f.String("openai-subject-token-type", "", "OpenAI WIF subject token type (RFC 8693 URN). Optional; defaults to urn:ietf:params:oauth:token-type:jwt. Env fallback: OPENAI_SUBJECT_TOKEN_TYPE.")
	f.Bool("openai-from-github-actions", false, "Enable GitHub Actions OIDC token source for OpenAI WIF. Reads ACTIONS_ID_TOKEN_REQUEST_URL and ACTIONS_ID_TOKEN_REQUEST_TOKEN from the runner environment and sets the token-source audience to https://api.openai.com/v1. Ignored (with a warning) if `--config` already sets `credential.tokenSource`.")
	f.StringP("workspace", "w", "", "Workspace directory (default: current directory)")
	f.Int("max-turns", 20, "Maximum agentic loop turns")
	f.Int("timeout", 600, "Wall-clock timeout in seconds")
	f.String("trace", "", "Path to JSONL trace file (sets --trace-emitter to jsonl unless --trace-emitter is explicitly set)")
	f.String("transport", "stdio", "Transport type: stdio, grpc")
	f.String("transport-addr", "", "gRPC target address (required when transport is grpc)")
	f.Int("followup-grace", 0, "Seconds to keep gRPC transport open for follow-up requests (0 = disabled; env: STIRRUP_FOLLOWUP_GRACE)")
	f.Float64("temperature", 0, "Sampling temperature forwarded to the provider on every turn. Range 0.0-2.0 (union of provider ranges). Unset leaves the harness default (0.1); explicit 0 sets greedy decoding.")
	f.String("log-level", "info", "Log level: debug, info, warn, error")
	f.String("prompt", "", "User prompt (can also be passed as a positional argument; falls back to --prompt-file then STIRRUP_PROMPT env var, then a prompt field in --config)")
	f.String("prompt-file", "", "Path to a file whose contents become the prompt. Read from CWD when relative. Trailing newlines are trimmed; the file is capped at 10 MiB and must be non-empty. Lower precedence than --prompt and the positional argument; higher than STIRRUP_PROMPT.")
	f.String("name", "", "Human-readable session label (metadata only, not injected into prompt)")

	f.String("executor", "local", "Executor: local, container, k8s, k8s-sandbox, api, none (none has no local filesystem or shell access — MCP-only / server-side-tool runs)")
	f.String("edit-strategy", "", "Edit strategy: whole-file, search-replace, udiff, multi (composite available only via --config). Defaults to multi when unset.")
	f.String("verifier", "none", "Verifier: none, test-runner, llm-judge (composite available only via --config)")
	f.String("git-strategy", "none", "Git strategy: none, deterministic")
	f.String("trace-emitter", "jsonl", "Trace emitter: jsonl, otel, gcs")
	f.String("otel-endpoint", "", "OTLP endpoint for the otel trace emitter (default: localhost:4317 for grpc; full URL ending in the gateway base path for http/protobuf, e.g. https://otlp-gateway-prod-us-east-0.grafana.net/otlp)")
	f.String("otel-protocol", "", "OTLP wire protocol for the otel trace emitter: \"\" (default — grpc), grpc, http/protobuf. HTTP/JSON is not supported; managed gateways like Grafana Cloud use http/protobuf. See docs/observability-cloud.md.")
	f.StringArray("otel-header", nil, "Repeatable key=value HTTP header attached to every OTLP export request (e.g. --otel-header Authorization=secret://LANGFUSE_AUTH). Values may be secret:// references resolved at exporter init; never pass raw secrets. Requires --otel-protocol=http/protobuf (the gRPC path would send credentials in plaintext). Mirrors traceEmitter.headers; the OTEL_EXPORTER_OTLP_HEADERS env var is the SDK-native fallback when no headers are configured.")
	f.String("otel-metrics-endpoint", "", "OTLP endpoint for the otel metrics exporter when metrics target a different collector than traces. Defaults to --otel-endpoint when unset. Mirrors traceEmitter.metricsEndpoint.")
	f.Bool("otel-capture-content", false, "Opt the otel trace emitter into recording prompt/completion content on spans via the GenAI semconv attributes (gen_ai.input.messages, gen_ai.output.messages, gen_ai.system_instructions). OFF by default: message content is likely to contain PII. Content is scrubbed for secret-shaped substrings before export. Mirrors traceEmitter.captureContent.")

	f.String("container-runtime", "", "OCI runtime for the container executor: runc, runsc (gVisor), kata, kata-qemu, kata-fc, kata-clh. Empty means the platform default — for container the engine default (typically runc), for k8s the cluster-default RuntimeClass, which may be unsandboxed; set this (e.g. gvisor for k8s) for guaranteed isolation. Requires the runtime to be registered with the host Docker/Podman daemon — see docs/safety-rings.md. For the k8s executor this same field maps to the Pod RuntimeClassName (closed set: runc, gvisor, kata-qemu, kata-fc, kata-clh); note shell completions list the container set only, so gvisor is valid for k8s but not offered. The k8s-sandbox executor is gVisor-only: leave this empty or set it to gvisor (any other value is rejected).")
	f.String("k8s-namespace", "", "Kubernetes namespace for the k8s and k8s-sandbox executors' sandbox Pod. Required when --executor=k8s or --executor=k8s-sandbox. Mirrors executor.k8sNamespace.")
	f.String("k8s-kubeconfig", "", "Path to a kubeconfig file for the k8s and k8s-sandbox executors. Empty prefers in-cluster config, then $KUBECONFIG. Mirrors executor.k8sKubeconfig.")
	f.StringArray("k8s-node-selector", nil, "Repeatable key=value nodeSelector label constraining where the k8s and k8s-sandbox executors' Pod schedules (e.g. --k8s-node-selector disktype=ssd). Mirrors executor.k8sNodeSelector.")
	f.String("k8s-service-account", "", "ServiceAccount name for the k8s and k8s-sandbox executors' Pod. Empty uses the namespace default. The token is never automounted regardless. Mirrors executor.k8sServiceAccount.")
	f.String("k8s-egress-proxy-url", "", "URL of the egress proxy the k8s / k8s-sandbox Pod routes HTTP_PROXY/HTTPS_PROXY through. Required when --executor=k8s or --executor=k8s-sandbox and the network mode is \"allowlist\"; rejected otherwise. Deploy the proxy from examples/k8s/egress-proxy/. Mirrors executor.k8sEgressProxyUrl.")
	f.String("permission-policy-file", "", "Path to a Cedar policy file for the policy-engine PermissionPolicy. When set and --permission-policy is unset elsewhere, also implies permissionPolicy.type=policy-engine. See examples/policies/ for starters.")
	f.String("code-scanner", "", "CodeScanner type: none, patterns, semgrep, composite. Composite requires --config (codeScanner.scanners). Empty defers to the mode-aware default (patterns for execution, none for read-only modes).")

	f.String("guardrail", "", "GuardRail type: none, granite-guardian, composite, cloud-judge. Composite requires --config (guardRail.stages).")
	f.String("guardrail-endpoint", "", "Endpoint URL for the granite-guardian or cloud-judge adapter.")
	f.String("guardrail-model", "", "Model identifier for the GuardRail classifier. Granite-guardian default: ibm-granite/granite-guardian-4.1-8b. Cloud-judge default: claude-haiku-4-5-20251001 (Anthropic API format) — when the primary provider is Bedrock, use the Bedrock-format ID (e.g. us.anthropic.claude-haiku-4-5-20251001-v1:0).")
	f.Bool("guardrail-fail-open", false, "When true, transport errors / timeouts produce VerdictAllow with a security event rather than blocking. Default false (fail closed).")

	f.String("deployment-environment", "", "OTel deployment.environment resource attribute (e.g. production, staging). Empty falls through to OTEL_DEPLOYMENT_ENVIRONMENT, then to \"local\".")
	f.String("service-namespace", "", "OTel service.namespace resource attribute (e.g. stirrup-eval, team-a). Empty falls through to OTEL_SERVICE_NAMESPACE, then to \"stirrup\".")
	f.String("log-export", "none", "Structured-log export: none (stderr only, the default) or otlp (stderr plus an OTLP/gRPC log exporter). Endpoint defaults to --otel-endpoint; OTEL_EXPORTER_OTLP_LOGS_ENDPOINT overrides it. Mirrors observability.logsExport.")

	f.Int("provider-retry-max-attempts", 0, "Maximum HTTP attempts (including the first) for the default provider. 1 disables retry. Hard ceiling: 5. Default (when unset): 3. Currently honoured only by the openai-compatible adapter; the other adapters fall through unconditionally pending their own wire-ups.")
	f.Duration("provider-retry-initial-delay", 0, "Base delay for exponential backoff before jitter, applied between retries on the default provider. Accepts Go duration syntax (e.g. 500ms, 1s). Default (when unset): 500ms.")
	f.Duration("provider-retry-max-delay", 0, "Per-attempt sleep ceiling for the default provider (also caps Retry-After hints). Applies only to the default provider; per-named-provider retry policy requires --config. Hard ceiling: 60s. Default (when unset): 16s.")
	f.Duration("provider-retry-wall-clock", 0, "Wall-clock budget for the entire retry sequence on the default provider. Applies only to the default provider; per-named-provider retry policy requires --config. Hard ceiling: 300s. Default (when unset): 90s.")

	f.String("export-workspace-to", "", "Upload the executor workspace as a gzipped tarball to this URI at end-of-run (e.g. gs://bucket/runs/<runId>/workspace.tar.gz). Only gs:// is supported in v1. Mirrors executor.workspaceExportTo.")

	f.Int("max-tool-parallel", 0, "Maximum number of async tool calls dispatched concurrently in a single turn. Range: 1-16. 0 uses the library default (4).")

	f.Bool("escalate-tool-choice", false, "Recover from a first-turn no-tool answer on a workspace-dependent task by retrying with provider-native required tool choice (or a stronger prompt where unsupported). OFF by default. See issue #230.")
	f.Int("escalate-tool-choice-max-retries", 0, "Maximum forced retries per inner-loop run when --escalate-tool-choice is set. Range: 1-3. 0 uses the default (1). No effect unless --escalate-tool-choice is set.")

	f.Bool("batch", false, "Use async batch submission for provider turns (50% cost reduction, up to 24h latency). Requires transport=grpc or --config with harnessSidePolling=true for stdio. See docs/batch.md.")

	addRunConfigFlagCompletions(cmd)
}

// addRunConfigFlagCompletions wires cobra dynamic-flag completion for
// every flag declared in addRunConfigFlags. Closed-set enum flags pull
// their value list from types.Valid*Values() so the completion surface
// tracks the validator without manual sync.
//
// Errors from RegisterFlagCompletionFunc and MarkFlagFilename only
// surface on a typo in the flag name (every flag named here is
// registered above in the same function call); ignoring them matches
// cobra's own idiom for completion registration.
func addRunConfigFlagCompletions(cmd *cobra.Command) {
	staticValues := func(name string, values []string) {
		_ = cmd.RegisterFlagCompletionFunc(name, func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
			return values, cobra.ShellCompDirectiveNoFileComp
		})
	}
	staticValues("mode", types.ValidRunModeValues())
	staticValues("provider", types.ValidProviderTypeValues())
	staticValues("executor", types.ValidExecutorTypeValues())
	staticValues("edit-strategy", types.ValidEditStrategyTypeValues())
	staticValues("verifier", types.ValidVerifierTypeValues())
	staticValues("git-strategy", types.ValidGitStrategyTypeValues())
	staticValues("transport", types.ValidTransportTypeValues())
	staticValues("trace-emitter", types.ValidTraceEmitterTypeValues())
	staticValues("otel-protocol", types.ValidTraceEmitterProtocolValues())
	staticValues("log-export", types.ValidLogsExportTypeValues())
	staticValues("container-runtime", types.ValidExecutorRuntimeValues())
	staticValues("code-scanner", types.ValidCodeScannerTypeValues())
	staticValues("guardrail", types.ValidGuardRailTypeValues())
	staticValues("provider-compat-profile", types.ValidCompatProfileValues())
	staticValues("tools-profile", types.ValidToolsProfileValues())

	// log-level and api-key-header are not validator-closed sets in
	// types/runconfig.go, but operators benefit from a hinted completion.
	staticValues("log-level", []string{"debug", "error", "info", "warn"})
	staticValues("api-key-header", []string{"Authorization", "api-key"})

	_ = cmd.MarkFlagFilename("config", "json")
	_ = cmd.MarkFlagFilename("prompt-file")
	_ = cmd.MarkFlagFilename("gcp-credentials-file", "json")
	_ = cmd.MarkFlagFilename("permission-policy-file", "cedar")
	_ = cmd.MarkFlagFilename("trace", "jsonl")
	_ = cmd.MarkFlagDirname("workspace")
}
