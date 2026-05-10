# Eval framework

Stirrup ships with a deterministic evaluation framework for measuring
harness behaviour and catching regressions. It lives in the `eval/`
module and is built as a separate binary (`stirrup-eval`) so it can be
run independently of the harness in CI or against a production trace
store.

The framework answers four operational questions:

1. **Did this change break anything?** — run a fixed suite of tasks
   before and after a change, compare the two results, fail CI on
   regressions.
2. **What is production actually doing?** — read aggregate metrics
   (pass rate, mean turns, p50/p95 duration) from a trace lakehouse so
   experiments have a real-world baseline to compare against.
3. **Are we drifting?** — diff metrics between adjacent time windows
   and flag significant changes (pass rate drop, turn-count inflation).
4. **What should we add to the suite?** — mine non-success runs out of
   the lakehouse and turn them into eval tasks.

---

## Building

```bash
go build -o stirrup-eval ./eval/cmd/eval
```

The binary is also produced by `just build` alongside `stirrup`.

For live runs (`eval run` without `--dry-run`) the eval binary shells
out to a `stirrup` harness binary, so build that too:

```bash
go build -o stirrup ./harness/cmd/stirrup
```

---

## Concepts

### Eval suite

An `EvalSuite` describes a collection of tasks with reproducible
starting states and outcome judges (`types/eval.go::EvalSuite`).
Suites are authored in HCLv2.

```hcl
suite "fix-nil-check-regressions" {
  description = "Tasks mined from production nil-pointer fixes"

  task "task-001" {
    description = "Fix the nil deref in pkg/foo/bar.go"
    repo        = "https://github.com/example/repo"
    ref         = "abc123"
    mode        = "execution"
    prompt      = <<-EOT
      The test in bar_test.go is failing with a nil pointer. Fix it.
    EOT

    judge {
      type    = "test-command"
      command = "go test ./pkg/foo/..."
    }
  }
}
```

Composite judges nest `judge` blocks recursively rather than using a
list expression, so the grammar stays homogeneous:

```hcl
judge {
  type    = "composite"
  require = "all"

  judge {
    type  = "file-exists"
    paths = ["brief.md"]
  }

  judge {
    type    = "file-contains"
    path    = "brief.md"
    pattern = "(?i)token"
  }
}
```

Each task gets a fresh temporary workspace. If `repo` and `ref` are
set the runner clones the repo at that ref before invoking the
harness. Tasks currently execute sequentially even when
`--concurrency` is passed (`eval/runner/runner.go:31`).

Run output artifacts (`result.json`, the per-task JSON written by
`eval run`, etc.) are JSON — a separate format used for
machine-readable results, not for authoring suites. Mined suites from
`mine-failures` are emitted as HCL so they can be loaded by `eval run`
without conversion.

Top-level blocks other than `suite` (e.g. `variable`, `locals`,
`for_each`) are rejected. Authors who need parameterisation should
generate suites from a higher-level tool and emit the static HCL.

Suite definitions live in `eval/suites/`. CI baselines live in
`eval/baselines/`.

### Judges

A judge decides whether a task passed by inspecting the workspace
after the harness has run (`eval/judge/judge.go`):

| Judge type      | What it checks                                                        |
|-----------------|-----------------------------------------------------------------------|
| `test-command`  | Runs a shell command in the workspace; passes on exit code 0. 5 min timeout. |
| `file-exists`   | At least one of `paths` exists.                                       |
| `file-contains` | `path` exists and matches the regex in `pattern`.                     |
| `composite`     | Combines child `judges` with `require: "all"` or `require: "any"`.    |

All workspace-relative paths go through symlink-aware containment so
judges cannot escape the workspace.

### Replay doubles

Eval is designed to be reproducible without hitting a model provider.
Two replay doubles power this:

- **`ReplayProvider`** (`harness/internal/provider/replay.go`)
  re-emits recorded `TurnRecord.ModelOutput` entries as stream events.
  No API calls; thread-safe atomic turn counter.
- **`ReplayExecutor`** (`harness/internal/executor/replay.go`) replays
  recorded tool outputs keyed by `(toolName, canonicalInput)` and
  tracks writes via `Writes()` so judges can assert what the harness
  *would have* done.

These let CI run eval suites deterministically against recorded
traces, and let the `replay` runner re-evaluate old recordings under
new judge criteria without re-running the harness
(`eval/runner/replay.go`).

### Trace lakehouse

The `TraceLakehouse` interface (`types/lakehouse.go`) abstracts
storage and querying of production run data. A file-backed
implementation (`eval/lakehouse/filestore.go`) ships for dev and CI;
cloud-backed adapters are tracked under the `lakehouse` label in
GitHub Issues.

The lakehouse is what `baseline`, `mine-failures`, `drift`, and
`compare-to-production` read from. It supports filtering by time
range, outcome, mode, and model, and computes aggregate metrics
including p50/p95 duration percentiles.

---

## Subcommands

```text
stirrup-eval <command> [options]
```

### `run` — execute a suite

```bash
./stirrup-eval run \
  --suite eval/suites/regression.hcl \
  --output results/ \
  --harness ./stirrup
```

Loads the suite (`.hcl` extension required), creates per-task
temp workspaces (cloning `repo` at `ref` when set), invokes the harness
binary as a subprocess, parses the JSONL trace it emits, and applies
each task's judge to the workspace. Writes a `result.json`
(`eval.SuiteResult`) into `--output`. Errors per-task are captured in
`TaskResult.Error` without halting the suite.

