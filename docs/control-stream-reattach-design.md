# Control stream re-attach: design note

Status: proposal for review, draft 3, 2026-11-16. Nothing here is
implemented. This note is the first acceptance criterion of issue #911
(an optional grace period and re-attach before a lost control stream
cancels the run). Code follows only after the note is merged and the
[open questions](#open-questions-for-the-maintainer) are answered.

Drafts 1 and 2 were reviewed adversarially. Draft 3 changes four
things of substance:

- Liveness now requires proof that the control plane is receiving, not
  only that it is sending.
- The hold on tool dispatch is now a gate on every phase that runs in
  the sandbox or acts outside the harness.
- The safety claim is narrowed to what holds, with the work that still
  runs unwatched listed and bounded.
- Both sides now detect a control plane that has lost session state.

Where a choice from an earlier draft was dropped,
[Alternatives considered](#alternatives-considered) says why.

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
- [The phase gate and requests while detached](#the-phase-gate-and-requests-while-detached)
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
default 0, range 60-300), opens a bounded window after the control
stream is lost.

- **Detection.** The control plane acknowledges what it has received.
  The harness treats any event or heartbeat left unacknowledged for
  30 s as a loss, so a control plane that keeps sending but has stopped
  receiving is detected.
- **While detached**, the model may keep streaming, but every tool call,
  hook, verifier run and post-run step waits at a gate until the stream
  is back. A detached run gains no capability. Steps already running
  when the loss is detected finish, and the up to 40 s before detection
  runs as if attached; [What still runs unwatched](#what-still-runs-unwatched)
  lists and bounds both. Requests to the control plane keep their
  timeouts and fail closed.
- **Redial.** The harness redials `CONTROL_PLANE_ADDR` and calls a new
  RPC, `ResumeTask`. It presents a resume credential that the control
  plane issued on the `task_assignment` and replaces on every re-attach.
- **Delivery.** Both directions carry sequence numbers, so after a
  re-attach each side re-sends exactly what the other has not processed.
  A sequence that goes backwards means the control plane lost state, and
  the session ends.
- **Lapse.** If the window passes without a re-attach, the run ends
  `control_stream_lost` as it does today, and its post-run work runs as
  it does today.
- **Fence.** The only sanctioned way to reschedule a task whose harness
  may still be alive is to revoke the session and then stop the harness
  and its sandbox, and to wait until something that can confirm it
  reports them stopped.

With the field unset, stream-loss and delivery behaviour is unchanged.
`ready` gains one capability value, one `config_fields` path and an
`incarnation`; no new field is set on any other event.

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
offers a session protocol instead:

- Re-attach is authenticated and acknowledged.
- Nothing is applied twice.
- Once a loss is detected, no gated step starts until the stream is
  back.
- The work that does run unwatched is bounded and does not grow with
  the window.
- The fence is state-based.

Whether that meets the bar is
[open question 1](#open-questions-for-the-maintainer).

## Goals and non-goals

| Goals | Non-goals |
|---|---|
| A run survives a control-plane restart, deploy or brief network break shorter than the window. | Surviving a harness process restart or a Pod reschedule. That needs the persisted session log of [`sessions-spec-draft.md`](sessions-spec-draft.md) (#456). |
| With the field unset, stream-loss and delivery behaviour is unchanged. | Exactly-once execution of side effects across a fence. The fence prevents two workers; it does not make tools idempotent. |
| A detached run gains no capability. Once a loss is detected, no gated step starts until the stream is back, and the work that runs unwatched is bounded independently of the window. | Stopping a step that is already running when a loss is detected. |
| After a re-attach, neither side applies an event twice or misses one silently. | Moving a live stream between control-plane replicas. A resumed stream may land on any replica; the control plane shares session state between them. |
| The loop stays a pure function of its interfaces: re-attach lives in the transport and the `cmd` package, and the phase gate is an injected interface. | Re-attach for `stirrup harness --transport grpc` or a `harnessapi` embedder. Neither has a `task_assignment` to carry a credential. |
| | A timed lease that lets a control plane reschedule without stopping the harness ([open question 2](#open-questions-for-the-maintainer)). |

## Prerequisites

These must land first, each as its own change, because the protocol
depends on them:

| Prerequisite | Why | Evidence |
|---|---|---|
| Session-unique request IDs | A control plane re-sends answers by request ID. Today two `AskUpstreamPolicy` instances can share one transport, the permission policy and the Rule-of-Two ask gate, and both number `perm-1`, `perm-2` and so on. A re-sent allow for one could then approve the other's tool. The collision exists without re-attach, since every handler sees every response. The fix must cover every allocator: both `perm` correlators, the `async-tool` and `sbid` correlators, and the batch client's own counter. | `harness/internal/core/factory.go:2129`, `:2265`; `harness/internal/permission/askupstream.go:74`; `correlator.go:179-184`; `harness/internal/core/types.go:367`; `harness/internal/sandboxidentity/exchange.go:142`; `batch.go:814-815` |
| Byte bounds on request inputs | `boundEventFields` caps `input` only on `tool_call` and `tools_loaded`. `permission_request`, `tool_result_request` and `batch_submission` are uncapped, so a single event can exceed the 4 MiB receive limit and make the control plane abort the stream. A `batch_submission`'s `input` is the whole serialised provider request, so it cannot be truncated: an oversized submission is refused, or the call falls back to a live provider call. The other two may be capped. | `harness/internal/transport/eventbound.go:47-57`, `batch.go:717-740` |

Session-wide heartbeats (#913) are not a prerequisite. While the window
is non-zero the transport sends its own heartbeats for the whole
session (see [Liveness](#liveness-and-loss-detection)). When #913 lands,
its heartbeats carry the same fields.

## Overview

The transport holds a session that outlives individual streams:

```text
              loss                       handshake ok
  attached ─────────▶ detached ─────────────────────▶ replaying
     ▲                   │  ▲                             │
     │                   │  └───────── loss ──────────────┤
     │                   ▼                                │
     │                 lost ◀── budget spent, permanent   │
     │                          rejection, OK end after   │
     │                          `reattach_accepted`, or   │
     │                          a sequence regression     │
     └──────────── replay acknowledged ◀──────────────────┘
```

| State | Emit | Control events in | Gated steps | `Liveness.Done()` |
|---|---|---|---|---|
| attached | Sent with `seq`; retained until acknowledged | Dispatched | Run | open |
| detached | Retained only; returns nil | None | Wait | open |
| replaying | Queued behind the replay; `Emit` blocks on HTTP/2 flow control as today | Dispatched after `reattach_accepted` | Wait until the replay is acknowledged | open |
| lost | Fails, as today | None | The run is cancelled; post-run steps proceed as today | closed; `Err()` names the cause |

`Done()` and `Err()` describe the session, not one stream. They fire
only on entering `lost`, so `watchControlStream` and
`ControlStreamLost` stay unchanged and still fire exactly once. The
transport keeps a separate per-stream signal for `Close`'s drain, which
today waits on the reader (`grpc.go:264-277`).

A stream that the control plane ends with status OK (`io.EOF` at the
harness) is a deliberate end, not a break:

- On an attached stream, or on a resumed stream after
  `reattach_accepted`, an OK end goes straight to `lost`, as it does
  today. During a run this is a loss; otherwise it is the normal end of
  the session. It also releases every retained event, so the exit
  status never depends on an `ack` for `done` that a control plane
  ending the RPC had no reason to send.
- On a resumed stream before `reattach_accepted`, an OK end without a
  `reattach_rejected` is treated as a failed attempt and retried.

Only a non-OK status, a transport error or a liveness timeout starts
the window. A control plane that drains for a deploy ends streams with
`UNAVAILABLE` to invite a re-attach.

## Configuration

| Item | Value |
|---|---|
| JSON | `controlStreamGraceSeconds` |
| Go | `RunConfig.ControlStreamGraceSeconds int` (`json:"controlStreamGraceSeconds,omitempty"`) |
| Proto | `int32 control_stream_grace_seconds = 62;` in `RunConfig`. Plain `int32`, like `heartbeat_seconds = 61` (`harness.proto:792`), because 0 means off. |
| Default | 0: no window; a loss cancels at once, as today. |
| Range | 0, or 60-300 seconds. A control plane declares a stream dead 30 s after its last event (see [Liveness](#liveness-and-loss-detection)), so a shorter window leaves no time to re-attach after a replica crash. |
| Per break | Each break lasts at most the window. |
| Per session | The total detached time across all breaks is capped at three times the window, and the number of accepted re-attaches at 10 ([open question 4](#open-questions-for-the-maintainer)). Spending either budget goes to `lost`. |
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

The cap of 300 s covers a rolling deploy or a control-plane restart. A
longer window lengthens waiting time, model spend and the delay before
a `cancel` sent during the break lands. It does not lengthen the
unwatched work, which is bounded by the detection bound and by the
steps already running (see
[What still runs unwatched](#what-still-runs-unwatched)). The cap is
[open question 3](#open-questions-for-the-maintainer).

## Liveness and loss detection

HTTP/2 keepalive is not a usable primary signal. grpc-go servers
enforce a 5-minute minimum ping interval by default and answer faster
pings with `GOAWAY too_many_pings`. Proxies and mesh sidecars answer
pings themselves, so a hung replica behind one is never detected. The
first stream is also dialled (`job.go:116`) before the window is known,
and keepalive is fixed at dial time. Liveness is therefore carried by
the application, and it is based on progress, not on traffic. Inbound
traffic alone proves only that the control plane can send; an
acknowledgement that covers what the harness sent proves that it is
receiving.

| Direction | Signal | Rule |
|---|---|---|
| Harness to control plane | `heartbeat` for the whole session, at the effective heartbeat interval or 10 s, whichever is shorter. Each carries a session-scoped `heartbeat_seq` and the harness's `last_control_seq`. The transport sends these itself; the loop's run heartbeats (`harness/internal/core/loop.go:1545`) gain the same fields. | The control plane treats 30 s without an event on the session's live stream, or the stream ending, as the stream being dead. Until then it refuses a re-attach for that session with a retryable rejection, so a valid credential cannot take over a stream that is still live. |
| Control plane to harness | `ControlEvent{type:"ack", ack_seq, heartbeat_ack}`, sent within 10 s of receiving each sequenced event or heartbeat, from the path that persisted it | `ack_seq` is the highest harness `seq` the control plane has durably processed; `heartbeat_ack` is the highest `heartbeat_seq` it has recorded. |

**The progress rule.** Every sequenced event and every heartbeat is
*outstanding* from the moment it is handed to the current stream's send
path until an `ack` covers it (`ack_seq ≥ seq`, or
`heartbeat_ack ≥ heartbeat_seq`). If any item has been outstanding for
more than 30 s, the harness treats the stream as lost. It cancels that
stream's context and enters `detached`. Cancelling a stream is local to
that stream; it is not the session context, so `recordStreamEnd` does
not read it as a local close.

- **Detection bound.** A heartbeat is sent at most 10 s after the
  control plane stops receiving, and it is outstanding for 30 s, so a
  receive path that stops is detected within 40 s. A stream that fails
  visibly (a reset, a non-OK status) is detected at once.
- **Acks that stall while other events arrive.** Other inbound events
  (`permission_response`, `user_response`, `cancel`, `batch_result`) are
  dispatched normally while the stream is attached, but they are not
  progress. If acks stop covering outstanding items, the harness
  detaches after 30 s even though events are still arriving. An event
  read before the detach is processed, and a `cancel` among them acts at
  once. Anything sent after the detach is not read, and the outbox
  re-sends it after the re-attach, because the harness reports what it
  processed. A control plane whose acknowledgement path keeps failing
  spends the per-session budgets and reaches `lost`.
- **Ack values that are wrong.** An `ack_seq` or `heartbeat_ack` lower
  than one already received is a sequence regression: the session goes
  to `lost` (see
  [Inbound sequence numbers](#inbound-sequence-numbers)). A value above
  the highest the harness has assigned is a protocol error and detaches
  the stream.
- **Replays.** An item's age is measured from its most recent send on
  the current stream, so a replayed event does not count as overdue
  because it was first sent before the break.

A blocked `Send` holds the transport's write lock (`grpc.go:137-150`).
Items waiting behind it are outstanding, so a control plane that stops
reading is detected by the same rule. Cancelling the stream unblocks
the `Send`, so a flow-control stall cannot hold a correlator timer past
its timeout by more than the detection bound.

## Redialling

| Parameter | Value | Reason |
|---|---|---|
| Target | `CONTROL_PLANE_ADDR`, read once at process start (`job.go:84`) | A `RunConfig` cannot redirect the redial. |
| Connection | A fresh `grpc.ClientConn` per attempt; the old one is closed after its reader has exited | The new connection re-resolves DNS and lands on any healthy replica, without grpc-go's connect backoff, which grows to 120 s. Waiting for the old reader keeps one reader dispatching at a time. |
| TLS material | The CA bundle and client key pair are re-read on every attempt | Short-lived certificates (cert-manager, SPIFFE) rotate during a session; today they are loaded once (`harness/internal/transport/tls.go:105-110`). |
| Backoff | First attempt immediately, then a full-jitter delay of up to `min(15 s, 0.5 s × 2^n)`; a server retry-pushback value is honoured | Many harnesses lose one replica at once; full jitter spreads their reconnects, and the cap protects a recovering control plane from a reconnect storm. |
| Per-attempt deadline | `min(10 s, time left in the break)` for the dial, the `ResumeTask` open and `reattach_accepted` | A stalled handshake does not consume the window. Control planes answer within 5 s, including the store's compare-and-set. |
| Stops | On `reattach_accepted`, a permanent rejection, an OK end after `reattach_accepted`, a sequence regression, a spent budget, or the session context ending (SIGTERM) | Each of these either completes the re-attach or makes it impossible. |

**Rejections.** Permanence is signalled explicitly, never inferred
from a status code, because proxies and replicas that are still
starting also return `UNAUTHENTICATED`, `PERMISSION_DENIED` or
`NOT_FOUND`. The control plane answers a `reattach` it will never
accept with `ControlEvent{type:"reattach_rejected", permanent: true,
reason}` and then ends the RPC with OK. The harness goes to `lost`, and
`RunTrace.error` carries the reason after scrubbing and a 1 KiB cap.
Every other outcome is retried within the window. That includes
`UNIMPLEMENTED` from an old replica during a rolling deploy, a
`reattach_rejected` with `permanent: false`, a lost compare-and-set or
a slow store (`ABORTED`), and an OK end before `reattach_accepted`.

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
   │── resumed {replay_through_seq: M, pending_requests} ─▶│ compare-and-set: stream becomes
   │── tombstones and events with N < seq ≤ M ─────────▶│ the live one; previous
   │◀─ control events with control_seq > last_control_seq │ credential retired
   │◀─ ack {ack_seq ≥ M} ───────────────────────────────│ the break ends here
   │── live events ────────────────────────────────────▶│
```

Strict ordering on a resumed stream:

1. The harness dispatches nothing before `reattach_accepted`. Any other
   first event abandons the attempt (retryable).
2. `reattach_accepted.ack_seq` above `last_seq` is a protocol error and
   abandons the attempt (retryable). Below the highest `ack_seq` the
   harness has received is a sequence regression and goes to `lost`; no
   retry can repair it, because the harness no longer holds the events
   the control plane has lost.
3. After `reattach_accepted`, a second `reattach_accepted` or a
   `task_assignment` is dropped and logged.
4. The replay is sent from its own goroutine, never from the reader, so
   the harness keeps reading acks and outbox events while the replay
   waits on flow control. The current transport already forbids control
   handlers from calling `Emit` (`grpc.go:173-174`) for the same reason.

Wire additions. RunConfig field 62 is reserved for this work. The other
numbers are the next free ones at `0f0bc8e9` and are illustrative.

| Message | Addition | Meaning |
|---|---|---|
| `HarnessService` | `rpc ResumeTask` | See above. |
| `RunConfig` | `control_stream_grace_seconds = 62` | See [Configuration](#configuration). |
| `HarnessEvent` | `uint64 seq = 30` | Session-scoped, from 1, on every retained event after the assignment, only when the window is non-zero. Absent on `ready`, `reattach`, `resumed`, `heartbeat` and `batch_waiting`. |
| `HarnessEvent` | `string incarnation = 31` | On `ready` and `reattach`: a random ID per harness process, so `(incarnation, seq)` stays unique if #456 later resumes across processes. Set on `ready` whenever the build advertises the capability, since the window is not known until the assignment. |
| `HarnessEvent` | `string session_id = 32`, `string run_id = 33` | On `reattach`: the session echo and the active run, both informational (see [Authenticating](#authenticating-a-re-attach)). |
| `HarnessEvent` | `uint64 last_seq = 34`, `uint64 last_control_seq = 35` | On `reattach`: the highest `seq` assigned, and the highest `control_seq` the harness processed. `last_control_seq` is also on every `heartbeat` while the window is non-zero. |
| `HarnessEvent` | `uint64 replay_through_seq = 36` | On `resumed`: the last `seq` that is a replay. Events above it are live. |
| `HarnessEvent` | `repeated PendingRequest pending_requests = 37` | On `resumed`: each request still waiting for a response, as `(type, request_id)`. |
| `HarnessEvent` | `bool evicted = 38` | On a tombstone; see [Retention](#outbound-sequence-numbers-and-retention). |
| `HarnessEvent` | `uint64 heartbeat_seq = 39` | On `heartbeat` while the window is non-zero: session-scoped, from 1. |
| `HarnessEvent` types | `reattach`, `resumed` | Sent only on `ResumeTask`. |
| `ControlEvent` | `uint64 control_seq = 12` | Session-scoped, assigned by the control plane to every outbox event (see [Inbound sequence numbers](#inbound-sequence-numbers)) when the window is non-zero. |
| `ControlEvent` | `uint64 ack_seq = 13` | On `ack` and `reattach_accepted`. |
| `ControlEvent` | `bool permanent = 14` | On `reattach_rejected`. `reason` (field 6) carries the explanation. |
| `ControlEvent` | `uint64 heartbeat_ack = 15` | On `ack`. |
| `ControlEvent` types | `ack`, `reattach_accepted`, `reattach_rejected` | `ack` is valid on both RPCs; the other two only as the first event on `ResumeTask`. None of the three carries a `control_seq` or is kept in the outbox. |
| Credential | `resume_token` | Carried in a dedicated proto message field on `task_assignment`, `reattach_accepted` and `reattach`, never translated into `types.ControlEvent` or `types.HarnessEvent`. |

All sends (`Emit`, heartbeats, the handshake and the replay) go through
one function that bounds the event, converts it and sends it. The
existing `TestHarnessEvents_PassTheByteBound` invariant
(`harness/internal/invariants/bytebound_test.go:40-82`) therefore
covers them without an allow-list entry. The credential is attached to
the proto message inside that function, after bounding.

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
| Client identity | Where the control plane can see a per-Pod client identity (a per-Pod mTLS certificate or a mesh SPIFFE ID), it must require that a re-attach comes from the same identity as the original stream. That makes a stolen credential useless off the Pod. Where every Pod shares one certificate, the credential is the only binding. |
| Generations | The session record holds a generation number, the hash of the credential last presented in an accepted re-attach (the previous one) and the hash of the credential last issued (the current one). A re-attach presenting either is accepted through a compare-and-set whose predicate is the generation and `revoked = false`. The loser of a race between replicas ends its RPC with `ABORTED`, which the harness retries. |
| Rotation | Every `reattach_accepted` issues a new credential. The harness replaces its copy before it sends `resumed`. |
| Retirement | When `resumed` arrives, the replica promotes its stream to the session's live stream and retires the previous credential, in one compare-and-set on the generation it accepted. If the generation has moved on, the promotion fails and the replica ends the RPC with `ABORTED`. If `resumed` never arrives, the next attempt presents the previous credential, which is still accepted. |
| Lifetime | The control plane rejects a credential permanently once the session is revoked, cancelled or completed, or once 30 s plus the window have passed since the last event received on the session's live stream. That time is persisted, taken from the store's clock rather than a replica's, and updated only by events on the current generation's live stream: a `reattach`, or an event on a superseded stream, does not refresh it. |
| Storage in the harness | One transport-private type whose `String`, `GoString`, `LogValue` and `MarshalJSON` redact it. The transport reads it from the proto `task_assignment` before translation, so no control handler sees it. It is never logged, traced, written to `RunConfig`, `RunResult` or the trace, passed to the executor or exposed to the model. A random handle matches no scrubber pattern, so these rules, pinned by an invariants test, are the only protection. |

**What mTLS already provides.** With `CONTROL_PLANE_TLS_CERT_FILE`
set, the harness presents a client certificate
(`harness/internal/transport/tls.go:105-110`). That proves the caller
holds a certificate from the control plane's CA. It does not prove
which session the caller owns when several Pods share one certificate,
for example one Kubernetes Secret mounted into every harness Pod. The
client-identity rule above closes that gap where per-Pod identities
exist. It does not replace the credential.

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
| Not sequenced | `heartbeat` and `batch_waiting`. A replayed liveness signal means nothing, and leaving them unsequenced keeps the sequence free of holes that are not losses. A heartbeat carries `heartbeat_seq` instead, which only liveness uses. |
| Release | An `ack` releases every event with `seq ≤ ack_seq`. With acknowledgement within 10 s, the retained set while attached is about 10 s of output. An OK end releases everything (see [Overview](#overview)). |
| Monotonic acknowledgement | `ack_seq` never decreases within a session. A decrease means the control plane lost events the harness has already released, so the session goes to `lost`. |
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

- **The outbox.** The control plane assigns a session-scoped
  `control_seq` to every event after `task_assignment` except `ack`,
  `reattach_accepted` and `reattach_rejected`, and keeps those events in
  a per-session outbox. The three exceptions are never stored: an `ack`
  is stale as soon as a newer one exists, and the other two carry the
  plaintext credential.
- **What the harness reports.** The harness records the highest
  `control_seq` it has dispatched. It reports it as `last_control_seq`
  on `reattach` and on every `heartbeat`. The control plane may trim
  the outbox at or below the value it last received on a heartbeat.
- **Re-sending.** After `resumed`, the control plane re-sends every
  outbox event above `last_control_seq`, in order. Only the replica
  holding the session's current generation sends from the outbox. A
  stale replica's sends on a dead stream are then not lost, because the
  outbox still holds them.
- **Contiguity.** On an attached or replaying stream, each sequenced
  control event must carry exactly one more than the last `control_seq`
  dispatched. A lower number means the control plane lost recent
  writes, for example after an asynchronous store failover or a restore
  from a snapshot. A higher number means it skipped events. Either is a
  sequence regression, and the session goes to `lost`; the event is not
  silently dropped. A `cancel` that arrives out of sequence is applied
  before the session goes to `lost`, since a cancel is idempotent
  (`cancelSession` keeps the first cause) and is the one event that
  must never be lost.
- **State loss at re-attach.** A control plane whose highest assigned
  `control_seq` is below the `last_control_seq` it receives on
  `reattach` has lost state. It must answer
  `reattach_rejected{permanent: true}` rather than number new events
  from its stale counter.

This covers `permission_response`, `tool_result_response`,
`sandbox_token_response`, `batch_result`, `user_response` and `cancel`.
A `user_response` is therefore never applied twice and never lost. The
responses are also idempotent by request ID, because the correlator
drops a response whose ID is no longer pending (`correlator.go:93-107`);
`user_response` is the one type for which `control_seq` is the only
protection against a double apply.

## The phase gate and requests while detached

**The phase gate.** While the session is detached or replaying, every
step that runs in the sandbox or acts outside the harness on the run's
behalf waits before it starts. The gate is an interface the factory
injects into the loop; the loop keeps no knowledge of transports. The
gate points are:

| Gate point | Where |
|---|---|
| Each tool call, before the permission check | `executeToolCall`, ahead of the `WorkspaceMutating` / `RequiresApproval` branch (`harness/internal/core/types.go:574`) |
| The same, in every sub-agent | The child loop is built field by field (`harness/internal/core/subagent.go:262-311`), so the gate must be copied to it, as `GuardRail` is |
| `preRun` hooks, for the primary run and every follow-up run | `loop.go:256-266` |
| The git strategy's setup | `loop.go:343` |
| Each verifier run | `loop.go:483` |
| Each post-run step: the git final checkpoint, `postRun` hooks, command-output finalisation, the workspace export and result delivery | `loop.go:555`, `:562`, `:576-577`; `harness/internal/core/handoff.go:159`; the same helpers on the setup-failure path (`loop.go:2427-2435`) |

The trace finish is not gated: it writes the run's own record, which a
control plane needs after a lapse.

The gate's result depends on the session state:

| State | Gate returns | A tool call, hook, setup or verifier step | A post-run step |
|---|---|---|---|
| attached | At once | Runs | Runs |
| detached, replaying | When the state changes | Waits | Waits |
| lost | `ErrControlStreamLost` | Does not run; the run is being cancelled with `control_stream_lost` | Runs, as on any loss today; this keeps the post-run behaviour that #740 made deliberate |
| ended locally (SIGTERM, `Close`) | At once | Does not run, because the run context is cancelled | Proceeds, as today: a second signal abandons post-run work (`harness/internal/core/types.go:150-158`, `loop.go:639-655`) |

A tool call waits under the run context, so the run's timeout or a
`cancel` releases it without running the tool. A post-run step waits
under the session, because post-run work also follows a timed-out run.
The model's stream is not gated, so the turn's output still arrives.
Gating only the tools that consult the permission policy would miss
MCP and other tools that are neither `WorkspaceMutating` nor
`RequiresApproval`, so the gate covers every call
([open question 7](#open-questions-for-the-maintainer)).

**Why before the permission check.** A gate after the permission check
would raise each tool's `permission_request` while detached. With the
default 60 s approval timeout, every approval-gated tool the model
called during a break longer than 60 s would fail with "Permission
check error", and the model might abandon its approach before the
re-attach lands. Gating first means the request is raised after the
re-attach and gets a real answer. The Rule-of-Two ask gate is part of
the permission chain (`factory.go:2265-2268`), so it is gated with it.
This changes how the issue's fifth criterion is met; see
[open question 12](#open-questions-for-the-maintainer).

**What was chosen, and why.** The gate covers the phases above because
each is one wait at an existing boundary, through the same interface. It
does not stop a step that is already running, and it does not close the
detection gap. Stopping a running command leaves the workspace in a
partial state and changes the trajectory of a run that the re-attach
would have saved. Closing the gap would need a control-plane round trip
before every tool call (see
[Alternatives considered](#alternatives-considered)). The claim is
narrowed to match.

### What still runs unwatched

"Unwatched" means the control plane cannot see the step or stop it.
None of these bounds depends on the window.

| What | For how long | Where |
|---|---|---|
| Everything, as if attached, between the moment the control plane stops receiving and detection | Up to 40 s; immediate when the stream fails visibly | [Liveness](#liveness-and-loss-detection) |
| A tool call already executing at detection | To its own end or timeout: `run_command` up to 300 s, MCP and `web_fetch` up to 30 s. A `spawn_agent` child keeps making model calls, but its tool calls wait at the inherited gate. | `harness/internal/tool/builtins/shell.go:77-85` |
| A hook already executing at detection | Up to its timeout, at most 1800 s per hook | `types/runconfig.go:1321` |
| A test-runner verifier already executing at detection | Up to its timeout, 5 minutes by default | `harness/internal/verifier/testrunner.go:14` |
| A post-run step already executing at detection | Up to its budget: 30 s for the git checkpoint, the hook budget, 5 minutes for the export, 60 s for result delivery | `loop.go:627`, `:2409-2417`; `handoff.go:18-23` |
| Processes that an earlier command detached into their own session or process group | Until the sandbox is torn down, as today | `shell.go:53` |
| Model calls: the turn's stream, guard classifiers, compaction summaries and an LLM-judge verifier already running | For the break, inside the run's token budget and timeout | — |
| After `lost`: the post-run work | As on any loss today | `loop.go:547-576` |

A detached run raises no new `permission_request`,
`tool_result_request` or sandbox-token refresh from a gated step. A
request raised before detection is still pending and follows the rules
below.

**Requests.** Every request keeps its existing timeout and fail-closed
result. While detached, `Emit` retains the request and returns nil, so
the correlator's timer runs from the emit (`correlator.go:141-147`),
exactly as for a request the control plane never answers.

| Request | Timeout | While detached | Where |
|---|---|---|---|
| `permission_request` (`ask-upstream`, or the Rule-of-Two ask gate) | 60 s default (`permissionPolicy.timeout`) | Raised only before detection, since the gate sits before the check. A request pending at detection fails at its normal timeout. The tool gets "Permission check error" with category `permission_error`, never an allow. | `askupstream.go:17`, `:138-151`; `types.go:574-584` |
| `tool_result_request` (`tools.controlPlane`) | `timeoutSeconds` per tool | A request pending at detection times out; the model sees a tool error. | `harness/internal/tool/controlplane.go:16-23` |
| `sandbox_token_request` | 60 s per request | The initial request, during construction, succeeds if the re-attach lands within 60 s; otherwise the build fails with `error`. The refresher pauses while detached ([open question 11](#open-questions-for-the-maintainer)). It schedules a refresh at 0.8 of the token's remaining lifetime and retries a transient failure at half of what remains. If the refresh point passed during the break, it refreshes as soon as the stream is back. | `sandboxidentity/exchange.go:24`, `:50-54`, `:168-177`; `harness/internal/sandboxidentity/refresh.go:23`, `:150-167`, `:274` |
| `batch_submission` | The run deadline | The wait continues; a `batch_result` can arrive only after re-attach. | `harness.proto:764-772`, `batch.go:735-757` |

The difference from today is timing only. Today an emit on a dead
stream fails at once (`correlator.go:141-144`). Inside the window the
request fails at its timeout. Neither path turns a missing answer into
an allow.

`resumed.pending_requests` lists `(type, request_id)` for every request
still waiting when `resumed` is sent, from a registration interface that
every request-emitting component implements. The components are the
permission policies, the async-tool and sandbox-token correlators, and
the batch client. An invariants test pins that each registers. A
control plane acts on a replayed request (`seq ≤ replay_through_seq`)
only if it is listed. A request that timed out before `resumed` is
never executed or shown to a person for approval, and no sandbox token
is minted for it. The list is a snapshot, so a listed request may still
time out before its answer arrives; the late answer is then dropped
(`correlator.go:93-107`). Requests above `replay_through_seq` are live.

## Trace context and observability

| Item | Behaviour |
|---|---|
| `task_assignment.trace_context` | Read once, into the primary run's context (`job.go:195-197`). A break does not touch the run's context or spans, so the trace continues unbroken. `trace_context` on any other ControlEvent type is still ignored (`harness.proto:632-638`). |
| `tool_result_request.trace_context` | Part of the retained event; the replay carries the original value. |
| `run_started.trace_reference` | Protected, so a control plane that missed it gets it in the replay. The `done` summary repeats it. |
| Follow-up runs | Still start their own traces. |
| OTel export | Independent of the control stream (OTLP); unaffected. |
| Run span | Gains span events `control_stream.detached` (scrubbed cause) and `control_stream.reattached` (detached duration, attempts, tombstones). The span-error and log-handler scrub invariants cover the cause. A break while no run is active is logged only. A gate wait is a span (`control_stream.gate_wait`) under the step it delays. |
| Logs | One `warn` line on detach, one `info` line per re-attach, one `warn` line on `lost`. Emits do not fail while detached, so `logEmitFailure` (`streamloss.go:42-51`) is unaffected. |
| `RunTrace` | No new field in v1 ([open question 8](#open-questions-for-the-maintainer)). |

## Lifecycle interactions

| Where the break lands | With the window non-zero | If the session goes to `lost` |
|---|---|---|
| Before `task_assignment` | Unchanged: the grace is not known yet and no credential exists. | The job fails as today (`job.go:180-181`). |
| Construction | The harness enables re-attach as soon as it accepts the assignment, before `buildJobLoop`, so the build watcher (`job.go:203`) fires only on `lost`. Construction itself is not gated: it creates the sandbox and requests the first sandbox token, which fails closed at its timeout. The run's `preRun` hooks then wait at the gate. | Construction is cancelled; the job fails with `control_stream_lost`, as today. |
| Active run | The run continues; gated steps wait. | `ControlStreamLost` runs; the run ends `control_stream_lost` after its post-run work; exit non-zero. |
| The last turn ends while detached | The verifier and then each post-run step wait at the gate. `done` follows them, so it is sent after the re-attach. | The run ends `control_stream_lost`; post-run work runs as today. |
| Run finishes while detached (a break during post-run work) | The step in progress finishes; later steps wait. `done` is protected and retained. The harness waits for a re-attach to deliver it, for at most the rest of the break. | The run keeps its real outcome in `RunTrace` and the `resultSink`. The process exits non-zero, since `done` was never delivered ([open question 9](#open-questions-for-the-maintainer)). |
| Idle follow-up window | The `followUpGrace` timer pauses while detached and resumes with its remaining time after the re-attach ([open question 13](#open-questions-for-the-maintainer)). Today it runs regardless (`loop.go:1451-1452`). Input queued before the break can start a follow-up run while detached (`loop.go:1455-1481`); its `preRun` hooks and tool calls wait. `followUpHistory: "continue"` is unaffected. | The window closes; the exit status is that of today's rule. |
| Follow-up window ends while detached | Not possible while the timer is paused, except when the session's detached-time budget runs out, which goes to `lost`. | Exit as today. |
| SIGTERM / SIGINT | The session context is cancelled; redialling stops; gates release; shutdown runs as today. | Not applicable. |

**Close order and deadlines.** In `stirrup job` the transport is
injected, so the factory does not own it (`factory.go:327-335`).
`loop.Close`, which closes the executor, runs before `tp.Close`
(`job.go:120`, `:236`). The sandbox is therefore gone before any drain
wait, and `jobSessionBudget` needs no change. Gate waits and the final
drain together add at most the session's detached-time budget, three
windows, to the process's lifetime. The harness Pod's
`activeDeadlineSeconds` must allow that on top of today's sizing.

**Outcome.** No new outcome. `lost` uses `control_stream_lost`, and
`RunTrace.error` names the cause: a spent budget, a permanent rejection
with its reason, an OK end, a sequence regression, or "re-attach buffer
exhausted".

## Security argument

Claim, in two parts:

1. **Across a break and a re-attach, the run gains no capability.**
   This is the issue's criterion. Only a holder of one of the session's
   two accepted credentials (the one last issued, or the one presented
   before it, until `resumed` retires it) can take over the session,
   and only from a stream the control plane already considers dead.
2. **Once a loss is detected, no gated step starts until the stream is
   back or the session is lost.** The work that runs unwatched is listed
   in [What still runs unwatched](#what-still-runs-unwatched), and none
   of its bounds depends on the window. After `lost`, the run behaves as
   on any loss today.

The design does not claim that a detached run takes no unwatched
action.

| Capability | Attached run | While detached | After re-attach |
|---|---|---|---|
| Tool execution | Under the permission policy | No new call starts; a call already running finishes | Under the policy, as before |
| Tool approval (`ask-upstream`) | Only on `permission_response{allowed:true}` (`askupstream.go:86`) | No new request; a pending one fails at its timeout | Only on an explicit allow for a request ID still pending; `control_seq` stops duplicates |
| Hooks, git setup, verifier, post-run steps | Run | No new step starts | Run |
| Control-plane tool results | From `tool_result_response` | Time out | As before |
| Sandbox identity token | Per `sandbox_token_response` | None issued; the refresher pauses; the current token expires on schedule | Issued only for listed pending requests, from the verified session binding |
| Model input (`user_response`) | Queued and screened | None arrives; queued input still runs | Exactly once, through `control_seq` |
| A new task | Duplicates ignored | Same | `task_assignment` is invalid on `ResumeTask` |
| Network reach | `CONTROL_PLANE_ADDR` | Redials to the same address only | Same |
| Run duration | `timeout` | Unchanged | Unchanged |

What the window costs, and how each cost is bounded:

| Cost | Bound or mitigation |
|---|---|
| The control plane cannot `cancel` a detached run. | No gated step starts while detached, so a `cancel` sent during the break, which waits in the outbox, lands before any of them. The steps that can still act are listed above with their bounds. Model calls continue for at most the break, inside the run's token budget. |
| A control plane that keeps sending but has stopped receiving. | The progress rule detects it within 40 s. Repeated failures spend the per-session budgets. |
| A control plane that loses recent writes. | A sequence regression in either direction ends the session; a re-attach to such a control plane is refused permanently. A `cancel` is never silently dropped. |
| An on-path attacker who can only drop packets, even under TLS, now delays the run instead of cancelling it. | The per-session budgets (three windows, 10 re-attaches) bound the delay. The gate means the attacker gains no new step; the unwatched work is bounded as above. |
| A lapsed run still does post-run work: the git checkpoint, `postRun` hooks (the sum of their timeouts, each up to 1800 s, plus 30 s), the workspace export (5 min), the trace finish (90 s) and result delivery (60 s). | This is today's behaviour on any loss (`harness/internal/core/loop.go:547-576`, `:2409-2417`; `harness/internal/core/handoff.go:18-23`). It is why the only sanctioned fence is to stop the harness and wait for confirmation. |
| Two workers, if the control plane reschedules while the original lives. | [Control-plane obligations](#control-plane-obligations), item 3. |
| The redial trusts the network again. | Plaintext re-attach is refused unless the operator opts in. |
| A stolen credential. A thief who wins a re-attach race holds the live stream, and the legitimate harness is refused as "still live" until its window lapses. Meanwhile the thief can mint sandbox identity tokens up to the per-session count, put attacker-written `permission_request` text in front of a human approver, read `user_response` text from the outbox and deliver a forged `done{success}`. | Theft needs a compromised harness process or a control-plane leak. The credential lives only in a redacting transport type, never reaches the sandbox, the model or a record, and is rejected under the `local` executor. The control plane stores only its hash, never logs or persists the plaintext, and keeps it out of the outbox. It is rotated on every re-attach and retired on `resumed`. It is accepted only for a stream the control plane considers dead, so it cannot take over a healthy run. A per-Pod client identity is required where one exists. Every session-record write is fenced by generation, so a superseded stream cannot act. |
| Retained events hold transcript content in memory. | Already scrubbed and bounded; only the unacknowledged suffix; 8 MiB cap. |

The read-only-mode invariant, the secret-reference rule and the event
byte bounds are untouched. The loop in
`harness/internal/core/loop.go` gains one injected interface, the
phase gate, and no transport knowledge.

## Control-plane obligations

A control plane that sets the window takes on these obligations. They
become part of the `control_stream_reattach` capability's contract.

1. **Persist the session record** where every replica can read it: the
   generation, the previous and current credential hashes, the last
   `seq` received, the time of the last event received on the live
   stream (from the store's clock), the `control_seq` outbox and its
   highest assigned value, and a revoked flag.
2. **Fence every session-record write by generation.** Promoting a
   stream on `resumed`, retiring a credential, recording `seq` or the
   last-event time, and acting on any request are each a
   compare-and-set on the generation the stream was accepted under. A
   replica whose generation is stale ends the RPC with `ABORTED` and
   acts on nothing more from that stream. Only events on the current
   generation's live stream update the last-event time.
3. **Fence with state, then stop the harness, then wait for
   confirmation.** Before rescheduling a task, set the session's revoked
   flag (every later re-attach gets a permanent rejection). Then stop
   the harness and its sandbox, and wait until something that can
   confirm it reports them stopped. Removing an API object is not
   confirmation.
   - **Kubernetes (`k8s`, `k8s-sandbox`).** Delete the harness Pod and
     its sandbox, and wait until both are gone after their kubelets
     confirmed termination. A Pod on an unreachable node stays
     terminating. `kubectl delete pod --force` and `kubectl delete node`
     establish nothing (see
     [`executors/k8s.md`](executors/k8s.md)). There, only deleting the
     node's VM through the cloud provider's API stops the process
     ([open question 14](#open-questions-for-the-maintainer)). Run Jobs
     with `backoffLimit: 0` so Kubernetes does not retry on its own.
   - **Cloud Run jobs.** Cancel the execution and wait until it reports
     that it is no longer running.
   - **Other hosts**, including a harness whose sandbox uses the
     `container` executor. Stop the host or the harness's container and
     its sandbox container, and wait for the engine or the host's
     platform to confirm.

   Until confirmation, the task stays unscheduled. A timed reschedule
   is not sanctioned: the reschedule time cannot be computed after a
   control-plane crash, and a lapsed run's post-run work can outlast
   any window.
4. **Check for a completed run** before rescheduling a session that
   never delivered `done`: read the `resultSink` and the Job's terminal
   status.
5. **Answer a `reattach` within 5 s**, including the store's
   compare-and-set, with `reattach_accepted`, a `reattach_rejected`
   (permanent or not) or `ABORTED`; a slow store produces `ABORTED`.
   Refuse it, retryably, while the session's live stream is not yet
   dead (30 s since its last event).
6. **Acknowledge from the receive path.** Send `ack` within 10 s of
   receiving each sequenced event or heartbeat, carrying only values
   the path that receives and persists events has written. A replica
   that declares a stream dead ends that RPC with `UNAVAILABLE` rather
   than keep it open.
7. **Number and keep control events** (`control_seq`, outbox) and send
   only from the replica holding the current generation. Never put
   `ack`, `reattach_accepted` or `reattach_rejected` in the outbox. Trim
   only at or below the `last_control_seq` the harness reported.
8. **Detect lost state.** Answer a `reattach` whose `last_control_seq`
   is above the highest `control_seq` assigned with
   `reattach_rejected{permanent: true}`. A store that can lose
   acknowledged writes (asynchronous replication, a restore) makes this
   path likely after a failover; synchronous replication avoids it.
9. **Act on replayed requests only when listed** in `pending_requests`,
   and never prompt a person for an approval that is not listed.
10. **Derive run identity on a resumed stream** only from the verified
    session record, after sending `reattach_accepted`. Count
    sandbox-token requests per session, not per stream. Issue tokens
    with a lifetime of at least the window plus 60 s.
11. **Issue the credential only over TLS** or a verified mesh identity,
    and revoke it when the task is rejected, cancelled or completed.
    Never log or persist the plaintext credential, including in an
    audit log of outbound events. Require the original client identity
    on a re-attach where per-Pod identities exist.
12. **End a stream with `UNAVAILABLE`**, not OK, when draining for a
    deploy, to invite a re-attach; OK means the session is over. A
    replica marked for drain refuses `ResumeTask` retryably, so one
    rolling deploy costs a session one re-attach rather than one per
    replica.
13. **Rate-limit `reattach`** per session and globally.

## Compatibility and the `ready` capability

| Combination | Result |
|---|---|
| Old harness, new control plane | `ready.capabilities` lacks `control_stream_reattach` and `ready.config_fields` lacks `controlStreamGraceSeconds`. An old harness drops the field without notice (#905 would make it reject instead), so the control plane gates on the capability. |
| New harness, old control plane | The field is never set; behaviour is unchanged. `ready` carries an `incarnation` the control plane ignores. |
| New harness, field set, mixed replicas during a rolling deploy | `ResumeTask` to an old replica returns `UNIMPLEMENTED`, which is retried until a new replica answers or the break ends. |

`control_stream_reattach` covers the field, `ResumeTask`, the
credential, both sequence numbers, `ack`, the heartbeat fields, the new
event types and the obligations above. It is added to
`types.HarnessCapabilities()` (`types/events.go:452`), whose exact list
is pinned by
`TestHarnessCapabilities_AdvertisesFeaturesAndIsolatesSlice`
(`types/events_test.go:208`). That test is updated; it is the only
existing test that changes.

## Alternatives considered

| Alternative | Why not |
|---|---|
| In-band `reattach` as the first message of `RunTask` (draft 1) | An old replica ignores the unknown type and either waits for `ready` or sends a `task_assignment` to nobody. A separate RPC fails fast with `UNIMPLEMENTED`. |
| A gap marker only, with no retention or sequence numbers | Requests raised just before the break, `run_started` and `done` would be lost, so every approval in flight would time out even after a fast re-attach, and the control plane could not tell a finished run from a running one without a `resultSink`. Duplicate inbound events would remain possible. |
| A timed fence (draft 1: wait the window plus a detection bound, then reschedule) | It is not computable after a control-plane crash, and it ignores post-run work that can run for tens of minutes. |
| An acknowledged lease that lets the harness expire itself and skip side-effecting post-run work | It would allow rescheduling without stopping the harness. It changes post-run behaviour that #740 made deliberate, and it relies on monotonic clocks across a VM pause. Kept as [open question 2](#open-questions-for-the-maintainer). |
| HTTP/2 keepalive as the loss detector (draft 1) | Default grpc-go servers reject 20 s pings; proxies answer pings themselves; the first stream is dialled before the window is known. |
| Any inbound event as proof of life (draft 2) | A control plane whose receive path has failed while a ticker keeps sending `ack` would keep the harness attached and running tools that nobody sees, which is the failure #659 removed. |
| Letting auto-allowed tools run while detached (draft 1) | That is the orphaned work #659 removed. |
| Gating tools only, not hooks, the verifier and post-run steps (draft 2) | A run whose last turn ends during a break would run its test suite and its `postRun` hooks, which may push a branch or open a pull request, while a `cancel` waits in the outbox. |
| Gating after the permission check (draft 2) | Approval-gated runs would fail every approval in a break longer than the approval timeout; see [Why before the permission check](#the-phase-gate-and-requests-while-detached). |
| Gating each tool call on the `ack` of its own `tool_call` event | It closes the detection gap, but it puts a control-plane store write and round trip in front of every tool call, and it needs the control plane to acknowledge each event at once rather than within 10 s. The progress rule bounds the gap at 40 s instead. |
| Stopping steps already running when a loss is detected | A killed command leaves the workspace part-changed and changes the trajectory of a run that a re-attach would have saved. Today's behaviour on `lost` already cancels the run. |
| A per-Pod proxy that terminates the stream locally | It moves the stream endpoint into a second process that can fail the same way, and adds a component to every deployment. |

## Implementation map

| Area | Change |
|---|---|
| Prerequisites | Session-unique request IDs across every allocator, with an invariants test; byte bounds on request inputs, refusing or falling back for an oversized `batch_submission`. |
| `types/runconfig.go` | Field, range, JSON name. The range error message names the 60 s minimum and its reason. |
| `proto/harness/v1/harness.proto`, `gen/` | `ResumeTask`, field 62, the event fields and types, `PendingRequest`; `just proto`. |
| `harness/internal/transport/grpc_translate.go` | The field, and the new event fields both ways, with a translation test for the event fields (the proto-mirror invariant covers `RunConfig` only, `harness/internal/invariants/protomirror_test.go:282`). |
| `harness/internal/transport/` | A session holder behind the existing `GRPCTransport` API: retention with tombstones, both sequence numbers and their regression checks, `ack` handling and the progress rule, the transport's own heartbeats, redial, the handshake, and the replay on its own goroutine, all through one bounded send path. Session-scoped `Done`/`Err` and a separate per-stream signal for `Close`. The credential type. A pending-request registry. |
| `harness/internal/core/` | The phase-gate interface and its calls at each gate point, injected by the factory and copied into sub-agent loops; the follow-up timer pause in `RunFollowUpLoop`; the gate-wait span and the break span events through an optional transport interface. No transport import in `loop.go`. |
| `harness/internal/sandboxidentity/` | Pause the refresher while detached, and refresh at once on re-attach if the refresh point passed. |
| `harness/cmd/stirrup/cmd/job.go` | After accepting the assignment: read the window and credential, run the checks in [Configuration](#configuration), enable re-attach before `buildJobLoop`. |
| `harness/cmd/stirrup/cmd/harness.go`, `harness/harnessapi/` | Ignore a non-zero window with a warning. |
| `harness/internal/invariants` | The credential is read in one place and never reaches a log, trace, `RunResult` or `RunConfig.Redact()` output; every request emitter registers pending IDs; every `AgenticLoop` built outside the factory, including sub-agents, carries the gate. |
| `examples/controlplane` | Implements `ResumeTask`, `ack`, the outbox and the session record in memory, so `smoke.sh` can exercise a break. |

## Test plan

Rows 1 to 5 are the issue's acceptance criteria. The rest pin the
design's own rules. Timing tests use injected clocks or short windows
unless the row says otherwise.

| # | Criterion | Test |
|---|---|---|
| 1 | Design note merged | This document. |
| 2 | Field unset: behaviour unchanged | The existing stream-loss tests pass unmodified: `TestRun_ControlStreamLost*` and `TestControlStreamLost_*` (`harness/internal/core/streamloss_test.go`); `TestRunJob_ControlPlaneEndingRPCDuringProviderCallCancelsRun`, `TestRunJob_ConnectionFailureDuringProviderCallCancelsRun`, `TestRunJob_StreamLostDuringConstructionStartsNoRun`, `TestRunJob_StreamEndedAfterDoneClosesFollowUpWindow`, `TestWatchControlStream_*` and `TestRunWithConfig_GRPCTransportLossCancelsRun` (`harness/cmd/stirrup/cmd/controlstream_test.go`); `TestGRPCTransport_*` in `grpc_liveness_test.go`. A new test asserts that with the field unset no event other than `ready` sets any new field, and that `ready` differs from `main`'s only by the capability, the path and `incarnation`. |
| 3 | Break mid-turn, re-attach inside the window, real outcome, complete sequence or explicit gap | Over bufconn, a fake control plane fails the RPC with `UNAVAILABLE` while the provider call is blocked, accepts the `ResumeTask`, then the provider is released. Asserts outcome `success`, a contiguous `seq` from 1 to `done`, and no tombstone. A variant with a small cap asserts tombstones for droppable events and every protected event intact. |
| 4 | The window lapses | The control plane never returns. Asserts `control_stream_lost`, the cause in `RunTrace.error`, a non-zero exit, and an end about one window after the loss. |
| 5 | A permission request raised while detached is auto-denied at its normal timeout (the issue's wording; the harness reports a permission error, never an allow) | 1 s policy timeout. A tool needing approval raises its request; the fake control plane breaks the stream before reading it. Asserts the tool result "Permission check error" with category `permission_error` at about 1 s, not at emit time and not at lapse. A second approval-gated tool called after detection raises no request until the re-attach, then raises it and runs on the answer. After the re-attach, the first request is in the replay, absent from `pending_requests`, and a late `permission_response` for it is dropped. |
| 6 | Phase gate | An auto-allowed tool, an MCP tool and a tool called by a `spawn_agent` child, each called after detection, do not run until re-attach, and never run if the session goes to `lost`. A break before the last turn ends holds the test-runner verifier and each post-run step until re-attach; on `lost` the post-run steps run and the verifier does not. A follow-up started from queued input during a break holds its `preRun` hooks. |
| 7 | OK end | An OK end during a run gives today's `control_stream_lost`; an OK end after `done` ends the session with no redial and releases retained events, so the exit status is the run's. An OK end before `reattach_accepted` is retried. |
| 8 | Liveness | A control plane whose acks continue with a frozen `ack_seq` and `heartbeat_ack` causes a detach within 40 s, while other control events still arrive; a `cancel` among them still acts. An idle session with a frozen `heartbeat_ack` is detected the same way. A control plane that stops reading (flow-control stall) is detected by the same rule. A replayed event is not counted overdue from its first send. A real grpc-go server with default keepalive enforcement sees no pings from the harness. |
| 9 | Rejections | `reattach_rejected{permanent:true}` goes to `lost` with the scrubbed reason. `UNIMPLEMENTED`, `UNAUTHENTICATED` without that event, and `ABORTED` are retried. |
| 10 | Credential lifecycle | Rotation on every re-attach; the previous credential accepted until `resumed`, rejected after; a lost `reattach_accepted` recovered with the previous credential; an oversized or non-printable credential rejected; a missing credential rejects the task. |
| 11 | Supersession and generation fencing | A `reattach` while the old stream is live is refused retryably and succeeds once the old stream is dead. A retried `reattach` does not refresh the last-event time. Two overlapping accepts on different replicas: the stale replica's `resumed` promotion fails with `ABORTED`, and it acts on no event from its stream. |
| 12 | Credential hygiene | The credential never appears in logs, the JSONL trace, OTel attributes, `RunResult`, `RunConfig.Redact()`, any `types` event, any control handler, or the example control plane's outbox. |
| 13 | Posture checks | Plaintext without the opt-in, and the `local` executor, each reject the task. |
| 14 | Inbound sequence | A `user_response` queued before the break and not yet echoed is not applied twice when the outbox re-sends it. A `cancel` sent during the break lands after re-attach. A `cancel` delivered while `replaying` cancels the held tools rather than releasing them when the replay's `ack` arrives. |
| 15 | Replay boundary | A replayed request outside `pending_requests` is not acted on by the example control plane. |
| 16 | Break during replay | A second break during the replay continues the same break; the window does not reset until the replay is acknowledged. |
| 17 | Budgets | Repeated breaks stop at three windows of detached time or 10 re-attaches. An event that makes the control plane abort the replay on every attempt ends in `lost` within the budget. |
| 18 | Buffer exhaustion | Protected events over the cap go to `lost` with "re-attach buffer exhausted". |
| 19 | Construction and follow-up | A break during a blocked `buildJobLoop` recovers. A follow-up started from queued input during a break, and a run that finishes during a break, both re-attach with a session-bound credential despite the changed `run_id`. The `followUpGrace` timer does not expire while detached, and a follow-up sent during the break starts after the re-attach. |
| 20 | Run finishes and never re-attaches | The real outcome is in `RunResult`; the exit status is non-zero. |
| 21 | SIGTERM while detached | Redialling stops; gates release; shutdown is not delayed by the window. |
| 22 | Validation and wiring | Range 0 or 60-300; the proto-mirror invariant covers field 62; `stirrup harness` ignores the field with a warning; `ready` carries the capability and the path; `-race` over `Emit` concurrent with replay. |
| 23 | Sequence regression | A `reattach_accepted.ack_seq`, or an `ack`, below one already received goes to `lost`. A live control event with a `control_seq` at or below the last dispatched, or skipping one, goes to `lost`; a `cancel` in that position is applied first. A control plane whose outbox counter is below `last_control_seq` answers a permanent rejection. |
| 24 | Sandbox-token refresh | No refresh request is sent while detached. A refresh point that passes during the break triggers a refresh as soon as the stream is back. |

## Open questions for the maintainer

Each question has a recommendation; the design above assumes it.

1. **Does this protocol meet the 2026-10-08 bar** for continuing after
   a loss? *Recommendation:* yes, as a session protocol, not as a
   lease. Its properties are an authenticated, acknowledged re-attach;
   no duplicate delivery; progress-based loss detection; no gated step
   after detection; and a state-based fence. The unwatched work it
   leaves is bounded independently of the window: up to 40 s before
   detection, and steps already running at detection. #456 stays the
   home of continuity across process restarts.
2. **A real lease?** An acknowledged lease would let the harness expire
   itself and skip side-effecting post-run steps after a lapse, so a
   control plane could reschedule without stopping the harness. It is
   also what would resolve the partitioned-node case in
   [open question 14](#open-questions-for-the-maintainer).
   *Recommendation:* not in v1. Stop-and-confirm is a new rule that
   this note introduces; the integration guide gains it with the
   feature. The lease changes post-run behaviour on every loss.
3. **The cap and the floor.** A cap of 60 s, 300 s or 600 s, and a
   floor of 60 s. *Recommendation:* 300 s and 60 s. A longer window
   costs waiting time, model spend and a later `cancel`. It does not
   add unwatched work. The floor follows from the 30 s dead-stream
   bound.
4. **Session budgets.** Three windows of total detached time and 10
   accepted re-attaches per session, or per run. *Recommendation:* per
   session as written. The drain obligation (a replica marked for drain
   refuses `ResumeTask` retryably) keeps one rolling deploy to one
   re-attach, so 10 is enough.
5. **Plaintext re-attach.** Refuse unless the operator opts in through
   the Pod environment (recommended), always refuse, or only warn.
6. **Retention cap.** Fixed at 8 MiB (recommended for v1), or an
   operator environment variable. A `RunConfig` field is not
   recommended.
7. **Phase gate scope.** Gate every tool call, `preRun` hooks, the git
   setup, each verifier run and each post-run step (recommended); gate
   tool calls only; or gate only tools that consult the permission
   policy.
8. **Record breaks in `RunTrace`?** For example `controlStreamBreaks`
   and `controlStreamDetachedMs`. *Recommendation:* span events only in
   v1.
9. **A run that finished while detached and never re-attached.**
   *Recommendation:* keep the real outcome in `RunTrace` and the
   `resultSink`, and exit non-zero, so a loss never exits zero (the
   #740 rule) and the control plane checks the sink before
   rescheduling. An OK end releases the retained set, so an attached
   run whose control plane ends the RPC after `done` is not caught by
   this rule. The alternative reports `control_stream_lost`, which
   discards a completed run's real result.
10. **The prerequisites.** Make request IDs session-unique across every
    allocator, and bound request inputs, refusing or falling back for
    an oversized `batch_submission`, first, as their own issues
    (recommended), since both are defects today. Session-wide
    heartbeats (#913) are no longer a prerequisite.
11. **Sandbox-token refreshes while detached.** A timed-out refresh is
    retried and counts against the eight-request budget, so a long break
    can spend it. *Options:* leave it (fail closed: refreshing stops
    once the budget is spent); pause the refresher while detached
    through a transport callback; or count only requests that the
    control plane acknowledged. *Recommendation:* pause while detached,
    and refresh at once after the re-attach if the refresh point
    passed, since a request nobody can answer should not spend the
    budget.
12. **Gate before or after the permission check.** The issue's fifth
    criterion asks that "a permission request raised while detached is
    auto-denied at its normal timeout". Gating before the check means no
    request is raised after detection. The criterion is then met by
    requests already pending when the loss is detected, including those
    raised into the undetected gap. Gating after the check meets it
    literally, but it fails every approval in a break longer than the
    approval timeout. *Recommendation:* before.
13. **The follow-up window while detached.** Pause the `followUpGrace`
    timer while detached (recommended), or let it run, which loses a
    follow-up sent during a break that outlasts the remaining grace.
    The session's detached-time budget bounds the extension.
14. **Rescheduling when the harness cannot be confirmed stopped.** A Pod
    on an unreachable node never finishes terminating, and a force
    delete removes the object without stopping the process.
    *Recommendation:* the task stays unscheduled until the node's VM is
    deleted through the cloud provider's API, or the node returns and
    its kubelet confirms termination. A force delete is never a fence.
    A lease ([open question 2](#open-questions-for-the-maintainer)) is
    the longer-term answer.

## Related issues

| Issue | Relation |
|---|---|
| #659, PR #740 | The current cancel-on-loss behaviour and the decision this note revisits. |
| #456 | Conversational continuity across process restarts; this note is limited to one process. |
| #913 | Session-wide liveness signals. The transport's own heartbeats cover these sessions; #913's heartbeats carry the same fields when it lands. |
| #905 | Rejecting unknown `RunConfig` fields; it changes what an old harness does with the field. |
| #693, #694 | The TLS options and recipes the plaintext rule relies on. |
| #7 | Earlier transport-security proposal with a session token and sequence numbers in both directions, the closest prior design. |

## Docs to update on implementation

- [`integration-guide.md`](integration-guide.md): the Delivery
  paragraph in [Watching a run live](integration-guide.md#watching-a-run-live)
  (replacing the pointer to this note), the stream lifecycle, both event
  tables, the heartbeat section, [Compatibility](integration-guide.md#compatibility),
  a new fencing section (stop-and-confirm before rescheduling, which
  the guide does not describe today), and the control-plane obligations
  above.
- `proto/harness/v1/harness.proto`: the `HarnessService` lifecycle
  comment, `ResumeTask`, the new event-table entries, and the
  sandbox-identity sentence that the control plane "derives the run
  identity from the authenticated stream the task was assigned on"
  (`harness.proto:240-241`).
- [`configuration.md`](configuration.md): the field reference.
- [`deployment.md`](deployment.md): the session table, the
  `activeDeadlineSeconds` sizing, and the plaintext opt-in variable.
- [`executors/k8s.md`](executors/k8s.md): the fence for an unreachable
  node, beside the existing note on force deletion.
