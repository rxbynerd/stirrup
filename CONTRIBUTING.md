# Contributing to stirrup

Thanks for taking an interest. This document covers what you need to
build, test, and submit changes. For project orientation start with the
[README](README.md); for architectural depth see
[`VERSION1.md`](VERSION1.md) and [`AGENTS.md`](AGENTS.md).

## Environment

- **Go 1.26.2+.** The Dockerfile builds against `golang:1.26.2-alpine`,
  so anything older may produce binaries that diverge from CI.
- **[just](https://github.com/casey/just)** for the convenience targets
  in the [`Justfile`](Justfile). Optional but used throughout this doc.
- **[Buf](https://buf.build)** if you touch any `.proto` files.
  Generated Go code lives in `gen/`, a separate Go module, and must be
  regenerated whenever the proto schema changes.
- **`golangci-lint` v2** if you want to reproduce CI's lint job locally.
- **Docker or Podman** if you intend to exercise the container executor
  or run the published image. Both are supported via the same Engine
  REST API.

## Workspace layout

The repository is a Go workspace (`go.work`) composed of four modules:

- `types/` — shared types, `RunConfig` validation, and the `version`
  package. Zero external dependencies.
- `gen/` — generated code from `proto/harness/v1/`. Edit the `.proto`,
  not the generated Go.
- `harness/` — the harness binary and its 12 components.
- `eval/` — the eval CLI, judges, runner, and lakehouse adapters.

The `harness/internal/*` packages are not part of the public API. The
embedding surface is `harness/harnessapi/`.

## Build, test, lint

```sh
just              # build + test
just build        # binaries: ./stirrup and ./stirrup-eval
just test         # go test ./harness/... ./types/... ./eval/...
just lint         # golangci-lint v2
just proto        # buf generate
just buf-lint     # buf lint
just docker       # docker build -t stirrup .
just clean        # remove built binaries
```

Or directly:

```sh
go test ./harness/... ./types/... ./eval/...
go build ./harness/... ./types/... ./eval/...
buf generate
buf lint
```

CI runs the equivalent of `just test` + a full build via the reusable
`_verify.yml` workflow on every push, plus an `eval-gate` job (eval
suites against committed baselines) and a container publish on every
merge to `main`. A failing `compare` exit blocks the publish. See
[`.github/workflows/`](.github/workflows/) for the full pipeline.

### gopls false positives

`gopls` regularly reports diagnostics referencing packages from other
workspace modules (e.g. `NewComposedPromptBuilder not declared by
package prompt`, or fields from `gen/`). These are almost always
spurious. Always verify with `go build` and `go test` before acting on
an LSP error.

## Lint policy

`.golangci.yml` runs `golangci-lint v2`. Suggestions matter, but a
linter is not the architect — when resolving findings, understand the
code's intent before changing it:

- **Linter suggestions are not mandates.** Diagnostics prefixed `QF`
  (quick-fix), `S` (simplification), or `SA` (static analysis suggestion)
  may conflict with deliberate patterns: compile-time type assertions,
  defensive coding around hostile inputs, or sentinel values that exist
  for forward compatibility. If `//nolint:<linter> // <reason>`
  preserves intent better than the suggested rewrite, prefer the nolint.
- **Never weaken a safety mechanism to satisfy a linter.** Compile-time
  type checks (`var _ T = expr`), interface satisfaction guards
  (`var _ Interface = (*Impl)(nil)`), and deliberate panics in
  unreachable branches exist for a reason. Suppress and document.
- **Treat auto-fix output as a draft.** `golangci-lint fmt` and `--fix`
  rewrite mechanically. Review the diff for semantic changes,
  particularly in test assertions.
- **Check for cascading breakage.** Removing an "unused" symbol may
  break a compile-time contract or a helper reserved for an in-progress
  branch. Grep for the symbol and read surrounding comments before
  deleting.

## Commit conventions

Commit subjects in this repo follow `<area>: <imperative subject>` in
lowercase, where `<area>` is a package or theme. Examples drawn straight
from `git log`:

```
core: surface provider errors via slog and transport warning
ci: SHA-pin third-party release actions (M-2)
permission: bound Cedar value recursion depth (M5, refs #42)
docs: describe release workflow and version-label conventions
provider, types: document Responses translation order and StopReason vocabulary
```

Notes:

- Multiple areas can share a subject — comma-separate them in the order
  they appear in the diff.
- Sub-task identifiers (`M-2`, `B5`) and `refs #N` cross-references are
  conventional and welcome but not required.
- Reserve `fix:` for bug fixes that don't cleanly belong to one area.
- The body should explain the *why* — what constraint or incident the
  change is responding to. The diff already shows the *what*.

Keep commits logical and reviewable. Squash WIP merges before opening a
PR.

## Pull requests

- Branch off `main`. Branch names follow `<type>/<short-description>`
  (`feat/issue-42-...`, `fix/surface-provider-errors`,
  `docs/post-issue-42-readme-pass`).
- Reference any related issue in the PR description (`refs #42`,
  `closes #62`).
- Run `just lint` and `just test` locally before pushing. The
  `eval-gate` job will catch behavioural regressions on `main`, but
  catching them pre-merge is cheaper.
- Keep PRs focused. The exception is the safety-ring style multi-step
  PR that ships an opt-in feature in one go — those are fine but
  benefit from clear sub-task labels in commit subjects.

## Touching protobuf

Edit `proto/harness/v1/*.proto`, then regenerate:

```sh
buf lint
buf generate
```

The generated code is committed (`gen/` is a tracked module). Keep
proto changes backwards-compatible where possible; `buf` will refuse to
break wire compatibility on a tagged release without explicit override.

## Touching the safety rings

If you change anything under `harness/internal/permission/`,
`harness/internal/security/codescanner/`,
`harness/internal/executor/egressproxy/`, or the relevant
`types/runconfig.go` validators, please re-read
[`docs/sandbox.md`](docs/sandbox.md) and confirm the operator-facing
behaviour still matches. Updates to `docs/sandbox.md` should land in the
same PR.

## Reporting security issues

Do not open a public issue for security findings. See
[`SECURITY.md`](SECURITY.md) for the disclosure process.

## License of contributions

By contributing you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE), the same license under which stirrup is
distributed.
