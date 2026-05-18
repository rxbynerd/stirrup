# RunConfig examples

This directory contains example `RunConfig` JSON files for the
`stirrup harness --config <path>` flag.

## Source of truth

The canonical schema for these files is the protobuf definition at
[`proto/harness/v1/harness.proto`](../../proto/harness/v1/harness.proto).
The Go types in [`types/runconfig.go`](../../types/runconfig.go) mirror
that schema and are what the loader unmarshals into.

The CLI loader (`loadRunConfigFile`) uses
`encoding/json.DisallowUnknownFields`, so a typo in a field name
fails fast with a clear error rather than being silently dropped.

## Files

| File | What it demonstrates |
|---|---|
| [`full.json`](full.json) | All-rings-active showcase: gVisor-isolated container, Cedar policy engine with `deny-side-effects` fallback, Rule of Two enforced, post-edit code scanner, Granite Guardian guardrail across all three phases, multi edit strategy, OTel traces and metrics, dynamic model router, deterministic git, and one MCP server. Passes `types.ValidateRunConfig` end-to-end. |
| [`openai_responses.json`](openai_responses.json) | OpenAI Responses API provider (`POST /v1/responses`), local executor, multi edit strategy, JSONL trace emitter, static router on `gpt-4.1`. Use this template when you want the Responses wire format (top-level `instructions`, typed `input[]` items, `max_output_tokens`) rather than Chat Completions. |
| [`azure-openai.json`](azure-openai.json) | Azure OpenAI Foundry's Responses endpoint via the same `openai-responses` provider type, with `apiKeyHeader: "api-key"` for key-based auth and `queryParams: {"api-version": "preview"}` for the api-version pin. Switch the `apiKeyHeader` to an empty string to use Entra ID bearer tokens (the default behaviour) instead. |
| [`vertex-gemini.json`](vertex-gemini.json) | Vertex AI Gemini provider on `gemini-2.5-pro`. Auth is GCP IAM (not an AI Studio API key): the credential layer defaults to Application Default Credentials, so `GOOGLE_APPLICATION_CREDENTIALS=/path/to/sa.json` or running on a workload-identity-enabled GKE/GCE instance is sufficient. User-mode `gcloud auth application-default login` credentials are explicitly rejected for autonomy reasons; configure a service account or workload identity instead. Override the project, location, or service-account file via `--gcp-project`, `--gcp-location`, or `--gcp-credentials-file`. |
| [`vertex-gemini-wif.json`](vertex-gemini-wif.json) | Vertex AI Gemini reached from a non-GCP runtime via Workload Identity Federation. The example surfaces an EKS-style `aws-irsa` token source, exchanges it at `sts.googleapis.com`, and impersonates a target service account through `iamcredentials.generateAccessToken`. Swap the `tokenSource.type` to `azure-imds`, `github-actions-oidc`, `file`, or `env` for other runtimes. See [`docs/credential-federation.md`](../../docs/credential-federation.md) for the matching GCP-side IAM setup. |
| [`cloud-run-vertex-gemini.json`](cloud-run-vertex-gemini.json) | Vertex AI Gemini from inside a Cloud Run job. Auth pins to `gcp-workload-identity` against the metadata server (the explicit fail-closed variant of `gcp-default`), the trace emitter uploads JSONL to GCS, and `executor.workspaceExportTo` tarballs the workspace to GCS at end-of-run. `resultSink.type=stdout-json` writes a single `STIRRUP_RESULT <json>` line to stdout — Cloud Run pipes stdout to Cloud Logging, so the caller extracts the structured result with `gcloud logging read`. See [`docs/cloud-run-jobs.md`](../../docs/cloud-run-jobs.md) for the operator walkthrough (APIs, results bucket, service-account IAM, `gcloud run jobs deploy`, both result-collection paths). |
| [`anthropic-wif-github-actions.json`](anthropic-wif-github-actions.json) | Anthropic Messages API authenticated via Workload Identity Federation from a GitHub Actions runner. Exchanges the workflow's OIDC JWT (`permissions: id-token: write`) for a short-lived Anthropic access token at `/v1/oauth/token`. No `apiKeyRef` — WIF is mutually exclusive with static API keys. See [`docs/anthropic-wif.md`](../../docs/anthropic-wif.md) for the matching Anthropic Console setup. |
| [`anthropic-wif-eks-irsa.json`](anthropic-wif-eks-irsa.json) | Anthropic Messages API authenticated via Workload Identity Federation from an EKS Pod with IRSA. The `aws-irsa` token source reads `AWS_WEB_IDENTITY_TOKEN_FILE` (the projected service-account token) and exchanges it for an Anthropic access token. Same federation rule shape as the GHA variant; only the IdP differs. |
| [`azure-openai-wif-aks.json`](azure-openai-wif-aks.json) | Azure OpenAI reached from AKS via Entra ID Workload Identity Federation. Uses the `openai-compatible` provider with `credential.type=azure-workload-identity` and a `file` token source pointing at the projected service-account token at `/var/run/secrets/azure/tokens/azure-identity-token`. See [`docs/azure-workload-identity.md`](../../docs/azure-workload-identity.md) for the Azure-side App Registration + federated identity credential setup. |
| [`azure-openai-wif-github-actions.json`](azure-openai-wif-github-actions.json) | Azure OpenAI reached from GitHub Actions via Entra ID Workload Identity Federation. Same shape as the AKS variant but with a `github-actions-oidc` token source and audience `api://AzureADTokenExchange`. The workflow needs `permissions: id-token: write`. |
| [`grafana-cloud.json`](grafana-cloud.json) | Native OTLP/HTTP export to Grafana Cloud's managed gateway. Uses `traceEmitter.protocol: "http/protobuf"`, the `/otlp` gateway base path, and `headers.Authorization: "secret://GRAFANA_CLOUD_AUTH"` (resolved from `Basic <base64(instanceID:apiToken)>` at exporter init). No Alloy/OTel-Collector sidecar needed. See [`docs/observability-cloud.md`](../../docs/observability-cloud.md) for the operator walkthrough and the equivalent shape for Honeycomb / Datadog / GCP Cloud Trace gateways. |

