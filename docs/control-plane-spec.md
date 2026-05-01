# Stirrup Control Plane + Runtime Spec (Draft)

## Purpose

This document defines the initial requirements for operating Stirrup in both runtime modes and for implementing a compatible control plane. Stirrup currently provides the agent runtime and transport contracts, but **does not yet ship with a reference control plane implementation**.

This spec is intended to guide teams building that control plane (API + scheduler + session orchestration + observability) so they can safely run Stirrup in production.

---

## 1) Runtime Modes in Stirrup

Stirrup has two primary operating modes:

1. **CLI harness mode** (`stirrup harness`)
2. **Kubernetes/job mode** (`stirrup job`)

Both modes execute the same interface-driven core loop and component system, but differ in how tasks are supplied and where control-plane responsibilities live.

### 1.1 CLI Harness Mode (`stirrup harness`)

#### Role
- Direct local invocation for development, debugging, automation, and ad-hoc runs.
- Operator provides prompt/config via CLI flags and environment.

#### Required execution inputs
- Prompt (`--prompt` or positional argument).
- Provider/model configuration (defaults available, but valid credentials are required).
- Workspace path (defaults to current directory).

#### Operational characteristics
- Can run over `stdio` transport (default) or gRPC transport.
- Best suited for direct user control and local feedback loops.
- Runs until max turns, timeout, completion, or terminal policy condition.

### 1.2 Job Mode (`stirrup job`)

#### Role
- Non-interactive worker process intended for containerized/Kubernetes deployment.
- Worker establishes outbound gRPC stream to a control plane and awaits task assignment.

#### Required execution inputs
- `CONTROL_PLANE_ADDR` environment variable (gRPC endpoint).
- Runtime credentials/secrets for selected provider(s).
- RunConfig payload delivered over transport by control plane.

#### Operational characteristics
- On startup, worker dials control plane, sends readiness signal, blocks for assignment, then executes assigned task over existing bidi stream.
- Control plane is authoritative for orchestration lifecycle (queueing, assignment, retries, cancellation, metadata).
- Designed for scalable worker pools where control and execution are separated.

---

## 2) Baseline Requirements for Running Stirrup

The following requirements apply regardless of mode.

### 2.1 Binary/runtime prerequisites
- Buildable Go workspace with modules: `harness`, `types`, `eval`, `gen`.
- Runtime environment with:
  - Network egress to model providers and any configured MCP servers.
  - Access to configured secret sources (env/file/SSM depending on setup).
  - File-system access to workspace if using local executor.

### 2.2 Authentication and secrets
- Provider credentials must be supplied via secret references (e.g., `secret://ANTHROPIC_API_KEY`) or provider-specific auth (e.g., IAM for Bedrock).
- API keys/cloud credentials must not be injected into untrusted command environments.
- Secret values must not be persisted in traces/logs.

### 2.3 RunConfig validity and guardrails
- RunConfig must pass runtime validation including caps and security invariants.
- Enforce bounded resources:
  - Max turns hard cap.
  - Timeout hard cap.
  - Follow-up grace cap.
  - Cost/token budget caps.
- Read-only modes must enforce non-mutating tool/executor semantics.

### 2.4 Security controls
- Input schemas validated before execution.
- Dynamic user/retrieved context must be treated as untrusted.
- Logs/traces must pass scrubber/redaction pipeline.
- Tool calls must obey selected permission policy.

### 2.5 Reliability controls
- Stall detection (repeated identical tool calls and repeated failures) should terminate pathological runs.
- Explicit per-command timeout and output size caps must be applied.
- Event stream and run state must be recoverable enough to support diagnostics and replay evaluation.

---

## 3) Control Plane: Scope and Responsibilities

A Stirrup control plane is the orchestration layer that manages remote workers running `stirrup job`. It is out-of-process and external to current Stirrup code.

### 3.1 Minimum responsibilities (MVP)
1. **Worker session management**
   - Accept outbound gRPC connections from workers.
   - Track worker identity, capabilities, liveness, and session state.
2. **Task queue + assignment**
   - Persist submitted tasks and metadata.
   - Match tasks to available workers.
   - Deliver task assignment payloads over established streams.
3. **Run lifecycle control**
   - Start, cancel, timeout, and mark terminal status.
   - Handle worker disconnects and reassignment policy.
4. **Event ingestion + persistence**
   - Receive streamed run events/traces.
   - Persist event timeline for observability/debugging/compliance.
5. **Result serving**
   - Provide APIs/UI hooks to retrieve run status, logs, artifacts, and terminal outcomes.

### 3.2 Non-goals for initial MVP
- Multi-tenant billing and chargeback.
- Complex DAG orchestration across dependent tasks.
- Advanced autoscaling logic beyond basic worker pool integration.

---

## 4) Control Plane Architecture Requirements

### 4.1 Connectivity model
- **Outbound only from worker to control plane** is preferred/default for K8s compatibility and network security.
- Control plane should not require inbound connectivity into worker pods.
- gRPC bidi stream is long-lived; design for keepalive, reconnect, and idempotent resume semantics.

