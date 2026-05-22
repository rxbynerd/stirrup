# Inspecting JSONL trace files

A run configured with `traceEmitter.type=jsonl` writes a newline-delimited
JSON document describing the run's turns, tool calls, token usage,
verification results, and final outcome. The `stirrup trace`
subcommand family inspects those files first-party so operators do
not have to reinvent the wheel with `cat`, `jq`, and ad-hoc shell.

This page is the operator walkthrough. For the wire schema, see the
`types.RunTrace`, `types.TurnTrace`, and `types.ToolCallSummary`
definitions in [`types/runtrace.go`](../types/runtrace.go).

## Quick choice

| Want to… | Use |
|---|---|
| Read a finished trace end-to-end | `stirrup trace show <file>` |
| Watch a still-running trace | `stirrup trace tail -f <file>` |
| Pull aggregate counters into a dashboard | `stirrup trace stats <file> --output=json` |
| Pull every record matching a predicate | `stirrup trace grep --jq '<path> <op> <value>' <file>` |

Every subcommand accepts `-` as the file argument to read from stdin,
so the family composes with shell pipelines:

```sh
tail -F live.jsonl | stirrup trace show -
gunzip -c archived.jsonl.gz | stirrup trace stats - --output=json
```

A `.gz` archive is read by piping through `gunzip` or `zcat`; the
subcommands themselves are uncompressed-only and do not embed a
gzip dependency.

## `trace show`

```sh
stirrup trace show example.jsonl
```

Pretty-prints the trace in chronological order: the run metadata
header, every tool call in the order it ran, the verification
verdicts, and a per-tool aggregate at the foot. Tool calls flagged
unsuccessful are surfaced in red; the outcome is colour-coded green
(success), red (error/verification_failed/tool_failures),
yellow (max_turns / max_tokens / budget_exceeded / stalled / timeout),
grey (cancelled), or blue (anything unrecognised).

ANSI colour is auto-detected: stdout to a TTY is coloured; stdout
piped to a file or `less` is not. Override the detection with:

```sh
stirrup trace show example.jsonl --color=never   # plain text always
stirrup trace show example.jsonl --color=always  # colour even when piping
```

## `trace tail`

```sh
stirrup trace tail live.jsonl       # one-shot equivalent of show
stirrup trace tail -f live.jsonl    # follow the file as it grows
```

`tail` reads the file once and prints every record (one-shot). With
`-f` (or `--follow`), it polls the file at `--interval` (default 100ms)
and prints records as they are appended. The polling interval is
configurable for very quiet files:

```sh
stirrup trace tail -f live.jsonl --interval=500ms
```

`tail` uses a polling loop rather than a kernel notification API.
Trace files are append-only and a sub-second poll is sufficient to
keep an operator's terminal in sync without inflating the dependency
footprint.

SIGINT (Ctrl-C) terminates the follow loop cleanly. `tail -f -` reads
from stdin — the follow flag is redundant in that mode because stdin
already blocks until more bytes are written.

> **Truncation and rotation are not followed.** `tail -f` keeps the
> same file handle open. If the file is truncated or rotated out
> from under it, the offset stays past the new EOF and the follow
> loop appears to stall with no further output and no error.
> Restart the command to pick up the rotated file. Full
> `tail --follow=name` semantics (inode-tracking) are out of scope
> — stirrup's own traces are append-only.

## `trace stats`

```sh
stirrup trace stats example.jsonl
stirrup trace stats example.jsonl --output=json | jq .totalTurns
```

`stats` walks the file once and prints a compact summary. Metric
names mirror the OTel counters in
[`harness/internal/observability/metrics.go`](../harness/internal/observability/metrics.go)
so a single vocabulary covers online dashboards and offline
inspection.

### Text output

```
trace stats
  runId:            run-1735900000
  harness version:  v1.7.0
  records:          1
  total turns:      12
  tokens in / out:  18432 / 4116
  tool calls:       45 (errors: 3)
  verifications:    1 run (passed: 1, failed: 0)
  longest call ms:  2811
  wall clock:       42.317s
  tool counts:
    edit_file                        18
    grep                             14
    read_file                        10
    run_command                       3  (errors: 3)
  outcomes:
    success                           1
  slowest tool calls:
     1. run_command                     2811ms  fail
     2. grep                            1204ms  ok
     ...
```

The `harnessVersion` line carries the version of the binary that
computed the stats, NOT the binary that wrote the trace. Use this to
correlate aggregate counters across deployments — a stats output
produced by a newer `stirrup` binary against an older trace records
the new binary's version.