## Precedence

When the user passes both `--config <path>` and individual flags, the
order of precedence is:

1. **File** — `--config` populates the full `RunConfig`.
2. **Explicit flags** — flags whose `cmd.Flags().Changed(...)` bit is
   set replace the corresponding file-provided field.
3. **Defaults** — flags left at their default value do **not**
   override the file. This is what makes `--config` ergonomic:
   defaults can stay defaults while the file's intent is preserved.

The positional `prompt` argument is a fallback only. It fills the
prompt slot when the file omits it and `--prompt` is not set, but
neither the file's `prompt` nor an explicit `--prompt` is overridden
by a positional.

## Annotated example walkthrough

The shipped `full.json` exercises every component selection and is
the most comprehensive example. It is also the showcase referenced
from the project [README](../../README.md): every safety ring is
active and Rule of Two is enforced.

```jsonc
{
  // Identity. Optional — the CLI generates a RunID if omitted.
  "runId": "example-full-runconfig",

  // Mode. "execution" allows write tools; "planning"/"review"/
  // "research"/"toil" enforce the read-only invariant.
  "mode": "execution",
  "prompt": "Replace this prompt with the task you want the harness to run.",

  // Untrusted context. The control plane populates these from issue
  // bodies, PR comments, etc. Each entry is wrapped in
  // <untrusted_context> tags before being shown to the model.
  "dynamicContext": {
    "issue_body": {
      "value": "External issue body or PR comment text. Treated as data, not instructions."
    }
  },

  // Provider + credentials. apiKeyRef is a secret:// reference, never
  // a raw key.
  "provider": {
    "type": "anthropic",
    "apiKeyRef": "secret://ANTHROPIC_API_KEY"
  },

  // Dynamic router: cheap for short turns, expensive past the
  // configured thresholds. cheap/expensive providers must reference
  // either the top-level provider or an entry in providers{}.
  "modelRouter": {
    "type": "dynamic",
    "provider": "anthropic",
    "model": "claude-sonnet-4-6",
    "cheapProvider": "anthropic",
    "cheapModel": "claude-haiku-4-6",
    "expensiveProvider": "anthropic",
    "expensiveModel": "claude-sonnet-4-6",
    "expensiveTurnThreshold": 5,
    "expensiveTokenThreshold": 50000
  },

  "promptBuilder": { "type": "default" },
  "contextStrategy": { "type": "sliding-window", "maxTokens": 200000 },

  // Ring 1: kernel-isolation runtime class. runc is the engine
  // default; runsc is gVisor; kata variants are kernel-VM isolation.
  // The runtime must be registered with the host Docker/Podman
  // daemon. capDrop: ALL, no-new-privileges, network: none are
  // applied regardless of what the file says.
  "executor": {
    "type": "container",
    "image": "ghcr.io/rxbynerd/stirrup:latest",
    "runtime": "runsc",
    "network": { "mode": "none" },
    "resources": { "cpus": 2.0, "memoryMb": 2048, "diskMb": 8192, "pids": 256 }
  },

  // Multi-strategy edit: unified edit_file tool with fallback across
  // udiff -> search-replace -> whole-file. fuzzyThreshold tunes the
  // udiff fallback's similarity matching.
  "editStrategy": { "type": "multi", "fuzzyThreshold": 0.85 },

  // Verifier: runs `command` after each turn. Other types: "none",
  // "llm-judge", "composite" (chains sub-verifiers — composite is
  // available only via --config, not via --verifier).
  "verifier": {
    "type": "test-runner",
    "command": "go test ./...",
    "timeout": 300
  },

  // Ring 3: Cedar policy engine evaluates each tool call. fallback is
  // consulted when no policy matches; chained policy engines are
  // rejected to avoid no-decision loops.
  "permissionPolicy": {
    "type": "policy-engine",
    "policyFile": "examples/policies/destructive-shell.cedar",
    "fallback": "deny-side-effects"
  },

  // Deterministic git: writes commits with stable author/date so
  // diffs are reproducible.
  "gitStrategy": { "type": "deterministic" },

  // Transport. "stdio" emits to the local process; "grpc" needs an
  // address and is what production K8s deployments use.
  "transport": { "type": "stdio" },

  // OpenTelemetry trace emitter (OTLP/gRPC by default).
  "traceEmitter": {
    "type": "otel",
    "endpoint": "localhost:4317",
    "metricsEndpoint": "localhost:4317"
  },

  // Tools. builtIn[] selects which built-in tools are exposed; the
  // multi-strategy edit_file tool is registered when "edit_file" is
  // present. mcpServers[] connects to remote MCP endpoints; tool
  // names are namespaced as mcp_{name}_{toolName}.
  "tools": {
    "builtIn": [
      "read_file",
      "list_directory",
      "search_files",
      "edit_file",
      "run_command",
      "web_fetch",
      "spawn_agent"
    ],
    "mcpServers": [
      {
        "name": "example",
        "uri": "https://example.com/mcp",
        "apiKeyRef": "secret://EXAMPLE_MCP_KEY"
      }
    ]
  },

  // Ring 4: Rule of Two structural invariant. With dynamicContext +
  // web_fetch + MCP, two of the three flags hold (untrusted input
  // and external comms). Sensitive data is not asserted here, so
  // enforcement passes; the loop emits rule_of_two_warning so the
  // operator can see two of three holding.
  "ruleOfTwo": { "enforce": true },

  // Ring 5: post-edit static analysis. Default is "patterns" for
  // execution mode (mode-aware). blockOnWarn promotes warn findings
  // to block; useful for production pinning.
  "codeScanner": { "type": "patterns", "blockOnWarn": false },

  // Probabilistic guardrail layered over the deterministic rings.
  // PreTurn / PreTool / PostTurn LLM classifier.
  "guardRail": {
    "type": "granite-guardian",
    "endpoint": "http://127.0.0.1:1234",
    "model": "ibm-granite/granite-guardian-4.1-8b",
    "phases": ["pre_turn", "pre_tool", "post_turn"],
    "timeoutMs": 1500,
    "minChunkChars": 256,
    "failOpen": false
  },

  // OTel resource attributes shared by traces and metrics.
  "observability": {
    "environment": "production",
    "serviceNamespace": "stirrup-eval"
  },

  // Limits. ValidateRunConfig caps maxTurns at 100, timeout at 3600s,
  // followUpGrace at 3600s, maxCostBudget at $100, maxTokenBudget at
  // 50M.
  "maxTurns": 20,
  "timeout": 600,
  "logLevel": "info"
}
```

## Running the example

```sh
./stirrup harness --config examples/runconfig/full.json
```

To override the prompt without editing the file:

```sh
./stirrup harness --config examples/runconfig/full.json \
  --prompt "Add a new test for the foo package"
```

Any flag listed in
[`docs/configuration.md`](../../docs/configuration.md) is honoured
as an override when explicitly set. Flags left at their default
value do not override the file.
