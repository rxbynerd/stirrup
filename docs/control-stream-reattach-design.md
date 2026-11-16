# Control stream re-attach: design note

Status: proposal for review, draft 2, 2026-11-16. Nothing here is
implemented. This note is the first acceptance criterion of issue #911
(an optional grace period and re-attach before a lost control stream
cancels the run). Code follows only after the note is merged and the
[open questions](#open-questions-for-the-maintainer) are answered.

Draft 1 was reviewed adversarially for design soundness, security and
documentation accuracy. The review changed the protocol in several
places: a separate RPC, control-plane acknowledgements, inbound sequence
numbers, a session-only credential binding, delete-and-wait as the only
sanctioned fence, and a hold on tool dispatch while detached. Where a
draft 1 choice was dropped, [Alternatives considered](#alternatives-considered)
says why.

Every file and line reference was checked against `main` at
`0f0bc8e9`. Re-verify them before acting on this note; the code will
have moved.

The reader is the session that implements the feature or reviews this
design. The integrator-facing contract stays in
[`integration-guide.md`](integration-guide.md). That guide carries only
a pointer to this note until the feature ships; see
[Docs to update on implementation](#docs-to-update-on-implementation).

## Contents

- [Summary](#summary)
- [Behaviour today](#behaviour-today)
- [Why a lost stream cancels the run](#why-a-lost-stream-cancels-the-run)
- [Goals and non-goals](#goals-and-non-goals)
- [Prerequisites](#prerequisites)
- [Overview](#overview)
- [Configuration](#configuration)
- [Liveness and loss detection](#liveness-and-loss-detection)
- [Redialling](#redialling)
- [The `ResumeTask` handshake](#the-resumetask-handshake)
- [Authenticating a re-attach](#authenticating-a-re-attach)
- [Outbound sequence numbers and retention](#outbound-sequence-numbers-and-retention)
- [Inbound sequence numbers](#inbound-sequence-numbers)
- [Tool dispatch and requests while detached](#tool-dispatch-and-requests-while-detached)
- [Trace context and observability](#trace-context-and-observability)
- [Lifecycle interactions](#lifecycle-interactions)
- [Security argument](#security-argument)
- [Control-plane obligations](#control-plane-obligations)
- [Compatibility and the `ready` capability](#compatibility-and-the-ready-capability)
- [Alternatives considered](#alternatives-considered)
- [Implementation map](#implementation-map)
- [Test plan](#test-plan)
- [Open questions for the maintainer](#open-questions-for-the-maintainer)
- [Related issues](#related-issues)
- [Docs to update on implementation](#docs-to-update-on-implementation)

## Summary

A new `RunConfig` field, `controlStreamGraceSeconds` (proto field 62,
default 0, cap 300), opens a bounded window after the control stream is
lost.

- **While detached**, the model may keep streaming, but every tool call
  waits at dispatch until the stream is back. A detached run therefore
  takes no side-effecting action that nobody is watching. Requests to the
  control plane keep their timeouts and fail closed.
- **Redial.** The harness redials `CONTROL_PLANE_ADDR` and calls a new
  RPC, `ResumeTask`. It presents a resume credential that the control
  plane issued on the `task_assignment` and replaces on every re-attach.
- **Delivery.** The control plane acknowledges what it has durably
  received. Both directions carry sequence numbers, so after a re-attach
  each side re-sends exactly what the other has not processed.
- **Lapse.** If the window passes without a re-attach, the run ends
  `control_stream_lost` as it does today.
- **Fence.** The only sanctioned way to reschedule a task whose harness
  may still be alive is to revoke the session and then delete its Pod
  and wait for the deletion to finish.

With the field unset, stream-loss and delivery behaviour is unchanged
and no new field is set on any event. `ready` gains one capability
value and one `config_fields` path.

## Behaviour today

| Stage | What happens on loss | Where |
|---|---|---|
| Transport | One `RunTask` stream per process. No redial exists. The read loop records why the stream ended: `io.EOF` becomes `ErrPeerEndedStream`, any other receive error is kept, and a local `Close` or a cancelled stream context is not a loss. | `harness/internal/transport/grpc.go:87-119`, `:187-208`, `:296-307` |
| Liveness | `Done()` closes when the reader stops; `Err()` gives the cause, or nil for a local end. No HTTP/2 keepalive is configured. | `harness/internal/transport/transport.go:25-33`, `grpc.go:93-101` |
| Before assignment | The job fails: "gRPC stream closed before receiving task assignment". | `harness/cmd/stirrup/cmd/job.go:180-181` |
| Construction | The build context is cancelled. The job emits `error` and `done{stop_reason:"control_stream_lost"}` (best-effort) and fails. | `job.go:199-216` |
| Active run | `watchControlStream` calls `AgenticLoop.ControlStreamLost`, which cancels the session through `cancelSession`. The run ends `control_stream_lost`. Git finalisation, `postRun` hooks, workspace export and the trace finish still run on detached contexts, and the process exits non-zero. | `harness/cmd/stirrup/cmd/controlstream.go:13-35`, `job.go:251-252`, `harness/internal/core/streamloss.go:15-23`, `harness/internal/core/userinput.go:175-193` |
| Idle follow-up window | The window closes promptly; the outcome is unchanged. The process exits with the primary run's status unless a follow-up was lost or failed a required export. | `streamloss.go:9-14`, `job.go:262-274` |
| Pending requests | The permission, async-tool and sandbox-token requests go through `Correlator.Await`: an emit failure fails the request at once, otherwise it fails at its timeout, and a response for an unknown ID is dropped. The batch adapter keeps its own pending map. | `harness/internal/transport/correlator.go:93-107`, `:122-160`; `harness/internal/provider/batch.go:735-757` |
| Outcome | `control_stream_lost`, "a run cancelled because its control stream ended while it was active". | `proto/harness/v1/harness.proto:1463-1473` |
| Delivery contract | "Delivery is at most once … a stream that breaks cannot be resumed." | `docs/integration-guide.md:807-812` |

## Why a lost stream cancels the run

Issue #659 found that after the stream ended, `stirrup job` kept
calling the model and running tools, then exited zero with a `done`
nobody received. A control plane that rescheduled on "stream closed
without `done`" then had two workers on one task. PR #740 (merged
2026-10-10) added the `Liveness` interface (commit `f96d676c`), the
outcome (`5b09d092`), the cancel (`c145aa58`) and the watchers in both
entry points (`8f67d7d2`).

The maintainer decided on 2026-10-08, on #659: "When the control stream
is lost, the harness cancels the active run immediately, with no knob
and no grace period", and "Any mode that continues after loss is out of
scope until a lease or session protocol exists (#456)". PR #740 gave
the reason as: "There is no way to resume a gRPC stream, so continuing
would only produce orphaned work."

This note revisits that decision only as an opt-in. It does not claim
the decision's precondition is met by a timed lease: a control plane
cannot compute a safe reschedule time, because a lapsed run still does
up to several minutes of post-run work (see
[Control-plane obligations](#control-plane-obligations)). The note
offers a session protocol instead. Re-attach is authenticated and
acknowledged, nothing is applied twice, a detached run takes no
side-effecting action, and the fence is state-based. Whether that meets
the bar is [open question 1](#open-questions-for-the-maintainer).

## Goals and non-goals

| Goals | Non-goals |
|---|---|
| A run survives a control-plane restart, deploy or brief network break shorter than the window. | Surviving a harness process restart or a Pod reschedule. That needs the persisted session log of [`sessions-spec-draft.md`](sessions-spec-draft.md) (#456). |
| With the field unset, stream-loss and delivery behaviour is unchanged. | Exactly-once execution of side effects across a fence. The fence prevents two workers; it does not make tools idempotent. |
| A detached run takes no tool action and gains no capability. | Moving a live stream between control-plane replicas. A resumed stream may land on any replica; the control plane shares session state between them. |
| After a re-attach, neither side applies an event twice or misses one silently. | Re-attach for `stirrup harness --transport grpc` or a `harnessapi` embedder. Neither has a `task_assignment` to carry a credential. |
| The loop stays a pure function of its interfaces: re-attach lives in the transport and the `cmd` package, and the dispatch hold is an injected interface. | A timed lease that lets a control plane reschedule without deleting the Pod ([open question 2](#open-questions-for-the-maintainer)). |

## Prerequisites

These must land first, each as its own change, because the protocol
depends on them:

| Prerequisite | Why | Evidence |
|---|---|---|
| Session-unique request IDs | A control plane re-sends answers by request ID. Today two `AskUpstreamPolicy` instances can share one transport, the permission policy and the Rule-of-Two ask gate, and both number `perm-1`, `perm-2` and so on. A re-sent allow for one could then approve the other's tool. The collision exists without re-attach, since every handler sees every response. | `harness/internal/core/factory.go:2129`, `:2265`; `harness/internal/permission/askupstream.go:74`; `correlator.go:179-184` |
| Byte bounds on request inputs | `boundEventFields` caps `input` only on `tool_call` and `tools_loaded`. `permission_request`, `tool_result_request` and `batch_submission` (the whole provider request) are uncapped, so a single event can exceed the 4 MiB receive limit and make the control plane abort the stream. | `harness/internal/transport/eventbound.go:47-57`, `harness/internal/provider/batch.go:717-740` |
| Session-wide heartbeats | The control plane detects a dead stream by harness silence. Today heartbeats run only while a run is in progress. | #913, `docs/integration-guide.md:313-319` |

## Overview

The transport holds a session that outlives individual streams:

```text
              loss                       handshake ok
  attached ─────────▶ detached ─────────────────────▶ replaying
     ▲                   │  ▲                             │
     │                   │  └───────── loss ──────────────┤
     │                   ▼                                │
     │                 lost ◀── budget spent, permanent   │
     │                          rejection, or OK end      │
     └──────────── replay acknowledged ◀──────────────────┘
```

| State | Emit | Control events in | Tool dispatch | `Liveness.Done()` |
|---|---|---|---|---|
| attached | Sent with `seq`; retained until acknowledged | Dispatched | Runs | open |
| detached | Retained only; returns nil | None | Waits | open |
| replaying | Queued behind the replay; `Emit` blocks on HTTP/2 flow control as today | Dispatched after `reattach_accepted` | Waits until the replay is acknowledged | open |
| lost | Fails, as today | None | The run is cancelled | closed; `Err()` names the cause |

`Done()` and `Err()` describe the session, not one stream. They fire
only on entering `lost`, so `watchControlStream` and
`ControlStreamLost` stay unchanged and still fire exactly once. The
transport keeps a separate per-stream signal for `Close`'s drain, which
today waits on the reader (`grpc.go:264-277`).

A stream that the control plane ends with status OK (`io.EOF` at the
harness) is a deliberate end, not a break. It goes straight to `lost`,
as it does today. During a run this is a loss; otherwise it is the
normal end of the session. Only a non-OK status, a transport error or a
liveness timeout starts the window. A control plane that drains for a
deploy ends streams with `UNAVAILABLE` to invite a re-attach.

## Configuration

| Item | Value |
|---|---|
| JSON | `controlStreamGraceSeconds` |
| Go | `RunConfig.ControlStreamGraceSeconds int` (`json:"controlStreamGraceSeconds,omitempty"`) |
| Proto | `int32 control_stream_grace_seconds = 62;` in `RunConfig`. Plain `int32`, like `heartbeat_seconds = 61` (`harness.proto:792`), because 0 means off. |
| Default | 0: no window; a loss cancels at once, as today. |
| Range | 0, or 30-300 seconds. `ValidateRunConfig` also requires the value to be at least four times the effective heartbeat interval, so that a control plane can tell a dead stream from a live one inside the window (see [Liveness](#liveness-and-loss-detection)). |
| Per break | Each break lasts at most the window. |
| Per session | The total detached time across all breaks is capped at three times the window, and the number of re-attaches at 10 ([open question 4](#open-questions-for-the-maintainer)). Spending either budget goes to `lost`. |
| Window reset | A break's clock stops only when the control plane acknowledges the end of the replay, not at the handshake. A stream that breaks again during replay continues the same break. |
| Run timeout | Unchanged. The run's `timeout` keeps running while detached. |
| Honoured by | `stirrup job` only. `stirrup harness`, `stirrup run-config` and `harnessapi` embedders accept the field and ignore it with a warning, so a recorded job configuration still replays. |

The field belongs in `RunConfig`, beside `followUpGrace` and
`heartbeatSeconds`: it is a session behaviour the control plane
chooses per task, and it is negotiated through `config_fields`. It
ships with its proto mirror and its copy in
`harness/internal/transport/grpc_translate.go`. The credential does not
belong in `RunConfig` (see
[Authenticating a re-attach](#authenticating-a-re-attach)).

`stirrup job` checks these before construction, outside
`ValidateRunConfig`, because they depend on the entry point and its
environment. Each failure rejects the task with `error` and
`done{stop_reason:"error"}`:

| Check | Reason |
|---|---|
| The `task_assignment` carries a `resume_token`. | The control plane asked for the feature but did not wire it. |
| Harness-side TLS is configured, or the operator opted into plaintext re-attach. | See [Authenticating a re-attach](#authenticating-a-re-attach). |
| `executor.type` is not `local`. | Model commands under the `local` executor run as the harness user and can read the harness's memory and TLS key, so the credential has no protection. |

The cap of 300 s covers a rolling deploy or a control-plane restart.
With the dispatch hold, a break costs waiting time and model spend, not
unwatched tool actions. The cap is
[open question 3](#open-questions-for-the-maintainer).

## Liveness and loss detection

HTTP/2 keepalive is not a usable primary signal. grpc-go servers
enforce a 5-minute minimum ping interval by default and answer faster
pings with `GOAWAY too_many_pings`. Proxies and mesh sidecars answer
pings themselves, so a hung replica behind one is never detected. The
first stream is also dialled (`job.go:116`) before the window is known,
and keepalive is fixed at dial time. Liveness is therefore carried by
the application:

| Direction | Signal | Rule |
|---|---|---|
| Control plane to harness | `ControlEvent{type:"ack", ack_seq}` at least every 10 s while the window is non-zero | `ack_seq` is the highest harness `seq` the control plane has durably processed. The harness treats 30 s without any inbound event as a loss and cancels that stream's context. Cancelling a stream is local to that stream; it is not the session context, so `recordStreamEnd` does not read it as a local close. |
| Harness to control plane | `heartbeat` for the whole session, at the effective heartbeat interval | The control plane treats three missed intervals, or the stream ending, as the stream being dead. Until then it refuses a re-attach for that session with a retryable rejection, so a valid credential cannot take over a stream that is still live. |

A blocked `Send` holds the transport's write lock (`grpc.go:137-150`).
The liveness timeout cancels the stream, which unblocks the `Send`, so
a flow-control stall cannot hold a correlator timer past the timeout by
more than the detection bound.

## Redialling

| Parameter | Value | Reason |
|---|---|---|
| Target | `CONTROL_PLANE_ADDR`, read once at process start (`job.go:84`) | A `RunConfig` cannot redirect the redial. |
| Connection | A fresh `grpc.ClientConn` per attempt; the old one is closed after its reader has exited | The new connection re-resolves DNS and lands on any healthy replica, without grpc-go's connect backoff, which grows to 120 s. Waiting for the old reader keeps one reader dispatching at a time. |
| TLS material | The CA bundle and client key pair are re-read on every attempt | Short-lived certificates (cert-manager, SPIFFE) rotate during a session; today they are loaded once (`harness/internal/transport/tls.go:105-110`). |
| Backoff | First attempt immediately, then a full-jitter delay of up to `min(15 s, 0.5 s × 2^n)`; a server retry-pushback value is honoured | Many harnesses lose one replica at once; full jitter spreads their reconnects, and the cap protects a recovering control plane from a reconnect storm. |
| Per-attempt deadline | `min(10 s, time left in the break)` for the dial, the `ResumeTask` open and `reattach_accepted` | A stalled handshake does not consume the window. Control planes answer within 5 s. |
| Stops | On `reattach_accepted`, a permanent rejection, an OK end, a spent budget, or the session context ending (SIGTERM) | Each of these either completes the re-attach or makes it impossible. |

**Rejections.** Permanence is signalled explicitly, never inferred
from a status code, because proxies and replicas that are still
starting also return `UNAUTHENTICATED`, `PERMISSION_DENIED` or
`NOT_FOUND`. The control plane answers a `reattach` it will never
accept with `ControlEvent{type:"reattach_rejected", permanent: true,
reason}` and then ends the RPC with OK. The harness goes to `lost`, and
`RunTrace.error` carries the reason after scrubbing and a 1 KiB cap.
Every other outcome is retried within the window. That includes
`UNIMPLEMENTED` from an old replica during a rolling deploy, a
`reattach_rejected` with `permanent: false`, a lost compare-and-set
(`ABORTED`) and a plain end of stream before `reattach_accepted`.

## The `ResumeTask` handshake

Re-attach uses a new RPC on the same service instead of the first
message of `RunTask`:

```proto
rpc ResumeTask(stream HarnessEvent) returns (stream ControlEvent);
```

An old replica answers `UNIMPLEMENTED` at once instead of mistaking the
stream for a new harness. The control plane can also authorise resumed
streams in an interceptor. `ready` and `task_assignment` are not valid
on `ResumeTask`.

```text
harness                                           control plane
   │ ResumeTask                                         │
   │── reattach {session_id, incarnation, run_id,     ─▶│ look up by credential hash;
   │            resume_token, last_seq,                 │ check the old stream is dead;
   │            last_control_seq}                       │ compare-and-set the generation
   │◀─ reattach_accepted {ack_seq: N, resume_token} ────│
   │── resumed {replay_through_seq: M, pending_requests} ─▶│ stream becomes the live one;
   │── tombstones and events with N < seq ≤ M ─────────▶│ previous credential retired
   │◀─ control events with control_seq > last_control_seq │
   │◀─ ack {ack_seq ≥ M} ───────────────────────────────│ the break ends here
   │── live events ────────────────────────────────────▶│
```

Strict ordering on a resumed stream:

1. The harness dispatches nothing before `reattach_accepted`. Any other
   first event abandons the attempt (retryable).
2. `ack_seq` above `last_seq` is a protocol error and abandons the
   attempt (retryable).
3. After `reattach_accepted`, a second `reattach_accepted` or a
   `task_assignment` is dropped and logged.

Wire additions. RunConfig field 62 is reserved for this work. The other
numbers are the next free ones at `0f0bc8e9` and are illustrative.

| Message | Addition | Meaning |
|---|---|---|
| `HarnessService` | `rpc ResumeTask` | See above. |
| `RunConfig` | `control_stream_grace_seconds = 62` | See [Configuration](#configuration). |
| `HarnessEvent` | `uint64 seq = 30` | Session-scoped, from 1, on every retained event after the assignment, only when the window is non-zero. Absent on `ready`, `reattach`, `resumed`, `heartbeat` and `batch_waiting`. |
| `HarnessEvent` | `string incarnation = 31` | On `ready` and `reattach`: a random ID per harness process, so `(incarnation, seq)` stays unique if #456 later resumes across processes. Set on `ready` only when the build advertises the capability. |
| `HarnessEvent` | `string session_id = 32`, `string run_id = 33` | On `reattach`: the session echo and the active run, both informational (see [Authenticating](#authenticating-a-re-attach)). |
| `HarnessEvent` | `uint64 last_seq = 34`, `uint64 last_control_seq = 35` | On `reattach`: the highest `seq` assigned, and the highest `control_seq` the harness processed. |
| `HarnessEvent` | `uint64 replay_through_seq = 36` | On `resumed`: the last `seq` that is a replay. Events above it are live. |
| `HarnessEvent` | `repeated PendingRequest pending_requests = 37` | On `resumed`: each request still waiting for a response, as `(type, request_id)`. |
| `HarnessEvent` | `bool evicted = 38` | On a tombstone; see [Retention](#outbound-sequence-numbers-and-retention). |
| `HarnessEvent` types | `reattach`, `resumed` | Sent only on `ResumeTask`. |
| `ControlEvent` | `uint64 control_seq = 12` | Session-scoped, assigned by the control plane to every event after `task_assignment` when the window is non-zero. |
| `ControlEvent` | `uint64 ack_seq = 13` | On `ack` and `reattach_accepted`. |
| `ControlEvent` | `bool permanent = 14` | On `reattach_rejected`. `reason` (field 6) carries the explanation. |
| `ControlEvent` types | `ack`, `reattach_accepted`, `reattach_rejected` | `ack` is valid on both RPCs; the other two only as the first event on `ResumeTask`. |
| Credential | `resume_token` | Carried in a dedicated proto message field on `task_assignment`, `reattach_accepted` and `reattach`, never translated into `types.ControlEvent` or `types.HarnessEvent`. |

All sends (`Emit`, the handshake and the replay) go through one
function that bounds the event, converts it and sends it. The existing
`TestHarnessEvents_PassTheByteBound` invariant
(`harness/internal/invariants/bytebound_test.go:40-82`) therefore
covers them without an allow-list entry. The credential is attached to the proto
message inside that function, after bounding.

## Authenticating a re-attach

The session ID cannot authenticate a re-attach.
`CONTROL_PLANE_SESSION_ID` is optional (`job.go:122-127`). It is echoed
in clear on `ready`, and at least one control-plane design treats it as
single-use. The run ID is on `run_started`. A resume credential is
needed.

| Concern | Design |
|---|---|
| Issued by | The control plane, on `task_assignment`, only on a stream that arrived over TLS or a verified mesh identity. It is not part of `RunConfig`, for the reason `trace_context` is not (`harness.proto:632-638`): `RunConfig` is recorded, redacted and replayed, and replaying a configuration must not grant a resume. The project's invariants also forbid secrets in `RunConfig`. |
| Format | A random handle of at least 256 bits, printable ASCII, at most 4 KiB. The control plane stores only its hash, looks the session up by that hash and compares in constant time. A self-contained signed token is not enough, since single use cannot be enforced without server-side state. |
| Validation in the harness | An empty, oversized or non-printable value is rejected, never truncated, using the rule of the sandbox token's `validToken` (`harness/internal/sandboxidentity/exchange.go:61-68`). |
| Bound to | The session only. `session_id` and `run_id` on `reattach` are consistency checks for logs, not part of authentication. A run can start or end during the break (construction finishing, a run finishing, or a follow-up started from queued input, `harness/internal/core/loop.go:1455-1481`). The replay tells the control plane about those transitions. |
| Generations | The session record holds a generation number, the hash of the credential last presented in an accepted re-attach (the previous one) and the hash of the credential last issued (the current one). A re-attach presenting either is accepted through a compare-and-set on the generation. The loser of a race between replicas ends its RPC with `ABORTED`, which the harness retries. |
| Rotation | Every `reattach_accepted` issues a new credential. The harness replaces its copy before it sends `resumed`. |
| Retirement | When `resumed` arrives on the stream that issued the new credential, that stream becomes the session's live stream and the previous credential is retired. If `resumed` never arrives, the next attempt presents the previous credential, which is still accepted. |
| Lifetime | The control plane rejects a credential permanently once the session is revoked, cancelled or completed, or once three missed heartbeat intervals plus the window have passed since the last event it received from the session. That time is persisted, so a replica that starts after a crash can compute it. |
| Storage in the harness | One transport-private type whose `String`, `GoString`, `LogValue` and `MarshalJSON` redact it. The transport reads it from the proto `task_assignment` before translation, so no control handler sees it. It is never logged, traced, written to `RunConfig`, `RunResult` or the trace, passed to the executor or exposed to the model. A random handle matches no scrubber pattern, so these rules, pinned by an invariants test, are the only protection. |

**What mTLS already provides.** With `CONTROL_PLANE_TLS_CERT_FILE`
set, the harness presents a client certificate
(`harness/internal/transport/tls.go:105-110`). That proves the caller
holds a certificate from the control plane's CA. It does not prove
which session the caller owns when several Pods share one certificate,
for example one Kubernetes Secret mounted into every harness Pod. A
control plane with per-Pod certificates should also require that a
re-attach comes from the same client identity as the original stream.
That makes a stolen credential useless off the Pod. It does not
replace the credential.

**What server-side TLS provides.** Today the harness trusts the network
once, at the first dial. A redial trusts it again. Whoever answers a
redial and completes the handshake receives the retained events (up to
the cap, from the `ack_seq` it chooses). That endpoint can also inject
`user_response` text, answer `permission_request` and
`tool_result_request` events, and send `batch_result`, which is the
model's output in batch mode. Server certificate verification
(`tls.go:88-113`: TLS 1.3, no skip-verify path) closes that. The design
therefore refuses to redial over plaintext. A mesh sidecar intercepts
transparently, so the harness cannot check that one is present. An
operator who accepts trusting the network path on every redial opts in
with a Pod environment variable,
`STIRRUP_CONTROL_STREAM_REATTACH_PLAINTEXT=1`, never with a `RunConfig`
field. The harness logs a startup warning naming it. This is
[open question 5](#open-questions-for-the-maintainer).

## Outbound sequence numbers and retention

A successful `Send` means the event was queued for HTTP/2, not that the
control plane read it. The harness therefore retains every sequenced
event until an `ack` covers it, and the retained set is only the
unacknowledged suffix.

| Rule | Value |
|---|---|
| Sequence scope | Per session, monotonic across the primary run and every follow-up run. Assigned after scrubbing and bounding, so the retained event is exactly what was sent or would have been sent. |
| Not sequenced | `heartbeat` and `batch_waiting`. A replayed liveness signal means nothing, and leaving them unsequenced keeps the sequence free of holes that are not losses. |
| Release | An `ack` releases every event with `seq ≤ ack_seq`. With a 10 s acknowledgement interval, the retained set while attached is about 10 s of output. |
| Byte cap | 8 MiB of encoded event size ([open question 6](#open-questions-for-the-maintainer)). |
| Eviction | Over the cap, the oldest droppable event is replaced by a tombstone: `seq`, `type`, `turn`, `request_id`, `evicted: true` and no payload. Structure is never lost. |
| Protected (never reduced to a tombstone) | `run_started`, `done`, `error`, `turn_started`, `turn_committed`, `turn_abandoned`, every request event still pending, `batch_cancel_request`, and a `warning` or `user_message` carrying `request_id`. |
| Droppable | `text_delta`, `thinking_delta`, `tool_call`, `tool_result`, `tools_loaded`, request events no longer pending, and other `user_message` and `warning` events. Their durable copy is the trace emitter or the `resultSink`, as the integration guide already advises. |
| Hard limit | If protected events plus tombstones would exceed the cap, the session goes to `lost` at once with the cause "re-attach buffer exhausted". The design fails closed rather than growing. Once request inputs are bounded (see [Prerequisites](#prerequisites)), reaching this needs an unusual burst. |

**Gaps.** A tombstone is the inline gap marker: the control plane sees
exactly which events lost their payload, and it reads them from the
trace if it needs them. Every sequence number is either delivered or
tombstoned, so a control plane checks contiguity and treats a missing
number as a protocol error.

**Duplicates.** The replay starts after the control plane's `ack_seq`,
so a conforming control plane receives no duplicates. It still
deduplicates by `(incarnation, seq)`.

## Inbound sequence numbers

Inbound delivery has the same problem in the other direction. A
`ControlEvent` sent on the broken stream may or may not have arrived,
and an accepted `user_response` produces no echo until the next turn
boundary (`userinput.go:227-247`, `:315-357`), so the replay cannot
show whether it arrived.

- The control plane assigns a session-scoped `control_seq` to every
  event after `task_assignment` and keeps them in a per-session outbox.
- The harness records the highest `control_seq` it has dispatched and
  reports it as `last_control_seq` on `reattach`.
- After `resumed`, the control plane re-sends every outbox event above
  `last_control_seq`. The harness drops any event at or below it.
- Only the replica holding the session's current generation sends from
  the outbox. A stale replica's sends on a dead stream are then not
  lost, because the outbox still holds them.

This covers `permission_response`, `tool_result_response`,
`sandbox_token_response`, `batch_result`, `user_response` and `cancel`.
A `user_response` is therefore never applied twice and never lost.

## Tool dispatch and requests while detached

**Dispatch hold.** While the session is detached or replaying, every
tool call waits before it executes, through an interface the factory
injects into the loop (the loop keeps no knowledge of transports). The
wait sits after the permission check: a tool that needs approval
raises its `permission_request` as usual. On re-attach the call
proceeds; on `lost` the run is cancelled and the call never runs. The
model's stream is not held, so the turn's output still arrives.
Holding only the tools that consult the permission policy would miss
MCP and other tools that are neither `WorkspaceMutating` nor
`RequiresApproval` (`harness/internal/core/types.go:574`), so the hold
covers every call ([open question 7](#open-questions-for-the-maintainer)).

**Requests.** Every request keeps its existing timeout and fail-closed
result. While detached, `Emit` retains the request and returns nil, so
the correlator's timer runs from the emit (`correlator.go:141-147`),
exactly as for a request the control plane never answers.

| Request | Timeout | While detached | Where |
|---|---|---|---|
| `permission_request` (`ask-upstream`) | 60 s default (`permissionPolicy.timeout`) | Fails at its normal timeout. The tool gets "Permission check error" with category `permission_error`, never an allow. | `askupstream.go:17`, `:138-151`; `harness/internal/core/types.go:578-584` |
| `tool_result_request` (`tools.controlPlane`) | `timeoutSeconds` per tool | Times out; the model sees a tool error. The tool itself is held at dispatch, so a request is raised during a break only if it was dispatched before. | `harness/internal/tool/controlplane.go:16-23` |
| `sandbox_token_request` | 60 s per request | The initial request (during construction) succeeds if the re-attach lands within 60 s; otherwise the build fails with `error`. A timed-out refresh is transient and is retried at half the token's remaining lifetime, and every attempt counts against the eight-request budget, because the count rises when `Emit` returns nil. Refresh resumes on its own after a re-attach. | `sandboxidentity/exchange.go:24`, `:50-54`, `:168-177`; `harness/internal/sandboxidentity/refresh.go:13-17`, `:150-167` |
| `batch_submission` | The run deadline | The wait continues; a `batch_result` can arrive only after re-attach. | `harness.proto:764-772`, `batch.go:735-757` |

The difference from today is timing only. Today an emit on a dead
stream fails at once (`correlator.go:141-144`). Inside the window the
request fails at its timeout. Neither path turns a missing answer into
an allow.

`resumed.pending_requests` lists `(type, request_id)` for every request
still waiting, from a registration interface that every
request-emitting component implements. The components are the
permission policies, the async-tool and sandbox-token correlators, and
the batch client. An invariants test pins that each registers. A
control plane acts on a replayed request (`seq ≤ replay_through_seq`)
only if it is listed. A request that has already timed out is never
executed or shown to a person for approval, and no sandbox token is
minted for it. Requests above `replay_through_seq` are live.

## Trace context and observability

| Item | Behaviour |
|---|---|
| `task_assignment.trace_context` | Read once, into the primary run's context (`job.go:195-197`). A break does not touch the run's context or spans, so the trace continues unbroken. `trace_context` on any other ControlEvent type is still ignored (`harness.proto:632-638`). |
| `tool_result_request.trace_context` | Part of the retained event; the replay carries the original value. |
| `run_started.trace_reference` | Protected, so a control plane that missed it gets it in the replay. The `done` summary repeats it. |
| Follow-up runs | Still start their own traces. |
| OTel export | Independent of the control stream (OTLP); unaffected. |
| Run span | Gains span events `control_stream.detached` (scrubbed cause) and `control_stream.reattached` (detached duration, attempts, tombstones). The span-error and log-handler scrub invariants cover the cause. A break while no run is active is logged only. |
| Logs | One `warn` line on detach, one `info` line per re-attach, one `warn` line on `lost`. Emits do not fail while detached, so `logEmitFailure` (`streamloss.go:42-51`) is unaffected. |
| `RunTrace` | No new field in v1 ([open question 8](#open-questions-for-the-maintainer)). |

## Lifecycle interactions

| Where the break lands | With the window non-zero | If the session goes to `lost` |
|---|---|---|
| Before `task_assignment` | Unchanged: the grace is not known yet and no credential exists. | The job fails as today (`job.go:180-181`). |
| Construction | The harness enables re-attach as soon as it accepts the assignment, before `buildJobLoop`, so the build watcher (`job.go:203`) fires only on `lost`. | Construction is cancelled; the job fails with `control_stream_lost`, as today. |
| Active run | The run continues; tool calls wait. | `ControlStreamLost` runs; the run ends `control_stream_lost` after its post-run work; exit non-zero. |
| Run finishes while detached | `done` is protected and retained. The harness waits for a re-attach to deliver it, for at most the rest of the break. | The run keeps its real outcome in `RunTrace` and the `resultSink`. The process exits non-zero, since `done` was never delivered ([open question 9](#open-questions-for-the-maintainer)). |
| Idle follow-up window | The `followUpGrace` timer keeps running. Input queued before the break can start a follow-up run while detached (`loop.go:1455-1481`); its tool calls wait. A re-attach resumes the window. `followUpHistory: "continue"` is unaffected. | The window closes; the exit status is that of today's rule. |
| Follow-up window ends while detached | The harness waits for the rest of the break only to deliver retained events. | Exit as today. |
| SIGTERM / SIGINT | The session context is cancelled; redialling stops; shutdown runs as today. | Not applicable. |

**Close order and deadlines.** In `stirrup job` the transport is
injected, so the factory does not own it (`factory.go:327-335`).
`loop.Close`, which closes the executor, runs before `tp.Close`
(`job.go:120`, `:236`). The sandbox is therefore gone before any drain
wait, and `jobSessionBudget` needs no change. The harness Pod's
`activeDeadlineSeconds` must allow up to one more window spent in
`Close`.

**Outcome.** No new outcome. `lost` uses `control_stream_lost`, and
`RunTrace.error` names the cause: a spent budget, a permanent rejection
with its reason, an OK end, or "re-attach buffer exhausted".

## Security argument

Claim: across a break and a re-attach, the run gains no capability and
takes no tool action that nobody can see. Only a holder of one of the
session's two accepted credentials (the one last issued, or the one
presented before it, until `resumed` retires it) can take over the
session, and only from a stream the control plane already considers
dead.

| Capability | Attached run | While detached | After re-attach |
|---|---|---|---|
| Tool execution | Under the permission policy | Held at dispatch | Under the policy, as before |
| Tool approval (`ask-upstream`) | Only on `permission_response{allowed:true}` (`askupstream.go:86`) | Fails at its timeout | Only on an explicit allow for a request ID still pending; `control_seq` stops duplicates |
| Control-plane tool results | From `tool_result_response` | Time out | As before |
| Sandbox identity token | Per `sandbox_token_response` | None issued; the current token expires on schedule | Issued only for listed pending requests, from the verified session binding |
| Model input (`user_response`) | Queued and screened | None arrives; queued input still runs | Exactly once, through `control_seq` |
| A new task | Duplicates ignored | Same | `task_assignment` is invalid on `ResumeTask` |
| Network reach | `CONTROL_PLANE_ADDR` | Redials to the same address only | Same |
| Run duration | `timeout` | Unchanged | Unchanged |

What the window costs, and how each cost is bounded:

| Cost | Bound or mitigation |
|---|---|
| The control plane cannot `cancel` a detached run. | Nothing it could stop is happening, because tools are held. Model calls continue for at most the break, inside the run's token budget. A `cancel` sent during the break is in the outbox and lands on re-attach. |
| An on-path attacker who can only drop packets, even under TLS, now delays the run instead of cancelling it. | The per-session budgets (three windows, 10 re-attaches) bound the delay. Held tools mean the attacker gains no unwatched action. |
| A lapsed run still does post-run work: the git checkpoint, `postRun` hooks (the sum of their timeouts, each up to 1800 s, plus 30 s), the workspace export (5 min), the trace finish (90 s) and result delivery (60 s). | This is today's behaviour on any loss (`harness/internal/core/loop.go:547-576`, `:2409-2417`; `harness/internal/core/handoff.go:18-23`). It is why the only sanctioned fence is delete-and-wait. |
| Two workers, if the control plane reschedules while the original lives. | [Control-plane obligations](#control-plane-obligations), item 3. |
| The redial trusts the network again. | Plaintext re-attach is refused unless the operator opts in. |
| A stolen credential could be presented by a thief. | It lives only in a redacting transport type, never reaches the sandbox, the model or a record, and is rejected under the `local` executor. It is rotated on every re-attach and retired on `resumed`. It is accepted only for a stream the control plane considers dead, so it cannot take over a healthy run. Optionally it is bound to a per-Pod mTLS identity. |
| Retained events hold transcript content in memory. | Already scrubbed and bounded; only the unacknowledged suffix; 8 MiB cap. |

The read-only-mode invariant, the secret-reference rule and the event
byte bounds are untouched. The loop in
`harness/internal/core/loop.go` gains one injected interface, the
dispatch hold, and no transport knowledge.

## Control-plane obligations

A control plane that sets the window takes on these obligations. They
become part of the `control_stream_reattach` capability's contract.

1. **Persist the session record** where every replica can read it: the
   generation, the previous and current credential hashes, the last
   `seq` received, the time of the last event received, the `control_seq`
   outbox, and a revoked flag.
2. **Answer a `reattach` within 5 s** with `reattach_accepted`, a
   `reattach_rejected` (permanent or not) or `ABORTED`. Refuse it,
   retryably, while the session's previous stream is still live.
3. **Fence with state, then delete.** Before rescheduling a task, set
   the session's revoked flag (every later re-attach gets a permanent
   rejection), delete the harness Pod and its sandbox, and wait until
   the Kubernetes API reports both gone. A timed reschedule is not
   sanctioned: the reschedule time cannot be computed after a
   control-plane crash, and a lapsed run's post-run work can outlast
   any window. Run Jobs with `backoffLimit: 0` so Kubernetes does not
   retry on its own.
4. **Check for a completed run** before rescheduling a session that
   never delivered `done`: read the `resultSink` and the Job's terminal
   status.
5. **Send `ack`** at least every 10 s, after persisting what it
   acknowledges.
6. **Number and keep control events** (`control_seq`, outbox) and send
   only from the replica holding the current generation.
7. **Act on replayed requests only when listed** in `pending_requests`,
   and never prompt a person for an approval that is not listed.
8. **Derive run identity on a resumed stream** only from the verified
   session record, after sending `reattach_accepted`. Count
   sandbox-token requests per session, not per stream. Issue tokens
   with a lifetime of at least the window plus 60 s.
9. **Issue the credential only over TLS** or a verified mesh identity,
   and revoke it when the task is rejected, cancelled or completed.
10. **End a stream with `UNAVAILABLE`**, not OK, when draining for a
    deploy, to invite a re-attach; OK means the session is over.
11. **Rate-limit `reattach`** per session and globally.

## Compatibility and the `ready` capability

| Combination | Result |
|---|---|
| Old harness, new control plane | `ready.capabilities` lacks `control_stream_reattach` and `ready.config_fields` lacks `controlStreamGraceSeconds`. An old harness drops the field without notice (#905 would make it reject instead), so the control plane gates on the capability. |
| New harness, old control plane | The field is never set; behaviour is unchanged. |
| New harness, field set, mixed replicas during a rolling deploy | `ResumeTask` to an old replica returns `UNIMPLEMENTED`, which is retried until a new replica answers or the break ends. |

`control_stream_reattach` covers the field, `ResumeTask`, the
credential, both sequence numbers, `ack`, the new event types and the
obligations above. It is added to `types.HarnessCapabilities()`
(`types/events.go:452`), whose exact list is pinned by
`TestHarnessCapabilities_AdvertisesFeaturesAndIsolatesSlice`
(`types/events_test.go:208`). That test is updated; it is the only
existing test that changes.

## Alternatives considered

| Alternative | Why not |
|---|---|
| In-band `reattach` as the first message of `RunTask` (draft 1) | An old replica ignores the unknown type and either waits for `ready` or sends a `task_assignment` to nobody. A separate RPC fails fast with `UNIMPLEMENTED`. |
| A gap marker only, with no retention or sequence numbers | Requests raised just before the break, `run_started` and `done` would be lost, so every approval in flight would time out even after a fast re-attach, and the control plane could not tell a finished run from a running one without a `resultSink`. Duplicate inbound events would remain possible. |
| A timed fence (draft 1: wait the window plus a detection bound, then reschedule) | It is not computable after a control-plane crash, and it ignores post-run work that can run for tens of minutes. |
| An acknowledged lease that lets the harness expire itself and skip side-effecting post-run work | It would allow rescheduling without deleting the Pod. It changes post-run behaviour that #740 made deliberate, and it relies on monotonic clocks across a VM pause. Kept as [open question 2](#open-questions-for-the-maintainer). |
| HTTP/2 keepalive as the loss detector (draft 1) | Default grpc-go servers reject 20 s pings; proxies answer pings themselves; the first stream is dialled before the window is known. |
| Letting auto-allowed tools run while detached (draft 1) | That is the orphaned work #659 removed. |
| A per-Pod proxy that terminates the stream locally | It moves the stream endpoint into a second process that can fail the same way, and adds a component to every deployment. |

## Implementation map

| Area | Change |
|---|---|
| Prerequisites | Session-unique request IDs with an invariants test; byte bounds on request inputs; session-wide heartbeats (#913). |
| `types/runconfig.go` | Field, range, the heartbeat relation, JSON name. |
| `proto/harness/v1/harness.proto`, `gen/` | `ResumeTask`, field 62, the event fields and types, `PendingRequest`; `just proto`. |
| `harness/internal/transport/grpc_translate.go` | The field, and the new event fields both ways, with a translation test for the event fields (the proto-mirror invariant covers `RunConfig` only, `harness/internal/invariants/protomirror_test.go:282`). |
| `harness/internal/transport/` | A session holder behind the existing `GRPCTransport` API: retention with tombstones, both sequence numbers, `ack` handling, liveness timeout, redial, the handshake and the replay, through one bounded send path. Session-scoped `Done`/`Err` and a separate per-stream signal for `Close`. The credential type. A pending-request registry. |
| `harness/internal/core/` | The dispatch-hold interface, consulted before execution, injected by the factory; span events through an optional transport interface. No transport import in `loop.go`. |
| `harness/cmd/stirrup/cmd/job.go` | After accepting the assignment: read the window and credential, run the checks in [Configuration](#configuration), enable re-attach before `buildJobLoop`. |
| `harness/cmd/stirrup/cmd/harness.go`, `harness/harnessapi/` | Ignore a non-zero window with a warning. |
| `harness/internal/invariants` | The credential is read in one place and never reaches a log, trace, `RunResult` or `RunConfig.Redact()` output; every request emitter registers pending IDs. |
| `examples/controlplane` | Implements `ResumeTask`, `ack`, the outbox and the session record in memory, so `smoke.sh` can exercise a break. |

## Test plan

Rows 1 to 5 are the issue's acceptance criteria. The rest pin the
design's own rules. Timing tests use injected clocks or short windows
unless the row says otherwise.

| # | Criterion | Test |
|---|---|---|
| 1 | Design note merged | This document. |
| 2 | Field unset: behaviour unchanged | The existing stream-loss tests pass unmodified: `TestRun_ControlStreamLost*` and `TestControlStreamLost_*` (`harness/internal/core/streamloss_test.go`); `TestRunJob_ControlPlaneEndingRPCDuringProviderCallCancelsRun`, `TestRunJob_ConnectionFailureDuringProviderCallCancelsRun`, `TestRunJob_StreamLostDuringConstructionStartsNoRun`, `TestRunJob_StreamEndedAfterDoneClosesFollowUpWindow`, `TestWatchControlStream_*` and `TestRunWithConfig_GRPCTransportLossCancelsRun` (`harness/cmd/stirrup/cmd/controlstream_test.go`); `TestGRPCTransport_*` in `grpc_liveness_test.go`. A new test asserts that with the field unset no event other than `ready` sets any new field, and that `ready` differs from `main`'s only by the capability and the path. |
| 3 | Break mid-turn, re-attach inside the window, real outcome, complete sequence or explicit gap | Over bufconn, a fake control plane fails the RPC with `UNAVAILABLE` while the provider call is blocked, accepts the `ResumeTask`, then the provider is released. Asserts outcome `success`, a contiguous `seq` from 1 to `done`, and no tombstone. A variant with a small cap asserts tombstones for droppable events and every protected event intact. |
| 4 | The window lapses | The control plane never returns. Asserts `control_stream_lost`, the cause in `RunTrace.error`, a non-zero exit, and an end about one window after the loss. |
| 5 | A permission request raised while detached is auto-denied at its normal timeout (the issue's wording; the harness reports a permission error, never an allow) | 1 s policy timeout, 5 s window, a tool needing approval dispatched during the break. Asserts the tool result "Permission check error" with category `permission_error` at about 1 s, not at emit time and not at lapse. After a re-attach at about 3 s, the request is in the replay, absent from `pending_requests`, and a late `permission_response` for it is dropped. |
| 6 | Dispatch hold | An auto-allowed tool called during a break does not run until re-attach, and never runs if the session goes to `lost`. The same holds for an MCP tool. |
| 7 | OK end | An OK end during a run gives today's `control_stream_lost`; an OK end after `done` ends the session with no redial. |
| 8 | Liveness | No `ack` for 30 s is a loss on the first stream. A control plane that stops reading (flow-control stall) is detected by the same timeout. A real grpc-go server with default keepalive enforcement sees no pings from the harness. |
| 9 | Rejections | `reattach_rejected{permanent:true}` goes to `lost` with the scrubbed reason. `UNIMPLEMENTED`, `UNAUTHENTICATED` without that event, and `ABORTED` are retried. |
| 10 | Credential lifecycle | Rotation on every re-attach; the previous credential accepted until `resumed`, rejected after; a lost `reattach_accepted` recovered with the previous credential; two replicas racing on the generation; an oversized or non-printable credential rejected; a missing credential rejects the task. |
| 11 | Supersession | A `reattach` while the old stream is live is refused retryably and succeeds once the old stream is dead. |
| 12 | Credential hygiene | The credential never appears in logs, the JSONL trace, OTel attributes, `RunResult`, `RunConfig.Redact()`, any `types` event, or any control handler. |
| 13 | Posture checks | Plaintext without the opt-in, and the `local` executor, each reject the task. |
| 14 | Inbound sequence | A `user_response` queued before the break and not yet echoed is not applied twice when the outbox re-sends it. A `cancel` sent during the break lands after re-attach. |
| 15 | Replay boundary | A replayed request outside `pending_requests` is not acted on by the example control plane. |
| 16 | Break during replay | A second break during the replay continues the same break; the window does not reset until the replay is acknowledged. |
| 17 | Budgets | Repeated breaks stop at three windows of detached time or 10 re-attaches. An event that makes the control plane abort the replay on every attempt ends in `lost` within the budget. |
| 18 | Buffer exhaustion | Protected events over the cap go to `lost` with "re-attach buffer exhausted". |
| 19 | Construction and follow-up | A break during a blocked `buildJobLoop` recovers. A follow-up started from queued input during a break, and a run that finishes during a break, both re-attach with a session-bound credential despite the changed `run_id`. |
| 20 | Run finishes and never re-attaches | The real outcome is in `RunResult`; the exit status is non-zero. |
| 21 | SIGTERM while detached | Redialling stops; shutdown is not delayed by the window. |
| 22 | Validation and wiring | Range 0 or 30-300; the heartbeat relation; the proto-mirror invariant covers field 62; `stirrup harness` ignores the field with a warning; `ready` carries the capability and the path; `-race` over `Emit` concurrent with replay. |

## Open questions for the maintainer

Each question has a recommendation; the design above assumes it.

1. **Does this protocol meet the 2026-10-08 bar** for continuing after
   a loss? *Recommendation:* yes, as a session protocol (authenticated,
   acknowledged re-attach; no duplicate delivery; no tool action while
   detached; state-based fence), not as a lease. #456 stays the home of
   continuity across process restarts.
2. **A real lease?** An acknowledged lease would let the harness expire
   itself and skip side-effecting post-run steps after a lapse, so a
   control plane could reschedule without deleting the Pod.
   *Recommendation:* not in v1. Delete-and-wait is already required by
   the integration guide's fencing advice, and the lease changes
   post-run behaviour on every loss.
3. **The cap.** 60 s, 300 s or 600 s. *Recommendation:* 300 s. With the
   dispatch hold, a longer break costs waiting time and model spend,
   not unwatched actions.
4. **Session budgets.** Three windows of total detached time and 10
   re-attaches per session, or per run. *Recommendation:* per session
   as written.
5. **Plaintext re-attach.** Refuse unless the operator opts in through
   the Pod environment (recommended), always refuse, or only warn.
6. **Retention cap.** Fixed at 8 MiB (recommended for v1), or an
   operator environment variable. A `RunConfig` field is not
   recommended.
7. **Dispatch hold scope.** Hold every tool call (recommended), or only
   tools that consult the permission policy.
8. **Record breaks in `RunTrace`?** For example `controlStreamBreaks`
   and `controlStreamDetachedMs`. *Recommendation:* span events only in
   v1.
9. **A run that finished while detached and never re-attached.**
   *Recommendation:* keep the real outcome in `RunTrace` and the
   `resultSink`, and exit non-zero, so a loss never exits zero (the
   #740 rule) and the control plane checks the sink before
   rescheduling. The alternative reports `control_stream_lost`, which
   discards a completed run's real result.
10. **The prerequisites.** Fix the shared `perm` request IDs and cap
    request inputs first, as their own issues (recommended), since both
    are defects today.
11. **Sandbox-token refreshes while detached.** A timed-out refresh is
    retried and counts against the eight-request budget, so a long break
    can spend it. *Options:* leave it (fail closed: refreshing stops
    once the budget is spent); pause the refresher while detached
    through a transport callback; or count only requests that the
    control plane acknowledged. *Recommendation:* pause while detached,
    since a request nobody can answer should not spend the budget.

## Related issues

| Issue | Relation |
|---|---|
| #659, PR #740 | The current cancel-on-loss behaviour and the decision this note revisits. |
| #456 | Conversational continuity across process restarts; this note is limited to one process. |
| #913 | Session-wide liveness signals; the harness-side liveness here depends on it. |
| #905 | Rejecting unknown `RunConfig` fields; it changes what an old harness does with the field. |
| #693, #694 | The TLS options and recipes the plaintext rule relies on. |
| #7 | Earlier transport-security proposal with a session token and sequence numbers in both directions, the closest prior design. |

## Docs to update on implementation

- [`integration-guide.md`](integration-guide.md): the Delivery
  paragraph in [Watching a run live](integration-guide.md#watching-a-run-live)
  (replacing the pointer to this note), the stream lifecycle, both event
  tables, [Compatibility](integration-guide.md#compatibility), the
  fencing contract, and the control-plane obligations above.
- `proto/harness/v1/harness.proto`: the `HarnessService` lifecycle
  comment, `ResumeTask`, the new event-table entries, and the
  sandbox-identity sentence that the control plane "derives the run
  identity from the authenticated stream the task was assigned on"
  (`harness.proto:240-241`).
- [`configuration.md`](configuration.md): the field reference.
- [`deployment.md`](deployment.md): the session table, the
  `activeDeadlineSeconds` sizing, and the plaintext opt-in variable.
