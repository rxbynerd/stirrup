## Philisophy

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
