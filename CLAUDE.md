# Working in stirrup

A coding-agent harness. Go workspace with four modules — `types`,
`harness`, `eval`, `gen` — composed via a single `RunConfig`.

This file is for AI assistants editing the codebase. The canonical
docs are:

- [`README.md`](README.md) — project orientation and a tour of a
  secure run.
- [`docs/architecture.md`](docs/architecture.md) — the 13-component
  model, agentic loop, factory, and deep dives.
- [`docs/configuration.md`](docs/configuration.md) — full CLI flag
  reference and `RunConfig` schema.
- [`docs/deployment.md`](docs/deployment.md) — `stirrup job`, gRPC
  contract, container image, releases.
- [`docs/security.md`](docs/security.md) and
  [`docs/safety-rings.md`](docs/safety-rings.md) — security
  foundations and the five operator-configurable rings.
- [`docs/executors/k8s.md`](docs/executors/k8s.md) — the
  Kubernetes (Pod-per-run) executor: architecture, config
  reference, deployment recipes, and egress. Reference manifests
  in [`examples/k8s/`](examples/k8s/); local `kind` cluster in
  [`scripts/dev/`](scripts/dev/).
- [`AGENTS.md`](AGENTS.md) — per-package map of `harness/internal/*`.
- [`CONTRIBUTING.md`](CONTRIBUTING.md) — build, test, lint,
  commit/PR conventions.

Read these before duplicating their content here.

## Operational guardrails

These are project-specific behaviours to apply on top of the
default Claude Code conventions.

### Build, test, lint

```sh
just              # build + test (default)
just build        # ./stirrup and ./stirrup-eval
just test         # go test ./harness/... ./types/... ./eval/...
just lint         # golangci-lint v2
just proto        # buf generate (after editing proto/*.proto)
```

