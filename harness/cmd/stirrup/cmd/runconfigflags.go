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
// RunConfig (--output-runconfig) live on the harness command directly; the run-config subcommand adds its own
// --validate / --compact / --redact afterwards.
func addRunConfigFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("config", "", "Path to a JSON RunConfig file (mirrors proto/harness/v1/harness.proto). Use \"-\" to read from stdin. Explicit flags still override individual fields; unset flags do not.")
	f.StringP("mode", "m", "planning", "Run mode: execution, planning, review, research, toil. Default is the read-only `planning` mode (no edits, no shell, deny-side-effects policy); pass `--mode execution` for editable runs with shell access.")
	f.String("model", "", "Model to use (sets ModelRouter.Model; for dynamic/per-mode routers in --config files this only sets the default-model field, not the cheap/expensive override). Defaults to claude-sonnet-4-6 for the anthropic provider; required for every other provider.")
	f.String("prompt-model", "", "Render the shipped system prompt templates as if for this model (sets promptBuilder.promptModel). The model called on the wire is unchanged — combine with --model to compare a prompt tuned for one model against another. Empty derives the prompt model from --model.")
	f.String("provider", "anthropic", "Provider type: anthropic, bedrock, gemini (Vertex AI), openai-compatible (Chat Completions), openai-responses (Responses API). The two OpenAI variants speak different wire formats and must be selected explicitly.")
	f.String("api-key-ref", "", "Secret reference for the API key. Defaults to secret://ANTHROPIC_API_KEY for the anthropic provider and secret://OPENAI_API_KEY for openai-compatible / openai-responses; bedrock and gemini take no key. Pass an empty value (--api-key-ref=\"\") to send no key to a keyless openai-compatible / openai-responses server at a custom --base-url; any other provider or endpoint rejects an empty value.")
	f.String("base-url", "", "API base URL for openai-compatible / openai-responses providers (e.g. https://<resource>.openai.azure.com/openai/v1)")
	f.String("api-key-header", "", "Header name for sending the API key. Empty = Authorization: Bearer (default). Set to \"api-key\" for Azure OpenAI key auth.")
	f.StringArray("query-param", nil, "Repeatable key=value query parameter appended to every provider request URL (e.g. api-version=preview). Used by the openai-* adapters.")
	f.String("provider-compat-profile", "", "Optional compatibility profile selecting a pre-defined provider-quirks rule. Closed set; current legal values: \"\" (no profile), \"zai-glm\" (Z.ai GLM tool_stream + legacy max_tokens).")
	f.String("provider-endpoint-profile", "", "Endpoint profile of an openai-compatible or openai-responses provider (sets provider.endpointProfile). Closed set; current legal values: \"\" (derived: openai for OpenAI's own API, generic for any other --base-url), \"openai\", \"azure-openai\", \"bedrock-mantle\", \"generic\". Hosted tool_search is only used on the openai profile.")
	f.String("tools-profile", "", "Model-facing toolset profile (sets tools.profile). Closed set; current legal values: \"\"/\"default\" (no aliasing, internal tool names), \"coding-classic\" (terse coding-CLI aliases: grep, find, bash). Dispatch identities are unchanged regardless of profile.")
	f.Bool("lazy-tools", false, "Turn on lazy tool loading (sets tools.lazyLoading.enabled): tools outside the eager set reach the model only after it searches for them. See docs/configuration.md#lazy-tool-loading.")
	f.String("lazy-tools-strategy", "", "How deferred tools are searched (sets tools.lazyLoading.strategy). Closed set; current legal values: \"\"/\"auto\" (the provider's native tool search where the model has one, the harness tool_search otherwise), \"harness\" (always tool_search, which hides every deferred tool's name and description on every provider).")
	f.StringArray("lazy-tools-eager", nil, "Repeatable internal tool ID sent on every turn without a search (sets tools.lazyLoading.eager). Unset defers only MCP-imported and control-plane tools; a list defers every other tool, built-ins included. Replaces any tools.lazyLoading.eager from --config.")
	f.Int("lazy-tools-max-results", 0, "How many tools one harness tool_search loads (sets tools.lazyLoading.maxResults). 0 selects 5; maximum 20.")
	f.StringArray("protected-write-target", nil, "Repeatable workspace-relative path that write tools may not target (e.g. AGENTS.md, config/policies/). A trailing / protects the directory and everything beneath it; otherwise the exact path only. Files loaded via --repository-guidance-file are protected in addition. Replaces any tools.protectedWriteTargets from --config.")
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
	f.String("followup-history", "", "History a follow-up run starts from: \"reset\" (default) starts it from its prompt alone; \"continue\" starts it from the previous run's committed history with the prompt appended")
	f.Float64("temperature", 0, "Sampling temperature forwarded to the provider on every turn. Range 0.0-2.0 (union of provider ranges). Unset leaves the harness default (0.1); explicit 0 sets greedy decoding.")
	f.String("reasoning-effort", "", "Reasoning depth requested from the model: \"minimal\", \"low\", \"medium\", \"high\", \"xhigh\", or \"max\". Empty leaves the model on its provider default. Provider-neutral: adapters project it onto the model's native control (Gemini thinkingLevel, Anthropic output_config.effort, OpenAI reasoning effort); models with no probed control ignore it. Per-model acceptance may be narrower — e.g. Gemini 3.7 Flash rejects \"minimal\" and no Claude model accepts it.")
	f.String("reasoning-summary", "", "Whether the model's reasoning text is requested and streamed as thinking_delta events: \"auto\" or \"off\". Empty means \"auto\", except that OpenAI Responses requests no summary unless \"auto\" is explicit. Provider-neutral: adapters project \"auto\" onto the model's native control (Gemini includeThoughts, OpenAI Responses reasoning.summary) or stream the reasoning the provider already returns; \"off\" stops the streaming and, where the provider has a request control (Gemini, OpenAI Responses), does not request it. Conversation history is unaffected.")
	f.String("log-level", "info", "Log level: debug, info, warn, error")
	f.String("prompt", "", "User prompt (can also be passed as a positional argument; falls back to --prompt-file then STIRRUP_PROMPT env var, then a prompt field in --config)")
	f.String("prompt-file", "", "Path to a file whose contents become the prompt. Read from CWD when relative. Trailing newlines are trimmed; the file is capped at 10 MiB and must be non-empty. Lower precedence than --prompt and the positional argument; higher than STIRRUP_PROMPT.")
	f.String("output-schema", "", "Path to a JSON Schema (Draft 2020-12) file the run's final answer must satisfy (sets outputSchema). The model must end with one JSON document conforming to it; the validated document is returned as structuredOutput, and a run whose answer still fails after two corrective attempts ends with outcome structured_output_failed. References must be local (\"#...\"). An empty value clears a schema set by --config.")
	f.String("name", "", "Human-readable session label (metadata only, not injected into prompt)")

	f.String("executor", "local", "Executor: local, container, k8s, k8s-sandbox, api, none (none has no local filesystem or shell access — MCP-only / server-side-tool runs)")
	f.String("edit-strategy", "", "Edit strategy: whole-file, search-replace, udiff, multi (composite available only via --config). Defaults to multi when unset.")
	f.String("verifier", "none", "Verifier: none, test-runner, llm-judge (composite available only via --config)")
	f.String("git-strategy", "none", "Git strategy: none, deterministic")
	f.String("trace-emitter", "jsonl", "Trace emitter: jsonl, otel, gcs")
	f.String("otel-endpoint", "", "OTLP endpoint for the otel trace emitter (default: localhost:4317 for grpc; full URL ending in the gateway base path for http/protobuf, e.g. https://otlp-gateway-prod-us-east-0.grafana.net/otlp)")
	f.String("otel-protocol", "", "OTLP wire protocol for the otel trace emitter: \"\" (default — the --otel-platform's required protocol, else grpc), grpc, http/protobuf. HTTP/JSON is not supported; managed gateways like Grafana Cloud use http/protobuf. See docs/observability-cloud.md.")
	f.StringArray("otel-header", nil, "Repeatable key=value HTTP header attached to every OTLP export request (e.g. --otel-header Authorization=secret://LANGFUSE_AUTH). Values may be secret:// references resolved at exporter init; never pass raw secrets. Requires --otel-protocol=http/protobuf, or an --otel-platform that requires it (the gRPC path would send credentials in plaintext). Mirrors traceEmitter.headers; the OTEL_EXPORTER_OTLP_HEADERS env var is the SDK-native fallback when no headers are configured.")
	f.String("otel-metrics-endpoint", "", "OTLP endpoint for the otel metrics exporter when metrics target a different collector than traces. Defaults to --otel-endpoint when unset. Mirrors traceEmitter.metricsEndpoint.")
	f.String("otel-metrics-temporality", "", "Aggregation temporality for otel metrics: cumulative or delta. Empty defers to OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE, then cumulative. Mirrors traceEmitter.metricsTemporality.")
	f.String("otel-platform", "", "Observability platform profile for the otel trace emitter. The profile fills unset OTLP settings (per-signal endpoints, headers, request signing, resource and span attributes); explicit flags and fields always win. Shell completion lists the supported values. Mirrors traceEmitter.platform; see docs/observability-cloud.md#platform-profiles.")
	f.StringArray("otel-platform-option", nil, "Repeatable key=value setting for the selected --otel-platform (e.g. --otel-platform-option region=us-east-1). Values are at most 256 bytes of printable UTF-8 and are never secrets. Replaces, rather than merges with, platformOptions from a config file. Mirrors traceEmitter.platformOptions.")
	f.Bool("otel-capture-content", false, "Opt the otel trace emitter into recording prompt/completion content on spans via the GenAI semconv attributes (gen_ai.input.messages, gen_ai.output.messages, gen_ai.system_instructions). OFF by default: message content is likely to contain PII. Content is scrubbed for secret-shaped substrings before export. Mirrors traceEmitter.captureContent.")
	f.Int("otel-capture-content-max-bytes", 0, "Cap each captured content attribute (gen_ai.input.messages, gen_ai.output.messages, gen_ai.system_instructions, gen_ai.tool.call.arguments/result) at this many bytes, shortening at a message or part boundary so JSON stays valid and marking the span with stirrup.<attribute>.truncated. 0 (the default) means no cap; otherwise at least 256. Requires --otel-capture-content. Mirrors traceEmitter.captureContentMaxBytes.")

	f.String("container-runtime", "", "OCI runtime for the container executor: runc, runsc (gVisor), kata, kata-qemu, kata-fc, kata-clh. Empty means the platform default — for container the engine default (typically runc), for k8s the cluster-default RuntimeClass, which may be unsandboxed; set this (e.g. gvisor for k8s) for guaranteed isolation. Requires the runtime to be registered with the host Docker/Podman daemon — see docs/safety-rings.md. For the k8s executor this same field maps to the Pod RuntimeClassName (closed set: runc, gvisor, kata-qemu, kata-fc, kata-clh); note shell completions list the container set only, so gvisor is valid for k8s but not offered. The k8s-sandbox executor is gVisor-only: leave this empty or set it to gvisor (any other value is rejected).")
	f.String("k8s-namespace", "", "Kubernetes namespace for the k8s and k8s-sandbox executors' sandbox Pod. Required when --executor=k8s or --executor=k8s-sandbox. Mirrors executor.k8sNamespace.")
	f.String("k8s-kubeconfig", "", "Path to a kubeconfig file for the k8s and k8s-sandbox executors. Empty prefers in-cluster config, then $KUBECONFIG. Mirrors executor.k8sKubeconfig.")
	f.StringArray("k8s-node-selector", nil, "Repeatable key=value nodeSelector label constraining where the k8s and k8s-sandbox executors' Pod schedules (e.g. --k8s-node-selector disktype=ssd). Mirrors executor.k8sNodeSelector.")
	f.String("k8s-service-account", "", "ServiceAccount name for the k8s and k8s-sandbox executors' Pod. Empty uses the namespace default. The token is never automounted regardless. Mirrors executor.k8sServiceAccount.")
	f.String("k8s-egress-proxy-url", "", "URL of the egress proxy the k8s / k8s-sandbox Pod routes HTTP_PROXY/HTTPS_PROXY through. Required when --executor=k8s or --executor=k8s-sandbox and the network mode is \"allowlist\"; rejected otherwise. Deploy the proxy from examples/k8s/egress-proxy/. Mirrors executor.k8sEgressProxyUrl.")
	f.String("permission-policy-file", "", "Path to a Cedar policy file for the policy-engine PermissionPolicy. When set and --permission-policy is unset elsewhere, also implies permissionPolicy.type=policy-engine. See examples/policies/ for starters.")
	f.String("code-scanner", "", "CodeScanner type: none, patterns, semgrep, composite. Composite requires --config (codeScanner.scanners). Empty defers to the mode-aware default (patterns for execution, none for read-only modes).")

	f.String("guardrail", "", "GuardRail type: none, granite-guardian, shieldstral, composite, cloud-judge. Composite requires --config (guardRail.stages).")
	f.String("guardrail-endpoint", "", "Endpoint URL for the granite-guardian, shieldstral, or cloud-judge adapter.")
	f.String("guardrail-api-key-ref", "", "Secret reference for GuardRail API key (e.g. secret://MISTRAL_API_KEY). Used by shieldstral.")
	f.String("guardrail-model", "", "Model identifier for the GuardRail classifier. Granite-guardian default: ibm-granite/granite-guardian-4.1-8b. Shieldstral has no default and requires the model id the server exposes (e.g. mistralai/Shieldstral-1.0-3B). Cloud-judge default: claude-haiku-5-5 (Anthropic API format) — when the primary provider is Bedrock, use the Bedrock-format ID (e.g. anthropic.claude-haiku-5-5).")
	f.Bool("guardrail-fail-open", false, "When true, transport errors / timeouts produce VerdictAllow with a security event rather than blocking. Default false (fail closed).")

	f.String("deployment-environment", "", "OTel deployment.environment.name resource attribute (e.g. production, staging). Empty falls through to OTEL_DEPLOYMENT_ENVIRONMENT, then to \"local\".")
	f.String("service-name", "", "OTel service.name resource attribute; backends list agents by it (e.g. triage-agent). Empty falls through to OTEL_SERVICE_NAME, then to \"stirrup\". Mirrors observability.serviceName.")
	f.String("service-namespace", "", "OTel service.namespace resource attribute (e.g. stirrup-eval, team-a). Empty falls through to OTEL_SERVICE_NAMESPACE, then to \"stirrup\".")
	f.String("log-export", "none", "Structured-log export: none (stderr only, the default) or otlp (stderr plus an OTLP log exporter using --otel-protocol and --otel-header). Endpoint defaults to --otel-endpoint; OTEL_EXPORTER_OTLP_LOGS_ENDPOINT overrides it. Mirrors observability.logsExport.")

	f.Int("provider-retry-max-attempts", 0, "Maximum HTTP attempts (including the first) for the default provider. 1 disables retry. Hard ceiling: 5. Default (when unset): 3. Currently honoured only by the openai-compatible adapter; the other adapters fall through unconditionally pending their own wire-ups.")
	f.Duration("provider-retry-initial-delay", 0, "Base delay for exponential backoff before jitter, applied between retries on the default provider. Accepts Go duration syntax (e.g. 500ms, 1s). Default (when unset): 500ms.")
	f.Duration("provider-retry-max-delay", 0, "Per-attempt sleep ceiling for the default provider (also caps Retry-After hints). Applies only to the default provider; per-named-provider retry policy requires --config. Hard ceiling: 60s. Default (when unset): 16s.")
	f.Duration("provider-retry-wall-clock", 0, "Wall-clock budget for the entire retry sequence on the default provider. Applies only to the default provider; per-named-provider retry policy requires --config. Hard ceiling: 300s. Default (when unset): 90s.")

	f.String("export-workspace-to", "", "Upload the executor workspace as a gzipped tarball to this URI at the end of every run, before its done event (e.g. gs://bucket/runs/<runId>/workspace.tar.gz). Only gs:// is supported in v1. Mirrors executor.workspaceExportTo.")
	f.Bool("export-workspace-required", true, "Whether a failed workspace export turns a successful run into outcome handoff_failed (non-zero exit). Defaults to true whenever --export-workspace-to is set; pass --export-workspace-required=false to only record the failure. Left unset, the --config value applies. Requires --export-workspace-to. Mirrors executor.workspaceExportRequired.")

	f.Int("max-tool-parallel", 0, "Maximum number of async tool calls dispatched concurrently in a single turn. Range: 1-16. 0 uses the library default (4).")

	f.Bool("escalate-tool-choice", false, "Recover from a first-turn no-tool answer on a workspace-dependent task by retrying with provider-native required tool choice (or a stronger prompt where unsupported). OFF by default. See issue #230.")
	f.Int("escalate-tool-choice-max-retries", 0, "Maximum forced retries per inner-loop run when --escalate-tool-choice is set. Range: 1-3. 0 uses the default (1). No effect unless --escalate-tool-choice is set.")

	f.StringArray("repository-guidance-file", nil, "Repeatable workspace-root file name (e.g. AGENTS.md) loaded into the system prompt after pre-run hooks, read through the executor. Off unless set. No paths, globs, or directory walk. Unpinned files render as advisory guidance that cannot change permissions, mode, or tools. Replaces any repositoryGuidance.files from --config.")
	f.Int("repository-guidance-max-bytes", 0, "Cap on rendered repository guidance, per file and in total. Range: 1-50000. 0 uses the default (32768). Mirrors repositoryGuidance.maxBytes.")
	f.StringArray("repository-guidance-pin", nil, "Repeatable file=sha256 pin promoting a --repository-guidance-file entry to operator-approved guidance. A missing file or digest mismatch fails the run (setup_failed) before any provider call. Mirrors repositoryGuidance.pinnedSha256.")

	f.String("correlation-session-id", "", "Control-plane session identifier stamped on every span as session.id and gen_ai.conversation.id, taking precedence over --name. Never placed in the model's context. Max 199 bytes. Mirrors correlation.sessionId.")
	f.String("correlation-task-id", "", "Control-plane task identifier stamped on every span as stirrup.task.id. Never placed in the model's context. Max 255 bytes. Mirrors correlation.taskId.")
	f.String("correlation-attempt-id", "", "Control-plane attempt identifier stamped on every span as stirrup.attempt.id. Never placed in the model's context. Max 255 bytes. Mirrors correlation.attemptId.")
	f.String("correlation-user-id", "", "Pseudonymous end-user identifier stamped on every span as user.id. Choose a value that is not directly identifying. Max 255 bytes. Mirrors correlation.userId.")
	f.String("correlation-url", "", "Control-plane page for this run, stamped on every span as stirrup.control_plane.url. Must be an absolute http or https URL without credentials. Max 1500 bytes. Mirrors correlation.url.")
	f.StringArray("correlation-label", nil, "Repeatable key=value label stamped on every span as stirrup.label.<key>. Merges over any correlation.labels from --config, a repeated key keeping the last value. Key shape and limits are checked by RunConfig validation (both commands). An empty value is rejected. Mirrors correlation.labels.")

	f.Bool("batch", false, "Use async batch submission for provider turns (50% cost reduction; each turn waits up to --timeout, capped at 3600s). Requires transport=grpc or --config with harnessSidePolling=true for stdio. See docs/batch.md.")

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
	staticValues("otel-metrics-temporality", types.ValidTraceEmitterMetricsTemporalityValues())
	staticValues("otel-platform", types.ValidTraceEmitterPlatformValues())
	staticValues("log-export", types.ValidLogsExportTypeValues())
	staticValues("container-runtime", types.ValidExecutorRuntimeValues())
	staticValues("code-scanner", types.ValidCodeScannerTypeValues())
	staticValues("guardrail", types.ValidGuardRailTypeValues())
	staticValues("provider-compat-profile", types.ValidCompatProfileValues())
	staticValues("provider-endpoint-profile", types.ValidEndpointProfileValues())
	staticValues("tools-profile", types.ValidToolsProfileValues())
	staticValues("lazy-tools-strategy", types.ValidLazyToolStrategyValues())

	// log-level and api-key-header are not validator-closed sets in
	// types/runconfig.go, but operators benefit from a hinted completion.
	staticValues("log-level", []string{"debug", "error", "info", "warn"})
	staticValues("api-key-header", []string{"Authorization", "api-key"})

	_ = cmd.MarkFlagFilename("config", "json")
	_ = cmd.MarkFlagFilename("prompt-file")
	_ = cmd.MarkFlagFilename("output-schema", "json")
	_ = cmd.MarkFlagFilename("gcp-credentials-file", "json")
	_ = cmd.MarkFlagFilename("permission-policy-file", "cedar")
	_ = cmd.MarkFlagFilename("trace", "jsonl")
	_ = cmd.MarkFlagDirname("workspace")
}
