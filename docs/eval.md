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
harness. With `--concurrency > 1`, the runner dispatches task trials
across a bounded worker pool while preserving suite order in
`result.json`; each trial still gets its own workspace, harness
subprocess, and trace file (`eval/runner/runner.go::runTasksConcurrently`).

A suite can run every task several times by setting `trials` in the
`suite` block; `stirrup-eval run --trials` overrides it when given.
The attribute must be at least 1 and defaults to one run per task.
See [Statistics](#statistics) for how trials are aggregated and
compared.

```hcl
suite "dogfood-seed" {
  trials = 3
  # task blocks ...
}
```

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

**Trace archive.** `trace_emitter.archive { type, file_path, bucket,
object_prefix }` mirrors `types.TraceArchiveConfig`, so a suite can
pin an explicit local or GCS destination for command-output sidecars
inline instead of falling back to `run_config_file`. `type` is
`"local"` (requires `file_path`) or `"gcs"` (requires `bucket`;
`object_prefix` is optional). Credential overrides for the GCS
destination are not exposed on this block; set them via
`run_config_file` when the default `gcp-workload-identity`
credential does not apply.

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
synthetic `"pass"` with reason `"dry run — skipped"`. A dry run
writes no artifacts: it prints the summary and exits. `--output`
is ignored under `--dry-run`, with a warning to stderr if set.

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

A judge decides whether a task passed. Most judges inspect the
workspace after the harness has run; the `tool-trace` judge inspects
the run's tool-call trace instead (`eval/judge/judge.go`):

| Judge type      | What it checks                                                        |
|-----------------|-----------------------------------------------------------------------|
| `test-command`  | Runs a shell command in the workspace; passes on exit code 0. 5 min timeout. |
| `file-exists`   | At least one of `paths` exists.                                       |
| `file-contains` | `path` exists and matches the regex in `pattern`.                     |
| `tool-trace`    | The run's `RunTrace.ToolCalls` satisfy a `tool_trace` block (see below). |
| `composite`     | Combines child `judges` with `require: "all"` or `require: "any"`.    |

All workspace-relative paths go through symlink-aware containment so
judges cannot escape the workspace.

#### The `tool-trace` judge

Where the file/command judges confirm the agent reached the right end
state, `tool-trace` confirms it got there by the expected tool-use
path — read-before-edit ordering, bounded search, in-loop recovery
from a renamed-tool miss, a no-tool answer that should or should not
have occurred. It is the trace-side complement used by the tool-use
reliability suite (`eval/suites/tooluse.hcl`). A `tool_trace` block
accepts:

| Field            | Meaning                                                                 |
|------------------|-------------------------------------------------------------------------|
| `sequence`       | Ordered list of internal tool names that must each appear, in this relative order (other calls between them are allowed). |
| `call "<name>"`  | A per-tool block: `min_calls`, `max_calls`, `all_succeeded`. A `max_calls = 0` forbids the tool entirely. |
| `forbid_unknown` | Fails when a tool call failed under a name that never later succeeds — i.e. an unresolved unknown-/renamed-tool miss. |

Names match the **internal** tool ID (`ToolCallSummary.InternalName`
when set, falling back to `Name`), so an assertion written against the
canonical name holds under any toolset-profile alias. Example:

```hcl
judge {
  type = "tool-trace"
  tool_trace {
    sequence = ["read_file", "edit_file"]
    call "edit_file" {
      min_calls     = 1
      all_succeeded = true
    }
  }
}
```

The judge receives the run's parsed `RunTrace` through
`JudgeContext.Trace`, which the runner populates from the per-task
trace it already parses. A `tool-trace` judge with no trace available
is an error, not a silent pass.

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

`ReplayProvider` also backs the tool-use reliability suite's
no-credential gate. The `stirrup-eval run` subcommand spawns the real
`stirrup harness` binary, which selects a provider from the RunConfig
and so always needs live credentials; there is no replay-provider
RunConfig path. To run the tool-use behaviours with no provider and no
network, `harness/internal/core/tooluse_replay_test.go` drives the
agentic loop in process with a `ReplayProvider` and a real
`LocalExecutor` over synthetic workspaces, asserting the same workspace
state and tool-call traces the HCL suite's judges check. That test is
the gate; `eval/suites/tooluse.hcl` is its live-provider-comparable
form.

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

**Manifest index.** The FileStore keeps an append-only JSONL manifest
at `<lakehouse>/manifest.jsonl` alongside the trace and recording
directories, so queries with a narrow filter can skip loading most
JSON files. Each line is one event:

```
{"kind":"trace","id":"...","startedAt":"...","outcome":"...","mode":"...","model":"...","provider":"..."}
{"kind":"recording","runId":"...","startedAt":"...","outcome":"...","mode":"...","model":"...","provider":"..."}
```

`StoreTrace`/`StoreRecording` append one entry per call; re-ingesting
the same ID appends a duplicate, and the read path uses the last
entry per ID (last-write-wins, matching the JSON-file write
semantics). Writers append via a single `O_APPEND` file handle, which
is atomic on POSIX for payloads under `PIPE_BUF` (manifest lines are
well under that); concurrent writers only guarantee that an append
which returned before another started precedes it in the file, and
the read path tolerates any order since last-write-wins is symmetric.
A missing or corrupt manifest is recoverable: the read path detects
the failure, falls back to scanning every JSON file on disk, and
rewrites the manifest as a side effect.

Trace and recording JSON files are themselves written via a
write-then-rename pattern (temp file in the target directory, then
`os.Rename`), so a concurrent reader never observes a torn or
zero-byte file, and two writers racing on the same ID never
interleave bytes into a corrupt document — the last successful
rename wins atomically.

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

### Statistics

One run per task cannot separate a flaky task from a broken one: a
task that passes 98% of the time still fails about once in fifty
runs. The framework therefore treats a suite run as a sample. Each
task can run K times (`trials`), and `compare` reasons about per-task
pass fractions with error bars rather than single outcomes. The
methods follow Miller, *Adding Error Bars to Evals*
([arXiv:2411.00640](https://arxiv.org/abs/2411.00640)), and are
implemented with the Go standard library in `eval/reporter/stats.go`.

#### Trials and the majority rule

Every trial runs in its own workspace and harness subprocess, so the
trials of a task are independent samples of it. `result.json` keeps one
`TaskResult` per task:

- `passFraction` is the share of the task's trials that passed. With
  one trial it is 1 for a pass and 0 otherwise.
- `trials` lists each trial's outcome, judge verdict, error, duration,
  and turn count. It is omitted when the task ran once, so single-run
  results keep their shape.
- `outcome` is the strict majority of the trial outcomes: `pass` when
  more than half the trials passed, `fail` when more than half failed,
  otherwise `error`. Errored trials never count toward `fail`, so an
  infrastructure or judge error cannot confirm a failure. A split with
  no strict majority (any even-K tie included) reports `error` with a
  message naming the split. `trace`, `judgeVerdict`, and `error` come
  from the first trial of the majority outcome; `durationMs` is the
  sum across trials.

`SuiteResult.trials` records K, and `passRate` is the mean per-task
pass fraction, which equals the familiar pass rate when K is 1. A
result file without `trials` fields, including every committed
baseline recorded before trials existed, loads as one trial per task.

#### Estimators

With n tasks, per-task pass fractions s<sub>i</sub>, and per-task
differences d<sub>i</sub> = current s<sub>i</sub> − baseline
s<sub>i</sub> over the tasks present in both results:

| Quantity | Definition |
|---|---|
| Suite pass rate | Mean of s<sub>i</sub>. |
| Task-level standard error (Miller eq. 1) | Sample standard deviation of s<sub>i</sub> divided by √n. Trials of one task are not independent samples of the suite, so they are never pooled. Undefined below two tasks. |
| Mean delta d̄ and paired standard error (Miller eq. 7) | Mean of d<sub>i</sub>, and the sample standard deviation of d<sub>i</sub> divided by √n. Pairing removes per-task difficulty from the variance, so when difficulty is consistent across runs the paired error is much smaller than two independent errors combined (0.071 against 0.173 in the worked example below). |
| 95% confidence interval | d̄ ± t<sub>n−1, 0.975</sub> × SE. The Student-t quantile comes from inverting the regularised incomplete beta function, so small suites get the wider interval they warrant. |
| One-sided upper bound U | d̄ + t<sub>n−1, 0.95</sub> × SE. U below zero rules out "no regression" at 95% confidence. |
| Sign-flip p-value | Two-sided permutation test on d<sub>i</sub>. Tasks with d<sub>i</sub> = 0 are dropped; with at most 20 remaining, all 2<sup>m</sup> sign assignments are enumerated and p is the share whose absolute sum reaches the observed one. Above 20 a normal approximation is used, and the report says so. |
| Minimum detectable effect | 2.80 × √(Var(d)/n), where 2.80 = z<sub>0.975</sub> + z<sub>0.80</sub>: the smallest true mean change the suite detects with 80% power at a two-sided 5% level. Smaller drops are unlikely to be confirmed. More tasks or more trials shrink it. |
| pass^k and pass@k | Per task with c passes in K trials, pass^k = C(c, k) / C(K, k), the chance that k trials drawn without replacement all pass ([τ-bench](https://arxiv.org/abs/2406.12045)), and pass@k = 1 − C(K − c, k) / C(K, k), the chance that at least one does ([Chen et al.](https://arxiv.org/abs/2107.03374)). Averaged over tasks for k = 1 up to the smallest per-task trial count. A falling pass^k shows inconsistency that the pass rate hides. |
| Wilson 95% interval | Wilson score interval for the suite pass rate over n tasks. |
| Noise floor | Reported when every paired baseline task passed every trial. With p the per-trial pass rate pooled over both results, a single-run flip gate false-alarms with probability 1 − p<sup>n</sup>, and the K-of-K flip rule with 1 − (1 − (1 − p)<sup>K</sup>)<sup>n</sup>. These cover the flip rule only; the upper-bound rule adds to the overall block rate. |

`eval/reporter/gate_test.go` reproduces a ten-task, three-trial worked
example: d̄ −0.100, SE 0.0711, 95% CI (−0.261, +0.061), sign-flip
p 0.375, MDE 0.20, pass^3 0.40 → 0.20, pass@3 0.80 → 0.80. The gate
reports `warn`: a ten-point drop the data cannot confirm.

#### Gate semantics

`compare` applies these rules in order; the first match decides.

| Gate | Condition | Exit |
|---|---|---|
| `block` | A deterministic flip: a task passed every baseline trial and no current trial. Applies at any n. | `1` |
| `inconclusive` | Fewer than three paired tasks, or the paired standard error is undefined. | `0` |
| `block` | U < 0: the drop is statistically confirmed. | `1` |
| `warn` | d̄ < −m, where m is `--warn-margin` (default 0.05), but U ≥ 0: a drop larger than the margin that the data do not confirm. | `0` |
| `pass` | Anything else. | `0` |

The regression list is separate from the gate decision. A task is
listed as a regression when it passed every baseline trial and its
current pass fraction is at or below `--flip-threshold` (default 0.5).
Improvements mirror the rule. A listed regression such as 3/3 → 1/3
blocks only through the rules above.

With one trial per task every d<sub>i</sub> is −1, 0, or +1, and a −1
is always a deterministic flip, so the gate blocks exactly when a task
that passed in the baseline did not pass in the current run.

For an unchanged agent with independent trials, a single-run all-pass
baseline, and the default options, enumerating every outcome gives
these per-run rates:

| Tasks | Per-trial pass rate | Block, one trial | Block, three trials | Warn, three trials |
|---|---|---|---|---|
| 5 | 98% | 9.6% | 0.19% | 26% |
| 5 | 95% | 22.6% | 2.4% | 51% |
| 10 | 98% | 18.3% | 1.7% | 10% |
| 10 | 95% | 40.1% | 14.6% | 30% |

`warn` is common by design: one task at 2/3 on a five-task suite moves
d̄ by −0.067, past the default margin. Block rates climb steeply as the
per-trial pass rate falls because an all-pass baseline overstates any
agent below 100%: part of every drop is the gap between a lucky
baseline and the true rate. A baseline recorded with `--trials 3`
stores fractional pass rates for flaky tasks and narrows that gap.

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

With `--trials K` (or the suite's `trials` attribute) every task runs
K times, each trial in a fresh workspace and harness subprocess, and
`result.json` aggregates the trials per task as described in
[Trials and the majority rule](#trials-and-the-majority-rule). The
worker pool schedules task-trial pairs, so `--concurrency` bounds
harness subprocesses across all trials. Retained artifacts move one
level down when K is above 1:

```text
<output>/<suite>/<task>/                 # K = 1
<output>/<suite>/<task>/trial-<i>/       # K > 1, i = 1..K
```

The summary printed to stdout states K and the number of harness runs
when K is above 1. A dry run validates each task once and reports the
runs it would have made.

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
| `--output`       | current dir      | Directory for `result.json` and per-task artifacts. Ignored under `--dry-run`, which writes no artifacts. |
| `--harness`      | `stirrup` on PATH| Harness binary to invoke for live runs.                      |
| `--concurrency`  | `1`              | Number of task trials executed in parallel. Workers preserve suite order in `result.json`. Values larger than the number of task trials cap at that number. Concurrent invocations talking to the same provider hit rate limits faster, so the value should respect the provider account's per-minute caps. |
| `--trials`       | `1`              | Independent runs per task. When the flag is not given, the suite's `trials` attribute applies, else one run. Values below 1 are rejected. Provider spend scales linearly with K. |
| `--dry-run`      | `false`          | Validate the suite (and, when present, the merged per-task RunConfig via `ValidateRunConfig`), print the summary, and exit without writing `result.json` or JUnit XML. |
| `--junit`        | empty            | Write JUnit XML to this path after `result.json`. Each task is one `testcase`. A task with more than one trial carries a `system-out` summary of every trial, its `failure` or `error` message is prefixed with the pass count (for example `1/3 trials passed: ...`), and the body ends with the reason of each non-passing trial. Single-trial output is unchanged. |
| `--model`        | empty            | Model to run every task with, forwarded to each harness invocation as `--model`. Overrides the harness default and any model pinned by the suite's `run_config` block. CI uses this to pin the per-push gate to a cheap model and the release sweep to stronger ones. |
| `--prompt-model` | empty            | Prompt model to render system prompts with, forwarded to each harness invocation as `--prompt-model`. The wire model is unchanged. See [Comparing prompts across models](#comparing-prompts-across-models). |
| `--provider`     | empty            | Provider type to run every task against, forwarded as `--provider`. Overrides the harness default and any provider pinned by the suite's `run_config` block. |
| `--base-url`     | empty            | API base URL for the `openai-compatible` / `openai-responses` providers, forwarded as `--base-url`. |
| `--api-key-ref`  | empty            | `secret://` reference for the provider API key, forwarded as `--api-key-ref`. A reference the harness resolves through `SecretStore` at runtime — never a literal key. |

The three provider flags exist for the same reason as `--model`: the
provider a suite runs against is a property of the invocation, not of
the suite, so CI can retarget a provider-neutral suite without editing
suite files. Each is emitted independently, so an invocation may
override just the base URL. The per-push eval gate uses all four to run
`dogfood-seed.hcl` against OpenRouter, three trials per task:

```bash
./stirrup-eval run \
  --suite eval/suites/dogfood-seed.hcl \
  --trials 3 \
  --provider openai-compatible \
  --base-url https://openrouter.ai/api/v1 \
  --api-key-ref secret://OPENROUTER_API_KEY \
  --model openai/gpt-5.6-luna
```

Exit code is `0` regardless of pass rate — use `compare` to gate CI.

### `compare` — diff two results

```bash
./stirrup-eval compare \
  --current results/result.json \
  --baseline eval/baselines/regression.json
```

Diffs two `SuiteResult` files task by task, computes the paired
statistics in [Statistics](#statistics) and per-task turn deltas from
`RunTrace`, decides a gate result, and prints a text report. It exits **`1` only when the gate is `block`**;
`warn`, `inconclusive`, and `pass` exit `0`. This is the gate the
`eval-gate` CI job uses. Either side may be a single-run result,
including a committed baseline with no `trials` fields, which compares
as one trial per task.

| Flag               | Default  | Description |
|--------------------|----------|-------------|
| `--current`        | required | Path to the current `SuiteResult` JSON. |
| `--baseline`       | required | Path to the baseline `SuiteResult` JSON. |
| `--warn-margin`    | `0.05`   | Mean pass-fraction drop that reports `warn` when the drop is not statistically confirmed. Must be within [0, 1]. |
| `--flip-threshold` | `0.5`    | Current pass fraction at or below which a task that passed every baseline trial is listed as a regression, and the baseline pass fraction at or below which a task that passes every current trial is listed as an improvement. Must be within [0, 1). |
| `--output`         | empty    | Also write the comparison report as JSON to this path. |

The text report opens with the gate result and its reasons, then the
pass rate of each side with its Wilson interval, task-level standard
error, task count n, and trials K, each followed by a pass^k / pass@k
row. A `Paired:` line gives n, d̄, the paired standard error, the 95%
t interval, the one-sided upper bound, the sign-flip p
(marked exact or approximate), and the minimum detectable effect; the
noise floor, regressions, and improvements follow. For the worked
example in [Statistics](#statistics):

```text
Eval Comparison: current-run vs baseline-run

Gate: WARN
  - mean delta -0.100 is below -0.050 but not confirmed (upper bound +0.030 >= 0)

Pass Rate: 60.0% → 50.0% (-10.0%)
  baseline: 60.0% (95% Wilson [31.3%, 83.2%]), SE 0.130, n=10 tasks, K=3
            pass^k (k=1..3): 0.600, 0.467, 0.400; pass@k: 0.600, 0.733, 0.800
  current:  50.0% (95% Wilson [23.7%, 76.3%]), SE 0.114, n=10 tasks, K=3
            pass^k (k=1..3): 0.500, 0.300, 0.200; pass@k: 0.500, 0.700, 0.800
Paired: n=10, mean delta -0.100, SE 0.0711, 95% t(9) CI [-0.261, +0.061], one-sided upper bound +0.030, sign-flip p 0.375 (exact), MDE 0.20

No regressions found.
```

The `--output` JSON (`eval.ComparisonReport`) is stable. `regressions`
and `improvements` carry each task's outcomes and pass fractions on
both sides; `tasks` lists every paired task with `baselinePassFraction`,
`currentPassFraction`, `baselineTrials`, `currentTrials`, and `delta`.
`summary` holds:

| Field | Meaning |
|---|---|
| `gate`, `gateReasons` | `pass`, `warn`, `block`, or `inconclusive`, and why. |
| `baselinePassRate`, `currentPassRate`, `passRateDelta` | Mean per-task pass fractions and their difference. |
| `hasRegressions` | Whether the regression list is non-empty. |
| `warnMargin`, `flipThreshold` | The options the comparison ran with. |
| `deterministicFlips` | Task IDs that passed every baseline trial and no current trial. |
| `baseline`, `current` | Per-side `tasks`, `trials`, `passRate`, `stdErr` (omitted below two tasks), `wilsonLow`, `wilsonHigh`, `passHatK`, `passAtK`. |
| `paired` | `tasks`, `meanDelta`, `stdErr`, `df`, `ciLow`, `ciHigh`, `upperBound`, `pValue`, `pValueExact`, `mde`. Omitted below two paired tasks. |
| `noiseFloor` | `perTrialPassRate`, `pooledTrials`, `singleRunFalseAlarm`, `flipRuleFalseAlarm`. Present only when every paired baseline task passed every trial. |

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

**Scrubbing posture.** Trace files written by the streaming JSONL
emitter have already passed through the harness's log scrubber on
the way to disk. The eval module does not import
`harness/internal/security` to apply a second scrubbing pass — that
package boundary is private (see `AGENTS.md`). Traces from older,
single-blob-only binaries went through `RunConfig.Redact()` at write
time instead. Ingest relies on this upstream scrubbing rather than
re-scrubbing on read.

### `mine-failures` — turn production failures into tasks

```bash
./stirrup-eval mine-failures \
  --lakehouse var/lakehouse \
  --after 2026-04-01 --before 2026-05-01 \
  --outcome failed \
  --limit 20 \
  --sample-by outcome \
  --output eval/suites/mined.hcl \
  --accept-quarantine
```

Queries production traces from the lakehouse, opportunistically
hydrates each with its `RunRecording` (when one exists), and
constructs an `EvalSuite` of regression tasks. Each task defaults
to a `test-command` judge running `go test ./...`; the
description carries the failing-turn context (last assistant
message excerpt and any failing tool call) so a human reading the
suite knows what went wrong without re-running the trace.

Filters (all optional):

- `--after <date>` / `--before <date>` — window the candidate traces.
- `--outcome <passed|failed|inconclusive>` — target a specific
  `EvalOutcome` bucket. Defaults to `failed`.
- `--include-inconclusive` — broaden to also include
  `inconclusive` traces (limit-hit or interrupted runs).
- `--include-batch` — by default, batch runs are excluded because
  their wall-clock is dominated by provider queue dynamics.
- `--limit N` — cap the output at N tasks.
- `--sample-by <outcome|model|mode|provider>` — when more
  candidates exist than `--limit`, stratify proportionally across
  the chosen dimension instead of taking the top N by recency.
  Empty (the default) takes the top N by recency.

When the lakehouse holds no recording for a candidate trace, the
task is still emitted but its description flags "thin trace only;
refine prompt manually" — operators decide whether to keep or
drop it.

Without `--output`, `mine-failures` runs in dry-run mode and
prints a preview to stderr without writing a suite file.

#### Quarantine envelope

Mined suites carry raw conversation content from production runs. If
the source recordings trip a quarantine classifier — `large_payload`
today, `unscrubbed_secret_event` and `pii_classification` reserved
for future control-plane scoring — `mine-failures` refuses to write
the suite unless `--accept-quarantine` is passed. `eval run` mirrors
the refusal: a suite whose HCL declares `quarantine_flags = [...]`
will not execute without the same flag. This puts the operator in
charge of declaring "this content is safe to exfiltrate" rather than
silently shipping classified material to contributors.

Committing a flagged suite to a public repo is a code-review smell;
the `quarantine_flags = [...]` attribute on the suite block makes
the situation visible in PR review. See #115 for the design
rationale.

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
of pass rate (with its 95% Wilson interval over the window's trace
count), mean turns, and p50/p95 duration for both windows plus
deltas. The interval is informational; the thresholds below use the
point estimates. Exits **`1` if either threshold trips**:

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
- **`eval-gate`** — depends on `verify`. On every push it builds
  the binaries, runs each suite in `eval/suites/` that has a
  matching baseline in `eval/baselines/` (unbaselined suites are
  opt-in local runs), pins the model to GPT-5.6 Luna over OpenRouter
  via `stirrup-eval run`'s provider flags, runs every task three times
  (`--trials 3`), compares each result to its baseline via
  `eval compare`, and uploads the result and comparison JSON as a
  build artifact. Three trials triple the per-push spend in exchange
  for the false-alarm rates in [Gate semantics](#gate-semantics).
  On-demand `provider-quirks-*` suites run without `--trials` (one
  trial per task unless the suite sets `trials`) to contain their
  larger per-run cost. Authentication is the `OPENROUTER_API_KEY`
  repository secret; runs that cannot read it (fork clones, Dependabot-actor
  pushes) skip the live run with a warning rather than reporting a
  false regression.
- **`publish-container`** — depends on `verify`. On `main` pushes it
  publishes the harness Docker image to `ghcr.io/rxbynerd/stirrup`.

A `block` from `compare` (non-zero exit) fails the gate. `warn` and
`inconclusive` exit `0`; the job raises them as workflow warning
annotations so they stay visible without failing the push.
At release time, `release.yml::eval-extended` re-runs the baselined
suites against stronger models (Claude Sonnet 5 and Claude Opus 4.8)
as a non-blocking-but-visible matrix: a blocking comparison turns the
matrix cell red without holding the release.

---

## Typical workflows

### Adding a regression suite to CI

1. Author an `EvalSuite` HCL file under `eval/suites/` (e.g.
   `eval/suites/<name>.hcl`).
2. Run it with `eval run --trials 3` and capture `result.json` as the
   baseline at `eval/baselines/<name>.json`. Three trials record a
   fractional pass rate for any flaky task, so the gate measures
   change instead of the gap between one lucky run and the task's
   true rate. A single-run baseline still loads and compares as one
   trial per task.
3. On subsequent CI runs, `eval-gate` runs the suite and compares to
   the committed baseline. A `block` fails the push; a `warn` is
   annotated on the run for review.
4. When a behaviour change is intentional, regenerate the baseline
   (again with `--trials 3`) and commit it as part of the PR.

### Comparing prompts across models

The shipped system prompts are templated per model
([configuration.md — System prompt templating](configuration.md#system-prompt-templating)),
so two runs with different `--model` values no longer share prompt
content. `--prompt-model` restores a controlled comparison by pinning
the prompt while varying the model (or vice versa):

```sh
# A/B: does the new model do better with its own prompt or the
# claude-fable-5 prompt it was not tuned for?
stirrup-eval run --suite eval/suites/dogfood-seed.hcl --model claude-fable-6
stirrup-eval run --suite eval/suites/dogfood-seed.hcl --model claude-fable-6 --prompt-model claude-fable-5
```

Baselines are keyed on `(suiteId, taskId)` outcomes and pass
fractions only, so prompt templating does not change baseline identity; regenerate a baseline
only when task outcomes legitimately change. The resolved prompt model
and tier are recorded on each run's root OTel span (`prompt.model`,
`prompt.tier`) for after-the-fact attribution.

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
- **Mined dogfood corpus** — a hand-curated `dogfood-seed.hcl`
  ships with v0.1 to give the eval-gate non-empty work; the
  longer-term path swaps it for a mined suite produced by
  `stirrup-eval mine-failures` against this repo's own
  recordings. See `eval/suites/README.md` for the promotion path.
