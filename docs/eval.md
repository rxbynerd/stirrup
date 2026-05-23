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
harness. With `--concurrency > 1`, the runner dispatches tasks
across a bounded worker pool while preserving suite order in
`result.json`; each task still gets its own workspace, harness
subprocess, and trace file (`eval/runner/runner.go::runTasksConcurrently`).

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

### Suite-level RunConfig surface

A suite can pin the harness configuration it expects to run against,
so the regression scenario it describes cannot be silently nullified
by the operator's environment. Three authoring constructs cover the
suite → task layering:

| Construct | Scope | Shape | Mutual exclusion |
|---|---|---|---|
| `run_config_file = "path.json"` | suite | Path to a `RunConfig` JSON file matching what `stirrup harness --config` already consumes. | Cannot coexist with inline `run_config`. |
| `run_config { ... }` | suite | Inline `RunConfig` baseline. | Cannot coexist with `run_config_file`. |
| `run_config_overrides { ... }` | per task | Sparse overlay applied on top of the suite baseline. | n/a |

Per-task `run_config_overrides` follows the existing
`types.RunConfigOverrides` shape and currently surfaces `provider`,
`model_router`, `context_strategy`, `edit_strategy`, `verifier`,
and `max_turns`. Only fields explicitly set on the overlay take
effect; everything else passes the baseline through unchanged.

Per-task mode is set via the task block's `mode` attribute, not
`run_config_overrides`. The runner always passes the task's mode
as the harness's `--mode` flag, which would otherwise silently
override anything written in the overlay; the HCL surface omits
`mode` from `run_config_overrides` so the conflict cannot arise.
A suite-level `run_config { mode = "..." }` is still effective —
it rides in the merged config file and is honoured by the
harness unless the task itself sets a `mode` attribute.

```hcl
suite "openai-responses-empty-tool-output-regression" {
  description = "..."

  run_config {
    provider {
      type        = "openai-responses"
      api_key_ref = "secret://OPENAI_KEY"
    }

    model_router {
      type     = "static"
      provider = "openai-responses"
      model    = "gpt-5.4-nano"
    }
  }

  task "empty-stdout-run-command-completes" {
    description = "..."
    prompt      = "..."

    # Optional sparse overlay (not used by this regression task).
    # run_config_overrides {
    #   max_turns = 4
    # }

    judge { ... }
  }
}
```

The live example is at
[`eval/suites/openai-responses-empty-tool-output.hcl`](../eval/suites/openai-responses-empty-tool-output.hcl).

**Precedence.** When a merged config is in use the runner passes
only the flags it actually needs to manage:

- `--workspace` — always passed (the per-task tmpdir has no
  in-config equivalent the suite could supply).
- `--trace` — not passed; the trace path is injected into the
  merged config's `trace_emitter.file_path` so the harness picks
  it up without triggering the flag's emitter-type coercion.
- `--prompt` — passed only when the task has a non-empty `prompt`
  attribute.
- `--mode` — passed only when the task has a non-empty `mode`
  attribute. A suite-level `run_config { mode = "..." }` rides in
  the merged config and is honoured when the task itself does not
  override.
- `--timeout` — not passed; the merged config carries it.

The legacy invocation path (a suite with no `run_config_file` and
no inline `run_config`) keeps passing the historic five flags
verbatim, so existing suites are unchanged.

**`run_config_file` path resolution.** The path stored in
`run_config_file` is used verbatim by the runner; relative paths
resolve against the working directory of the `stirrup-eval`
invocation, not the directory containing the suite file. For a
suite checked into a repository, the recommendation is to use an
absolute path or to invoke the runner from a stable working
directory. Authors who want a suite-relative path can compose one
explicitly in their CI script (e.g.
`stirrup-eval run --suite "$REPO/eval/suites/foo.hcl"` after `cd`
into the repo root).

**Retention.** When `--output` is set, each retained task
directory carries a `run_config.redacted.json` companion next to
`trace.jsonl`, `harness.stdout.txt`, and `harness.stderr.txt`. The
redaction guarantee matches `RunConfig.Redact()`: every
`secret://` reference is rewritten to `secret://[REDACTED]`
before the file lands on disk, so a retained artifact never
carries a resolved secret out of the process. The reference
itself never leaves the suite — only its redacted form is
persisted.