| Flag             | Default          | Description                                                  |
|------------------|------------------|--------------------------------------------------------------|
| `--suite`        | required         | Path to `EvalSuite` HCL file (`.hcl`).                       |
| `--output`       | current dir      | Directory for `result.json` and per-task artifacts.          |
| `--harness`      | `stirrup` on PATH| Harness binary to invoke for live runs.                      |
| `--concurrency`  | `1`              | Requested parallelism. Honoured as `1` until concurrency lands. |
| `--dry-run`      | `false`          | Validate the suite and emit a synthetic all-pass result.     |

Exit code is `0` regardless of pass rate — use `compare` to gate CI.

### `compare` — diff two results

```bash
./stirrup-eval compare \
  --current results/result.json \
  --baseline eval/baselines/regression.json
```

Diffs two `SuiteResult` files. Detects regressions
(`pass → fail/error`) and improvements (`fail/error → pass`),
computes per-task turn deltas from `RunTrace`, prints a text report,
and exits **`1` if any regressions are present**. This is the gate
the `eval-gate` CI job uses.

### `baseline` — pull production metrics

```bash
./stirrup-eval baseline \
  --lakehouse var/lakehouse \
  --after 2026-03-01 \
  --mode execution \
  --output baselines/production.json
```

Reads aggregate metrics (`types.TraceMetrics`) from a lakehouse,
optionally filtered by time range, mode, and model. Writes JSON to
`--output` if set and prints a summary (count, pass rate, mean
turns, p50/p95 duration) to stdout. Use this to seed an experiment
baseline from real production data instead of static fixtures.

### `mine-failures` — turn production failures into tasks

```bash
./stirrup-eval mine-failures \
  --lakehouse var/lakehouse \
  --limit 20 \
  --output eval/suites/mined.hcl
```

Queries non-success recordings from the lakehouse and constructs an
`EvalSuite` from them, defaulting each task to a `test-command` judge
running `go test ./...`. The resulting suite is written to `--output`
as canonical HCL (parseable by `eval run` directly) and is a starting
point — judges and prompts typically need editing before the suite is
committed.

### `drift` — compare adjacent time windows

```bash
./stirrup-eval drift \
  --lakehouse var/lakehouse \
  --window 7d \
  --compare-window 7d \
  --mode execution
```

Computes metrics for the last `--window` and compares them to the
preceding `--compare-window` (defaults to `--window`). Prints a table
of pass rate, mean turns, and p50/p95 duration for both windows plus
deltas. Exits **`1` if either threshold trips**:

- pass rate dropped more than 5 percentage points, or
- mean turns increased more than 20%.

`--window` accepts Go durations (`24h`, `30m`) or a `Nd` suffix for
days (`7d`).

### `compare-to-production` — lab vs. production

```bash
./stirrup-eval compare-to-production \
  --results results/result.json \
  --lakehouse var/lakehouse \
  --after 2026-03-01 \
  --experiment-id exp-nil-fixes
```

Loads an eval `SuiteResult` and production metrics from the
lakehouse, builds a `LabVsProductionReport`, and prints a side-by-side
table of pass rate and turns. Useful for sanity-checking that an eval
suite's results track production behaviour rather than testing a
distorted slice of tasks.

---

## CI integration

The repo's GitHub Actions workflow at `.github/workflows/ci.yml` runs
the framework as a gating job:

- **`verify`** — `go test` across `types/`, `harness/`, and `eval/`,
  plus binary builds. Runs on every push and PR.
- **`eval-gate`** — depends on `verify`. On `main` pushes it builds
  the binaries, runs every suite in `eval/suites/`, compares each
  result to the matching baseline in `eval/baselines/` via
  `eval compare`, and uploads the result JSON as a build artifact.
- **`publish-container`** — depends on `verify`. On `main` pushes it
  publishes the harness Docker image to `ghcr.io/rxbynerd/stirrup`.

A non-zero exit from `compare` (regressions present) fails the gate
and blocks the container publish.

---

## Typical workflows

### Adding a regression suite to CI

1. Author an `EvalSuite` HCL file under `eval/suites/` (e.g.
   `eval/suites/<name>.hcl`).
2. Run it once with `eval run` and capture `result.json` as the
   baseline at `eval/baselines/<name>.json`.
3. On subsequent CI runs, `eval-gate` runs the suite and compares to
   the committed baseline. PRs that introduce regressions fail.
4. When a behaviour change is intentional, regenerate the baseline
   and commit it as part of the PR.

### Continuous quality monitoring

Point a production deployment at a `TraceLakehouse` (file store for
now). Schedule:

- `eval drift --window 7d` daily — page on threshold breach.
- `eval baseline` weekly — commit refreshed baselines so eval
  suites track production reality.
- `eval mine-failures` on demand — when a class of failure shows up
  in production, mine recent recordings into a new suite to lock in
  the regression test.

### Iterating on a judge

`eval/runner/replay.go` re-evaluates recorded runs through judges
without re-running the harness. This is the fast loop for iterating
on judge criteria — change the regex or composite logic, replay the
recording set, see whether outcomes match expectations. Pair with
`compare` to diff judge changes against a baseline.

---

## Roadmap

Active work tracked in GitHub Issues under the `eval` label:

- **Concurrent task execution** — `--concurrency` is currently
  accepted but ignored; the runner is sequential.
- **Cloud-backed lakehouse adapters** — interface is stable; cloud
  adapters depend on control plane storage choices.
- **A first mined suite** — CI infrastructure is ready; the
  `eval/suites/` and `eval/baselines/` directories are seeded by
  mining production traces from a real repo.
