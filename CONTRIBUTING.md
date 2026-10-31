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
- `harness/` — the harness binary and its 12 components.
- `eval/` — the eval CLI, judges, runner, and lakehouse adapters.
- `examples/controlplane/` — a minimal control plane that drives
  `stirrup job` over gRPC, used by the control-plane smoke tests.

The `harness/internal/*` packages are not part of the public API. The
embedding surface is `harness/harnessapi/`.

## Build, test, lint

```sh
just check        # before pushing: build every module, test, lint
just              # build + test
just build        # binaries: ./stirrup and ./stirrup-eval
just build-modules # compile every module in go.work
just test         # go test over harness, types, eval, examples/controlplane
just lint         # golangci-lint v2
just fuzz-scrub   # short fuzz runs of the secret scrubber
just vuln         # govulncheck across every workspace module
just proto        # buf generate
just buf-lint     # buf lint
just docker       # docker build -t stirrup .
just clean        # remove built binaries
just op-session   # load live-probe keys from 1Password into a tmux session
just op-session-check # report which of those keys are present
```

`just check` is the one command to run before pushing. The
repository root is not a Go module, so `go build ./...` there fails
with "directory prefix . does not contain modules listed in go.work".
Instructions and pull requests name `just check` (or `just
build-modules` for the build alone) instead.

On every push, CI runs the equivalent of `just test`, a race-detector
pass, `just fuzz-scrub` and a full build through the reusable
`_verify.yml` workflow. It then fails if the build or tests changed
`go.work.sum` or any committed `go.sum`, so checksum changes are
committed with the change that caused them. Dependabot updates each
module's `go.sum` but not `go.work.sum`, so the
[`dependabot-checksums.yml`](.github/workflows/dependabot-checksums.yml)
workflow regenerates the checksum files on each Dependabot Go update
branch, commits them there, and dispatches the verify and lint
workflows against that commit. An `eval-gate` job runs
the baselined eval suites against committed baselines, pinned to a
cheap model, and every merge to `main` publishes a container. A failing
`compare` exit fails the eval gate.
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
- Run `just check` locally before pushing. The
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

A `RunConfig` field ships with its proto mirror and a line in
`harness/internal/transport/grpc_translate.go`;
`TestRunConfigProtoMirror_EveryFieldIsMirroredAndTranslated` in
`harness/internal/invariants` fails when either is missing. A field
deliberately kept off the wire goes on that test's `configOnlyFields`
allow-list with a reason, and on the "Features not on the wire" table
in [`docs/integration-guide.md`](docs/integration-guide.md) in the same
change.

**Field-number blocks for parallel work.** When several branches add
proto fields at the same time (a tracker issue split across concurrent
pull requests), the tracker assigns each branch its own block of field
numbers per message before work starts, counting up from the highest
number the message already uses on `main`: with `RunConfig` at 60, for
example, 61-63 for one branch and 64-66 for the next. The tracker
records the assignment. Each branch uses only its block, so the branches
never collide on a field number and merge in any order. Numbers a branch
leaves unused stay reserved for it until the tracker closes.

**Rebase conflicts in `gen/`.** Conflicts inside `gen/` are mechanical:
take either side of the conflicting generated files, resolve any
conflict in `proto/`, then run `just proto` and stage the regenerated
`gen/`. Never hand-merge generated code.

CI rebuilds the descriptor set from `proto/` on every run and uploads
it as the `proto-descriptor-set` artifact, so a `.proto` edit that does
not compile fails the verify gate regardless of whether `gen/` was
refreshed. The pinned `buf` version lives in
[`.github/workflows/_verify.yml`](.github/workflows/_verify.yml);
bump it there alongside any local toolchain bump.

## Live provider keys

Live probes and opt-in eval suites read provider keys from the
environment. `just op-session` loads them from 1Password into a tmux
session once, so several commands (or several agents) share one
1Password approval per key:

```sh
just op-session stirrup-keys \
  'ANTHROPIC_API_KEY=op://<vault>/<item>/credential' \
  'OPENAI_API_KEY=op://<vault>/<item>/credential' \
  'OPENROUTER_API_KEY=op://<vault>/<item>/credential'
```

Each argument is `NAME=op://...`; vault paths are never stored in the
repository. A path may contain only letters, digits, spaces and
`. _ / ? = -`, the characters 1Password allows in names (other names
are referenced by ID). The recipe creates the session if it does not
exist, reads each secret inside it, exports the variable only when the
read returned a non-empty value, and prints `KEY_OK NAME` or
`KEY_MISSING NAME` for each key. It waits up to 180 seconds for the
1Password approvals and exits non-zero when any key is missing,
including a prompt that timed out; a key approved after the wait is
exported but reported missing, so confirm it with `op-session-check`.
A value is never printed or written to disk. The recipes need `op`,
`tmux` and a bash- or zsh-compatible shell in the session.

Before reporting that a live probe ran, confirm the key it needed is
still present:

```sh
just op-session-check stirrup-keys ANTHROPIC_API_KEY
```

Run probes in the session with
`tmux send-keys -t '=stirrup-keys:' '<command> > <log> 2>&1' Enter` and
read the log file. Never `env`-dump or echo a key in that session.
Both recipes type commands into the session's active pane, so run them
while it is idle. End the session with
`tmux kill-session -t '=stirrup-keys'` when the probes are done.

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