### JSON output

```sh
stirrup trace stats example.jsonl --output=json
```

Produces a single JSON line whose shape is `TraceStats` in
[`harness/cmd/stirrup/cmd/trace_stats.go`](../harness/cmd/stirrup/cmd/trace_stats.go).
The JSON form is stable across the same minor version of the binary
and is intended as a dashboard / report ingestion shape:

```json
{
  "runId": "run-1735900000",
  "harnessVersion": "v1.7.0",
  "records": 1,
  "totalTurns": 12,
  "tokensInput": 18432,
  "tokensOutput": 4116,
  "toolCalls": 45,
  "toolErrors": 3,
  "toolCallsByName": {"edit_file": 18, "grep": 14, ...},
  "toolErrorsByName": {"run_command": 3},
  "outcomes": {"success": 1},
  "longestToolCallMs": 2811,
  "totalWallClockMs": 42317,
  "slowestToolCalls": [...]
}
```

`--top N` controls how many entries appear in the text-mode
`slowest tool calls` table. JSON output always carries the full list.

## `trace grep`

`grep` filters records and prints the matching JSON lines verbatim,
one record per line — equivalent to `jq -c 'select(<predicate>)'` but
without the `jq` binary dependency.

### Substring matching

```sh
stirrup trace grep edit_file example.jsonl
```

Prints every record whose raw JSON contains the literal substring
`edit_file`.

### JSON-path predicate

```sh
stirrup trace grep --jq '.outcome == "success"' example.jsonl
stirrup trace grep --jq '.turns != 0' example.jsonl
stirrup trace grep --jq '.toolCalls.0.name == "edit_file"' example.jsonl
stirrup trace grep --jq '.outcome contains "fail"' example.jsonl
```

The `--jq` predicate is a deliberately tiny three-operator grammar:

```
<path> ( == | != | contains ) <value>
```

The path is a dot-separated walk of object keys and numeric array
indices. The value is a double-quoted string, a bare number, or one
of the literals `true`, `false`, `null`. The grammar is enough to
cover the common "give me the records where X is Y" workflow without
embedding a full jq interpreter. For richer filtering, the substring
and predicate combine, and the matching JSON is emitted verbatim so
downstream `jq` can run on the filtered stream if installed.

### Combining

The positional pattern and `--jq` predicate are AND-combined — both
must match. Use `--invert-match` to negate the result.

```sh
# Records whose outcome failed AND whose JSON mentions a specific tool.
stirrup trace grep run_command --jq '.outcome != "success"' example.jsonl
```

## Reading from stdin

Every subcommand accepts `-` as the file argument. This is the canonical
shape for non-file sources: `gunzip`, `kubectl logs`, `gcloud storage cat`.

```sh
gcloud storage cat gs://stirrup-traces/run-42.jsonl | stirrup trace stats -
```

The `traceEmitter.type=jsonl` path also accepts `/dev/stdout` as a
filename, so a run can stream its trace through a shell pipe:

```sh
stirrup harness --trace=/dev/stdout --trace-emitter=jsonl ... | stirrup trace tail -
```

## Edge cases the family handles

- **Truncated or in-progress JSONL.** Malformed lines are skipped with
  a `slog.Debug` log; the surrounding command continues. A tail-watch
  of a live run therefore surfaces partial output without failing.
- **Oversized records.** Records larger than the 4 MiB per-line cap
  (matching the trace emitter's own write cap) are skipped with a
  debug log.
- **Empty files.** `show` and `stats` fail with a clear "no
  well-formed records" message; `tail` (one-shot) and `grep` exit
  cleanly with no output, matching `cat` / `grep` POSIX semantics.

## Out of scope

- **Compressed traces (`.gz`).** Pipe through `zcat`. Embedding a
  gzip dependency would inflate the binary for a workflow shell
  already covers.
- **A full `jq` interpreter.** The three-op predicate covers the
  motivating cases; the subcommands deliberately do not vendor a jq
  library.
- **File-watcher notification APIs (fsnotify et al.)** Polling at
  100 ms is responsive enough for append-only files and avoids
  pulling in a per-platform dependency.

## Related docs

- [`observability-cloud.md`](observability-cloud.md) — managed APM
  routing for the OTel emitter.
- [`safety-rings.md`](safety-rings.md) — what the harness records and
  why.
- [`architecture.md`](architecture.md) — the 13-component model the
  trace records describe.