**Dry-run validation.** `stirrup-eval run --dry-run` builds the
merged config for each task and feeds it to
`types.ValidateRunConfig`. Validation errors are surfaced
per task in the resulting `SuiteResult` (outcome `"error"`, the
validator's message in `JudgeVerdict.Reason`) without aborting
the suite; sibling tasks are still validated and reported. A
suite with no `run_config_file` and no inline `run_config`
preserves the legacy dry-run shape: every task is reported as a
synthetic `"pass"` with reason `"dry run — skipped"`.

**Replay-mode caveat.** `ReplayProvider` re-emits recorded
`TurnRecord.ModelOutput` entries and never speaks HTTP. A suite
that pins `provider` under a replay-mode invocation produces a
configuration whose provider field has no effect: the replay
provider is selected ahead of any wire-format adapter. The
provider pin is still useful as documentation of the original
recording's posture, but it does not gate the run.

**Backwards compatibility.** A suite with no `run_config_file`,
no inline `run_config`, and no per-task `run_config_overrides`
behaves exactly as before — the runner falls back to the legacy
invocation (`--prompt`, `--mode`, `--workspace`, `--trace`,
`--timeout`) and writes no `run_config.redacted.json`. The new
fields are purely additive.

**Currently unsupported.** The inline `run_config` block decodes
most of `types.RunConfig` but defers a small number of fields
whose HCL representation is awkward under gohcl's attribute
model. These must be authored via `run_config_file` (which is
parsed as JSON, where they are straightforward) until the parser
grows dedicated handling:

- `providers` (named multi-provider lineup, `map[string]ProviderConfig`)
- `dynamic_context` (`map[string]DynamicContextValue`)
- `guard_rail.custom_criteria` (`map[string]string`)
- `tools.mcp_servers` (slice of structs)
- `transport` (the eval runner is stdio-only)

Setting these via `run_config_file` is the recommended escape
hatch; the suite still benefits from inline `run_config_overrides`
for the supported subset.

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

#### Where recordings come from

`TurnRecord` payloads are produced live by the
`traceEmitter.type=jsonl` emitter in
`harness/internal/trace/jsonl.go`. The emitter streams one
`turn_record` event per agentic-loop turn into the configured trace
file, carrying the full `ModelInput.Messages`, the model's
`ModelOutput` content blocks, and every tool call's raw
`Input`/`Output`. `types/trace.Reader.ReadRecording` reassembles a
`types.RunRecording` from the event stream, including from
partially-written files left behind by an interrupted run.

Pre-streaming traces (single-blob `RunTrace` lines) parse through the
same reader as recordings with no transcript turns; replay against
those files is a degenerate case that succeeds only if the eval suite
asks for zero turns.

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

### Outcome taxonomy

`RunTrace.Outcome` records *why the loop stopped*: `success`,
`error`, `max_turns`, `verification_failed`, `verification_error`,
`budget_exceeded`, `stalled`, `tool_failures`, `cancelled`,
`timeout`, `max_tokens`. By itself it conflates two very different
states in execution mode: "the harness made the correct change"
vs. "the loop exited cleanly with zero useful changes." Metrics
derived from `Outcome == "success"` therefore lie about quality.

`types.EvalOutcome` (`types/evaloutcome.go`) collapses
`(Outcome, VerificationResults)` onto three buckets:

| Termination outcome                                                                       | Verifier ran? | Verdict   | `EvalOutcome` |
|-------------------------------------------------------------------------------------------|---------------|-----------|---------------|
| `success`                                                                                 | yes           | all pass  | `passed`      |
| `success`                                                                                 | yes           | any fail  | `failed`      |
| `success`                                                                                 | no            | n/a       | `passed` *    |
| `verification_failed`, `error`, `tool_failures`                                           | any           | any       | `failed`      |
| `max_turns`, `budget_exceeded`, `timeout`, `max_tokens`, `stalled`, `cancelled`, `verification_error` | any | any | `inconclusive` |
| anything else (unknown / empty)                                                           | any           | any       | `inconclusive` |

\* The success-without-verifier branch is trusted as `passed` for
v0.1 to keep existing baselines stable. Operators who want stricter
fidelity should wire a verifier (even a cheap smoke-test command);
a future opt-in `evalOutcomeQuality: verified | unverified` label
is tracked in #273.

`baseline` and `drift` report `passRate`, `failRate`, and
`inconclusiveRate` — the three rates sum to 1.0 by construction.
`mine-failures` defaults to mining only `EvalOutcome == failed`;
pass `--include-inconclusive` to also mine limit-hit and interrupted
runs.

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

When the suite declares a baseline (`run_config_file` or inline
`run_config`), the runner merges the per-task
`run_config_overrides` overlay, writes the result to a per-task
temp file, and invokes `stirrup harness --config <merged>.json`
alongside the five workspace-scoped flags. The retained artifact
tree gains a `run_config.redacted.json` per task. See
[Suite-level RunConfig surface](#suite-level-runconfig-surface).

| Flag             | Default          | Description                                                  |
|------------------|------------------|--------------------------------------------------------------|
| `--suite`        | required         | Path to `EvalSuite` HCL file (`.hcl`).                       |
| `--output`       | current dir      | Directory for `result.json` and per-task artifacts.          |
| `--harness`      | `stirrup` on PATH| Harness binary to invoke for live runs.                      |
| `--concurrency`  | `1`              | Number of tasks executed in parallel. Workers preserve suite order in `result.json`. Values larger than the task count cap at `len(tasks)`. Concurrent invocations talking to the same provider hit rate limits faster — pick a value that respects your provider account's per-minute caps. |
| `--dry-run`      | `false`          | Validate the suite (and, when present, the merged per-task RunConfig via `ValidateRunConfig`) and emit a synthetic result. |

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
optionally filtered by time range, `--mode`, `--model`, and
`--provider` (e.g. `anthropic`, `openai-responses`, `gemini`).
Writes JSON to `--output` if set and prints a summary (count, pass
rate, mean turns, p50/p95 duration) to stdout. Use this to seed an
experiment baseline from real production data instead of static
fixtures.

### `ingest` — populate a lakehouse from JSONL traces

```bash
./stirrup-eval ingest \
  --lakehouse var/lakehouse \
  --trace tmp/sessions/run-1.jsonl \
  --trace tmp/sessions/run-2.jsonl
```

Reads one or more JSONL trace files (produced by
`stirrup harness --trace ...`) and persists them into a FileStore
lakehouse. Two on-wire shapes are accepted transparently per file:

- **Streaming event format (since #270)** — line-delimited events with
  a `kind` discriminator. One file represents one run; ingest writes
  both `traces/<runId>.json` and `recordings/<runId>.json`. Full
  transcripts are preserved on the recording so replay and
  mine-failures have something to chew on.
- **Legacy single-blob format** — one `RunTrace` per line, no
  discriminator. Each line ingests as one `traces/<id>.json` entry;
  no recording is produced (the legacy shape has no transcript).

Use `--trace -` to read from stdin (single file only). `--trace` is
repeatable; mixed-format invocations are supported (per-file detection).

A streaming trace that ended without a `run_finished` event (an
interrupted run — SIGKILL, OOM) is ingested with
`FinalOutcome.Outcome=="interrupted"` by default so it stays
discoverable to mine-failures and replay. Pass `--skip-partial` to
drop interrupted captures.

Re-ingesting the same file is idempotent (last-write-wins, atomic
rename via #267) so retries do not corrupt the lakehouse.

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

```bash
./stirrup-eval replay \
  --lakehouse var/lakehouse \
  --suite eval/suites/some-suite.hcl \
  --workspace path/to/preserved-workspace \
  --outcome failed \
  --output results/replay.json
```

`stirrup-eval replay` re-evaluates recorded runs through suite
judges without re-running the harness or hitting any provider.
This is the fast loop for iterating on judge criteria — change the
regex or composite logic, replay the recording set, see whether
outcomes match expectations. Pair with `compare` to diff judge
changes against a baseline.

Selection of recordings is either explicit (`--recording <runId>`,
repeatable) or by outcome filter (`--outcome failed`). Each
recording is paired with a suite task by position (task `i %
len(tasks)`) — a sole-task suite applies one judge across every
selected recording, which is the common authoring pattern.

`--workspace` is the directory the judges evaluate against. A
recording carries the conversation and tool I/O but not the
post-run file state, so judges that need file state
(`file-exists`, `file-contains`, `test-command`) require the
workspace to be preserved separately — `eval run --output ...`
retains per-task artifacts that suit this. For content-only
judges, `--workspace` can be empty.

The harness-replay flavour (replaying through a stirrup binary
configured with ReplayProvider+ReplayExecutor) is a future
follow-up; v0.1 is judge-only.

---

## Roadmap

Active work tracked in GitHub Issues under the `eval` label:

- **Cloud-backed lakehouse adapters** — interface is stable; cloud
  adapters depend on control plane storage choices.
- **A first mined suite** — CI infrastructure is ready; the
  `eval/suites/` and `eval/baselines/` directories are seeded by
  mining production traces from a real repo.