A failing build or test is a real error. A failing `gopls`
diagnostic against a workspace package (e.g. "X not declared by
package Y") is almost always a false positive caused by `go.work`
resolution. Always verify with `go build` and `go test` before
acting on an LSP error.

### Lint policy

`golangci-lint v2` is the source of truth, but linter suggestions
are not mandates. Diagnostics prefixed `QF` (quick-fix), `S`
(simplification), or `SA` (static analysis suggestion) sometimes
conflict with deliberate patterns: compile-time type assertions
(`var _ T = expr`), interface satisfaction guards
(`var _ Interface = (*Impl)(nil)`), defensive coding around hostile
inputs, sentinel values for forward compatibility. Prefer
`//nolint:<linter> // <reason>` when it preserves intent better
than the suggested rewrite. Never weaken a safety mechanism to
satisfy a linter. Treat `golangci-lint --fix` and `fmt` output as
drafts and review the diff for semantic changes.

### Commit conventions

Subjects are `<area>: <imperative subject>` in lowercase. Comma
separate when multiple areas share a change. Body explains *why*,
not *what* — the diff already shows the *what*. Examples drawn
from `git log`:

```
core: surface provider errors via slog and transport warning
permission: bound Cedar value recursion depth
docs: describe release workflow and version-label conventions
```

Branch names follow `<type>/<short-description>`
(`feat/issue-42-...`, `fix/surface-provider-errors`,
`docs/post-issue-42-readme-pass`).

### Documentation tone

Docs use an **impersonal, instructional voice**. Avoid second-person
("you", "your") where it can be removed without loss of meaning.
Prefer "the harness" over "you", "operators" or "callers" over
"you", "the run" over "your run". This matches the
`doc-tone-reviewer` agent's convention and keeps prose stylistically
consistent across docs.

## Project-specific invariants

These are structurally enforced by the codebase. Do not weaken them
to make a feature easier:

- **Secrets never in `RunConfig`.** API keys are `secret://`
  references resolved at runtime through `security.SecretStore`.
  `RunConfig.Redact()` strips secret references before any trace
  or recording is persisted. Adding a "raw key" path is a
  regression.
- **`http.DefaultClient` is banned in production code.** Every
  HTTP client must declare an explicit timeout (120 s for
  streaming, 30 s for MCP / web fetch). The pattern in
  `provider/anthropic.go` is the template.
- **Read-only modes** (`planning`, `review`, `research`, `toil`)
  enforce a hard invariant in `ValidateRunConfig`: the tool list
  excludes `write_file` / `run_command` / `edit_file`, and the
  permission policy is not `allow-all`. A new mode added to the
  list inherits the invariant. The CLI defaults `--mode` to
  `planning` so a bare invocation is safe-by-default; `execution`
  is the explicit opt-in for editable runs.
- **Hand-rolled HTTP over SDKs.** The container executor uses the
  Docker Engine REST API directly to avoid the `github.com/docker/docker`
  dependency tree. Provider adapters are stdlib HTTP+SSE for the
  same reason. Adding a vendor SDK requires a justification on
  par with the existing exceptions in
  [`docs/architecture.md#external-dependencies`](docs/architecture.md#external-dependencies).
- **The agentic loop is a pure function of its interfaces.** The
  loop in `harness/internal/core/loop.go` must not import a
  concrete component implementation, must not read environment
  variables directly, and must not access the filesystem. New
  behaviour goes behind an interface and is injected by the
  factory.
- **`harness/internal/*` is private.** The public Go API surface is
  `harness/harnessapi/`. Don't expand the embedding API
  unintentionally by exporting types from `internal/`.

## Where things live

Quick map for "I need to change X" lookups:

| Want to change… | Look in… |
|---|---|
| Provider behaviour or wire format | `harness/internal/provider/<name>.go` |
| System prompt content or model tiers (the `.md` files are Go text/templates) | `harness/internal/prompt/systemprompts/*.md`, `harness/internal/prompt/modeltier.go` (operator doc: `docs/configuration.md#system-prompt-templating`) |
| Tool definition or schema | `harness/internal/tool/builtins/<name>.go` |
| Edit fallback logic | `harness/internal/edit/multi.go` |
| Lifecycle hooks (pre/post-run exec, #461) | `harness/internal/hook/` (Runner/Noop/ExecRunner), loop wiring in `harness/internal/core/loop.go` (operator doc: `docs/configuration.md#lifecycle-hooks`) |
| Permission gating logic | `harness/internal/permission/<type>.go` |
| Cedar policy semantics | `harness/internal/permission/policyengine.go` |
| Rule-of-Two runtime classifier (detector / monitor+latch / gate) | `harness/internal/security/sensitivepatterns.go`, `harness/internal/ruleoftwo/`, `harness/internal/permission/ruleoftwogate.go` (arming in `core/factory.go`; operator doc: `docs/safety-rings.md`) |
| Container runtime / network mode wiring | `harness/internal/executor/container*.go` |
| K8s executor (Pod-per-run) + egress NetworkPolicy | `harness/internal/executor/k8s.go`, `k8s_netpol.go` (operator doc: `docs/executors/k8s.md`) |
| `k8s-sandbox` executor (Agent Sandbox CRD: `agents.x-k8s.io/v1alpha1` Sandbox → controller-created Pod) | `harness/internal/executor/agentsandbox.go`, `agentsandbox_api.go` (operator doc: `docs/executors/k8s-agent-sandbox.md`) |
| `--k8s-*` CLI flags (`--k8s-namespace`, `--k8s-kubeconfig`, `--k8s-node-selector`, `--k8s-service-account`, `--k8s-egress-proxy-url`; serve both `k8s` and `k8s-sandbox`) | `harness/cmd/stirrup/cmd/runconfigflags.go` (defs), `harness.go` (mapping) |
| `stirrup egress-proxy` subcommand | `harness/cmd/stirrup/cmd/egress_proxy.go` |
| Egress proxy | `harness/internal/executor/egressproxy/` |
| Code scanner | `harness/internal/security/codescanner/` |
| Result sink (RunResult payload) | `harness/internal/resultsink/` |
| Workspace GCS export | `harness/internal/workspaceexport/` |
| `RunConfig` validation | `types/runconfig.go` |
| CLI flag definitions (shared between `harness` and `run-config`) | `harness/cmd/stirrup/cmd/runconfigflags.go` |
| CLI flag definitions (`harness`-only behaviour: `--export-workspace-required`, `--output-runconfig`) | `harness/cmd/stirrup/cmd/harness.go` |
| Resolved-config builder (file/stdin/flags → RunConfig) | `harness/cmd/stirrup/cmd/runconfigbuilder.go` |
| `stirrup run-config` subcommand | `harness/cmd/stirrup/cmd/runconfig.go` |
| gRPC schema | `proto/harness/v1/harness.proto` (then `buf generate`) |
| Eval suite parser | `eval/spec/` |
| Eval CLI subcommands | `eval/cmd/eval/main.go` |

Stirrup runs unmodified as a Google Cloud Run job — the container
contract (no port, exit-on-completion, SIGTERM-then-SIGKILL) matches
the existing harness binary's shape and the
`gcp-workload-identity` credential source talks to Cloud Run's GCE
metadata server transparently. The result sink (`resultSink.type`)
and the GCS trace emitter / workspace exporter are the
result-collection surfaces designed for serverless targets where
there is no native output channel. Operator walkthrough at
[`docs/cloud-run-jobs.md`](docs/cloud-run-jobs.md).

## Things not to do

- **Don't duplicate doc content.** If something belongs in a doc
  under `docs/`, edit that doc and link to it from here. CLAUDE.md
  is an index and a guardrail, not a knowledge base.
- **Don't reintroduce backwards-compat shims** for removed concepts.
  The project is pre-1.0; clean is preferred.
- **Don't generate cosmetic comments.** Default to no comments.
  Only write a comment when *why* is non-obvious — a hidden
  constraint, a workaround for a specific bug, behaviour that
  would surprise the next reader. Comments that describe *what*
  the code already says are noise.
- **Don't claim a UI / CLI feature is done from a passing build.**
  Type-check passes ≠ feature works. If the change is observable,
  drive it end-to-end before reporting completion.
- **Don't act on an issue's `file:line` citations without
  re-verifying.** The backlog tracks deferred work in detail
  ("Deferred from #NNN … becomes load-bearing when …"), so issue
  bodies and their line references drift as the code moves around
  them. Before creating a task or implementing from an issue,
  confirm each cited symbol/line against current `main`: issues are
  routinely already-resolved or refactored past, and the closing
  PR is often discoverable via `git log -L` or `git blame` on the
  cited lines. Cross-reference that PR before assuming work remains.

## Known false positives

- `gopls` reporting "X not declared by package Y" across module
  boundaries (`gen/` ↔ `types/` ↔ `harness/`) is almost always a
  workspace-resolution artefact. Verify with `go build`.
- Lint suggestions to replace `var _ Interface = (*Impl)(nil)` with
  the concrete value are wrong — the assertion form is the
  intentional compile-time satisfaction guard.

