# Stirrup documentation

Stirrup is an operator-owned coding worker. The operator chooses the
model provider, the execution environment, the permission policy and
the control plane, and each run leaves evidence that can be inspected
afterwards. A run starts either from the command line with
`stirrup harness` or from a control plane over gRPC with `stirrup job`.

The pages below are grouped by what the reader is doing. A newcomer
can read them in this order:

1. The [README](../README.md), for what stirrup is and a first run from
   a source checkout.
2. [Philosophy](philosophy.md) and [Architecture](architecture.md), for
   how the harness is put together.
3. [Safety rings](safety-rings.md), for the controls an operator can
   turn on and how they combine.
4. [Configuration](configuration.md), the reference for every flag and
   `RunConfig` field.
5. [Production deployment](deployment.md) for an operator deploying
   stirrup, or the [Integration guide](integration-guide.md) for a
   developer building a control plane for it.

## Start

| Page | Covers |
|---|---|
| [README](../README.md) | What stirrup is, its tenets and a first run with `stirrup harness` from a source checkout. |
| Install (planned) | Installing from release binaries and container images, tracked in [#696](https://github.com/rxbynerd/stirrup/issues/696). Until that page exists, build from source as the README describes. |
| [Autonomous deployment preset](autonomous-preset.md) | A tested `RunConfig` for unattended runs, and what it does not cover. |

## Concepts

| Page | Covers |
|---|---|
| [Philosophy](philosophy.md) | The design principles behind the harness. |
| [Architecture](architecture.md) | The thirteen-component model, the agentic loop, how the factory builds a run from a `RunConfig`, and a deep dive into each component. |
| [Safety rings](safety-rings.md) | The five operator-configurable rings: runtime isolation, the egress allowlist, the Cedar policy engine, the Rule of Two and the post-edit code scanner. |
| [Guardrails](guardrails.md) | The LLM-based safety classifier that screens content before a turn, before each tool call and after a turn. |
| [Security foundations](security.md) | The controls that are always on, such as secret handling, scrubbing and sandbox hardening. |

## Reference

| Page | Covers |
|---|---|
| [Configuration](configuration.md) | Every CLI flag and `RunConfig` field, with precedence rules and examples. |
| [Provider adapters](providers.md) | Each model provider adapter, its credentials and its wire behaviour. |
| [Provider quirks layer](provider-quirks.md) | Per-model wire and behaviour differences, and the rules that encode them. |
| [Batch provider mode](batch.md) | Routing every turn through a provider's batch endpoint. |
| [Eval framework](eval.md) | `stirrup-eval`: suites, judges, comparison and the CI gate. |
| [Inspecting JSONL trace files](trace-inspection.md) | The trace format and the `stirrup trace` subcommands. |

## Operate

| Page | Covers |
|---|---|
| [Production deployment](deployment.md) | Running `stirrup job` against a control plane, the container images and the operator checklist. |
| [Kubernetes executor](executors/k8s.md) | The `k8s` executor, which runs each run in its own sandbox Pod, and its egress controls. |
| [Kubernetes Agent Sandbox executor](executors/k8s-agent-sandbox.md) | The `k8s-sandbox` executor, built on the Agent Sandbox custom resource. |
| [Stirrup on Cloud Run jobs](cloud-run-jobs.md) | Running stirrup as a Google Cloud Run job and collecting its results. |
| [Credential federation](credential-federation.md) | How the harness resolves provider credentials, including federation that avoids static keys. |
| [Anthropic Workload Identity Federation](anthropic-wif.md) | Federated credentials for the Anthropic API. |
| [OpenAI Workload Identity Federation](openai-wif.md) | Federated credentials for the OpenAI API. |
| [Azure Entra ID Workload Identity Federation](azure-workload-identity.md) | Federated credentials for Azure OpenAI. |
| [Shipping Stirrup telemetry to a managed APM](observability-cloud.md) | OpenTelemetry export, platform profiles and per-backend recipes. |
| [Container publishing](container-publishing.md) | How the images are built and published to GitHub Container Registry and Artifact Registry. |
| [Cross-provider smoke tests](smoke-tests.md) | The manually dispatched workflows that exercise each provider's federation path. |

## Develop

| Page | Covers |
|---|---|
| [Integration guide](integration-guide.md) | The gRPC contract from a control plane's side, with a cookbook. |
| [Contributing](../CONTRIBUTING.md) | Building, testing, linting and commit conventions. |
| [`AGENTS.md`](../AGENTS.md) | A per-package map of the harness internals. |
| [Security policy](../SECURITY.md) | How to report a vulnerability. |

## Design records

These pages hold proposals and research rather than a description of
current behaviour.

| Record | Covers |
|---|---|
| [Sessions](sessions-spec-draft.md) | Append-only session persistence and `--continue`. |
| [Langfuse feedback and distributed tracing](langfuse-feedback-and-trace-propagation-research.md) | Research into trace-reference reporting and W3C trace context propagation. |
| [Langfuse feedback: implementation](langfuse-feedback-and-trace-propagation-implementation.md) | The types, fields, call sites and tests for that work in stirrup. |
| [Langfuse feedback: qualification](langfuse-feedback-and-trace-propagation-qualification.md) | The plan for qualifying how Langfuse handles the data stirrup sends. |
| [Langfuse user feedback: outer application](langfuse-feedback-outer-application-worksheet.md) | The application around stirrup that owns the user-facing rating page. |
