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

### On your machine

```sh
just build # produces ./stirrup and ./stirrup-eval

# simplest example — bare invocation lands in read-only `planning` mode by default
ANTHROPIC_API_KEY=... ./stirrup harness --prompt "Outline a fix for the failing test in main_test.go"

# editable run — opt in to writes and shell access with --mode execution
ANTHROPIC_API_KEY=... ./stirrup harness --mode execution --prompt "Fix the failing test in main_test.go"

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

### Composing configs in a pipeline

For development workflows that build up a `RunConfig` incrementally,
`stirrup run-config` emits the resolved JSON document without
invoking the loop, and `stirrup harness` reads a base config from
stdin so each stage layers one more adjustment before the final stage
runs the agent:

```sh
stirrup run-config --model claude-opus-4-7 \
  | stirrup run-config --max-turns 100 \
  | stirrup run-config --mode execution --executor container \
  | stirrup harness --prompt "refactor module X"
```

`stirrup harness --output-runconfig <path>` captures the exact
config a flag-only invocation *would* have used — useful for
post-mortem replays or pinning a stable configuration. See
[`docs/configuration.md`](docs/configuration.md#building-runconfigs-interactively).

### Executors

The agent runs inside one of four executors, selected with
`--executor` / `executor.type`:

| Executor | Boundary | Notes |
|---|---|---|
| `local` | none (host process) | Trusted local iteration. |
| `container` | a Docker/Podman container | Single-host sandbox with an in-process egress proxy. |
| `k8s` | a Pod on a Kubernetes cluster | Pod-per-run with a hardened `securityContext`, per-tenant RuntimeClass, and `NetworkPolicy` egress. See [`docs/executors/k8s.md`](docs/executors/k8s.md). |
| `api` | no shell (VCS-backed, read-only) | Reviews/plans against a Git host without a workspace. |

The relevant executor flags are:

| Flag | Notes |
|---|---|
| `--executor` | `local` (default), `container`, `k8s`, or `api`. |
| `--image` | Sandbox container image (`container` / `k8s`). |
| `--container-runtime` | OCI runtime (`container`) or Pod `RuntimeClassName` (`k8s`): `gvisor`, `kata-qemu`, `kata-fc`, `kata-clh`, `runc`. |
| `--k8s-namespace` | Namespace for the `k8s` sandbox Pod (required for `k8s`). |
| `--k8s-kubeconfig` | Kubeconfig path; empty prefers in-cluster config then `$KUBECONFIG`. |
| `--k8s-node-selector` | Repeatable `key=value` Pod `nodeSelector` constraint. |
| `--k8s-service-account` | ServiceAccount name; the token is never automounted. |
| `--k8s-egress-proxy-url` | Egress proxy URL (required in `allowlist` network mode). |

#### Running on Kubernetes

```sh
# Apply the standing objects once per cluster (namespace, RBAC, RuntimeClasses):
kubectl apply -f examples/k8s/namespace.yaml
kubectl apply -f examples/k8s/rbac.yaml
kubectl apply -f examples/k8s/runtimeclass.yaml

# Run the agent in a sandbox Pod under gVisor:
ANTHROPIC_API_KEY=... ./stirrup harness \
  --executor k8s \
  --image ghcr.io/rxbynerd/stirrup-sandbox:latest \
  --k8s-namespace stirrup-sandbox \
  --container-runtime gvisor \
  --mode execution \
  --prompt "Fix the failing test in main_test.go"
```

Full reference manifests are under
[`examples/k8s/`](examples/k8s/) and a local `kind` cluster under
[`scripts/dev/`](scripts/dev/). The operator guide —
architecture, config reference, deployment recipes, egress, and
troubleshooting — is [`docs/executors/k8s.md`](docs/executors/k8s.md).

`stirrup harness --output <text|json|none>` (alias `-o`) selects the
post-run summary surface. `text` (default) prints today's stderr
summary, `json` emits a single `STIRRUP_RESULT` line on stdout
parseable as [`types.RunResult`](types/result.go), and `none`
suppresses both. The structured shape reuses the existing
`resultSink.type=stdout-json` payload, so a `--output=json` invocation
and a `resultSink`-configured run produce the same wire format. See
[`docs/configuration.md`](docs/configuration.md#run-output).

### In GitHub Actions

See [`.github/workflows/smoke-anthropic.yml`](.github/workflows/smoke-anthropic.yml) for an example of using `stirrup harness` in a GitHub Actions workflow via [Anthropic Workload Identity Federation](https://platform.claude.com/docs/en/manage-claude/workload-identity-federation).

## Documentation

| Topic | Doc |
|---|---|
| Component model, agentic loop, deep dives | [`docs/architecture.md`](docs/architecture.md) |
| CLI flags, `RunConfig` precedence, examples | [`docs/configuration.md`](docs/configuration.md) |
| Production deployment via `stirrup job` (K8s, gRPC) | [`docs/deployment.md`](docs/deployment.md) |
| Kubernetes executor (Pod-per-run sandbox) | [`docs/executors/k8s.md`](docs/executors/k8s.md) |
| Running stirrup as a Google Cloud Run job | [`docs/cloud-run-jobs.md`](docs/cloud-run-jobs.md) |
| Five safety rings (operator guide) | [`docs/safety-rings.md`](docs/safety-rings.md) |
| In-harness security foundations | [`docs/security.md`](docs/security.md) |
| LLM-based safety classifier (`GuardRail`) | [`docs/guardrails.md`](docs/guardrails.md) |
| Eval framework (`stirrup-eval`) | [`docs/eval.md`](docs/eval.md) |
| Provider adapters | [`docs/providers.md`](docs/providers.md) |
| Batch mode | [`docs/sandbox.md`](docs/batch.md) |
| Cross-cloud credential federation | [`docs/credential-federation.md`](docs/credential-federation.md) |
| Anthropic Workload Identity Federation | [`docs/anthropic-wif.md`](docs/anthropic-wif.md) |
| Azure Workload Identity Federation | [`docs/azure-workload-identity.md`](docs/azure-workload-identity.md) |
| OpenAI Workload Identity Federation | [`docs/openai-wif.md`](docs/openai-wif.md) |
| Cloud observability backends (Grafana, etc.) | [`docs/observability-cloud.md`](docs/observability-cloud.md) |
| Project philosophy | [`docs/philosophy.md`](docs/philosophy.md) |
| Per-package layout (orientation for AI agents) | [`AGENTS.md`](AGENTS.md) |
| Build, test, lint, commit conventions | [`CONTRIBUTING.md`](CONTRIBUTING.md) |
| Security disclosure policy | [`SECURITY.md`](SECURITY.md) |

## License

Apache 2.0. See [`LICENSE`](LICENSE).
