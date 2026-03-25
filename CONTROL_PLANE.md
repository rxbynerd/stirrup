# Control Plane Implementation Spec

## Purpose

This document is the implementation-facing spec for Stirrup's control plane as of March 2026.

It replaces the earlier design draft with something narrower and more accurate:

- It treats the existing harness worker protocol as authoritative.
- It distinguishes what already exists in the repo from what still needs to be built.
- It calls out the current protocol and wiring gaps that should be fixed before or during control plane work.
- It keeps production aspirations secondary to an MVP that can actually be implemented against the current codebase.

## Current state in the repo

What already exists:

| Area | Status |
|---|---|
| Harness core loop | Implemented in Go |
| `RunConfig` model | Implemented in [`types/runconfig.go`](types/runconfig.go) |
| Harness worker gRPC transport | Implemented in [`proto/harness/v1/harness.proto`](proto/harness/v1/harness.proto) and [`harness/internal/transport/grpc.go`](harness/internal/transport/grpc.go) |
| K8s/job worker entrypoint | Implemented in [`harness/cmd/job/main.go`](harness/cmd/job/main.go) |
| Permission ask-upstream flow | Implemented harness-side in [`harness/internal/permission/askupstream.go`](harness/internal/permission/askupstream.go) |
| Follow-up requests after completion | Implemented harness-side via `user_response` + `FollowUpGrace` |
| Providers / executors / tools / trace emitters | Implemented inside the harness |

What does not exist yet:

| Area | Status |
|---|---|
| Control plane service | Not implemented |
| Session store | Not implemented |
| Workspace provisioner | Not implemented |
| External control plane API | Not implemented |
| Scheduler / triggers | Not implemented |
| Credential minting service | Not implemented |
| A2A adapter | Not implemented |

The control plane should therefore be designed around the harness that exists today, not around a hypothetical future worker protocol.

## Authoritative worker contract

The internal control plane <-> harness protocol already exists and should be reused, not redesigned.

### Transport

- Protocol: gRPC bidirectional stream
- Service: `harness.v1.HarnessService`
- RPC: `RunTask(stream HarnessEvent) returns (stream ControlEvent)`
- Directionality: the harness is the client and dials outbound to the control plane

This is already implemented in:

- [`proto/harness/v1/harness.proto`](proto/harness/v1/harness.proto)
- [`harness/internal/transport/grpc.go`](harness/internal/transport/grpc.go)
- [`harness/cmd/job/main.go`](harness/cmd/job/main.go)

### Worker startup sequence

The current worker lifecycle is:

1. The job process starts and requires `CONTROL_PLANE_ADDR`.
2. It opens the `RunTask` bidi stream to the control plane.
3. It emits a `ready` `HarnessEvent`.
4. It waits for a `task_assignment` `ControlEvent`.
5. It translates the wire `RunConfig` to `types.RunConfig`.
6. It runs `core.BuildLoopWithTransport(...)`.
7. It streams harness events during execution.
8. If `FollowUpGrace` is set, it can accept post-run `user_response` follow-ups before exit.

### `HarnessEvent` types that matter today

Current code emits or understands these event types:

| Type | Direction | Current status |
|---|---|---|
| `ready` | harness -> control plane | Emitted |
| `text_delta` | harness -> control plane | Emitted |
| `tool_result` | harness -> control plane | Emitted |
| `permission_request` | harness -> control plane | Emitted |
| `done` | harness -> control plane | Emitted |
| `error` | harness -> control plane | Emitted |
| `warning` | harness -> control plane | Emitted by loop, but not documented in proto comments |
| `heartbeat` | harness -> control plane | Defined, not meaningfully used yet |
| `tool_call` | harness -> control plane | Defined, but not currently emitted by the main loop |

### `ControlEvent` types that matter today

| Type | Direction | Current status |
|---|---|---|
| `task_assignment` | control plane -> harness | Required |
| `permission_response` | control plane -> harness | Implemented and used |
| `user_response` | control plane -> harness | Implemented only for post-run follow-up |
| `cancel` | control plane -> harness | Defined, but not handled by the main loop or job entrypoint |

### Interaction model that exists today

Current interactive behavior is narrower than the previous draft assumed:

- Permission approvals are implemented.
- Follow-up prompts after the primary run are implemented.
- In-run clarification requests are not implemented.
- Interactive chat during the main run is not implemented.
- Graceful cancellation over `cancel` is not implemented.

The control plane spec should not claim more than this without corresponding harness work.

## `RunConfig` is the worker task model

