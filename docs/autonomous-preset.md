# Autonomous deployment preset

[`examples/runconfig/autonomous-preset.json`](../examples/runconfig/autonomous-preset.json)
is a complete `RunConfig` for running stirrup unattended against a
repository checkout. CI loads it, validates it, and runs the dry-run
preflight against it (`harness/cmd/stirrup/cmd/autonomous_preset_test.go`),
and fails when it drops any of the safeguards below.

The file is a starting point, not a turnkey deployment. The placeholders
(image tag, export bucket, verifier command, collector endpoint, egress
hosts, prompt) are replaced per deployment; the safeguards are kept.

## What the preset sets

| Setting | Value | Purpose |
|---|---|---|
| `executor.type`, `executor.runtime` | `container`, `runsc` | Ring 1: agent commands run in a gVisor-isolated container, not on the harness host. |
| `executor.image` | `ghcr.io/rxbynerd/stirrup-sandbox` | The sandbox image, which ships the shell, `tar` and `git` the executor needs. The harness image (`ghcr.io/rxbynerd/stirrup`) is distroless and cannot serve as a sandbox. |
| `executor.network` | `allowlist` with package-proxy hosts | Ring 2: egress limited to named hosts. |
| `permissionPolicy` | `policy-engine` over [`autonomous-allow-list.cedar`](../examples/policies/autonomous-allow-list.cedar), fallback `deny-all` | Ring 3: only the listed tools run, and destructive shell patterns are forbidden. |
| `ruleOfTwo.enforce` | `true` | Ring 4: Rule of Two enforcement, which rejects and gates combinations of untrusted input, sensitive data and external communication that lack operator approval. |
| `codeScanner` | `patterns` | Ring 5: post-edit static scan. |
| `verifier` | `test-runner` | Checks the work after each turn. The command is a placeholder (`go test ./...`) that must be replaced with the repository's own check; a verifier that cannot fail gives no assurance. |
| `gitStrategy` | `deterministic` | Commits the result on a `stirrup/<run>` branch as the handoff artifact, and reports the branch, result SHA and base SHA in `RunResult.git`. Requires an executor with exec, so it cannot be paired with `api` or `none`. |
| `resultSink` | `stdout-json` | Writes the `RunResult` to stdout as a `STIRRUP_RESULT` line. The line carries the git handoff and the workspace export receipt. |
| `executor.workspaceExportTo` | `gs://` URI | Archives the workspace through the executor at the end of every run, so the outcome survives the sandbox. The export is required by default: a failed export fails the run. |
| `traceEmitter` | `otel` | Sends traces to a collector, which outlives the job. A `jsonl` file on an ephemeral job host would be lost with it. |
| `maxTurns`, `timeout`, `maxTokenBudget` | `30`, `1800`, `2000000` | Bounds on turns, wall-clock time and tokens. |

The tool set is limited to the workspace read tools, `edit_file` and
`run_command`. `web_fetch`, `spawn_agent` and MCP servers are left out:
each widens the untrusted-input and exfiltration surface that the policy
file would then have to cover.

## Behaviour that holds today

- The workspace export is required. With `executor.workspaceExportTo`
  set, a failed upload ends an otherwise-successful run with outcome
  `handoff_failed` and a non-zero exit, under `stirrup harness` and
  `stirrup job` alike and for follow-up runs as well.
  `executor.workspaceExportRequired: false` (or
  `--export-workspace-required=false`) records the failure without
  failing the run. The export runs on every outcome, including a run
  that failed during setup. `RunResult.workspaceExport` holds the
  receipt: status `uploaded`, `empty` or `failed`, plus the archive's
  size, file count and SHA-256.
- The deterministic git strategy runs `git` inside the sandbox through
  the executor, never on the harness host, so the sandbox image must
  contain `git`. A workspace that is not a repository is initialised
  with a base commit. A workspace nested inside another repository, or
  a worktree or submodule whose `.git` file names a host gitdir, is
  refused. Commits carry the author `stirrup <stirrup@noreply.local>`.
  The agent can rewrite history inside the sandbox, so a control plane
  should diff against the base commit it already knows rather than rely
  on the branch alone.
