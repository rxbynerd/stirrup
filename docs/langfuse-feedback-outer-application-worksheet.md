# Langfuse user feedback: outer-application worksheet

Status: proposal, 2026-10-17. Companion to
[the research worksheet](langfuse-feedback-and-trace-propagation-research.md)
and [the Stirrup implementation worksheet](langfuse-feedback-and-trace-propagation-implementation.md).
This document describes the application that owns the user-facing
rating page. It has no repository yet, so the worksheet lives here until
one exists; the Stirrup worksheet is the source of truth for the
surfaces it consumes.

Hairpin is the functional-test control plane. The design depends on a
control plane that persists and exposes Stirrup's trace reference and
forwards a correlation block
([hairpin#18](https://github.com/rxbynerd/hairpin/issues/18)); it does
not depend on Hairpin itself.

## What the application consumes from Stirrup

| Surface | Package | Shape |
|---|---|---|
| Trace reference | S1 | `{ traceId, runSpanId, sampled }` on the `run_started` event, on `done.trace`, on `RunTrace` and on `RunResult`; absent when the run records no OTel span |
| Correlation block | S2 | `RunConfig.correlation { sessionId, taskId, attemptId, userId, url }`, stamped on every span; `userId` is opaque to Stirrup |
| Langfuse mapping | S3 | With the `langfuse` profile, `user.id` becomes `langfuse.user.id`, `url` becomes `langfuse.trace.metadata.control_plane_url`, task and attempt IDs become trace metadata |
| Sampling signal | S1/S6 | `sampled: false` means the run's spans were not exported; feedback can be stored but has no trace to land on |

## Responsibilities

1. **Mapping.** Persist, per attempt:
   `response/attempt → Stirrup run ID → trace ID + run span ID + sampled → Langfuse host and project`.
   Read the reference from the control plane's job record after
   `run_started` (and again from `done` as a fallback). Keep every
   attempt's reference; one task can have several attempts and a
   follow-up produces another run with a fresh reference.
2. **Submission.** Set `correlation` on each job: `sessionId` = the
   application's session, `taskId` and `attemptId` = its own
   identifiers, `userId` = its pseudonymous user key, `url` = the
   canonical page for that attempt under the configured public origin.
   Pseudonymise before setting `userId`; nothing downstream does it.
3. **Rating endpoint.** `POST .../feedback { value, comment }`,
   authenticated by the application. Resolve the attempt to its stored
   reference; never accept a browser-supplied trace ID as the subject.
   Persist a row keyed by attempt and reviewer before any delivery
   attempt. Derive the Langfuse score `id` from that key so retries and
   edits upsert and two reviewers never overwrite each other.
4. **Delivery.** An outbox worker posts
   `POST {host}/api/public/scores` with `id`, `traceId`,
   `observationId` = run span ID, `name: "helpfulness"`, `value`,
   `comment`, and Basic auth from the project key pair. An attempt with
   `sampled == false` or no reference keeps the row and marks delivery
   "no trace". Delivery failure leaves the row and retries. Scores may
   arrive before the trace and link later; that is not an error.
5. **Attribution.** Store the configuration revision or model identity
   with the rating, so a score can be credited to a setup. A rating
   alone does not say which configuration deserves it.
6. **Links.** Build `{host}/project/{projectId}/traces/{traceId}` from
   the host and project stored with the reference, never from today's
   configuration for an old run. Private trace links need the viewer's
   Langfuse access.
7. **Separation.** Keep human feedback distinct from verifier verdicts
   and inferred behavioural scores. Start with helpfulness plus an
   optional reason; add dimensions from real operator demand.

## Verification

- A pinned local stack: Langfuse (self-hosted v4.x), Stirrup with the
  `langfuse` profile, Hairpin with hairpin#18, and this application (or
  a minimal stub of it).
- Submit two attempts, rate the second, edit the rating, restart both
  the application and Hairpin, confirm the score sits on the second
  attempt's run span in Langfuse.
- Browser path: application page → Langfuse trace; trace metadata
  `control_plane_url` → application page.
- Negative paths: feedback on an attempt with `sampled: false` (row
  retained, "no trace"); feedback after an export failure (row retained,
  delivery marked); a submission with an invalid `correlation.url`
  rejected by the control plane.