The control plane should treat `types.RunConfig` as the canonical task description for workers.

That means the control plane's job is to assemble a valid `RunConfig`, launch a worker, and bridge the stream.

Relevant current facts:

- Validation already exists in [`types/runconfig.go`](types/runconfig.go).
- The harness already knows how to construct providers, routers, prompts, executors, permissions, trace emitters, MCP clients, and git strategies from it.
- The control plane should avoid duplicating those composition rules.

### Important implication

The control plane does not need a second orchestration-specific task schema for workers. It may expose a higher-level client API later, but internally it should reduce to `RunConfig` before dispatch.

## Current gaps that must be acknowledged

These are the main mismatches between the current code and the earlier draft.

### 1. The worker proto does not fully cover `types.RunConfig`

The wire `RunConfig` in [`proto/harness/v1/harness.proto`](proto/harness/v1/harness.proto) and the translation in [`harness/internal/transport/grpc_translate.go`](harness/internal/transport/grpc_translate.go) currently miss fields that exist in `types.RunConfig`.

Notably missing or not translated:

| Field | Problem |
|---|---|
| `FollowUpGrace` | Exists in `types.RunConfig`, missing from proto |
| `MaxCostBudget` | Exists in proto, not translated into internal `RunConfig` |
| `EditStrategy.FuzzyThreshold` | Exists in types, missing from proto translation |
| `Verifier.Criteria` | Exists in types, missing from proto translation |
| `Verifier.Model` | Exists in types, missing from proto translation |
| `PermissionPolicy.Timeout` | Exists in types, missing from proto translation |
| `TraceEmitter.Endpoint` | Exists in types, missing from proto translation |

Before depending on these features from the control plane, the worker proto and translators should be brought into sync with `types.RunConfig`.

### 2. Terminal trace delivery is not wired end-to-end

The proto allows a `HarnessEvent` to carry `trace`, but the current loop emits `done` before `Trace.Finish(...)` and does not attach the resulting trace to the terminal event.

Current consequence:

- A control plane cannot reliably get the final `RunTrace` over the worker stream today.

This should be fixed before treating trace persistence as a control plane responsibility.

### 3. `cancel` is not actually implemented

The protocol defines `cancel`, but the harness job and main loop do not act on it.

Current consequence:

- A control plane can mark a session cancelled in its own state.
- It cannot yet reliably instruct a running harness to stop gracefully through the existing stream.

### 4. `tool_call` is defined but not emitted

The core loop currently forwards `text_delta` and `tool_result`, but not `tool_call`.

Current consequence:

- A control plane can observe tool outputs.
- It cannot currently stream a "tool started" event to clients from real harness runs.

### 5. The old draft assumed control-plane-managed credential minting

That is not the current code shape.

What exists today:

- The harness resolves `secret://` references itself.
- `AutoSecretStore` supports env, file, and AWS SSM.
- Executors use whatever env/secret refs are present in `RunConfig`.

For the first control plane implementation, it is enough to:

- inject env vars or secret refs the harness already understands
- avoid building a separate credential-vault subsystem first

### 6. The old draft assumed the control plane owned all workspace setup

The actual split is more nuanced:

- `executor.local` expects an existing workspace directory
- `executor.container` expects an existing host directory to bind-mount into `/workspace`
- `executor.api` is read-only and uses GitHub contents API instead of a local checkout

That means the control plane does need workspace preparation for local/container runs when a session targets a repository, but it should not duplicate executor behavior that already lives in the harness.

## Recommended control plane scope

The control plane should own the pieces that do not belong in the worker:

| Responsibility | Notes |
|---|---|
| Session lifecycle | Create, start, observe, complete, fail, cancel |
| Workspace preparation | Clone/fetch repo when a local/container executor needs a working tree |
| Worker launch | In-process for local MVP, then process/container/job launch |
| Stream bridging | Accept outbound worker stream and route events to the session |
| Permission brokerage | Translate `permission_request` into pending approval state and send `permission_response` |
| Session persistence | Session metadata, event log, terminal outcome |
| Cleanup | Remove temporary workspaces and worker resources |

The control plane should not own these in the first implementation:

- cost estimation
- advanced short-lived credential minting
- scheduler/webhook automation
- multi-tenant RBAC
- A2A interoperability
- fleet-wide model routing policy

Those can come later after the worker/session lifecycle is working.

## MVP architecture

The first implementation should be small and directly aligned to the current repo.

### Core components

