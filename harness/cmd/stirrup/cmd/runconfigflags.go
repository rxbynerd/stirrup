package cmd

import "github.com/spf13/cobra"

// addRunConfigFlags registers every flag that maps to a RunConfig field
// (or to the prompt-resolution chain that lands on RunConfig.Prompt).
// Both `stirrup harness` and `stirrup run-config` call this so the two
// subcommands cannot drift on default values, help text, or supported
// fields. Pure CLI-behaviour flags that do not round-trip through
// RunConfig (--export-workspace-required, --output-runconfig) live on
// the harness command directly; the run-config subcommand adds its own
// --validate / --compact / --redact afterwards.
//
// Flag defaults are preserved verbatim from the pre-refactor
// harnessCmd.init() body so a flag-only invocation of either command
// produces identical RunConfigs.
func addRunConfigFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("config", "", "Path to a JSON RunConfig file (mirrors proto/harness/v1/harness.proto). Use \"-\" to read from stdin. Explicit flags still override individual fields; unset flags do not.")
	f.StringP("mode", "m", "planning", "Run mode: execution, planning, review, research, toil. Default is the read-only `planning` mode (no edits, no shell, deny-side-effects policy); pass `--mode execution` for editable runs with shell access.")
	f.String("model", "claude-sonnet-4-6", "Model to use (sets ModelRouter.Model; for dynamic/per-mode routers in --config files this only sets the default-model field, not the cheap/expensive override)")
	f.String("provider", "anthropic", "Provider type: anthropic, bedrock, gemini (Vertex AI), openai-compatible (Chat Completions), openai-responses (Responses API). The two OpenAI variants speak different wire formats and must be selected explicitly.")
	f.String("api-key-ref", "secret://ANTHROPIC_API_KEY", "Secret reference for API key")
	f.String("base-url", "", "API base URL for openai-compatible / openai-responses providers (e.g. https://<resource>.openai.azure.com/openai/v1)")
	f.String("api-key-header", "", "Header name for sending the API key. Empty = Authorization: Bearer (default). Set to \"api-key\" for Azure OpenAI key auth.")
	f.StringArray("query-param", nil, "Repeatable key=value query parameter appended to every provider request URL (e.g. api-version=preview). Used by the openai-* adapters.")
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

	f.String("executor", "local", "Executor: local, container, api")
	f.String("edit-strategy", "multi", "Edit strategy: whole-file, search-replace, udiff, multi (composite available only via --config)")
	f.String("verifier", "none", "Verifier: none, test-runner, llm-judge (composite available only via --config)")
	f.String("git-strategy", "none", "Git strategy: none, deterministic")
	f.String("trace-emitter", "jsonl", "Trace emitter: jsonl, otel, gcs")
	f.String("otel-endpoint", "", "OTLP endpoint for the otel trace emitter (default: localhost:4317 for grpc; full URL ending in the gateway base path for http/protobuf, e.g. https://otlp-gateway-prod-us-east-0.grafana.net/otlp)")
	f.String("otel-protocol", "", "OTLP wire protocol for the otel trace emitter: \"\" (default — grpc), grpc, http/protobuf. HTTP/JSON is not supported; managed gateways like Grafana Cloud use http/protobuf. See docs/observability-cloud.md.")

	f.String("container-runtime", "", "OCI runtime for the container executor: runc, runsc (gVisor), kata, kata-qemu, kata-fc. Empty means engine default (typically runc). Requires the runtime to be registered with the host Docker/Podman daemon — see docs/safety-rings.md.")
	f.String("permission-policy-file", "", "Path to a Cedar policy file for the policy-engine PermissionPolicy. When set and --permission-policy is unset elsewhere, also implies permissionPolicy.type=policy-engine. See examples/policies/ for starters.")
	f.String("code-scanner", "", "CodeScanner type: none, patterns, semgrep, composite. Composite requires --config (codeScanner.scanners). Empty defers to the mode-aware default (patterns for execution, none for read-only modes).")

	f.String("guardrail", "", "GuardRail type: none, granite-guardian, composite, cloud-judge. Composite requires --config (guardRail.stages).")
	f.String("guardrail-endpoint", "", "Endpoint URL for the granite-guardian or cloud-judge adapter.")
	f.String("guardrail-model", "", "Model identifier for the GuardRail classifier. Granite-guardian default: ibm-granite/granite-guardian-4.1-8b. Cloud-judge default: claude-haiku-4-5-20251001 (Anthropic API format) — when the primary provider is Bedrock, use the Bedrock-format ID (e.g. us.anthropic.claude-haiku-4-5-20251001-v1:0).")
	f.Bool("guardrail-fail-open", false, "When true, transport errors / timeouts produce VerdictAllow with a security event rather than blocking. Default false (fail closed).")

	f.String("deployment-environment", "", "OTel deployment.environment resource attribute (e.g. production, staging). Empty falls through to OTEL_DEPLOYMENT_ENVIRONMENT, then to \"local\".")
	f.String("service-namespace", "", "OTel service.namespace resource attribute (e.g. stirrup-eval, team-a). Empty falls through to OTEL_SERVICE_NAMESPACE, then to \"stirrup\".")

	f.Int("provider-retry-max-attempts", 0, "Maximum HTTP attempts (including the first) for the default provider. 1 disables retry. Hard ceiling: 5. Default (when unset): 3. Currently honoured only by the openai-compatible adapter; the other adapters fall through unconditionally pending their own wire-ups.")
	f.Duration("provider-retry-initial-delay", 0, "Base delay for exponential backoff before jitter, applied between retries on the default provider. Accepts Go duration syntax (e.g. 500ms, 1s). Default (when unset): 500ms.")
	f.Duration("provider-retry-max-delay", 0, "Per-attempt sleep ceiling for the default provider (also caps Retry-After hints). Applies only to the default provider; per-named-provider retry policy requires --config. Hard ceiling: 60s. Default (when unset): 16s.")
	f.Duration("provider-retry-wall-clock", 0, "Wall-clock budget for the entire retry sequence on the default provider. Applies only to the default provider; per-named-provider retry policy requires --config. Hard ceiling: 300s. Default (when unset): 90s.")

	f.String("export-workspace-to", "", "Upload the executor workspace as a gzipped tarball to this URI at end-of-run (e.g. gs://bucket/runs/<runId>/workspace.tar.gz). Only gs:// is supported in v1. Mirrors executor.workspaceExportTo.")

	f.Int("max-tool-parallel", 0, "Maximum number of async tool calls dispatched concurrently in a single turn. Range: 1-16. 0 uses the library default (4).")

	f.Bool("batch", false, "Use async batch submission for provider turns (50% cost reduction, up to 24h latency). Requires transport=grpc or --config with harnessSidePolling=true for stdio. See docs/sandbox.md.")
}
