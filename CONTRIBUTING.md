# Contributing to stirrup

Thanks for taking an interest. This document covers what you need to
build, test, and submit changes. For project orientation start with the
[README](README.md); for architectural depth see
[`docs/architecture.md`](docs/architecture.md) and per-package detail
in [`AGENTS.md`](AGENTS.md).

## Environment

- **Go 1.27.3+.** Workspace minimum is `1.27.3` (`go.work` and every
  module's `go.mod`); the Dockerfile pins `golang:1.27.3-alpine` for
  reproducible CI builds. Anything older than 1.27.3 will fail to
  compile. The `go` directive is kept in lockstep with the
  Dockerfile's base image version so the `govulncheck` CI gate scans
  the same standard-library toolchain the release image ships.
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

The repository is a Go workspace (`go.work`) composed of five modules:

- `types/` — shared types, `RunConfig` validation, and the `version`
  package. Zero external dependencies.
- `gen/` — generated code from `proto/harness/v1/`. Edit the `.proto`,
  not the generated Go.
- `harness/` — the harness binary and its 13 components.
- `eval/` — the eval CLI, judges, runner, and lakehouse adapters.
- `examples/controlplane/` — a runnable minimal control plane with its
  own `go.mod`, tests and smoke script (`just controlplane-smoke`). It
  is a separate module, so `go test` inside `harness/` does not run its
  tests; `just test` and `just lint` include it.

A new workspace member needs a `COPY <module>/go.mod ./<module>/go.mod`
line in the [`Dockerfile`](Dockerfile) builder stage, next to the one
for `examples/controlplane`. `go.work` lists every member, and the image
build fails to load the workspace when a listed module's `go.mod` is
missing.

The `harness/internal/*` packages are not part of the public API. The
embedding surface is `harness/harnessapi/`.

## Build, test, lint

```sh
just              # build + test
just build        # binaries: ./stirrup and ./stirrup-eval
just test         # go test ./harness/... ./types/... ./eval/... ./examples/controlplane/...
just lint         # golangci-lint v2
just fuzz-scrub   # short fuzz runs of the secret scrubber
just vuln         # govulncheck across all five workspace modules
just proto        # buf generate
just buf-lint     # buf lint
just docker       # docker build -t stirrup .
just clean        # remove built binaries
```

Or directly:

```sh
go test ./harness/... ./types/... ./eval/... ./examples/controlplane/...
go build ./harness/... ./types/... ./eval/...
buf generate
buf lint
```

CI runs the equivalent of `just test` + `just test-race` +
`just fuzz-scrub` + a full build via the reusable `_verify.yml` workflow
on every push. The CI race pass omits `harness/internal/sandboxidentity`,
which `just test-race` includes. Pushes to `main` and manual
dispatches also run an `eval-gate` job (baselined eval suites against
committed baselines, pinned to a cheap model), and every merge to `main`
publishes containers. A failing `compare` exit fails the eval gate.
See [`.github/workflows/`](.github/workflows/) for the full pipeline.

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

## Writing tests

Tests are read far more often than they are written, and a failing
test is usually read by someone who did not write it. Optimise for a
reader with no context.

- **Assert with testify through a per-scope object.** Every function
  or closure that asserts starts with `must := require.New(t)` (and
  `check := assert.New(t)` where a soft assertion is wanted) and then
  asserts as `must.Equal(want, got)`. The object captures the `t` it
  was built from, so each `t.Run` closure builds its own `must` from
  the subtest's `t`; reusing the parent's object inside a subtest
  fails the parent instead and no linter catches it. The names are
  fixed as `must` and `check` across the suite. Pick the most specific
  form (`NoError`, `ErrorIs`, `ErrorContains`, `Len`, `Empty`,
  `Contains`, `ElementsMatch`, `EqualValues`, `JSONEq`,
  `WithinDuration`) rather than `True` over a hand-written comparison.
  Hand-rolled `if got != want { t.Fatalf(...) }` blocks and
  `reflect.DeepEqual` are not accepted in new tests. `testifylint` runs
  under `just lint` and rejects swapped arguments, `Equal` on errors,
  and the other common misuses in both the object and package styles.
- **`require` stops the test; keep it on the test goroutine.** Inside
  `go func` bodies, `httptest` handlers and any other callback that may
  run off the test goroutine, use `check` or hand the failure back over
  a channel. `must` there panics instead of failing the test. Inside
  `must.EventuallyWithT` the condition asserts through the `CollectT`
  it receives (`require.NoError(c, err)`), never through the outer
  `must`.
- **Name each test so it reads on its own.** Test names follow
  `Test<Subject>_<Behaviour>[_<Detail>]`, for example
  `TestRunConfig_Validate_RejectsAllowAllInPlanningMode`. JUnit output
  loses the package, so the name has to carry the subject. Subtest
  names are short lowercase phrases.
- **Prefer subtests over loops with `t.Errorf`.** A table-driven test
  runs each case under `t.Run` with the case's name, uses the subtest's
  own `t` and its own `must`, so the first failure names the case and
  stops there.
- **Helpers build fixtures; assertions stay in the test.** A helper that
  only wraps an assertion hides what the test checks. Keep helpers for
  setup and teardown, mark them with `t.Helper()`, and use `t.TempDir`,
  `t.Setenv` and `t.Cleanup` instead of manual equivalents.
- **Comments describe the behaviour protected**, not the incident, pull
  request or session that motivated the test.

## Commit conventions

Commit subjects in this repo follow `<area>: <imperative subject>` in
lowercase, where `<area>` is a package or theme. Examples drawn straight
from `git log`:

```text
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
  `eval-gate` job catches behavioural regressions, but it runs only on
  pushes to `main` and on manual dispatch — live eval runs spend real
  tokens, so they are deliberately not per-push. To get the signal on a
  branch before merge, run it on demand:

  ```sh
  gh workflow run ci.yml --ref <branch>
  ```

  Catching regressions locally is cheaper still; see
  [`eval/suites/README.md`](eval/suites/README.md) for the local
  invocation.
- A new page under `docs/` needs an entry in two places: the table for
  its group in [`docs/index.md`](docs/index.md) and the same group's
  `pages` list in [`docs-site.json`](docs-site.json), which the docs
  site builds its navigation from.
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

CI rebuilds the descriptor set from `proto/` on every run and uploads
it as the `proto-descriptor-set` artifact, so a `.proto` edit that does
not compile fails the verify gate regardless of whether `gen/` was
refreshed. The pinned `buf` version lives in
[`.github/workflows/_verify.yml`](.github/workflows/_verify.yml);
bump it there alongside any local toolchain bump.

## Touching the safety rings

If you change anything under `harness/internal/permission/`,
`harness/internal/security/codescanner/`,
`harness/internal/executor/egressproxy/`, or the relevant
`types/runconfig.go` validators, please re-read
[`docs/safety-rings.md`](docs/safety-rings.md) and confirm the operator-facing
behaviour still matches. Updates to `docs/safety-rings.md` should land in the
same PR.

## Reporting security issues

Do not open a public issue for security findings. See
[`SECURITY.md`](SECURITY.md) for the disclosure process.

## License of contributions

By contributing you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE), the same license under which stirrup is
distributed.
