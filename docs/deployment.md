# Production deployment

Stirrup is designed to be deployed as a short-lived Kubernetes job
that talks to a long-running control plane over gRPC. This document
describes the contract between the two, the container image, and the
release process. For local development the [README](../README.md)
covers the basics.

## Architecture

```mermaid
sequenceDiagram
    participant CP as Control plane
    participant K as Kubernetes API
    participant H as stirrup job (Pod)
    Note over CP,H: Per task
    CP->>K: Create Job<br/>(env: CONTROL_PLANE_ADDR, ANTHROPIC_API_KEY-equivalent secret)
    K-->>H: Pod scheduled
    H->>CP: Dial gRPC, open RunTask stream
    H->>CP: HarnessEvent{type:"ready", harness_version}
    H->>H: Write /tmp/healthy (liveness probe)
    CP->>H: ControlEvent{type:"task_assignment", task: RunConfig}
    H->>H: Build AgenticLoop with the supplied RunConfig
    loop For each turn
        H->>CP: heartbeat (every 30s)
        H-->>CP: text_delta · tool_call · tool_result
        opt RequiresApproval tool
            H->>CP: permission_request
            CP-->>H: permission_response
        end
        opt Async tool
            H->>CP: tool_result_request
            CP-->>H: tool_result_response
        end
    end
    H->>CP: HarnessEvent{type:"done", stop_reason, trace}
    H->>H: Remove /tmp/healthy
    H->>K: Process exits 0
```

The harness *connects outbound* to the control plane. There is no
inbound port to expose, no service mesh hop to configure, and no
shared filesystem with the control plane. The only inputs are the
environment variables passed at Pod creation time and whatever the
control plane sends over the bidi stream.

## Transport security posture (v0.1)

> **The gRPC transport is plaintext and unauthenticated.** The
> harness dials the control plane with
> `insecure.NewCredentials()`, and `TransportConfig` exposes only
> `{type, address}` — there is no TLS, token, or mTLS knob on the
> config surface in v0.1.

v0.1 targets single-operator, self-hosted deployment. The design
assumes a trusted network path between job Pods and the control
plane; acceptable shapes are:

- **Same host** — control plane and harness on one machine,
  dialling over loopback.
- **Private network** — a private VPC / cluster network where the
  control-plane address is not reachable from untrusted networks.
- **Mesh-provided mTLS** — a service mesh (Istio, Linkerd) that
  transparently encrypts and authenticates Pod-to-Pod traffic
  outside the harness process.

Do not point `--transport-addr` / `CONTROL_PLANE_ADDR` across an
untrusted network: everything on the stream — prompts, tool
results, permission responses — would transit in cleartext, and
any endpoint that can reach the harness's dial target could pose
as the control plane. Secrets are less exposed than they appear
(API keys travel as `secret://` references, never raw values, and
the transport scrubs secret-shaped strings from outbound events),
but the stream contents and the control channel itself are
unprotected.

Transport TLS configuration is planned post-v0.1. The internal
transport constructor already accepts TLS credentials
(`transport.WithTLSCredentials`); what is missing is the
`RunConfig` / CLI surface to reach it, so today the option is
available only to Go embedders wiring the transport directly.

## The `stirrup job` subcommand

`stirrup job` is the K8s entrypoint. It takes no flags — everything
comes from the environment and from the `RunConfig` delivered as the
first `ControlEvent` on the stream.

### Required environment variables

| Variable | Purpose |
|---|---|
| `CONTROL_PLANE_ADDR` | gRPC target address of the control plane (e.g. `control-plane.svc:9090`). |

### Optional environment variables

| Variable | Purpose |
|---|---|
| `CONTROL_PLANE_SESSION_ID` | Session correlation ID echoed back in the initial `ready` event so the control plane can match this gRPC stream to the session that launched it. |
| `STIRRUP_FOLLOWUP_GRACE` | Seconds to keep the gRPC stream open after the agentic loop completes, so the control plane can deliver follow-up `user_response` events. Capped at 3600 s. |

Per-provider secrets (Anthropic API key, AWS / GCP / Azure
credentials) are *not* passed via stirrup-specific env vars — they
follow the configured `secret://` references in the `RunConfig` or
the credential federation chain (IRSA, GKE Workload Identity, Azure
IMDS, GitHub Actions OIDC). See
[`credential-federation.md`](credential-federation.md).

### Lifecycle

1. **Connect.** The harness dials `CONTROL_PLANE_ADDR` over gRPC and
   opens a single `RunTask` bidi stream for the lifetime of the
   task.
2. **Ready.** The harness sends a `HarnessEvent{type:"ready", harness_version}`
   so the control plane can verify the binary version before
   dispatching work.
3. **Liveness.** A liveness probe file is written to `/tmp/healthy`
   so the K8s readiness/liveness probes have something to inspect.
4. **Wait for assignment.** The harness blocks on a
   `ControlEvent{type:"task_assignment"}` carrying the `RunConfig`,
   with a 5-minute timeout. A `cancel` event received before
   assignment is honoured: the harness exits cleanly without
   running anything.
5. **Build and run.** Once the `RunConfig` arrives, the wall-clock
   timeout is applied to the context, the agentic loop is built via
   `core.BuildLoopWithTransport` reusing the existing gRPC transport,
   and execution begins.
6. **Stream events.** Throughout the run the harness emits
   `text_delta`, `tool_call`, `tool_result`, `heartbeat` (every 30
   s), and — depending on the permission policy — `permission_request`
   events. Async tools may emit `tool_result_request`.
