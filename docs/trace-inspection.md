# Inspecting JSONL trace files

A run configured with `traceEmitter.type=jsonl` writes a newline-delimited
JSON document describing the run's turns, tool calls, token usage,
verification results, and final outcome. The `stirrup trace`
subcommand family inspects those files first-party so operators do
not have to reinvent the wheel with `cat`, `jq`, and ad-hoc shell.

This page is the operator walkthrough. For the wire schema, see the
`types.RunTrace`, `types.TurnTrace`, `types.TurnRecord`, and
`types.ToolCallSummary` definitions in
[`types/runtrace.go`](../types/runtrace.go).

## On-disk shape

A JSONL trace file is a stream of one-line JSON events, each carrying
a `kind` discriminator. A complete run produces:

```jsonl
{"kind":"run_started","schemaVersion":"2","runId":"run-1","harnessVersion":"...","config":{...redacted...},"startedAt":"..."}
{"kind":"hook_record","runId":"run-1","hook":{"phase":"preRun","index":0,"command":"...","exitCode":0,"durationMs":40}}
{"kind":"system_prompt","runId":"run-1","systemPrompt":"...","systemPromptSha256":"..."}
{"kind":"turn_record","runId":"run-1","turn":1,"modelInput":{...},"modelOutput":[...],"toolCalls":[...]}
{"kind":"tool_call_record","runId":"run-1","toolCall":{"id":"...","name":"read_file","input":null,"output":"...","durationMs":12,"success":true}}
{"kind":"post_edit_check","postEditCheck":{"tool":"edit_file","path":"main.go","index":0,"command":"...","exitCode":0,"durationMs":35}}
{"kind":"command_output_record","runId":"run-1","commandOutput":{"archiveId":"...","runId":"run-1","toolUseId":"...","stdout":{"rawBytes":123,"scrubbedBytes":111,"rawSha256":"...","scrubbedSha256":"..."}}}
{"kind":"turn_record","runId":"run-1","turn":2,"modelInput":{...},"modelOutput":[...]}
{"kind":"run_finished","trace":{...RunTrace summary...},"completedAt":"..."}
```

