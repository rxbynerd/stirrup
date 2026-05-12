# stirrup

A **production-grade coding harness** built for **safe
autonomous operation**.

The majority of coding harnesses are single-player tools, focused on
excellent local experiences, providing trend-setting TUIs or
comprehensive IDE integration. Any kind of API/RPC channel is an
afterthought, often just to support a desktop client.

Stirrup is intentionally designed and built from the ground up with the following tenets in mind:

- **Production-grade safety** provided by [a comprehensive set of rings](docs/safety-rings.md), designed to provide defence-in-depth security.
- **Autonomous operation** via [a gRPC interface](proto/harness/v1/harness.proto), allowing remote control & management of the coding harness.
- **Composability** via [a single declarative `RunConfig`](docs/configuration.md), allowing easy swapping of components without modifying the loop.
- [**Evaluations**](docs/eval.md) as a first-class feature, supporting both Stirrup's development and its use-cases.

## Quick start

Stirrup has two main modes of operation:

- `stirrup harness` is for local/one-off tasks that do not require a control plane.
- `stirrup job` connects to the control plane given at `CONTROL_PLANE_ADDR`, intended to be deployed as a Kubernetes `Job`.

Deployment targets:

- Kubernetes `Job` (the canonical production target) — see [`docs/deployment.md`](docs/deployment.md).
- Google Cloud Run job (one-shot batch, exit-on-completion) — see [`docs/cloud-run-jobs.md`](docs/cloud-run-jobs.md).
- Embedded library via [`harness/harnessapi`](harness/harnessapi) — when the agentic loop runs inside a larger Go process.

### On your machine

```sh
just build # produces ./stirrup and ./stirrup-eval

# simplest example
ANTHROPIC_API_KEY=... ./stirrup harness --prompt "Fix the failing test in main_test.go"

# using example full RunConfig, see examples/runconfig/README.md for further details
./stirrup harness \
  --config examples/runconfig/full.json \
  --prompt "Fix the failing test in main_test.go"

# using AWS Bedrock (AWS_PROFILE should be set)
./stirrup harness \
  --prompt "Give me a summary of the safety rings doc" \
  --provider bedrock \
  --model global.anthropic.claude-sonnet-4-6 \
  --workspace .

# using OpenAI Responses
OPENAI_KEY="$(op read "op://Private/qeu6gafabhkpsm6hhzattx6p4m/credential")" ./stirrup harness \
  --api-key-ref secret://OPENAI_KEY \
  --provider openai-responses \
  --model gpt-5.4-nano \
  --prompt "Review the last commit for factual inaccuracies"

```

### In GitHub Actions

See [`.github/workflows/smoke-anthropic.yml`](.github/workflows/smoke-anthropic.yml) for an example of using `stirrup harness` in a GitHub Actions workflow via [Anthropic Workload Identity Federation](https://platform.claude.com/docs/en/manage-claude/workload-identity-federation).

## Documentation

| Topic | Doc |
|---|---|
| Component model, agentic loop, deep dives | [`docs/architecture.md`](docs/architecture.md) |
| CLI flags, `RunConfig` precedence, examples | [`docs/configuration.md`](docs/configuration.md) |
| Production deployment via `stirrup job` (K8s, gRPC) | [`docs/deployment.md`](docs/deployment.md) |
| Running stirrup as a Google Cloud Run job | [`docs/cloud-run-jobs.md`](docs/cloud-run-jobs.md) |
| Five safety rings (operator guide) | [`docs/safety-rings.md`](docs/safety-rings.md) |
| In-harness security foundations | [`docs/security.md`](docs/security.md) |
| LLM-based safety classifier (`GuardRail`) | [`docs/guardrails.md`](docs/guardrails.md) |
| Eval framework (`stirrup-eval`) | [`docs/eval.md`](docs/eval.md) |
| Provider adapters | [`docs/providers.md`](docs/providers.md) |
| Cross-cloud credential federation | [`docs/credential-federation.md`](docs/credential-federation.md) |
| Anthropic Workload Identity Federation | [`docs/anthropic-wif.md`](docs/anthropic-wif.md) |
| Azure Workload Identity Federation | [`docs/azure-workload-identity.md`](docs/azure-workload-identity.md) |
| Cloud observability backends (Grafana, etc.) | [`docs/observability-cloud.md`](docs/observability-cloud.md) |
| Project philosophy | [`docs/philosophy.md`](docs/philosophy.md) |
| Per-package layout (orientation for AI agents) | [`AGENTS.md`](AGENTS.md) |
| Build, test, lint, commit conventions | [`CONTRIBUTING.md`](CONTRIBUTING.md) |
| Security disclosure policy | [`SECURITY.md`](SECURITY.md) |

## License

Apache 2.0. See [`LICENSE`](LICENSE).
