# Control stream re-attach: design note

Status: proposal for review, 2026-11-15. Nothing here is implemented.
This note is the first acceptance criterion of issue #911 (an optional
grace period and re-attach before a lost control stream cancels the
run). Code follows only after the note is merged and the
[open questions](#open-questions-for-the-maintainer) are answered.

Every file and line reference was checked against `main` at
`0f0bc8e9`. Re-verify them before acting on this note; the code will
have moved.

The reader is the session that implements the feature or reviews this
design. The integrator-facing contract stays in
[`integration-guide.md`](integration-guide.md); this note changes it
only once the feature ships (see
[Docs to update on implementation](#docs-to-update-on-implementation)).

## Contents

- [Summary](#summary)
- [Behaviour today](#behaviour-today)
- [Why a lost stream cancels the run](#why-a-lost-stream-cancels-the-run)
- [Goals and non-goals](#goals-and-non-goals)
- [Overview](#overview)
- [Configuration](#configuration)
- [Detecting the loss and redialling](#detecting-the-loss-and-redialling)
- [The re-attach handshake](#the-re-attach-handshake)
- [Authenticating a re-attach](#authenticating-a-re-attach)
- [Sequence numbers, retention and gaps](#sequence-numbers-retention-and-gaps)
- [Requests while detached](#requests-while-detached)
- [Control events lost in the break](#control-events-lost-in-the-break)
- [Trace context and observability](#trace-context-and-observability)
- [Lifecycle interactions](#lifecycle-interactions)
- [Security argument](#security-argument)
- [Control-plane obligations](#control-plane-obligations)
- [Compatibility and the `ready` capability](#compatibility-and-the-ready-capability)
- [Implementation map](#implementation-map)
- [Test plan](#test-plan)
- [Open questions for the maintainer](#open-questions-for-the-maintainer)
- [Docs to update on implementation](#docs-to-update-on-implementation)

## Summary

A new `RunConfig` field, `controlStreamGraceSeconds` (proto field 62,
default 0, cap 300), opens a bounded window after the control stream is
lost. Inside the window the run keeps going exactly as an attached run
whose control plane answers nothing. The harness redials
`CONTROL_PLANE_ADDR` with backoff and presents a resume credential. The
control plane issued that credential on the `task_assignment` and
rotates it on every re-attach. Events emitted during the break, and
the most recent events sent before it, are retained up to a byte cap
with session-scoped sequence numbers. On re-attach the harness replays
everything after the last sequence number the control plane received,
and it reports anything it had to drop. If the window passes without a
re-attach, the run ends `control_stream_lost` exactly as it does today.

With the field unset, nothing changes. No sequence numbers are sent, no
buffer exists, and a loss cancels the run at once.

## Behaviour today

| Stage | What happens on loss | Where |
|---|---|---|
| Transport | One `RunTask` stream per process. No redial exists anywhere. The read loop records why the stream ended. A local `Close` or a cancelled stream context is not a loss. | `harness/internal/transport/grpc.go:87-119` (dial and open), `:187-208` (read loop), `:296-307` (`recordStreamEnd`) |
| Liveness | `Done()` closes when the reader stops; `Err()` gives the cause, or nil for a local end. | `harness/internal/transport/transport.go:25-33` |
| Before assignment | The job fails: "gRPC stream closed before receiving task assignment". | `harness/cmd/stirrup/cmd/job.go:180-181` |
| Construction | The build context is cancelled. The job emits `error` and `done{stop_reason:"control_stream_lost"}` (best-effort) and fails. | `job.go:199-216` |
| Active run | `watchControlStream` calls `AgenticLoop.ControlStreamLost`, which cancels the session through `cancelSession`. The run ends `control_stream_lost` after its bounded post-run work, and the process exits non-zero. | `harness/cmd/stirrup/cmd/controlstream.go:13-35`, `job.go:251-252`, `harness/internal/core/streamloss.go:15-23`, `harness/internal/core/userinput.go:175-193` |
| Idle follow-up window | The window closes promptly; the last run's outcome is unchanged. | `streamloss.go:9-14`, `job.go:262-265` |
| Pending requests | Every request goes through `Correlator.Await`. An emit failure fails the request at once; otherwise it fails at its timeout. A response for an unknown request ID is dropped. | `harness/internal/transport/correlator.go:122-160`, `:93-107` |
| Outcome | `control_stream_lost`, "a run cancelled because its control stream ended while it was active". | `proto/harness/v1/harness.proto:1463-1473` |
| Delivery contract | "Delivery is at most once … a stream that breaks cannot be resumed." | `docs/integration-guide.md:807-812` |

## Why a lost stream cancels the run

Issue #659 found that after the stream ended, `stirrup job` kept
calling the model and running tools, then exited zero with a `done`
nobody received. A control plane that reschedules on "stream closed
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

This note revisits that decision only as an opt-in. The resume
credential, the one-stream-per-session rule and the sequence replay
below are the lease and session protocol that decision asked for,
scoped to one process lifetime. Whether they meet that bar is
[open question 1](#open-questions-for-the-maintainer).

## Goals and non-goals

| Goals | Non-goals |
|---|---|
| A run survives a control-plane restart, deploy or brief network break shorter than the window. | Surviving a harness process restart or a Pod reschedule. That needs the persisted session log of [`sessions-spec-draft.md`](sessions-spec-draft.md) (#456). |
| With the field unset, behaviour is unchanged byte for byte. | Exactly-once delivery. Delivery becomes ordered and deduplicable by sequence number, not exactly-once. |
| A detached run can do nothing an attached run with a silent control plane could not do. | Moving a live stream between control-plane replicas. The new stream may land on any replica; the control plane shares session state between them. |
| The control plane can tell what it missed. | Re-attach for `stirrup harness --transport grpc`, which has no `task_assignment` and so no credential (see [Configuration](#configuration)). |
| The loop stays a pure function of its interfaces. All re-attach logic lives in the transport and the `cmd` package. | Recovering a construction-time loss before a credential exists, that is, before `task_assignment`. |

## Overview

The transport moves between four states:

```text
                 loss detected                  redial + handshake ok
   attached ─────────────────────▶ detached ───────────────────────▶ attached
      ▲                               │ │                                │
      │                               │ └── window lapses, or a ─▶ lost  │
      │                               │     permanent rejection          │
      └──────── replay done ◀── replaying ◀──────────────────────────────┘
```

| State | Emit | Control events in | `Liveness.Done()` |
|---|---|---|---|
| attached | Sent with a sequence number and retained in the ring | Dispatched to handlers | open |
| detached | Retained only; returns nil | None arrive | open |
| replaying | Retained and queued behind the replay | Dispatched | open |
| lost | Fails, as today | None | closed; `Err()` names the lapse and the first cause |

`Done()` and `Err()` describe the whole session, not one stream. They
fire only when the window lapses. `watchControlStream` and
`ControlStreamLost` therefore stay unchanged and still fire exactly
once. They fire later, on lapse instead of on loss.

## Configuration

| Item | Value |
|---|---|
| JSON | `controlStreamGraceSeconds` |
| Go | `RunConfig.ControlStreamGraceSeconds int` (`json:"controlStreamGraceSeconds,omitempty"`) |
| Proto | `int32 control_stream_grace_seconds = 62;` in `RunConfig`. Plain `int32`, like `heartbeat_seconds = 61` (`harness.proto:792`), because 0 already means off. |
| Default | 0: cancel at once, as today. |
| Range | 0-300 seconds, rejected outside it by `ValidateRunConfig`. |
| Measured from | The moment the harness detects the loss. Each loss gets a fresh window after a successful re-attach. |
| Counts against | Nothing new. The run's `timeout` keeps running while detached; the window does not extend a run or a follow-up window. |
| CLI flag | None in v1. `stirrup harness` rejects a non-zero value at startup, since no `task_assignment` exists to carry a credential. `stirrup run-config --validate` accepts it. |
| Gated by | The `control_stream_reattach` capability on `ready`, and the path in `ready.config_fields`. |

The cap of 300 seconds covers a rolling deploy or a control-plane
restart. It bounds the unwatched work a loss can produce, and it is
the same order as the 60 s permission timeout
(`harness/internal/permission/askupstream.go:17`). It is
[open question 3](#open-questions-for-the-maintainer).

The field belongs in `RunConfig`, next to `followUpGrace` and
`heartbeatSeconds`: it is a session behaviour the control plane
chooses per task, and it is negotiated through `config_fields`. The
credential does not belong there (see
[Authenticating a re-attach](#authenticating-a-re-attach)). As
CLAUDE.md requires, the field ships with its proto mirror and its copy
in `harness/internal/transport/grpc_translate.go`.

A non-zero value on a `task_assignment` that carries no
`resume_token` is a configuration error. The harness rejects the task
with `error` and `done{stop_reason:"error"}`, like any other invalid
`RunConfig`, rather than silently running with the window off.

## Detecting the loss and redialling

**Detection.** The harness detects a loss in two ways: the reader's
`Recv` fails (as today), or a `Send` fails. A silent partition (no
RST, no FIN) is detected only by HTTP/2 keepalive, which the harness
does not configure today. When the window is non-zero, the harness
enables client keepalive pings: an interval of 20 s and an
acknowledgement timeout of 10 s, so a silent partition is detected
within about 30 s. The control plane's keepalive enforcement policy
must allow pings that often; otherwise it ends the connection with
`GOAWAY too_many_pings`.

**Redial.**

| Parameter | Value | Reason |
|---|---|---|
| Target | `CONTROL_PLANE_ADDR`, read once at process start | A `RunConfig` cannot redirect the redial. |
| Connection | A fresh `grpc.ClientConn` per attempt, with the same TLS credentials; the old one is closed | Re-resolves DNS and lands on any healthy replica. It inherits no backoff state from grpc-go, whose default connect backoff grows to 120 s and would waste the window. |
| First attempt | Immediately on detection | Most breaks are a replica going away behind a stable Service address. |
| Backoff | 250 ms, doubling, capped at 4 s, ±20 % jitter | Many harnesses lose one replica at the same moment, so the jitter spreads their reconnects. |
| Per-attempt deadline | `min(10 s, time left in the window)`, covering the dial, the `RunTask` open and the handshake | A stalled handshake does not consume the window. |
| Stops | On a successful handshake, a permanent rejection, the window lapsing, or the stream context being cancelled (SIGTERM) | |

**Permanent rejections.** When the control plane ends the new RPC with
`UNAUTHENTICATED`, `PERMISSION_DENIED`, `NOT_FOUND` or
`FAILED_PRECONDITION`, the window lapses at once, since retrying
cannot succeed. Any other status, or a plain end of stream before the
handshake completes, is retried.

**TLS certificates.** The client certificate and CA files are read once
at startup (`job.go:89-100`) and reused for every attempt. A
certificate that expires during a session makes every redial fail, and
the window lapses.

## The re-attach handshake

A resumed stream does not start with `ready`. A control plane that saw
a second `ready` would take it for a new harness and send a second
`task_assignment`. The resumed stream starts with its own event
instead:

```text
harness                                          control plane
   │ RunTask (new stream)                              │
   │── reattach {id: session, run_id, resume_token,  ─▶│ verify the token, its binding to
   │            last_seq}                              │ the session and run; supersede
   │                                                   │ the old stream
   │◀─ reattach_accepted {resume_after_seq: N,       ──│
   │                      resume_token: fresh}         │
   │── resumed {id: run, resume_from_seq: N+1,        ─▶│
   │            dropped_events, pending_request_ids}    │
   │── retained events with seq > N, in order ────────▶│
   │── live events ───────────────────────────────────▶│
```

Wire additions. The `RunConfig` field number is reserved for this work.
The other numbers are the next free ones at `0f0bc8e9` and are
illustrative; the implementation takes whatever is free when it lands.

| Message | Addition | Meaning |
|---|---|---|
| `RunConfig` | `control_stream_grace_seconds = 62` | See [Configuration](#configuration). |
| `HarnessEvent` | `uint64 seq = 30` | Session-scoped sequence number, from 1. Set on every event after the assignment, only when the window is non-zero. Unset on `ready`, `reattach` and `resumed`. |
| `HarnessEvent` | `string run_id = 31` | On `reattach`: the active run's ID, empty when idle. |
| `HarnessEvent` | `string resume_token = 32` | On `reattach` only. Sensitive. |
| `HarnessEvent` | `uint64 resume_from_seq = 33` | On `resumed`: the first sequence number the replay can start from. |
| `HarnessEvent` | `uint64 dropped_events = 34` | On `resumed`: how many events after `resume_after_seq` were evicted and will not be replayed. |
| `HarnessEvent` | `repeated string pending_request_ids = 35` | On `resumed`: request IDs still waiting for a response. |
| `HarnessEvent` | `uint64 last_seq = 36` | On `reattach`: the highest sequence number assigned. |
| `HarnessEvent` types | `reattach`, `resumed` | New. Old consumers ignore them, as the contract requires (`harness.proto:257`). |
| `ControlEvent` | `string resume_token = 12` | On `task_assignment` (the first credential) and on `reattach_accepted` (its replacement). Sensitive. |
| `ControlEvent` | `uint64 resume_after_seq = 13` | On `reattach_accepted`: the highest sequence number the control plane received. 0 means it received none. |
| `ControlEvent` types | `reattach_accepted` | New. Valid only as the first event on a resumed stream; ignored anywhere else. |

The handshake is carried in messages rather than gRPC metadata, so it
is described in the proto like the rest of the contract. The new
fields go through `boundEventFields`
(`harness/internal/transport/eventbound.go:38-58`), with
`resume_token` capped at 4 KiB, and through the invariants package's
byte-bound and proto-mirror tests.

## Authenticating a re-attach

The session ID cannot authenticate a re-attach on its own.
`CONTROL_PLANE_SESSION_ID` is optional (`job.go:122-127`), it is echoed
in clear on `ready`, and at least one control plane design treats it
as single-use. A resume credential is needed.

| Concern | Design |
|---|---|
| Issued by | The control plane, on the `task_assignment` ControlEvent, as `resume_token`. It is not part of `RunConfig`, for the same reason `trace_context` is not (`harness.proto:633-637`): `RunConfig` is recorded, redacted and replayed, and replaying a configuration must not grant a resume. CLAUDE.md also forbids secrets in `RunConfig`. |
| Format | Opaque to the harness: printable ASCII, at most 4 KiB. The control plane chooses: a random handle into its session store, or a signed token. |
| Bound to | The session the task was assigned on, and the active run. The control plane checks that the `id` (session) and `run_id` on `reattach` match the binding, and that `run_id` is the run it last saw start, or empty when the session is idle. |
| Lifetime | The control plane should reject a credential presented after the window it set has passed, measured from when it saw the loss. |
| Rotation | Every `reattach_accepted` carries a new credential, and the old one is spent. The harness replaces its copy before it sends `resumed`. |
| Lost rotation | If the stream dies between `reattach_accepted` and the harness reading it, the harness still holds the spent credential. The control plane therefore accepts the previous credential until the new one has been presented once ([open question 4](#open-questions-for-the-maintainer)). |
| Storage in the harness | In transport memory only, like the sandbox identity token. It is never logged, traced, put in an event other than `reattach`, written to `RunConfig`, `RunResult` or the trace, passed to the executor, or exposed to the model. |
| One stream per session | A valid `reattach` supersedes the session's previous stream: the control plane sends nothing more on it and drops anything that arrives on it. The harness only ever reads one stream. |

**What mTLS already provides.** With `CONTROL_PLANE_TLS_CERT_FILE`
set, the harness presents a client certificate (`job.go:38-47`). That
proves the caller holds a certificate from the control plane's CA. It
does not prove which session the caller owns, because a mesh or a
shared Secret often gives every harness Pod the same identity. A
control plane with per-Pod certificates can additionally require that
a re-attach comes from the same client identity as the original
stream. That check makes a stolen credential useless off the Pod, but
it does not replace the credential.

**What server-side TLS provides.** Today the harness trusts the network
once, at the first dial. A redial adds a new moment of trust: an
attacker able to break the connection and answer the redial would be
handed an in-flight run, its transcript, and the authority to answer
its `permission_request` events. Server certificate verification
closes that. The design therefore refuses to redial over plaintext:
with the window non-zero and no `CONTROL_PLANE_TLS_*` set, the harness
rejects the task. An operator whose transport security comes from a
mesh sidecar (plaintext to `localhost`) opts in with a Pod environment
variable, `STIRRUP_CONTROL_STREAM_REATTACH_PLAINTEXT=1`, never with a
`RunConfig` field, matching the rule that transport security is set by
the operator, not the control plane
([`integration-guide.md`](integration-guide.md#transport-security-posture-v01)).
This is [open question 2](#open-questions-for-the-maintainer).

## Sequence numbers, retention and gaps

**Why a ring, not only a detach buffer.** A successful `Send` means
the event was queued for HTTP/2, not that the control plane read it.
Events sent just before a break may never have arrived, and the
harness cannot tell which. It therefore keeps a ring of the most
recent events, sent or not, and the control plane says where it got to
(`resume_after_seq`).

| Rule | Value |
|---|---|
| Sequence scope | Per session, monotonic across the primary run and every follow-up run. Assigned in `Emit` after scrubbing and bounding, so the retained bytes are exactly what was sent or would have been sent. |
| Byte cap | 8 MiB of encoded `HarnessEvent` size (fixed in v1; [open question 5](#open-questions-for-the-maintainer)). |
| Not retained | `heartbeat` and `batch_waiting`. A replayed liveness signal means nothing; `resumed` itself shows the harness is alive. Neither is sent while detached, and they keep their schedule after re-attach. |
| Eviction | When the ring is over the cap, the oldest droppable event goes first. When no droppable event is left, the oldest protected event from before the loss goes. Protected events emitted after the loss are never evicted. |
| Protected | `run_started`, `done`, `error`, `turn_started`, `turn_committed`, `turn_abandoned`, every `*_request` event, `batch_submission`, `batch_cancel_request`, a `warning` carrying `request_id`, and a `user_message` carrying `request_id`. These carry the run's structure and everything the control plane must act on or deduplicate. |
| Droppable | `text_delta`, `thinking_delta`, `tool_call`, `tool_result`, `tools_loaded`, other `user_message` and `warning` events. The durable copy of all of them is the trace emitter or the `resultSink`, as the integration guide already advises. |
| Bound on protected events | Those emitted during one break are bounded by the structure of the run: a break of at most 300 s within one run (no follow-up can start while detached), at most two turn events per turn under `maxTurns`, one `run_started`, `error` and `done`, at most 16 echoed `user_response` events, and requests bounded by their own timeouts. |

**Replay.** After `reattach_accepted{resume_after_seq: N}` the harness
sends `resumed` and then every retained event with `seq > N`, in order,
with its original sequence number. New events wait behind the replay,
so order is preserved end to end.

**Gaps.** An evicted event leaves a hole in the sequence numbers.
`resumed.dropped_events` counts the holes after `N`, and
`resumed.resume_from_seq` is the first retained sequence number when
eviction reached past `N`. A control plane that sees
`dropped_events > 0` knows the live transcript for that stretch is
incomplete and reads it from the trace instead. That is the "explicit
gap marker" the issue asks for. A second form, an inline gap event in
place of the dropped stretch, is
[open question 6](#open-questions-for-the-maintainer).

**Duplicates.** A control plane that reports `N` accurately receives no
duplicates. One that cannot (for example, after a restart that lost
its last-seen counter) sends `resume_after_seq: 0` and deduplicates by
`seq`.

## Requests while detached

Every request keeps its existing timeout and fail-closed result. While
detached, `Emit` retains the request and returns nil, so the
correlator's timer runs from the emit, exactly as for a request the
control plane never answers. If the stream comes back in time, the
request is replayed and can still be answered.

| Request | Timeout today | While detached | Where |
|---|---|---|---|
| `permission_request` (`ask-upstream`) | 60 s default (`permissionPolicy.timeout`) | Auto-denied at its normal timeout. The request is not denied at emit time, and it is not held until the window lapses. | `askupstream.go:17`, `:138-151`; `correlator.go:146-155` |
| `tool_result_request` (`tools.controlPlane`) | `timeoutSeconds` per tool | Times out; the model sees a tool error. | `harness/internal/tool/controlplane.go:16-23` |
| `sandbox_token_request` | 60 s, fail closed | Initial request (during construction): the run aborts before a sandbox exists, as today. Pre-delivery request: construction aborts and the sandbox is torn down. Refresh: refreshing stops, the previous token stays until it expires, and a `warning` is emitted. Refreshing does not restart after re-attach in v1 ([open question 7](#open-questions-for-the-maintainer)). | `harness/internal/sandboxidentity/exchange.go:24`, `harness.proto:229-247` |
| `batch_submission` | The run deadline | The wait continues; a `batch_result` can arrive only after re-attach. | `harness.proto:764-772` (the `timeout` comment) |

The difference from today is one of timing only. Today an emit on a
dead stream fails at once (`correlator.go:141-144`), so the request
fails before its timeout. Inside the window it fails at its timeout.
Neither path ever turns a missing answer into an allow.

`resumed.pending_request_ids` lists the requests still waiting. The
control plane re-sends any answer it had already given for one of them,
because that answer may have been lost in the break. An answer for an
ID that has since timed out is dropped (`correlator.go:93-107`), so
re-sending is always safe. The list is built from the transport's
correlators and the batch adapter's pending map; it is a snapshot, and
an ID can expire just after it is sent.

## Control events lost in the break

Inbound delivery stays at most once per stream: a `ControlEvent` the
control plane sent on the broken stream may or may not have arrived.

| Event sent during the break | What the control plane does after `resumed` |
|---|---|
| `permission_response`, `tool_result_response`, `sandbox_token_response`, `batch_result` | Re-send if the request ID is in `pending_request_ids`. |
| `user_response` | Check the replay for a `user_message` or `warning` echoing its `request_id`. If neither is there, it was not received; re-send it. Both are protected, so they are not evicted. |
| `cancel` | Re-send. A second `cancel` is harmless; "one cancel is always enough" still holds for one that arrives. |

## Trace context and observability

| Item | Behaviour |
|---|---|
| `task_assignment.trace_context` | Read once, before construction, into the primary run's context (`job.go:195-197`). A stream loss does not touch the run's context or its spans, so the run's trace continues unbroken across a break. `trace_context` on any other ControlEvent type, including `reattach_accepted`, is still ignored (`harness.proto:633`). |
| `tool_result_request.trace_context` | Part of the retained event; a replay carries the original value. |
| `run_started.trace_reference` | Protected, so a control plane that missed it gets it in the replay. The `done` summary repeats it regardless. |
| Follow-up runs | Still start their own traces. Re-attach does not change that. |
| OTel export | Independent of the control stream (OTLP); unaffected. |
| Run span | Gains span events `control_stream.detached` (cause) and `control_stream.reattached` (detached duration, attempts, `dropped_events`), so a trace shows where a run ran unwatched. |
| Logs | One `warn` line on detach, one `info` line per successful re-attach, and one `warn` line on lapse. Emit failures do not occur while detached, so the `logEmitFailure` suppression (`streamloss.go:42-51`) is unaffected. |
| `RunTrace` | No new field in v1 ([open question 8](#open-questions-for-the-maintainer)). |

## Lifecycle interactions

| Where the loss lands | With the window non-zero | If the window lapses |
|---|---|---|
| Before `task_assignment` | Unchanged. The grace is not known yet and no credential exists. | Not applicable. The job fails as today (`job.go:180-181`). |
| Construction | The harness enables re-attach as soon as it accepts the assignment, before `buildJobLoop`. The build watcher (`job.go:203`) therefore fires only on lapse, and construction carries on. A `sandbox_token_request` it raises still has its 60 s limit. | Construction is cancelled and the job fails with `control_stream_lost`, as today. |
| Active run | The run continues. | `ControlStreamLost` runs and the run ends `control_stream_lost`, exit non-zero, as today. |
| Run finishes while detached | `done` is protected and retained. The harness waits for a re-attach to deliver it, for at most the time left in the window. | The run keeps its real outcome, which the `resultSink` and trace emitters have already recorded, and the process exits with that outcome's status. A warning logs that `done` was never delivered ([open question 9](#open-questions-for-the-maintainer)). |
| Idle follow-up window | The `followUpGrace` timer keeps running; no `user_response` can arrive while detached. A re-attach inside both windows resumes the follow-up window, and a `user_response` then starts a run as usual. `followUpHistory: "continue"` is unaffected: re-attach never touches history. | The follow-up window closes, and the last run's outcome is unchanged, as today. |
| Follow-up window ends while detached | The session is over. The harness waits for the rest of the re-attach window only to deliver retained events. | The process exits with the last run's status. |
| SIGTERM / SIGINT while detached | The stream context is cancelled, redialling stops, and the shutdown path runs as today. | Not applicable. |

**Session budget.** The transport closes before the executor
(`grpc.go:214-216`), so a drain wait at session end keeps the sandbox
alive. `jobSessionBudget` (`job.go:297-304`) must add the window to
the session it sizes. Operators raise `activeDeadlineSeconds` by the
same amount.

**Close.** When the session ends attached, `Close` keeps today's 2 s
half-close drain (`grpc.go:217`). When the session ends detached,
`Close` first waits up to the rest of the window for a re-attach and
the replay, then performs the same half-close.

**Outcome and exit status.** No new outcome. A lapse uses
`control_stream_lost`, and `RunTrace.error` names the lapse:
`control stream lost: re-attach window of 120s lapsed: <first cause>`.
A permanent rejection names the status the control plane returned.

## Security argument

Claim: for every way a control plane can affect a run, a detached run
has at most the capability of an attached run whose control plane
sends nothing.

| Capability | Attached run | Detached run |
|---|---|---|
| Tool approval (`ask-upstream`) | Allowed only on a `permission_response{allowed:true}` | No response can arrive, so the request is auto-denied at its timeout. |
| Auto-allowed tools | Run under the permission policy | The same: the policy, Cedar, the Rule-of-Two gate, guardrails, the executor's network mode and the budgets are all inside the run and do not read the stream. |
| Control-plane tool results | From `tool_result_response` | Time out to a tool error. |
| Sandbox identity token | Issued per `sandbox_token_response` | No new token. The current token expires on its own schedule, and refreshing stops (fail closed). |
| New input to the model (`user_response`) | Queued and screened | None arrives. |
| A new task | Duplicate assignments are ignored | Same; `task_assignment` is not valid on a resumed stream. |
| Network reach | The stream to `CONTROL_PLANE_ADDR` | Redials to the same address, fixed at process start. A `RunConfig` cannot change the target. |
| Run duration | `timeout` | Unchanged; the window does not extend it. |

What the window does cost, and how each cost is bounded:

| Cost | Bound or mitigation |
|---|---|
| The control plane cannot `cancel` a detached run. | At most the window (300 s cap). Within it, the control plane re-attaches and cancels, or deletes the Pod. |
| Up to one window of work nobody watches, per break. | This is the cost #659 removed, reintroduced only by opt-in and only for a bounded time. The control plane chooses the window. |
| Two workers, if the control plane reschedules while the original can still re-attach. | See [Control-plane obligations](#control-plane-obligations). |
| The redial trusts the network again. | Re-attach refuses plaintext unless the operator opts in ([Authenticating a re-attach](#authenticating-a-re-attach)). |
| A stolen credential lets the thief pose as the harness to the control plane, and receive control events meant for the run, including a sandbox identity token on a request the thief sends. | The credential exists only in harness memory and never reaches the sandbox, the model or a record. It is single-use and rotated on every re-attach. It is valid only within the window for one session and run. Optionally it is bound to the mTLS client identity. Provider API keys and the sandbox token already live in the same harness process memory. |
| A thief with the credential could supersede a healthy stream. | Only with an unspent credential, which leaves the harness process only inside a `reattach`, over TLS. Requiring the same mTLS identity closes this for per-Pod certificates ([open question 4](#open-questions-for-the-maintainer)). |
| Retained events hold transcript content in memory. | Already scrubbed and bounded. The same content is already in the run's history in memory. 8 MiB cap. |

The read-only-mode invariant, the secret-reference rule and the
event byte bounds are untouched. The loop in
`harness/internal/core/loop.go` gains no new input.

## Control-plane obligations

A control plane that sets the window takes on these obligations:

1. **Persist the session binding** (session, run, current and previous
   credential, last sequence number received) where any replica can
   read it. A control plane that loses it on restart rejects with
   `NOT_FOUND` and gains nothing from the feature.
2. **Answer `reattach` before anything else** on a new stream, with
   `reattach_accepted` or a permanent status, within 10 s.
3. **Fence before rescheduling.** Do not start a replacement worker
   while the original could still re-attach: wait at least the window,
   plus the keepalive detection bound (about 30 s), plus a margin, from
   when the control plane saw the loss. Otherwise, delete the Pod (or
   Job, or Sandbox) and wait for it to terminate. Run Jobs with
   `backoffLimit: 0`, so Kubernetes does not retry on its own.
4. **Allow the keepalive pings**: a minimum ping interval of 20 s or
   less.
5. **Re-send** the answers, `user_response` events and `cancel` that
   [Control events lost in the break](#control-events-lost-in-the-break)
   lists, and deduplicate events by `seq`.
6. **Stretch liveness deadlines.** Missing heartbeats during a break
   are expected; a heartbeat-based reaper must allow the window before
   it reaps.

## Compatibility and the `ready` capability

| Combination | Result |
|---|---|
| Old harness, new control plane | `ready.capabilities` lacks `control_stream_reattach`, and `ready.config_fields` lacks `controlStreamGraceSeconds`. An old harness would drop the field without notice, so the control plane must gate on the capability and send the field only when it is advertised. |
| New harness, old control plane | The field is never set, so behaviour is unchanged. |
| New harness, field set, control plane does not implement `reattach` | Every redial fails or times out, and the run ends `control_stream_lost` one window later than today. |

`control_stream_reattach` covers the field, the credential fields, the
sequence numbers, the three new event types and the obligations above.
It is added to `types.HarnessCapabilities()` (`types/events.go:450`).

## Implementation map

| Area | Change |
|---|---|
| `types/runconfig.go` | Field, bounds 0-300, JSON name. |
| `proto/harness/v1/harness.proto`, `gen/` | Field 62, the event fields and types, and comments; `just proto`. |
| `harness/internal/transport/grpc_translate.go` | Copy the field, the new event fields and the new ControlEvent fields both ways. |
| `harness/internal/transport/` | A re-attaching stream holder behind the existing `GRPCTransport` API: the ring, sequence numbers, redial, handshake and replay, plus session-scoped `Done`/`Err`. `eventbound.go` caps the new fields. The correlators expose their pending IDs. |
| `harness/cmd/stirrup/cmd/job.go` | After accepting the assignment: read the window and the credential, check the TLS rule, and enable re-attach before `buildJobLoop`. Add the window to `jobSessionBudget`. |
| `harness/cmd/stirrup/cmd/harness.go` | Reject a non-zero window. |
| `harness/internal/core/` | Span events only, injected through the transport's callbacks. No change to `loop.go` control flow. |
| `harness/internal/invariants` | The new fields join the byte-bound and proto-mirror pins. A test that the credential never reaches a log, trace, `RunResult` or `RunConfig.Redact()` output. |

## Test plan

The first five rows are the issue's acceptance criteria; the rest
pin the design's own rules.

| # | Criterion | Test |
|---|---|---|
| 1 | Design note merged | This document. |
| 2 | Field unset: behaviour unchanged byte for byte | The existing stream-loss tests pass unmodified: `TestRun_ControlStreamLost*` and `TestControlStreamLost_*` (`harness/internal/core/streamloss_test.go`), `TestRunJob_ControlPlaneEndingRPCDuringProviderCallCancelsRun`, `TestRunJob_ConnectionFailureDuringProviderCallCancelsRun`, `TestRunJob_StreamLostDuringConstructionStartsNoRun`, `TestRunJob_StreamEndedAfterDoneClosesFollowUpWindow`, `TestWatchControlStream_*`, `TestRunWithConfig_GRPCTransportLossCancelsRun` (`harness/cmd/stirrup/cmd/controlstream_test.go`), and `TestGRPCTransport_*` in `grpc_liveness_test.go`. A new test compares the encoded bytes of every event of a run against a run with the field unset, asserting no `seq`. |
| 3 | Drop mid-turn, restore inside the window, real outcome, complete sequence or explicit gap | Over bufconn, a fake control plane ends the RPC while the provider call is blocked, accepts the `reattach`, then the provider is released. The test asserts outcome `success`, contiguous `seq` from 1 to `done`, and `dropped_events: 0`. A variant with a ring forced small asserts `dropped_events > 0`, the holes it counts, and that every protected event is present. |
| 4 | The window lapses | The control plane never comes back. The test asserts `control_stream_lost`, a lapse message in `RunTrace.error`, a non-zero job exit, and an end time about one window after the loss. |
| 5 | A permission request raised while detached is auto-denied at its normal timeout | With a 1 s policy timeout and a 5 s window, a tool needing approval is called during the break. The test asserts denial at about 1 s, not at emit time and not at lapse. After a re-attach at about 3 s, the request is in the replay but not in `pending_request_ids`, and a late `permission_response` for it is dropped. |
| 6 | Permanent rejection | `PERMISSION_DENIED` on the handshake ends the window at once. |
| 7 | Rotation | The new credential is used on the second re-attach. A spent credential is rejected by the fake control plane once its successor has been presented once. |
| 8 | Credential hygiene | The credential never appears in logs, the JSONL trace, OTel attributes, `RunResult`, `RunConfig.Redact()` or any event but `reattach`. |
| 9 | Plaintext rule | A non-zero window without TLS, and without the opt-in variable, rejects the task with `error` and `done`. |
| 10 | Missing credential | A non-zero window with no `resume_token` rejects the task. |
| 11 | Construction | A break during a blocked `buildJobLoop` that recovers inside the window: the run starts and completes. |
| 12 | Follow-up | A break in the idle window, then a re-attach and a `user_response`: a follow-up run starts. |
| 13 | Run finishes while detached | `done` is delivered after re-attach. With no re-attach, the real outcome and its exit status are kept. |
| 14 | Pending answers | A `permission_response` lost in the break is re-sent on `pending_request_ids` and allows the tool. |
| 15 | `cancel` after re-attach | It ends the run `cancelled`. |
| 16 | SIGTERM while detached | Redialling stops and shutdown is not delayed by the window. |
| 17 | Validation and mirror | Bounds 0-300; negative and 301 rejected; the proto-mirror invariant covers field 62; `stirrup harness` rejects a non-zero value. |
| 18 | Capability | `ready` carries `control_stream_reattach`, and `config_fields` carries the path. |

All timing tests use injected clocks or short windows, so they do not
join the known timing flakes.

## Open questions for the maintainer

Each question has a recommendation; the design above assumes it.

1. **Does this protocol meet the 2026-10-08 bar?** The decision on
   #659 put continue-after-loss out of scope "until a lease or session
   protocol exists (#456)". *Recommendation:* yes, for one process
   lifetime. The credential is the lease, the one-stream rule is the
   fence, and the sequence replay is the session protocol. #456 stays
   the home of continuity across process restarts.
2. **Refuse plaintext re-attach?** *Options:* refuse unless the
   operator opts in with `STIRRUP_CONTROL_STREAM_REATTACH_PLAINTEXT=1`;
   only warn; always refuse. *Recommendation:* refuse with the opt-in.
   A redial without server authentication hands an in-flight run to
   whoever answers.
3. **The cap.** *Options:* 60 s, 300 s, 600 s. *Recommendation:* 300 s.
   It covers a rolling deploy and keeps unwatched work to a few model
   turns.
4. **Credential handover.** Accept the previous credential until the
   new one is used once (recommended), or accept only the newest one,
   which risks a lapse when a rotation is lost. Separately: should the
   design *require* the same mTLS client identity on re-attach when
   mTLS is on? *Recommendation:* a strong control-plane recommendation,
   not a harness rule, since the harness cannot check it.
5. **Ring size.** Fixed at 8 MiB (recommended for v1), or an operator
   environment variable. A `RunConfig` field is not recommended.
6. **Gap marker shape.** A count on `resumed` plus holes in `seq`
   (recommended), or also an inline `stream_gap` event in place of each
   dropped stretch.
7. **Restart sandbox-token refresh after re-attach?** *Recommendation:*
   not in v1. Stopping is fail closed. Restarting changes the
   sandbox-identity contract and belongs in its own change.
8. **Record breaks in `RunTrace`?** For example `controlStreamBreaks`
   and `controlStreamDetachedMs`. *Recommendation:* span events only in
   v1. Add the fields once a control plane needs them in `RunResult`.
9. **A run that finished while detached and was never re-attached.**
   Keep its real outcome and exit status (recommended): the result sink
   and trace carry it, and reclassifying would discard a completed
   run's result. The alternative reports `control_stream_lost`.
10. **Keepalive.** Enable 20 s / 10 s client keepalive only when the
    window is non-zero (recommended), or always, which would also speed
    up loss detection today but changes the wire behaviour for every
    control plane.

## Docs to update on implementation

- [`integration-guide.md`](integration-guide.md): the
  [Delivery](integration-guide.md#watching-a-run-live) paragraph, the
  stream lifecycle, both event tables, [Compatibility](integration-guide.md#compatibility),
  the sandbox-identity sentence that the control plane "derives the
  run identity from the authenticated stream the task was assigned on"
  (`harness.proto:240-241`), and the fencing contract.
- [`configuration.md`](configuration.md): the field reference.
- [`deployment.md`](deployment.md): the session table, the
  `activeDeadlineSeconds` sizing, and the plaintext opt-in variable.
- `proto/harness/v1/harness.proto`: the lifecycle comment at the top of
  `HarnessService`, and the new event-table entries.
