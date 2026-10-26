# Philosophy

**A pure-function core.** The agentic loop depends only on
interfaces. Thirteen components — provider, router, prompt, context
strategy, tools, executor, edit strategy, verifier, permission
policy, transport, git, tracing, guardrail — are composed via a
single declarative `RunConfig`. Swap the provider or the executor
without touching the loop.

**Five deterministic safety rings.** Stirrup runs LLM-produced code,
so it composes five layered controls: kernel-isolation runtime
classes, an in-process egress allowlist proxy, a Cedar-backed policy
engine, the Rule-of-Two structural invariant, and a post-edit code
scanner. Every ring is evaluated outside the model, so a prompt cannot
change its decision. Each ring covers specific attack shapes at a
different point in the run and has documented limits; see
[Safety rings](safety-rings.md).

**Secrets never live in `RunConfig`.** API keys are `secret://`
references resolved through env vars, files, or AWS SSM. The
`slog.Handler` that writes logs runs every string through a
credential-pattern scrubber before any handler sees it — defence
in depth against recognised credential formats.

**Cross-cloud credential federation.** GKE Workload Identity, AWS
IRSA, Azure IMDS, GitHub Actions OIDC, Anthropic WIF, and Azure
Entra ID federation are first-class. No static API keys in CI/CD.

**Five providers, hand-rolled.** Anthropic SSE, AWS Bedrock
Converse, OpenAI Chat Completions, OpenAI Responses, and Google
Gemini via Vertex AI. Each adapter is a few hundred lines of stdlib
HTTP — every line is auditable.

**Built for hosted runs, not a laptop.** The harness is designed
to run inside a sandboxed, hosted environment driven by a control
plane, whether the run is fully autonomous or watched and steered
by a person.

**The control stream is for real-time control; telemetry is for
long-term analysis.** The gRPC stream carries what a control plane
needs while a run is in flight: a live transcript to show, approvals
and tool results to answer, and input that steers the run. Trace
emitters and OTel exporters carry what operators need afterwards:
durable records for debugging, cost, evaluation, and dashboards. The
two are independent. A run streams to its control plane whatever
telemetry it exports, and the stream is not a substitute for a
durable record.

**The eval framework is a peer, not an afterthought.** Deterministic
replay providers and replay executors mean CI eval suites run
without hitting a paid API. Live runs, replay evaluation, drift
detection, failure mining, and lab-vs-production comparison ship in
`stirrup-eval`.