- The `--dry-run` step `git-strategy-probe` checks that `git` runs in
  the sandbox. For the `container` executor no container starts in a
  dry-run, so a missing `git` surfaces when the run sets up.
- The permission policy path is resolved against the process working
  directory. The example path works from the repository root; a
  deployment copies the policy next to the config and edits the path.

## Running it

Validate the file:

```sh
stirrup run-config --config examples/runconfig/autonomous-preset.json --validate
```

Run the preflight, which builds every component and probes the provider,
container engine, egress hosts, collector and export bucket without
spending tokens:

```sh
ANTHROPIC_API_KEY=... stirrup harness --dry-run \
  --config examples/runconfig/autonomous-preset.json
```

An environment without a container engine, provider credentials, DNS or
a collector gates the matching probes off:

```sh
stirrup harness --dry-run \
  --config examples/runconfig/autonomous-preset.json \
  --no-probe-provider --no-probe-executor --no-probe-egress --no-probe-trace
```

The export-bucket probe has no gate and needs Google Cloud credentials.
The flags are described in
[`configuration.md`](configuration.md#dry-run-preflight).

Run it:

```sh
ANTHROPIC_API_KEY=... stirrup harness \
  --config examples/runconfig/autonomous-preset.json \
  --workspace /path/to/checkout \
  --prompt "Fix the failing test in main_test.go"
```

Through `stirrup job`, the same file is assigned as the `RunConfig` of a
task. [`examples/controlplane/`](../examples/controlplane/README.md) is a
runnable minimal control plane that assigns a `RunConfig` file to a
connected `stirrup job`, which demonstrates the preset against that
contract. The prerequisites below apply to the job unchanged.

```sh
go run ./examples/controlplane -config examples/runconfig/autonomous-preset.json
```

## Prerequisites outside stirrup

| Prerequisite | Owner |
|---|---|
| Sandbox image, pinned by version or digest rather than `:latest`, and reachable by the container engine | Platform team |
| `runsc` installed and registered with the container engine | Platform team |
| Provider credential, preferably through [credential federation](credential-federation.md) rather than a static key | Platform or security team |
| Export bucket and the identity allowed to write to it | Platform team |
| OTLP collector at `traceEmitter.endpoint` | Observability team |
| Verifier command that exercises the target repository | Repository owner |
| Egress hosts the repository's build needs | Repository owner, reviewed by security |
| Control plane, when run through `stirrup job` | Control-plane operator |

## What the preset does not cover

- Transport security for `stirrup job`. The preset sets nothing for the
  control-plane channel, which is plaintext and unauthenticated unless
  `CONTROL_PLANE_TLS_*` is set on the job. Either the network path to the
  control plane must be trusted or TLS must be configured. See
  [Transport security posture](deployment.md#transport-security-posture-v01)
  and [Transport TLS settings](deployment.md#transport-tls-settings).
- Provider choice, model tiering and cost controls beyond the token
  budget.
- Sandbox identity and git-proxy credentials, covered in
  [`configuration.md`](configuration.md#sandbox-identity-and-git-proxy-wiring).
- Repository guidance. The preset sets no `repositoryGuidance` block, so
  no `AGENTS.md` or similar file is loaded into the system prompt. See
  [`configuration.md`](configuration.md) for the block.
- MCP servers. The preset configures none, so there is no required-server
  startup check to satisfy.
- Run-end budget warnings. The preset leaves `windDown` off; see
  [Limits and budgets](configuration.md#limits-and-budgets).

## Variant: `k8s-sandbox`

The same safeguards apply on Kubernetes with `executor.type` set to
`k8s-sandbox` (or `k8s`). The differences are `executor.runtime: "gvisor"`
in place of `runsc`, `executor.k8sNamespace`, and, for `allowlist` egress,
`executor.k8sEgressProxyUrl`. The image, policy, verifier, git strategy
and export settings are unchanged. See
[`k8s-sandbox-gvisor.json`](../examples/runconfig/k8s-sandbox-gvisor.json)
and [`executors/k8s-agent-sandbox.md`](executors/k8s-agent-sandbox.md).