| Component | MVP responsibility |
|---|---|
| `SessionManager` | Own session state machine and coordinate lifecycle |
| `SessionStore` | Persist session records and event log |
| `WorkspacePreparer` | Materialize a working tree when needed |
| `Provisioner` | Launch local worker / process / job |
| `WorkerBridge` | Speak the existing `HarnessService.RunTask` protocol |
| `ApprovalBroker` | Track pending permission requests and feed decisions back to the worker |

### Suggested repo shape

```text
controlplane/
  cmd/controlplane/
  internal/core/
  internal/session/
  internal/store/
  internal/provisioner/
  internal/workspace/
  internal/workergrpc/
```

No new module is required unless dependency isolation becomes useful later.

## Session model

The control plane should have a small explicit session state machine.

### States

| State | Meaning |
|---|---|
| `CREATED` | Session accepted but not yet preparing workspace/worker |
| `PREPARING` | Workspace and `RunConfig` are being assembled |
| `WAITING_FOR_WORKER` | Worker launched, waiting for `ready` |
| `RUNNING` | Task assignment sent and worker stream active |
| `AWAITING_APPROVAL` | Session is still running but blocked on a permission request |
| `COMPLETING` | Worker reached terminal event and cleanup/persistence is happening |
| `COMPLETED` | Successful or expected terminal completion |
| `FAILED` | Provisioning or worker failure |
| `CANCELLED` | Control-plane-level cancellation outcome |

### Required session record fields

| Field | Purpose |
|---|---|
| `SessionID` | Stable control-plane identifier |
| `RunID` | Worker run identifier from `RunConfig` |
| `State` | Current lifecycle state |
| `CreatedAt` / `UpdatedAt` / `CompletedAt` | Lifecycle timestamps |
| `Request` | The client-facing request or derived task spec |
| `RunConfig` | Final worker config sent to the harness |
| `WorkspacePath` | Prepared checkout directory if applicable |
| `PendingPermissionRequest` | Outstanding approval request if any |
| `EventLog` | Ordered worker/control events |
| `Outcome` | Final outcome string |
| `Error` | Terminal failure summary if needed |

## Worker launch profiles

The previous draft over-specified this. The first version should keep profiles simple.

### `local`

Purpose:

- fastest development loop
- no Docker or Kubernetes requirement

Implementation:

- prepare a local workspace directory if needed
- build a `RunConfig`
- run the harness in-process via `core.BuildLoopWithTransport(...)`
- use a memory transport that mimics the existing control/event bridge

This is acceptable even though it does not exercise real gRPC, because it is the cheapest way to implement and debug the control-plane lifecycle first.

### `process`

Purpose:

- realistic worker boundary without container orchestration

Implementation:

- start `harness/cmd/job` as a subprocess
- expose the control plane's gRPC server locally
- let the worker connect back over the real `HarnessService.RunTask` stream

This is a good second profile after `local`.

### `kubernetes`

Purpose:

- production deployment shape

Implementation:

- create a Job for `harness/cmd/job`
- set `CONTROL_PLANE_ADDR`
- wait for `ready`
- send `task_assignment`

This should be delayed until the local/process flow is stable.

## Workspace preparation rules

The control plane should prepare workspaces based on executor choice.

### If `executor.type == "local"`

- The worker must receive a real local directory in `executor.workspace`.
- If the session targets an existing local path, use it directly.
- If the session targets a remote repo, clone/fetch it into a managed temp workspace first.

### If `executor.type == "container"`

- The worker still needs a real host directory for bind-mounting.
- The control plane should prepare that host directory exactly as for local.
- The harness's container executor then mounts it into `/workspace`.

### If `executor.type == "api"`

- No local checkout is required.
- The control plane only needs to provide valid `vcsBackend` config and secrets.

## Client-facing API recommendation

There is no external control plane API in the repo yet.

For the first implementation, keep the client API minimal and separate from the worker protocol.

Recommended operations:

| Operation | Purpose |
|---|---|
| `CreateSession` | Create session and start work |
| `GetSession` | Inspect current state |
| `StreamSessionEvents` | Observe incremental output |
| `RespondToPermissionRequest` | Approve or deny pending side-effect requests |
| `SendFollowUp` | Send a post-run follow-up while grace window is open |
| `CancelSession` | Mark session cancelled and terminate worker when cancellation is implemented |

Important:

- Do not reuse the worker `HarnessService` as the public client API.
- The worker protocol is internal.
- The client API can be gRPC or HTTP, but it should map onto session operations, not raw worker events.

## Event mapping

