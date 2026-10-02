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
| `diff-review`   | A model reviews the change the agent made to the workspace against `criteria` (see [below](#the-diff-review-judge)). Needs a judge-model credential. |
| `tool-trace`    | The run's `RunTrace.ToolCalls` satisfy a `tool_trace` block (see below). |
| `composite`     | Combines child `judges` with `require: "all"` or `require: "any"`, evaluating them in order and stopping at the first decisive one (see [below](#the-composite-judge)). |

All workspace-relative paths go through symlink-aware containment so
judges cannot escape the workspace.

#### The `diff-review` judge

`diff-review` asks a model whether the agent's change meets `criteria`.
It exists for outcomes a command or regex cannot check, such as "the
retry loop wraps the HTTP call without changing the public signature".

```hcl
judge {
  type     = "diff-review"
  criteria = "The change adds a retry loop around the HTTP call without changing the exported function signature."
  llm {
    provider    = "anthropic"
    model       = "claude-haiku-4-5-20251001"
    api_key_ref = "secret://ANTHROPIC_API_KEY"
  }
}
```

**What the model sees.** Before the agent runs, the runner commits the
workspace, after any `repo` clone and `files` seeding and with ignored
files included, to a judge-owned bare repository in a temporary
directory outside the workspace. Only tasks whose judge (or a nested
composite child) is `diff-review` get this baseline. After the run the
judge diffs the workspace against that commit through a temporary
index: edits, deletions, and new files, including ignored files and
files the agent committed. Git never reads the workspace's own `.git`,
so commits, resets, ignore rules, and attributes the agent creates do
not change what is reviewed, and seeded and cloned content is never
attributed to the agent. A task without `repo` presents no `.git` to
the agent. For these tasks the merged `runconfig.json` is written
outside the workspace so that it is not part of the diff.

The prompt holds the criteria, then the change summary
(`git diff --stat`, up to 100 files at 160 columns) and the diff inside
a data fence (see the security notes), then the instruction to copy the
call's nonce. A nested git repository in the workspace is an error,
since git cannot diff its content. An empty diff against a runner
baseline is an error, because the agent produced no reviewable change,
and the model is not called. Adding every file, ignored ones included,
makes baselining and capture slower in workspaces with large trees such
as `node_modules`.

**Baselines in replay.** `run --output DIR` retains each `diff-review`
task's baseline beside its trace, as `DIR/<suite>/<task>/judge-baseline.json`
and a `judge-baseline.bundle` git bundle. `replay --judge-baseline` with
that file diffs `--workspace` against the same commit. Without it,
`replay` falls back to the `HEAD` commit of the workspace's own
repository: the workspace must be the root of a git repository with at
least one commit, and an empty diff goes to the model as `(no changes)`,
since nothing records that the agent changed anything. Each record's
`baselineSource` is `runner`, `sidecar`, or `workspace-head`
accordingly.

**The `llm` block.** The block is accepted on `diff-review` judges only
and is rejected on every other type, in HCL and JSON suites alike.

| Field               | Default                   | Meaning                                                       |
|---------------------|---------------------------|---------------------------------------------------------------|
| `provider`          | `anthropic`               | `anthropic` (Messages API) or `openai-compatible` (Chat Completions). |
| `model`             | required                  | Model identifier.                                             |
| `base_url`          | provider default          | Endpoint root. Required for `openai-compatible`; an `http` or `https` URL with a host and no embedded credentials, subject to the [endpoint policy](#the-diff-review-judge). |
| `api_key_ref`       | `secret://ANTHROPIC_API_KEY` for `anthropic` at the Anthropic API; none otherwise | `secret://ENV_NAME` or `secret://file:///path`. Literal keys and `secret://ssm://` references are rejected at load time, and the error never repeats the value. An `anthropic` judge with any other `base_url` must name its own reference. An `openai-compatible` judge with no reference sends no credential. |
| `timeout_seconds`   | `30`                      | Bounds the whole call, retries and waits included. Maximum `300`; larger values are rejected. |
| `max_input_bytes`   | `65536`                   | Cap on the diff bytes sent to the model. The `--stat` summary is sent in addition and is not counted. |
| `max_tokens`        | `1024`                    | Output token cap. The `openai-compatible` client sends it as `max_completion_tokens` only, so servers that accept only `max_tokens` are unsupported. |
| `temperature`       | omitted                   | Sent only when set; some reasoning models reject it.          |
| `structured_output` | `json_schema`             | `json_schema` constrains the reply server-side to the verdict schema, including the call's nonce; `prompt_only` sends no schema and relies on the prompt and the reply parser, for endpoints without schema support. |
| `allow_truncated`   | `false`                   | Judge the head of an oversized diff instead of erroring. Unsafe against adversarial tasks: git orders the diff by path, so padding files can push the real change past the cap. |

References resolve in the eval process: `secret://NAME` reads the
environment variable `NAME` (letters, digits, and underscores, not
starting with a digit), and `secret://file:///path` reads a file.
`run`, `run --dry-run`, and `replay` resolve every distinct reference
and check every endpoint before any task runs (see
[`run`](#run--execute-a-suite)). The key never appears in suite files,
results, or verdict records.

**Defaults and the `--judge-*` flags.** A `diff-review` judge without an
`llm` block uses `anthropic`, `claude-haiku-4-5-20251001`, and
`secret://ANTHROPIC_API_KEY`. That default is a continuity choice for
existing suites, not a recommendation. The `--judge-provider`,
`--judge-model`, `--judge-base-url`, and `--judge-api-key-ref` flags on
`run` and `replay` replace it for every judge that lacks an `llm`
block; an explicit block always wins. A CI gate that authenticates
against a single provider should pass these flags so the judge uses the
gate's provider rather than a second credential:

```bash
./stirrup-eval run \
  --suite eval/suites/some-suite.hcl \
  --judge-provider openai-compatible \
  --judge-base-url https://openrouter.ai/api/v1 \
  --judge-api-key-ref secret://OPENROUTER_API_KEY \
  --judge-model openai/gpt-6-luna
```

The default key reference applies only to the `anthropic` provider with
an empty `base_url` or one whose host is `api.anthropic.com`, so the
Anthropic key is never sent to a gateway or another provider's endpoint
by default.

**Verdict contract.** The model answers with one JSON object holding
`nonce`, `reasoning`, `verdict` (`pass` or `fail`), and `feedback`; the
reasoning comes before the verdict so the decision follows the
analysis. The nonce is a fresh random value drawn for each call after
the diff is captured, stated outside the fence; in `json_schema` mode
the schema constrains `nonce` to that single value.

The reply parser applies the harness's nonce-extraction rules
(`harness/internal/jsonextract`; both implementations share the
vectors in `eval/judge/testdata/`). It scans the reply for balanced
top-level JSON objects and considers only those whose `nonce` equals
the call's nonce, so a verdict object echoed from the diff is never
selected. The selected object must hold exactly the four string
properties, with no duplicate or case-variant keys; an extra property
is an error in both modes, including from `prompt_only` models that add
fields. Parse statuses:

| `parseStatus`      | Meaning                                                              |
|--------------------|----------------------------------------------------------------------|
| `ok`               | Exactly one object carried the nonce and conformed.                  |
| `last_match`       | Several identical objects carried the nonce; the verdict is theirs.  |
| `no_json`          | The reply held no balanced JSON object.                              |
| `schema_violation` | No object carried the nonce, objects carrying it differed, the object carrying it did not conform, or the reply exhausted the 4 MiB scan budget. |
| `refusal`          | The model declined, including `content_filter` on Chat Completions. |
| `truncated_output` | The model stopped at `max_tokens`, or on any stop reason other than end of turn or a stop sequence, including none. |

Every status other than `ok` and `last_match` is an `error` verdict.

**Error versus fail.** `fail` means the model reviewed the change and
rejected it. Anything that prevents a verdict is `error`, never `fail`:
an unreachable or refused endpoint, a non-200 response once retries are
spent, a refusal or incomplete reply, an unparseable or non-matching
reply, a missing credential, a diff over `max_input_bytes` (unless
`allow_truncated`), an empty diff against a runner baseline, a nested
repository, or a replay workspace that is not a git repository. A task
whose `diff-review` judge errors reports outcome `error` with the
judge's verdict retained, so provider outages are not read as
regressions. When truncation is allowed, a note outside the fence
states the full diff size and how many bytes are shown.

**Retries.** HTTP 429 and 5xx responses are retried, up to three
attempts in total, after a jittered exponential backoff from 500 ms or
the provider's `Retry-After` (seconds or an HTTP date). All attempts and
waits share the `timeout_seconds` budget, and a wait that does not fit
in what remains ends the call at once. Other 4xx responses and
transport errors are not retried. Redirects are never followed; a 3xx
response is an error.

**Endpoint policy.** Unspecified, link-local, and multicast addresses
are refused, as are the cloud metadata services (`169.254.169.254`,
which is link-local, `fd00:ec2::254`, and `metadata.google.internal`).
When a key is attached, `http://` is accepted only to loopback and
private (RFC 1918 or ULA) addresses, so a local server such as LM Studio
or vLLM works without TLS but a key never crosses a public network in
cleartext; a public endpoint needs `https://`. Literal addresses are
checked at load time, hostnames are resolved and checked before any task
runs, and every connection is checked again on its resolved address, so
a hostname that rebinds to a refused address is caught. The client
honours `HTTPS_PROXY`, `HTTP_PROXY`, and `NO_PROXY`; behind a proxy the
connection-time check sees the proxy's address, so only the check before
the run applies to the endpoint itself.

**Provenance.** Each `diff-review` verdict carries a `record`
(`types.JudgeRecord`): provider, requested and served model, token
counts, latency, the SHA-256 and size of the full diff, whether it was
truncated, the baseline source, the parse status, the stop reason, and
a `configHash` over the prompt layout version, the system prompt, the
verdict schema template (without the per-call nonce), provider, model,
base URL (scheme, host, and path only), criteria, structured-output
mode, temperature, `max_tokens`, and `max_input_bytes`. Two verdicts
with equal hashes were produced under the same judge definition.
A `diff-review` judge nested in a `composite` keeps its record: each
entry in the composite's `details` carries the sub-judge's `status` and,
for LLM-backed sub-judges, its `record`, including when the sub-judge
errors.

**Security notes.**

- The diff is untrusted: the agent wrote it. The summary and diff sit
  inside a data fence whose markers carry a fresh 128-bit random nonce
  drawn after the diff is captured. Every run of three or more `<` in
  the diff is broken with a space, so the diff cannot forge a marker
  (merge-conflict markers reach the model altered), and the system
  prompt instructs the model to treat everything inside the fence as
  data. The fence matches the harness's data fence exactly; both
  implementations share the vectors in `eval/judge/testdata/`.
- These measures reduce, but do not eliminate, the chance that a
  crafted diff steers the verdict. Suites should pair `diff-review`
  with a deterministic judge (`test-command`, `file-contains`) under
  `composite` with `require = "all"` for outcomes that matter, listing
  the deterministic judge first so that its failure skips the model
  call.
- The eval host runs git over workspace content the agent wrote. The
  judge-owned repository, host and system git configuration replaced
  by empty files, disabled hooks, and a scrubbed environment keep these
  agent-controlled inputs from running commands or changing the diff:
  hooks and `core.hooksPath`, clean and smudge filters, `.git/config`,
  `.git/info/*`, replace refs, a `.git` file redirecting elsewhere,
  agent commits and resets, `-diff` and `binary` attributes, ignore
  rules, `core.fsmonitor`, external diff drivers and textconv, and the
  host's `~/.gitconfig` and `/etc/gitconfig`.
- Not covered: git parser vulnerabilities triggered by hostile
  workspace content; in-tree `.gitattributes` `eol`,
  `working-tree-encoding`, and `ident` settings, which change the bytes
  the model is shown; and, under the `local` executor, an agent writing
  anywhere the eval user can, including the judge's temporary
  directory. The `workspace-head` fallback in `replay` reads the
  workspace's own repository, which is treated as trusted,
  operator-supplied input.
- The full diff, including any secret the agent wrote into the
  workspace, is sent to the configured provider.
- The verdict reason is model-authored. Control characters, including
  ANSI escapes, become spaces and the reason is cut to 2 KiB before it
  reaches results, JUnit, or the terminal.
- Credentials are `secret://` references only. The resolved key (and
  its JSON- and Go-escaped forms, for keys of 8 bytes or more) is
  replaced with `[redacted]` in every provider-derived string that
  reaches a verdict: error bodies and messages, the model's reply, the
  served model, and the stop reason. Provider error text is flattened
  to one line and truncated to 1 KiB after redaction, and an error from
  a 3xx response omits its body.

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

#### The `composite` judge

A `composite` judge combines nested `judge` blocks. `require` is `all`
(the default) or `any`. The suite loader rejects a composite with no
nested judges and any other `require` value.

**Order matters.** Nested judges run in declaration order, and
evaluation stops at the first one that decides the outcome: the first
fail or error under `all`, the first pass under `any`. Judges after the
stop are not run, and their `details` entries have status `skipped`.
Cheap deterministic judges (`file-exists`, `file-contains`,
`tool-trace`, `test-command`) belong before `diff-review`, so that a
deterministic fail under `all`, or a deterministic pass under `any`,
spares the model call:

```hcl
judge {
  type    = "composite"
  require = "all"

  judge {
    type  = "file-exists"
    paths = ["retry.go"]
  }

  judge {
    type    = "test-command"
    command = "go test ./..."
  }

  judge {
    type     = "diff-review"
    criteria = "The change adds a retry loop without changing the exported signature."
  }
}
```

**Errors.** A nested judge that errors, such as an unreachable model
endpoint, does not abort the composite. How it counts depends on
`require`:

| `require` | Nested judge outcome | Effect on the composite                                                   |
|-----------|----------------------|---------------------------------------------------------------------------|
| `all`     | pass                 | Evaluation continues.                                                     |
| `all`     | fail                 | Composite is `fail`; remaining judges are skipped.                        |
| `all`     | error                | Composite is `error`; remaining judges are skipped. The composite cannot be known to pass. |
| `any`     | pass                 | Composite is `pass`; remaining judges are skipped.                        |
| `any`     | fail                 | Evaluation continues.                                                     |
| `any`     | error                | Evaluation continues; a later pass still makes the composite `pass`.      |

When no nested judge passes under `any`, the composite is `error` if at
least one nested judge errored and `fail` otherwise. When every nested
judge passes under `all`, the composite is `pass`. An `error` composite
has `passed: false`, and nested composites propagate their status in
the same way.

**Verdict.** The composite's `status` is always set. Its `reason` names
the deciding nested judge by 1-based position and type, and counts the
skipped judges when there are any, for example `sub-judge 2 of 3
(file-contains) failed (require all); 1 skipped`. For an `error`
decision the reason ends with the nested judge's error message.
`details` holds one entry per nested judge in declaration order, each
with `type`, `status` (`pass`, `fail`, `error`, or `skipped`), `passed`,
`reason`, and the `record` of an LLM-backed judge, which is kept for
error verdicts too. A nested composite appears as a single entry with
its own status and reason; its own `details` are not repeated. The
composite verdict itself has no `record`.

A misconfigured tree is not a verdict. An empty composite, an invalid
`require`, or an unknown judge type anywhere in the tree is reported as
an error before any nested judge runs.

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
| `--output`       | current dir      | Directory for `result.json` and per-task artifacts. Ignored under `--dry-run`, which writes no artifacts. |
| `--harness`      | `stirrup` on PATH| Harness binary to invoke for live runs.                      |
| `--concurrency`  | `1`              | Number of tasks executed in parallel. Workers preserve suite order in `result.json`. Values larger than the task count cap at `len(tasks)`. Concurrent invocations talking to the same provider hit rate limits faster — pick a value that respects your provider account's per-minute caps. |
| `--dry-run`      | `false`          | Validate the suite (and, when present, the merged per-task RunConfig via `ValidateRunConfig`), print the summary, and exit without writing `result.json` or JUnit XML. |
| `--model`        | empty            | Model to run every task with, forwarded to each harness invocation as `--model`. Overrides the harness default and any model pinned by the suite's `run_config` block. CI uses this to pin the per-push gate to a cheap model and the release sweep to stronger ones. |
| `--prompt-model` | empty            | Prompt model to render system prompts with, forwarded to each harness invocation as `--prompt-model`. The wire model is unchanged. See [Comparing prompts across models](#comparing-prompts-across-models). |
| `--provider`     | empty            | Provider type to run every task against, forwarded as `--provider`. Overrides the harness default and any provider pinned by the suite's `run_config` block. |
| `--base-url`     | empty            | API base URL for the `openai-compatible` / `openai-responses` providers, forwarded as `--base-url`. |
| `--api-key-ref`  | empty            | `secret://` reference for the provider API key, forwarded as `--api-key-ref`. A reference the harness resolves through `SecretStore` at runtime — never a literal key. |
| `--judge-provider` | empty          | Provider (`anthropic` or `openai-compatible`) for `diff-review` judges that have no `llm` block. Empty keeps the Anthropic default. See [The `diff-review` judge](#the-diff-review-judge). |
| `--judge-model`  | empty            | Model for `diff-review` judges without an `llm` block. Empty keeps the provider's built-in default. |
| `--judge-base-url` | empty          | API base URL for those judges. Required with `--judge-provider openai-compatible`. |
| `--judge-api-key-ref` | empty       | `secret://` reference for the judge key, resolved by the eval process. |

The `--judge-*` flags apply only to `diff-review` judges; the harness
never sees them. Before any task runs, including under `--dry-run`, the
runner resolves the configuration of every `diff-review` judge
(composite children included), resolves each distinct `api_key_ref`
once and discards the value, checks each endpoint against the
[endpoint policy](#the-diff-review-judge), and checks that `git`
is on `PATH`. A failure stops the invocation before any harness run and
names the task and the reference, never the key.

The three provider flags exist for the same reason as `--model`: the
provider a suite runs against is a property of the invocation, not of
the suite, so CI can retarget a provider-neutral suite without editing
suite files. Each is emitted independently, so an invocation may
override just the base URL. The per-push eval gate uses all four to run
`dogfood-seed.hcl` against OpenRouter:

```bash
./stirrup-eval run \
  --suite eval/suites/dogfood-seed.hcl \
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
- **`eval-gate`** — depends on `verify`. On every push it builds
  the binaries, runs each suite in `eval/suites/` that has a
  matching baseline in `eval/baselines/` (unbaselined suites are
  opt-in local runs), pins the model to GPT-5.6 Luna over OpenRouter
  via `stirrup-eval run`'s provider flags, compares each result to its
  baseline via `eval compare`, and uploads the result JSON as a build
  artifact. Authentication is the `OPENROUTER_API_KEY` repository
  secret; runs that cannot read it (fork clones, Dependabot-actor
  pushes) skip the live run with a warning rather than reporting a
  false regression.
- **`publish-container`** — depends on `verify`. On `main` pushes it
  publishes the harness Docker image to `ghcr.io/rxbynerd/stirrup`.

A non-zero exit from `compare` (regressions present) fails the gate.
At release time, `release.yml::eval-extended` re-runs the baselined
suites against stronger models (Claude Sonnet 5 and Claude Opus 4.8)
as a non-blocking-but-visible matrix: a regression turns the matrix
cell red without holding the release.

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

Baselines are keyed on `(suiteId, taskId)` outcomes only, so prompt
templating does not change baseline identity; regenerate a baseline
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
judges without re-running the harness; only `diff-review` judges
call a model.
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

`diff-review` judges replay against `--workspace`. With
`--judge-baseline`, naming a `judge-baseline.json` retained by
`run --output`, the diff is taken against the baseline commit the run
used; without it, against the `HEAD` of the workspace's own repository
(see [Baselines in replay](#the-diff-review-judge)). `replay` accepts
the same `--judge-*` flags as `run`, applies the same checks before
replaying, and records a judge that cannot rule as outcome `error` with
its verdict retained.

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
