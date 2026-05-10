# stirrup

A **production-grade coding agent harness** built for **secure
autonomous operation**. Designed to run as a short-lived job — the
control plane starts it per task, it runs the agentic loop to
completion, and exits. There is no in-process session store, no
cross-tenant memory, and no inbound port to expose.

> **Status:** pre-1.0. The public API is best-effort stable inside
> `harnessapi/` and `types/`; the rest of `harness/internal/*` is not.
> The release pipeline (tag-driven cross-platform binaries, SBOMs,
> GHCR image) is wired but no `v*.*.*` tag has been cut yet.

## Why stirrup

**A pure-function core.** The agentic loop depends only on
interfaces. Thirteen components — provider, router, prompt, context
strategy, tools, executor, edit strategy, verifier, permission
policy, transport, git, tracing, guardrail — are composed via a
single declarative `RunConfig`. Swap the provider or the executor
without touching the loop.

**Five deterministic safety rings.** Stirrup runs LLM-produced code,
so it composes five layered controls the agent cannot circumvent:
kernel-isolation runtime classes, an in-process egress allowlist
proxy, a Cedar-backed policy engine, the Rule-of-Two structural
invariant, and a post-edit code scanner. Each ring catches a
different class of attack at a different point in the run.

**Secrets never live in `RunConfig`.** API keys are `secret://`
references resolved through env vars, files, or AWS SSM. The
`slog.Handler` that writes logs runs every string through a
seven-pattern scrubber before any handler sees it — secret leakage
through logs is structurally impossible.

**Cross-cloud credential federation.** GKE Workload Identity, AWS
IRSA, Azure IMDS, GitHub Actions OIDC, Anthropic WIF, and Azure
Entra ID federation are first-class. No static API keys in CI/CD.

**Five providers, hand-rolled.** Anthropic SSE, AWS Bedrock
Converse, OpenAI Chat Completions, OpenAI Responses, and Google
Gemini via Vertex AI. Each adapter is a few hundred lines of stdlib
HTTP — every line is auditable.

**The eval framework is a peer, not an afterthought.** Deterministic
replay providers and replay executors mean CI eval suites run
without hitting a paid API. Live runs, replay evaluation, drift
detection, failure mining, and lab-vs-production comparison ship in
`stirrup-eval`.

## Try it

Build the binary (Go 1.26.1+):

```sh
go build -o stirrup ./harness/cmd/stirrup
```

Run a one-off task against the default Anthropic provider:

```sh
ANTHROPIC_API_KEY=... ./stirrup harness --prompt "Fix the failing test in main_test.go"
```

Or load a fully-populated `RunConfig` from a file:

```sh
./stirrup harness --config examples/runconfig/full.json \
  --prompt "Fix the failing test in main_test.go"
```

## A tour: secure autonomous run

The invocation below runs a task inside a gVisor-isolated container,
evaluates every tool call through a Cedar policy, scans every
successful edit, applies the Rule-of-Two structural invariant, and
ships traces and metrics over OTLP — all from a single `RunConfig`
file:

```sh
./stirrup harness \
  --config examples/runconfig/full.json \
  --prompt "Refactor the cache layer to share a single connection pool"
```

The shipped [`full.json`](examples/runconfig/full.json) stitches
together every safety ring:

```jsonc
{
  // Ring 1: kernel-isolation runtime class.
  // runc (default) | runsc (gVisor) | kata, kata-qemu, kata-fc.
  "executor": {
    "type": "container",
    "image": "ghcr.io/rxbynerd/stirrup:latest",
    "runtime": "runsc",
    "network": { "mode": "none" },          // or "allowlist" → Ring 2
    "resources": { "cpus": 2.0, "memoryMb": 2048, "diskMb": 8192, "pids": 256 }
  },

  // Ring 3: Cedar policy engine evaluates each tool call as
  //   (User::"<runId>", Action::"tool:<name>", Tool::"<name>", {input, workspace})
  // No-decision falls through to the configured fallback.
  "permissionPolicy": {
    "type": "policy-engine",
    "policyFile": "examples/policies/destructive-shell.cedar",
    "fallback": "deny-side-effects"
  },

  // Ring 4: Rule-of-Two structural invariant.
  // Rejects RunConfigs that hold untrusted input + sensitive data +
  // external comms simultaneously, unless gated by ask-upstream.
  "ruleOfTwo": { "enforce": true },

  // Ring 5: post-edit static analysis. Block findings roll back the
  // write; warn findings emit code_scan_warning and continue.
  "codeScanner": { "type": "patterns" },

  // Probabilistic guard layered on top of the deterministic rings.
  // PreTurn / PreTool / PostTurn LLM classifier.
  "guardRail": {
    "type": "granite-guardian",
    "phases": ["pre_turn", "pre_tool", "post_turn"]
  },

  // Multi-strategy edits: udiff → search-replace → whole-file fallback.
  // The factory wraps the chosen strategy with the codeScanner above.
  "editStrategy": { "type": "multi", "fuzzyThreshold": 0.85 },

  // Deterministic git so reviews see reproducible commits.
  "gitStrategy": { "type": "deterministic" },

  // OpenTelemetry traces and metrics over OTLP/gRPC.
  // (Use protocol: "http/protobuf" for managed gateways.)
  "traceEmitter": { "type": "otel", "endpoint": "localhost:4317" },

  // Anthropic provider with a secret reference — never a raw key.
  "provider": {
    "type": "anthropic",
    "apiKeyRef": "secret://ANTHROPIC_API_KEY"
  }
}
```

Threat model, posture trade-offs, and what each ring does *not*
catch are documented in
[`docs/safety-rings.md`](docs/safety-rings.md). The annotated
walkthrough of `full.json` lives at
[`examples/runconfig/README.md`](examples/runconfig/README.md).

## Documentation

| Topic | Doc |
|---|---|
| Component model, agentic loop, deep dives | [`docs/architecture.md`](docs/architecture.md) |
| CLI flags, `RunConfig` precedence, examples | [`docs/configuration.md`](docs/configuration.md) |
| Production deployment via `stirrup job` (K8s, gRPC) | [`docs/deployment.md`](docs/deployment.md) |
| Five safety rings (operator guide) | [`docs/safety-rings.md`](docs/safety-rings.md) |
| In-harness security foundations | [`docs/security.md`](docs/security.md) |
| LLM-based safety classifier (`GuardRail`) | [`docs/guardrails.md`](docs/guardrails.md) |
| Eval framework (`stirrup-eval`) | [`docs/eval.md`](docs/eval.md) |
| Provider adapters | [`docs/providers.md`](docs/providers.md) |
| Cross-cloud credential federation | [`docs/credential-federation.md`](docs/credential-federation.md) |
| Anthropic Workload Identity Federation | [`docs/anthropic-wif.md`](docs/anthropic-wif.md) |
| Azure Workload Identity Federation | [`docs/azure-workload-identity.md`](docs/azure-workload-identity.md) |
| Cloud observability backends (Grafana, etc.) | [`docs/observability-cloud.md`](docs/observability-cloud.md) |
| Per-package layout (orientation for AI agents) | [`AGENTS.md`](AGENTS.md) |
| Build, test, lint, commit conventions | [`CONTRIBUTING.md`](CONTRIBUTING.md) |
| Security disclosure policy | [`SECURITY.md`](SECURITY.md) |

## License

Apache 2.0. See [`LICENSE`](LICENSE).