The control plane should persist raw worker events, but expose a slightly cleaner client event stream.

### Raw worker events to preserve

- `ready`
- `text_delta`
- `tool_result`
- `permission_request`
- `done`
- `error`
- `warning`

### Client-visible event categories

| Client event | Source |
|---|---|
| `session_state_changed` | Control-plane lifecycle transitions |
| `output_delta` | Worker `text_delta` |
| `tool_result` | Worker `tool_result` |
| `permission_requested` | Worker `permission_request` |
| `warning` | Worker `warning` |
| `session_completed` | Worker `done` plus final control-plane metadata |
| `session_failed` | Worker `error` or provisioning failure |

The raw event log should remain available for debugging.

## Permission handling

The current harness-side contract is clear and should be used directly.

Flow:

1. Worker emits `permission_request` with `request_id`, `tool_name`, and JSON input.
2. Control plane records the pending request on the session.
3. Client approves or denies.
4. Control plane sends `permission_response` with matching `request_id`.

For MVP:

- only one outstanding permission request per session should be assumed
- timeouts can be handled in the control plane or by letting the worker-side timeout fire
- auto-approve rules should not be part of the first implementation

## Follow-up handling

The current harness already supports follow-up prompts after the main run if `FollowUpGrace` is set.

Implications for the control plane:

- follow-up is not "chat during execution"
- it is a post-completion grace window for another prompt
- the worker protocol uses `user_response`

This is worth supporting in the control plane MVP only if `FollowUpGrace` is first added to the worker proto.

Until then, the control plane should treat follow-up as out of scope.

## Persistence requirements

MVP persistence can stay simple.

### Required

- session row / record
- ordered event log
- final `RunConfig`
- terminal outcome

### Nice to have but not MVP-blocking

- full `RunTrace`
- replay support for reconnecting clients
- long-term lakehouse storage
- cost analytics

For local development, an in-memory store is sufficient.
For the next step, SQLite is a reasonable first durable store.

## Required code changes before or during control plane work

These items should be treated as near-term implementation tasks, not future nice-to-haves.

1. Sync worker proto `RunConfig` with `types.RunConfig`.
2. Translate all existing proto fields in [`harness/internal/transport/grpc_translate.go`](harness/internal/transport/grpc_translate.go).
3. Decide how the worker delivers terminal `RunTrace`, then implement it.
4. Either implement `cancel` handling or remove it from the MVP surface.
5. Either emit `tool_call` events from the main loop or explicitly exclude them from the control plane MVP.
6. Update proto comments to include `warning`, or stop emitting undocumented event types.

## Recommended implementation order

### Phase 0: protocol cleanup

1. Fix worker proto / translation mismatches.
2. Decide terminal trace delivery.
3. Decide whether MVP includes cancellation.

### Phase 1: local control plane

1. Add `controlplane/` package structure.
2. Implement in-memory `SessionStore`.
3. Implement `SessionManager`.
4. Implement local `WorkspacePreparer`.
5. Implement local `Provisioner` using in-process harness execution.
6. Implement raw event capture and session event mapping.
7. Implement permission brokerage.

Deliverable:

- create a session
- run it locally through the control plane
- observe output
- approve/deny side-effecting tools
- complete and persist session state

### Phase 2: real worker boundary

1. Add a gRPC worker server that the harness job can dial.
2. Add subprocess-based `process` provisioner using `harness/cmd/job`.
3. Reuse the same session manager and event broker.

Deliverable:

- the control plane can drive the existing job entrypoint over the real worker gRPC stream

### Phase 3: Kubernetes

1. Add Job launcher.
2. Add cleanup/reconciliation.
3. Add durable store.

Deliverable:

- production-shaped orchestration without changing the worker contract

## Explicit non-goals for the first implementation session

The following should not be treated as MVP requirements:

- multi-tenant auth
- webhook scheduler
- cron scheduler
- credential vault service
- STS/GitHub App minting
- replay buffers
- S3 trace lakehouse
- A2A dispatch
- fleet-level routing policy

Those are valid future features, but they should not distort the first control plane implementation.

## Bottom line

The control plane should be built as a thin orchestration layer around the harness that already exists.

The most important constraints are:

- keep `proto/harness/v1/harness.proto` as the internal worker protocol
- use `types.RunConfig` as the worker task model
- implement session lifecycle, workspace prep, worker launch, event bridging, and permission brokerage first
- patch the current worker/proto mismatches before depending on features that are not actually wired today

If the implementation session follows this document, it should produce a real control plane MVP instead of a second speculative design.