`turn_record` carries the full transcript the model saw and produced
that turn, including the exact scrubbed tool results shown to it.
Anthropic `thinking` blocks keep their type and reasoning text, scrubbed
like assistant text; their signatures, and the data of
`redacted_thinking` blocks, are never written (see [thinking-block
replay](providers.md#thinking-block-replay)).
Under [lazy tool loading](configuration.md#lazy-tool-loading), a
provider's native tool search appears in `modelOutput` as a
`tool_search` block and a `tool_search_result` block, and again in
later turns' `modelInput.messages`. `modelInput.tools` marks each
definition a native search defers with `deferred: true`, and
`toolsLoaded` lists the tools the turn's searches loaded. A
native-search turn also records each deferred MCP or control-plane
tool's `namespace` and `namespaceDescription`, with the unnormalised
name. See [What is
recorded](configuration.md#what-is-recorded).
`command_output_record` links a `run_command` call to bounded stream hashes,
sizes, references, and its compressed sidecar archive without embedding full
sandbox output in JSONL. `tool_call_record`
is emitted in addition so a live consumer sees each call as soon as
it lands. `run_finished` carries the same `RunTrace` summary the
legacy single-blob format used; `stirrup trace show / stats / grep`
read it transparently regardless of which on-disk shape produced it.

Lines are flushed after each write, so a `SIGKILL`'d run leaves a
JSONL file that parses up to the last completed event. A trace that
predates the streaming format (one single-line `RunTrace` per file)
is read as if it were a streaming file containing only the
`run_finished` event.

`stirrup trace show` and the rest of the `stirrup trace` family
operate on the same `run_finished`-equivalent shape so operator
workflows are unchanged across the format transition.

### Schema version and run tags

`run_started` carries `schemaVersion`. Version `2` tags every per-run
record (`turn_record`, `tool_call_record`, `hook_record`,
`command_output_record` and `system_prompt`) with the `runId` of the run that produced it,
and adds the `subrun_started` and `subrun_finished` kinds. A record
that belongs to a sub-agent also carries `parentRunId`. Readers skip
event kinds they do not recognise, so a reader that predates a kind
ignores its lines. `post_edit_check` lines carry no run tag.

### Sub-agent runs

A sub-agent's records stream into the parent's file, bracketed by two
kinds that are distinct from `run_started` and `run_finished`. A reader
written before these kinds existed skips them instead of mistaking the
child for the recorded run:

```jsonl
{"kind":"subrun_started","runId":"run-1/sub-1","parentRunId":"run-1","startedAt":"...","config":{...redacted...}}
{"kind":"turn_record","runId":"run-1/sub-1","parentRunId":"run-1","turn":1,"modelInput":{...},"modelOutput":[...]}
{"kind":"subrun_finished","runId":"run-1/sub-1","parentRunId":"run-1","completedAt":"...","subRun":{"runId":"run-1/sub-1","parentRunId":"run-1","outcome":"success","turns":3,"tokenUsage":{...},"startedAt":"...","completedAt":"...","durationMs":8120}}
```

`subrun_started` carries the child's redacted `RunConfig`.
`subrun_finished` carries a summary only (outcome, turn count, token
usage, timestamps, and duration). The child's `RunTrace` is not written
to the file. A child's `runId` has the form `<parentRunID>/sub-<n>`,
where `n` counts the sub-agents the parent has spawned, starting at 1.

The child's tool calls also appear in the parent's `run_finished`
summary with `runId` and `parentRunId` set, and its turns count toward
the parent's `turns` and `tokenUsage`. A consumer that wants parent-only
aggregates filters on those tags; `stirrup trace stats` reports the
number of tagged calls as `subAgentToolCalls`. A child's system prompt
and verification results are not written to the parent's file.

`ReadRecording` (the reader in
[`types/trace`](../types/trace/reader.go)) rebuilds the recording from
these lines. A `turn_record` that names a parent run and carries a run
ID other than the recording's own belongs to a child. It is grouped,
with that child's `subrun_started` and `subrun_finished`, into
`RunRecording.SubRuns` (`subRuns` in JSON), one entry per child in order
of first appearance. It never appears in `RunRecording.Turns`. Each
entry carries `runId`, `parentRunId`, `config`, `turns`, and a `summary`
that is absent when the file ends before the child finished. Records
without run tags, as in traces written before schema version 2, stay in
`Turns` and produce no `subRuns`. `RunRecording.CommandOutputs` describes
the run's whole sidecar archive, which sub-agents share, so its entries
span every run and each names its own `runId`.

### Request reproduction

Several records exist so that the request sent on each turn can be
reproduced from the file:

- **`harnessVersion`** on `run_started` names the harness build that
  wrote the trace.
- **`system_prompt`** follows `run_started` and carries `systemPrompt`
  and `systemPromptSha256`. The prompt text is scrubbed of
  secret-shaped content before it is written, or written verbatim under
  `--debug`. The digest is of the prompt as sent, before scrubbing, so
  it identifies the prompt even when the text was redacted. A
  sub-agent's system prompt is not recorded.
- **`modelInput.request`** on each `turn_record` holds the request
  parameters of that turn:

  | Field | Meaning |
  |---|---|
  | `temperature` | Value after defaulting; absent when the request carried none. |
  | `maxTokens` | Output limit sent. |
  | `reasoningEffort` | Reasoning effort sent; absent when none. |
  | `reasoningSummary` | Summary mode resolved for the adapter in use. |
  | `toolChoice` | `auto`, `required`, `none`, or `tool`. |
  | `cacheKey` | Provider-neutral cache affinity digest, never the run ID itself. |

- **`responseModel`** on `turn_record` is the model identifier the
  provider reported answering with, which can differ from
  `modelInput.model`, the requested alias. It is absent when the
  provider reports none.

`ReadRecording` surfaces `harnessVersion` and the `system_prompt` line as
`RunRecording.HarnessVersion`, `RunRecording.SystemPrompt`, and
`RunRecording.SystemPromptSha256`.
Traces written before these records existed lack all of them, and the
recording fields are then empty.

### Turn-record fields

Besides the transcript, each `turn_record` carries markers for the
synthetic steering the harness applied on that turn. Each is omitted
when it does not apply:

| Field | Meaning |
|---|---|
| `outputLimitRecovery` | The turn stopped at the output limit (`max_tokens`) and the run continued with a synthetic instruction instead of ending. |
| `stallNudge` | The turn's tool calls completed a repeated call pattern, and the next request carries a synthetic instruction to change approach instead of the run ending as `stalled`. |
| `windDown` | The budget threshold that made the harness send its wind-down instruction at the start of the turn: `turns`, `tokens`, or `time`. |

Messages in `modelInput.messages` carry an optional `origin`, which names
the harness component whose output the message holds when that output can
echo content from outside the harness. The value `verifier_feedback`
marks the user message that carries a failed verifier's feedback into a
verification retry. Harness output merged into a message of another
origin carries `origin` on its text block instead. The field is
independent of `synthetic`.

Per-turn summaries (`TurnTrace`) are not written as JSONL lines, so
`TurnTrace.retries` is visible only to in-process emitters and as the
`turn.retries` attribute on the OTel `chat {model}` span. It counts the
earlier attempts at the turn that failed with a retryable provider
error and were discarded; the record describes the final attempt. The tokens those attempts
reported are included in the turn's token usage.

### Post-edit checks

Each [`tools.postEdit`](configuration.md) check execution streams as a
`post_edit_check` line and accumulates in `RunTrace.postEditChecks`
(`postEditChecks` in the `run_finished` trace), in dispatch order. A
record holds `toolUseId`, `turn`, `tool`, `path`, `index`, the
configured `command` template before path substitution, `exitCode`,
`durationMs`, `timedOut`, `error`, `outputBytes`, and `truncated`. The
diagnostics themselves are not recorded in these entries. They reach the
trace only inside the tool result they were appended to. `path`,
`command`, and `error` are scrubbed. A non-empty `error` means the check
did not produce an exit code (timeout, rejected path, or executor
failure), so an `exitCode` of `0` alone does not mean the check passed.

### Verification results

`RunTrace.verificationResults` holds one entry per verifier attempt, in
order, and is empty when the verifier is `none`. `attempt` is the 1-based
attempt number. A `passed: false` entry with `error` set means the
verifier failed to produce a verdict, as opposed to failing the work.
`feedback`, `error`, and every string inside `details` are scrubbed of
secret-shaped content and then capped at 4 KiB, with a truncation marker
on a capped value. The last entry is the one that decides the outcome.

### Run-level fields

The `run_finished` trace carries these fields beyond the counters:

| Field | Meaning |
|---|---|
| `workspaceExport` | Receipt of the end-of-run workspace export: `uri`, `status` (`uploaded`, `empty`, or `failed`), `bytes`, `sha256`, `files`, `required`, and a scrubbed `error` when `status` is `failed`. Absent when no export destination is configured. |
| `error` | Scrubbed, length-capped reason for a non-success `outcome`, the run's earliest failure. Absent on success, except that a successful run whose trace could not be persisted carries it on the returned trace only, never in the file. |
| `structuredOutput` | The final answer's compact JSON document when the run configured an `outputSchema` and the answer satisfied it. Scrubbed string by string. Present only when `outcome` is `success`. |

### Token usage

The `run_finished` trace's `tokenUsage` object (also the `tokenUsage`
of a `RunResult` and the `input_tokens` / `output_tokens` /
`cache_read_tokens` / `cache_write_tokens` / `reasoning_tokens` fields
of the gRPC `RunTrace`) sums every turn:

| Field | Meaning |
|---|---|
| `input` | Whole prompt, cached tokens included. The provider's figure where the provider reports one, otherwise the harness's estimate. |
| `output` | Output tokens, reasoning included. |
| `cacheRead` | Part of `input` served from the provider's prompt cache. |
| `cacheWrite` | Part of `input` written to the provider's prompt cache. |
| `reasoning` | Part of `output` spent on reasoning or thinking. |

"Every turn" includes the turns of sub-agents forwarded into the trace.
`tokenUsage` also includes auxiliary model calls made on the run's
provider credentials outside agentic turns: summary generation for the
context strategy, the `llm-judge` verifier, and the `cloud-judge`
guard. Two sibling fields break out the ledger's other figures:

| Field | Meaning |
|---|---|
| `auxiliaryTokens` | The part of `tokenUsage` spent on auxiliary calls. Omitted when zero. |
| `externalGuardTokens` | Usage reported by a guard on its own endpoint (`granite-guardian` or `shieldstral`). Not part of `tokenUsage` and not charged to `maxTokenBudget`. Omitted when zero. |

Only the top-level run records these two fields. A sub-agent's
auxiliary calls are charged to the parent's figures.

The three breakdown fields are omitted when zero, which is the case
for a provider that does not report them. A single run can mix
provider-reported and estimated turns, so the aggregate carries no
source flag. Per turn, the source is visible on the `turn completed`
log line (`tokens.input_reported`), on the `provider.stream` span
(`tokens.input_reported`), and on the OTel `chat {model}` span
(`stirrup.tokens.input_reported`, see
[`observability-cloud.md`](observability-cloud.md#token-usage-on-chat-spans)).
The `provider.stream` span carries `tokens.cache_read`,
`tokens.cache_write`, and `tokens.reasoning` only when non-zero; the
log line always carries all three.

The input falls back to the estimate whenever the adapter reports no
input figure, for example an `openai-compatible` server that ignores
`stream_options.include_usage`, or an Anthropic-compatible endpoint
that reports only `output_tokens`. Provider counts are clamped to
`0`…`2147483647`, cache figures are capped at the input, and reasoning
is capped at the output, so a malformed report cannot break the
invariants above.

Per-provider sources:

| Provider | `input` | `cacheRead` / `cacheWrite` | `reasoning` |
|---|---|---|---|
| `anthropic` | `input_tokens` + `cache_creation_input_tokens` + `cache_read_input_tokens` | `cache_read_input_tokens` / `cache_creation_input_tokens` | `output_tokens_details.thinking_tokens` |
| `openai-responses` | `input_tokens` | `input_tokens_details.cached_tokens` / `.cache_write_tokens` | `output_tokens_details.reasoning_tokens` |
| `openai-compatible` | `prompt_tokens` | `prompt_tokens_details.cached_tokens` / none | `completion_tokens_details.reasoning_tokens` |
| `gemini` | `promptTokenCount` | `cachedContentTokenCount` / none | `thoughtsTokenCount` (also added into `output`) |
| `bedrock` | `totalTokens` − `outputTokens`; without `totalTokens`, `inputTokens` + `cacheReadInputTokens` + `cacheWriteInputTokens` | `cacheReadInputTokens` / `cacheWriteInputTokens` | none |

The `anthropic` mapping is checked against live responses, and the
`openai-compatible` trailing-usage chunk against a captured LM Studio
stream. The OpenAI cache and reasoning details, and the
`openai-responses`, `gemini`, and `bedrock` mappings, follow each
provider's documented usage object and have not been probed live. In
particular, whether Bedrock's `inputTokens` includes the cache figures
is unconfirmed, which is why the input is derived from `totalTokens`
when present.

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
(success), red (error / verification_failed / verification_error /
tool_failures), yellow (max_turns / max_tokens / budget_exceeded /
stalled / timeout / context_exhausted),
grey (cancelled), or blue (anything unrecognised).

Each verification attempt prints on its own line: the 1-based attempt
number, `PASS` or `FAIL`, `err=` followed by the verifier's error when
it failed to produce a verdict, and the attempt's feedback, with
continuation lines indented. A run that retried verification therefore
shows every attempt, not only the last.

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
  tokens in / out:  18432 (cache read 12288) / 4116 (reasoning 950)
  tool calls:       45 (errors: 3)
  permission denials: 2
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

The `verifications` line counts attempts, not runs. When at least one
attempt failed to produce a verdict, the line ends with `, verifier
errors: N`, and the JSON form carries `verificationsRun`,
`verificationsPassed`, `verificationsFailed`, and `verificationErrors`,
each omitted when zero.

The parenthesised cache and reasoning shares appear only when
non-zero, and the matching `tokensCacheRead`, `tokensCacheWrite`, and
`tokensReasoning` JSON keys are omitted when zero. `stirrup trace
show` and the stderr summary of `stirrup harness` render the same
shares.

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
  "tokensCacheRead": 12288,
  "tokensCacheWrite": 2048,
  "tokensReasoning": 950,
  "toolCalls": 45,
  "toolErrors": 3,
  "permissionDenials": 2,
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
  debug log. The rest of the file is still read. `ReadRecording`
  returns the number of skipped lines as `RunRecording.SkippedLines`
  (`skippedLines` in JSON, omitted when zero) and logs a warning,
  because a non-zero count means the recording is partial. `stirrup-eval
  ingest` reports such a recording as partial on stderr and still stores
  it.
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