7. **Done.** A final `HarnessEvent{type:"done", stop_reason, trace}`
   carries the run metrics and the reason the loop ended
   (`end_turn`, `max_turns`, `timeout`, `stalled`, `tool_failures`,
   `cancelled`, `budget_exceeded`, `error`, `setup_failed`,
   `hook_failed`). `error`, `setup_failed`, and `hook_failed` are the
   early-termination outcomes — a build-system-prompt failure, a
   `GitStrategy.Setup` failure, or (issue #461) a fatal `preRun` /
   `postRun` lifecycle hook failure — and, like every other outcome,
   are always paired with a `done` event and a `RunResult` on the
   configured `resultSink`, even though the harness process's own Go
   error return is also non-nil for these. The nested `trace.outcome`
   is the canonical terminal status for downstream analytics — the
   full `RunTrace.Outcome` set, which adds `success`,
   `verification_failed`, `verification_error`, and `max_tokens` on
   top of the loop's stop reasons. `trace.stop_reason` mirrors it for
   backward compatibility.
8. **Follow-up grace** *(optional)*. If `STIRRUP_FOLLOWUP_GRACE > 0`,
   the stream stays open for that many seconds so the control plane
   can deliver `user_response` events that resume the loop.
9. **Exit.** The liveness probe file is removed; the process exits 0
   on success or non-zero on transport / build / runtime failure.

The full event vocabulary lives in
[`proto/harness/v1/harness.proto`](../proto/harness/v1/harness.proto) —
that is the source of truth for the wire contract.

## Container image

Releases publish two image tags to GitHub Container Registry:

- `ghcr.io/rxbynerd/stirrup:<tag>` for tagged releases (`v1.2.3`).
- `ghcr.io/rxbynerd/stirrup:main` from CI on every merge to `main`.

The image is `gcr.io/distroless/static-debian12:nonroot`-based: a
single statically-linked binary, no shell, no package manager, runs
as `nonroot` (uid 65532).

## Release process

Releases are tag-driven via `.github/workflows/release.yml`. To cut
one:

```sh
git tag -a v1.2.3 -m "Release notes"
git push origin v1.2.3
```

`workflow_dispatch` against an existing tag re-runs the workflow for
retries.

The workflow:

1. Re-runs the verify job (build + test).
2. Cross-compiles `stirrup` and `stirrup-eval` for
   `linux/{amd64,arm64}` and `darwin/{amd64,arm64}` in parallel.
3. Generates SPDX and CycloneDX SBOMs.
4. Aggregates artifacts under a single `SHA256SUMS` manifest.
5. Publishes a GitHub Release. Tags containing `-`
   (e.g. `v1.2.3-rc1`) are marked as prereleases automatically.

Artifact signing (cosign / Sigstore) is intentionally out of scope
for now; a commented-out signing seam sits in `release.yml` between
the SHA256SUMS step and the release-create step.

## Version labels

The version label baked into binaries follows this convention:

| Build origin | `stirrup --version` output |
|---|---|
| `release.yml` on a tag | `v1.2.3 (ab74b75)` |
| `ci.yml` on `refs/heads/main` | `main (ab74b75)` |
| `ci.yml` on any other ref | `dev (ab74b75)` |
| `go build` / `go run` locally | `dev` |

The labels are injected via `-ldflags` against
`github.com/rxbynerd/stirrup/types/version.{version,commit}`.

## CI

`.github/workflows/ci.yml` runs three jobs on each push:

- **`verify`** — `go test` plus binary builds for the `types`,
  `harness`, and `eval` modules. Runs on every push and PR via the
  reusable `_verify.yml` workflow.
- **`eval-gate`** — runs `eval/suites/` against `eval/baselines/` on
  the `main` branch only, exits non-zero on regressions, uploads
  results as artifacts. Blocks the publish step.
- **`publish-container`** — builds and pushes the image to GHCR on
  the `main` branch only, after `eval-gate` passes.

The Anthropic WIF smoke test (`smoke-anthropic.yml`) is a separate
gated workflow that exercises the federation flow end-to-end.

## Operator checklist

Before deploying:

1. Decide a deployment posture using
   [`safety-rings.md`](safety-rings.md). The safe defaults for
   first-run production are: `executor.type=container`,
   `executor.runtime=runsc`, `network.mode=allowlist`,
   `permissionPolicy.type=policy-engine` with a starter policy from
   [`examples/policies/`](../examples/policies/), `codeScanner.type=patterns`,
   and Rule of Two enforcement on (the default).
2. Wire credentials through the credential federation layer rather
   than baking API keys into Pod env vars. See
   [`credential-federation.md`](credential-federation.md).
3. Configure observability: OTLP/gRPC to your collector or OTLP/HTTP
   to a managed gateway. See
   [`observability-cloud.md`](observability-cloud.md).
4. Set `livenessProbe.exec.command: ["sh", "-c", "test -f /tmp/healthy"]`
   on the Pod. (The image has no shell, so use `[""]` style probes
   only when adding a debug sidecar.)
5. Enforce a `Job.spec.activeDeadlineSeconds` slightly larger than
   `RunConfig.timeout` so K8s reaps any stuck Pod even if the
   harness's own wall-clock timeout fails to fire.
6. Confirm the network path between job Pods and the control plane
   is trusted (private network or mesh mTLS) — the gRPC transport
   itself is plaintext and unauthenticated in v0.1. See
   [Transport security posture](#transport-security-posture-v01).

## Embedding the harness

The public Go API surface is `harness/harnessapi/`. Everything under
`harness/internal/*` is intentionally not part of the public API.

```go
import "github.com/rxbynerd/stirrup/harness/harnessapi"

loop, err := harnessapi.BuildLoopWithTransport(ctx, runConfig, transport)
if err != nil { /* handle */ }
err = loop.Run(ctx)
```

Use this when you need the agentic loop in-process — for example, in
a single-binary tool that bundles its own control-plane logic. For
typical multi-tenant deployments, run the binary as a Job and let
the gRPC contract do the talking.
