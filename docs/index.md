# Stirrup documentation

Stirrup is an operator-owned coding worker. The operator chooses the
model provider, the execution environment, the permission policy and
the control plane, and each run leaves evidence that can be inspected
afterwards. The main ways to start a run are from the command line
with `stirrup harness` and from a control plane over gRPC with
`stirrup job`.

Stirrup is not a control plane or a hosted service, and it is built
for sandboxed, hosted runs rather than as an assistant on a laptop.
Each process is a short-lived job: it is started per task, runs the
agentic loop to completion and exits, with no session store and no
memory shared across tenants. `stirrup job` connects to a control plane
that the operator provides;
[`examples/controlplane/`](../examples/controlplane/README.md) is a
minimal one for trying the contract.

The pages below are grouped by what the reader is doing. A newcomer
can read them in this order:

1. The [README](../README.md), for what stirrup is and a first run from
   a source checkout. Building needs Go and `just`; see
   [Environment](../CONTRIBUTING.md#environment).
2. [Philosophy](philosophy.md), then [the thirteen
   components](architecture.md#the-thirteen-components) and [the
   agentic loop](architecture.md#the-agentic-loop) in Architecture, for
   how the harness is put together.
3. [Safety rings](safety-rings.md), for the controls an operator can
   turn on and how they combine.
4. Configuration: how [flags, files and stdin
   combine](configuration.md#precedence), [building a
   `RunConfig`](configuration.md#building-runconfigs-interactively), and
   checking one with the [dry-run preflight](configuration.md#dry-run-preflight).
5. [Production deployment](deployment.md) for an operator deploying
   `stirrup job`, or the [Integration guide](integration-guide.md) for a
   developer building the control plane it connects to.

## Start

| Page | Covers |
|---|---|
| [README](../README.md) | What stirrup is, its tenets and a first run with `stirrup harness` from a source checkout. The docs site shows it as Introduction. |
| Install (planned) | Installing from release binaries and container images, tracked in [#696](https://github.com/rxbynerd/stirrup/issues/696). Until that page exists, build from source as the README describes. |

## Concepts

| Page | Covers |
|---|---|
| [Philosophy](philosophy.md) | The design principles behind the harness. |
| [Architecture](architecture.md) | The thirteen-component model, the agentic loop, how the factory builds a run from a `RunConfig`, and the component sections. |
| [Safety rings](safety-rings.md) | The five operator-configurable rings: runtime isolation, the egress allowlist, the Cedar policy engine, the Rule of Two and the post-edit code scanner. |
| [Guardrails](guardrails.md) | The LLM-based safety classifier that screens content before a turn, before each tool call and after a turn. |
| [Security foundations](security.md) | Secret handling, scrubbing and sandbox hardening, and the security properties of opt-in features such as hooks, post-edit checks and sandbox identity tokens. |

## Reference

| Page | Covers |
|---|---|
| [Configuration](configuration.md) | The CLI flags and `RunConfig` fields, with precedence rules and examples. |
| [Provider adapters](providers.md) | Each model provider adapter, its credentials and its wire behaviour. |
| [Provider quirks layer](provider-quirks.md) | Per-model wire and behaviour differences, and the rules that encode them. |
| [Batch provider mode](batch.md) | Routing every turn through a provider's batch endpoint. |
| [Eval framework](eval.md) | `stirrup-eval`: suites, judges, comparison and the CI gate. |
| [Inspecting JSONL trace files](trace-inspection.md) | The trace format and the `stirrup trace` subcommands. |

## Operate

| Page | Covers |
|---|---|
| [Production deployment](deployment.md) | Running `stirrup job` against a control plane, the container images and the operator checklist. |
| [Autonomous deployment preset](autonomous-preset.md) | A tested `RunConfig` for unattended runs, and what it does not cover. |
| [Kubernetes executor](executors/k8s.md) | The `k8s` executor, which runs each run in its own sandbox Pod, and its egress controls. |
| [Kubernetes Agent Sandbox executor](executors/k8s-agent-sandbox.md) | The `k8s-sandbox` executor, built on the Agent Sandbox custom resource. |
| [Stirrup on Cloud Run jobs](cloud-run-jobs.md) | Running stirrup as a Google Cloud Run job and collecting its results. |
| [Credential federation](credential-federation.md) | How the harness resolves provider credentials, including federation that avoids static keys. |
| [Anthropic Workload Identity Federation](anthropic-wif.md) | Federated credentials for the Anthropic API. |
| [OpenAI Workload Identity Federation](openai-wif.md) | Federated credentials for the OpenAI API. |
| [Azure Entra ID Workload Identity Federation](azure-workload-identity.md) | Federated credentials for Azure OpenAI. |
| [Shipping Stirrup telemetry to a managed APM](observability-cloud.md) | OpenTelemetry export, platform profiles, and recipes for Grafana Cloud, Langfuse, Phoenix, CloudWatch and Datadog. |
| [Container publishing](container-publishing.md) | How the images are built and published to GitHub Container Registry and Artifact Registry. |
| [Cross-provider smoke tests](smoke-tests.md) | The manually dispatched workflows that exercise each provider's federation path. |

## Develop

| Page | Covers |
|---|---|
| [Integration guide](integration-guide.md) | The gRPC contract from a control plane's side, with a cookbook. |
| [Contributing](../CONTRIBUTING.md) | Building, testing, linting and commit conventions. |
| [`AGENTS.md`](../AGENTS.md) | A per-package map of the harness internals, written for AI coding agents. |
| [Security policy](../SECURITY.md) | How to report a vulnerability. |

## Design records

Each record states its own status in its header. For behaviour that
has shipped, the reference pages are authoritative.

| Record | Status | Covers |
|---|---|---|
| [Sessions](sessions-spec-draft.md) | Draft 2 | Append-only session persistence and `--continue`. Only in-process continuity has shipped, as `followUpHistory: "continue"` in the [Integration guide](integration-guide.md#follow-up-turns). |
| [Langfuse feedback and distributed tracing](langfuse-feedback-and-trace-propagation-research.md) | Proposal | Research into trace-reference reporting and W3C trace context propagation. |
| [Langfuse feedback: implementation](langfuse-feedback-and-trace-propagation-implementation.md) | Proposal | The types, fields, call sites and tests for that work in stirrup. Packages S1 to S6 and S8 are implemented; the shipped behaviour is in the [Integration guide](integration-guide.md#joining-a-control-plane-trace-trace_context) and [Langfuse](observability-cloud.md#langfuse). |
| [Langfuse feedback: qualification](langfuse-feedback-and-trace-propagation-qualification.md) | Pending | The plan for qualifying how Langfuse handles the data stirrup sends. |
| [Langfuse user feedback: outer application](langfuse-feedback-outer-application-worksheet.md) | Proposal | The application around stirrup that owns the user-facing rating page. |