### 4.2 Task model
Each task submitted to the control plane should include at least:
- Stable task ID (idempotency key).
- Requested mode (`execution`, `planning`, `review`, etc.).
- Prompt/instruction payload.
- RunConfig overrides or profile reference.
- Workspace/materialization inputs (repo URL, commit SHA, artifact bundle, or pre-mounted path contract).
- Policy envelope (timeouts, budgets, permission policy constraints).

### 4.3 Worker registration contract
On session startup, worker should provide:
- Worker ID and version/build metadata.
- Supported executors/providers/transports.
- Environment class (e.g., runtime image tag, CPU/memory profile).
- Optional labels for routing (region, trust tier, tenant isolation domain).

### 4.4 State machine requirements
Define explicit, durable task/run states, e.g.:
- `queued` → `assigned` → `running` → (`completed` | `failed` | `cancelled` | `timed_out`)
- Recovery sub-states for transport loss (`orphaned`, `requeue_pending`, etc.) are recommended.

State transitions must be:
- Validated (no illegal jumps).
- Auditable (who/what changed state + timestamp).
- Idempotent for retried control-plane operations.

### 4.5 Failure and retry semantics
- Assignment acknowledgment timeout: if worker does not ack task start, requeue task.
- Mid-run disconnect handling:
  - Grace window for worker reconnect.
  - If unrecoverable, mark failed or requeue according to mode/policy.
- Retries should be policy-driven with capped attempts and backoff.
- Ensure idempotent handling when duplicate events arrive after reconnect.

---

## 5) Data and API Requirements for Control Plane

### 5.1 External API surfaces
A practical first cut includes:
- `POST /tasks` (submit task)
- `GET /tasks/{id}` (task status)
- `POST /tasks/{id}/cancel` (cancellation)
- `GET /runs/{id}/events` (stream/history)
- Optional websocket/SSE for real-time consumer updates

(Transport naming/protocol is implementation-defined; functionality is required.)

### 5.2 Persistence model
Store at minimum:
- Task spec and submission metadata.
- Assignment history (worker IDs, attempts).
- Run status timeline.
- Event stream and structured diagnostics.
- Artifact pointers (logs, traces, outputs, patch bundles).

### 5.3 Trace and observability integration
- Persist emitted JSONL traces or map events to internal telemetry schema.
- Emit metrics for queue depth, assignment latency, success/failure rate, timeout rate, and token/cost consumption.
- Support correlation IDs across API request, assignment, and worker event pipeline.

---

## 6) Kubernetes Deployment Requirements (Job Mode)

### 6.1 Worker pod requirements
- Container image containing `stirrup` binary and required runtime dependencies.
- Environment variables for control plane endpoint and secret references.
- Resource requests/limits sized for model/tool workloads.
- Secure service account/IAM mapping as needed for provider access.

### 6.2 Network and security
- Egress policy allowing control plane and configured provider endpoints.
- TLS for gRPC control channel.
- Optional mTLS between worker and control plane for strong identity.
- Namespace isolation and least-privilege RBAC.

### 6.3 Scaling and scheduling
- Horizontal scaling of stateless workers.
- Queue-driven autoscaling signal (e.g., pending tasks).
- Taint/toleration or node pool partitioning for trusted/untrusted workload classes.

---

## 7) Intricacies and Design Risks for Control Plane Builders

1. **Exactly-once is hard; design for at-least-once + idempotency.**
   - Duplicate assignment/events can happen around disconnects and retries.
2. **Workspace materialization strategy is foundational.**
   - Control plane must define whether workers clone repos, mount volumes, or fetch snapshot bundles.
3. **Policy layering can conflict.**
   - Global tenant policy, task policy, and RunConfig policy need deterministic precedence.
4. **Long-running streams need backpressure strategy.**
   - Event bursts and large tool outputs can overload API consumers or storage.
5. **Cancellation must propagate consistently.**
   - Ensure cancellation signal reaches worker loop and terminal state is persisted once.
6. **Version skew is inevitable.**
   - Define compatibility matrix across control plane, worker image, proto/schema versions.

---

## 8) Recommended First Milestone (Reference Control Plane Bootstrap)

A sensible first milestone for the new control-plane project:

1. Implement gRPC endpoint compatible with `stirrup job` session lifecycle.
2. Add persistent queue and run-state store.
3. Support single-attempt task assignment to connected ready worker.
4. Ingest and persist full event stream.
5. Expose simple task submission + status API.
6. Add cancellation and timeout enforcement.
7. Deliver minimal dashboard/CLI for operator visibility.

This milestone should prioritize correctness, observability, and recoverability over feature breadth.

---

## 9) Open Questions to Resolve in the Control Plane Project

- Canonical task payload schema: what is required vs optional?
- How should workspace sources be standardized (git, tarball, object-store snapshot, PVC)?
- What are guaranteed delivery semantics for assignment and events?
- Should follow-up interactions share the original worker session or use new sessions?
- What multi-tenant isolation model is required at launch?
- What retention policy applies to traces/logs/artifacts with sensitive data concerns?

Resolving these questions should be part of the control plane design RFC process.
