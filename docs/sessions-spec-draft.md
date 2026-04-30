# Sessions: append-only persistence and `--continue`

**Status:** Draft 2. Decisions resolved for formal review.
**Scope:** opt-in local CLI follow-ups, gRPC follow-up continuity, and
session "teleporting" between environments.

---

## 1. Motivation

Today, when `stirrup harness` finishes its first user prompt the process
exits and the conversation is lost. There is no way to ask a local
follow-up against the same message history. The existing
`--followup-grace` flag keeps the gRPC transport open, but only the gRPC
control plane can deliver follow-up `user_response` events. More
importantly, `RunFollowUpLoop` currently mutates `config.Prompt` and calls
`Run` again, and `Run` rebuilds message history from the new prompt. That
is a new run, not a continuation.

We want three behaviours:

1. **Local follow-ups.** After an opt-in recorded run finishes,
   `stirrup harness --continue <session-id-or-path> --prompt "..."`
   resumes the same conversation with the prior message history.
2. **Correct gRPC follow-ups.** `RunFollowUpLoop` extends the existing
   in-memory message history instead of starting each follow-up from an
   empty conversation.
3. **Teleporting.** A session captured in environment A, such as a remote
   sandbox, can be copied to environment B, such as a laptop, and resumed
   there as long as B can satisfy the same non-secret `RunConfig`, provide
   required credential bindings, and point at a compatible workspace.

All three fall out of one primitive: an opt-in, append-only, on-disk log
of session events written as the run progresses.

---

## 2. Current state

| Concern | Today |
|---|---|
| Message history lifetime | Local variable in `Run` / `runInnerLoop`; gone when `Run` returns. |
| Existing `--trace` JSONL | One summary line at end with token totals and tool-call summaries. It is telemetry, not conversational state, and does not contain message content. |
| `--followup-grace` | `RunFollowUpLoop` sets `config.Prompt` and calls `Run`, which rebuilds `messages` from the prompt. Prior turns are dropped. |
| Existing recording types | `types.RunRecording` and `[]types.TurnRecord` describe post-hoc replay/eval records. They are useful reference points, but they are not a live session log and are not populated by the live loop. |
| Secret hygiene | `RunConfig.Redact()` exists for traces/recordings. Transports and logs scrub emitted strings. In-memory `messages` are not scrubbed because the model may need the exact content. |
| Tool-use correlation | `ToolUseID` couples `tool_use` and `tool_result` blocks. Provider APIs reject mismatched IDs, so continuation must round-trip them exactly. |
| Context management | `ContextStrategy.Prepare` may send a compacted `preparedMessages` slice to the provider, but the canonical `messages` slice remains the source of truth inside the loop. |
| Sub-agents | Run synchronously inside a tool call with isolated message history. The parent only needs to persist the `spawn_agent` `tool_use` and `tool_result` round trip. |

**Implication:** the missing pieces are a live session writer at the
message mutation points, a reader that hydrates canonical `[]types.Message`
state for `--continue`, and a loop entrypoint that can run from existing
messages.

---

## 3. Design Principles

1. **Opt-in only.** Session files may contain prompts, code, tool outputs,
   dynamic context, workspace paths, and secret-looking content. They are
   never written unless the user explicitly asks for session recording or
   continuation.
2. **Resumability wins over redacting message history.** The session log
   stores exact committed conversation messages, including tool outputs, so
   the next provider call receives a coherent continuation.
3. **Credential bindings stay outside the session file.** The file does not
   store provider API keys, resolved secret values, or provider secret
   references. Required credential bindings are supplied again through CLI
   flags, environment-backed defaults, or a `--config` file on resume.
4. **The log is conversation state, not exact replay state.** Sessions
   persist canonical messages. They do not persist every `preparedMessages`
   slice after context compaction, every text delta, or an exact provider
   input transcript.
5. **Committed turns are the recovery boundary.** A user message is durable
   once written. Assistant/tool output for a provider turn is only used on
   resume after the matching `turn_committed` event.

---

## 4. Session Log File

A session is one newline-delimited JSON file. Each line is a
self-describing event:

```json
{ "v": 1, "ts": "2026-04-30T12:34:56.123456789Z", "type": "event_type", "...": "..." }
```

Default location:

```text
${XDG_STATE_HOME:-$HOME/.local/state}/stirrup/sessions/<sessionId>.jsonl
```

For a fresh recorded run, `sessionId` is the run's first `runId`. Every
continued run generates a new `runId` and inherits the original
`sessionId`. If the user supplies `--session-file`, the file path is
explicit but the same ID rule still applies inside the header.

If `$HOME` cannot be resolved, the user must provide `--session-file`.
The parent session directory is created with mode `0700` if it does not
already exist. Session files are created with `O_CREAT|O_EXCL|O_NOFOLLOW`
and mode `0600`; the descriptor's mode is fixed via `fchmod` rather than a
path-based chmod, to avoid TOCTOU between open and chmod. A fresh
recording will not write to a pre-existing path; if the path exists the
CLI fails with a clear error and the user must remove the file or pick a
different path. For `--continue`, the file is opened with `O_NOFOLLOW` so
symlinks at the target path are refused.

Properties:

- **Append-only at event boundaries.** Writers append events; they never
  rewrite complete prior events. If recovery finds a partial final line,
  the next writer may truncate only that incomplete line before appending.
- **File-locked.** Opening a session for writing acquires an exclusive
  non-blocking OS file lock. A second writer fails fast with a clear error.
  v1 targets Linux and Darwin using `flock`/`LOCK_EX|LOCK_NB` via the
  standard library `syscall` package. On unsupported platforms, enabling
  sessions fails with a clear "session locking unsupported" error. `flock`
  is advisory: it binds cooperating writers (other `stirrup` processes)
  but does not prevent a hostile non-cooperating process from appending.
  Combined with `0700` on the directory and `0600` on session files, this
  is sufficient for the v1 single-user threat model — see §11.
- **Self-describing.** The first line is always a `header` event with
  schema version, session ID, harness version, and secretless config.
- **Crash-tolerant.** Readers ignore a partial final line and discard
  uncommitted assistant/tool output. A continuing writer records the byte
  offset after the last complete line and truncates to that offset before
  writing new events.
- **Durability checkpoints.** The writer issues a single append write per
  event. It calls `Sync()` after `header`, each `run_started`, each
  `user_message`, each `turn_committed`, and each `run_finished`. It does
  not sync high-volume mid-turn events because session v1 does not record
  text deltas.

Session files are sensitive local artifacts. The security perimeter for v1
is explicit opt-in, `0600` file permissions, and user-controlled file
placement. Encrypted session files are out of scope.

---

## 5. Event Schema v1

### 5.1 Shared Fields

All events include:

| Field | Meaning |
|---|---|
| `v` | Schema version. v1 for this spec. |
| `ts` | RFC3339Nano timestamp from the writer. |
| `type` | Event discriminator. |

Events that belong to a run include `runId`. Events that belong to a
provider turn include `turn`.

**Turn numbering is run-scoped.** The first provider turn in each run is
`turn=0`. A session may contain many runs, and each run may contain many
provider turns. A typical local follow-up run starts with one new user
message, then may have one or more assistant/tool-result turns before the
run finishes.

### 5.2 Events

| Type | Purpose | Required payload |
|---|---|---|
| `header` | First line. Identifies the session. | `sessionId`, `harnessVersion`, `createdAt`, `config`, `requiredSecretFields` |
| `run_started` | A new CLI invocation or gRPC follow-up began. | `runId`, `sessionId`, `config` (secretless effective config for this run), `configDrift?` (true when `--allow-config-drift` permitted otherwise-rejected changes; see §8.4), `driftedFields?` (list of config paths that differ from the header) |
| `user_message` | A user-role message was appended to canonical history. | `runId`, `source`, `message: types.Message` |
| `turn_started` | A provider turn began. | `runId`, `turn`, `model`, `provider` |
| `assistant_message` | The assistant message returned by the provider. | `runId`, `turn`, `message: types.Message` |
| `tool_result` | A tool call result was produced during a `tool_use` turn. | `runId`, `turn`, `toolUseId`, `toolName`, `result: types.ToolResult`, `durationMs` |
| `turn_committed` | Assistant and tool-result messages for this turn are valid to replay. | `runId`, `turn`, `modelStopReason`, `turnOutcome`, `tokens`, `toolResultCount` |
| `verification_result` | A verifier ran after an inner loop. | `runId`, `attempt`, `passed`, `feedback`, `details?` |
| `run_finished` | The run completed. | `runId`, `outcome`, `tokens`, `durationMs` |
| `writer_closed` | A writer exited without first emitting `run_finished` (signal, crash, context cancel, unrecoverable error). Not a terminal session marker. | `runId?`, `reason` |

`source` on `user_message` is one of:

- `initial_prompt`
- `followup_prompt`
- `grpc_user_response`
- `verifier_feedback`
- `compaction_summary` (reserved; v1 does not emit this. Defined now so
  that the planned compaction work in §14 can insert summaries into
  canonical history without bumping the schema version.)

`verification_result` is telemetry. It is not used directly to rebuild
message history. When verifier feedback is fed back to the model, the
actual appended feedback is recorded as a `user_message` with
`source="verifier_feedback"`. This avoids double-appending verifier text on
resume.

`writer_closed` is emitted only when a writer exits without first writing
`run_finished` for the active run, for example after SIGTERM, context
cancellation, or an unrecoverable error. Normal completions end with
`run_finished`, not `writer_closed`. The event is not terminal: a later
`--continue` appends after it in the same file.

When reconstruction coalesces an unanswered trailing user message with a
new follow-up prompt (see §6), the resulting message sent to the provider
contains content from both the prior `user_message` event and the new
one. Individual `user_message` events are therefore not standalone
provider messages — correct provider history is only obtained by running
the reconstruction algorithm.

### 5.3 Secretless Config in Header

The session header stores a config snapshot that is sufficient to describe
non-secret execution shape, but not credential bindings. The snapshot is
derived from `types.RunConfig` with these fields removed or zeroed:

- `RunID`, because run identity is carried by `run_started` and other run
  events
- `Session`, because session read/write state is runtime control data and
  must not recursively persist inside the header or `run_started` config
- `Provider.APIKeyRef`
- `Providers[*].APIKeyRef`
- `Provider.Credential.TokenSource.Path` and
  `Providers[*].Credential.TokenSource.Path`
- `Provider.Credential.TokenSource.EnvVar` and
  `Providers[*].Credential.TokenSource.EnvVar`
- `Executor.VcsBackend.APIKeyRef`
- `Tools.MCPServers[*].APIKeyRef` or equivalent secret-reference fields
- `Prompt`, because prompt content is represented by `user_message` events
- `DynamicContext`, because callers commonly inject ad-hoc tokens through
  this map and the prompt content the user actually saw is represented by
  `user_message` events anyway
- `SystemPromptOverride` and `PromptBuilder.Template`, because both are
  free-text fields that may carry sensitive instructions and are already in
  the rejected-without-`--allow-config-drift` set in §8.4 — there is no
  reason to round-trip them through the header

Provider credential metadata that is not a secret binding, such as
`Credential.Type`, `Credential.RoleARN`, `Credential.SessionName`,
`Credential.Audience` (the OIDC audience string is structural config, not
a credential value), `Region`, `Profile`, and `BaseURL`, stays in the
header unless a future provider marks a specific field as sensitive. URL
fields — `BaseURL`, `Executor.Proxy`, and `Tools.MCPServers[*].URI` —
must not contain URL userinfo or known secret query parameters; if any do,
the userinfo is stripped before serialisation and the field is added to
`requiredSecretFields` so the user supplies it explicitly on resume.

The header also stores `requiredSecretFields`, a list of config paths that
must be supplied externally on resume when that provider/tool/backend needs
a secret binding. Example:

```json
{
  "type": "header",
  "sessionId": "run-1777560000000000000",
  "harnessVersion": "dev",
  "requiredSecretFields": ["provider.apiKeyRef"],
  "config": {
    "runId": "",
    "mode": "execution",
    "provider": { "type": "anthropic" },
    "modelRouter": { "type": "static", "provider": "anthropic", "model": "claude-sonnet-4-6" }
  }
}
```

On resume, missing required secret fields are filled from explicit CLI
flags, `--config`, or defaults such as `--api-key-ref
secret://ANTHROPIC_API_KEY`. If a required binding is still missing, the
CLI fails before building the loop and names the missing field.

Path syntax for `requiredSecretFields` entries follows the JSON
representation of `RunConfig` (camelCase keys), with `.` for nested
objects and `[name]` for map entries:

- `provider.apiKeyRef`
- `providers[anthropic-prod].apiKeyRef`
- `provider.credential.tokenSource.path`
- `tools.mcpServers[github].apiKeyRef`
- `tools.mcpServers[internal].uri`

`requiredSecretFields` from the header is informational only, used to
build user-facing error messages when a binding is missing. The CLI
re-derives the authoritative set of required credential bindings from the
merged effective `RunConfig` by inspecting which provider, executor, and
tool types are selected and which of their credential fields remain empty
after merging external inputs. A tampered or empty `requiredSecretFields`
in a session header therefore cannot bypass credential prompting.

Common credential cases:

| Provider / credential mode | Header keeps | `requiredSecretFields` |
|---|---|---|
| Anthropic or OpenAI-compatible with static API key | provider type, model/router fields, base URL when non-sensitive | `provider.apiKeyRef` or `providers.<name>.apiKeyRef` |
| Bedrock with `aws-default` | region/profile and credential type | none; AWS environment/profile resolution happens at runtime |
| Bedrock with `web-identity` + file token source | region, role ARN, session name, token source type | `provider.credential.tokenSource.path` or provider-map equivalent |
| Bedrock with `web-identity` + env token source | region, role ARN, session name, token source type | `provider.credential.tokenSource.envVar` or provider-map equivalent |
| API executor backed by GitHub/GitLab | backend type, repo, ref | `executor.vcsBackend.apiKeyRef` |
| MCP server using an API key | server name/URL/tool shape | `tools.mcpServers[<name>].apiKeyRef` |

### 5.4 Example: No-Tool Turn

```json
{"v":1,"ts":"...","type":"run_started","runId":"run-2","sessionId":"run-1"}
{"v":1,"ts":"...","type":"user_message","runId":"run-2","source":"followup_prompt","message":{"role":"user","content":[{"type":"text","text":"Summarise what changed."}]}}
{"v":1,"ts":"...","type":"turn_started","runId":"run-2","turn":0,"model":"claude-sonnet-4-6","provider":"anthropic"}
{"v":1,"ts":"...","type":"assistant_message","runId":"run-2","turn":0,"message":{"role":"assistant","content":[{"type":"text","text":"I changed the parser and added tests."}]}}
{"v":1,"ts":"...","type":"turn_committed","runId":"run-2","turn":0,"modelStopReason":"end_turn","turnOutcome":"success","tokens":{"input":1234,"output":32},"toolResultCount":0}
{"v":1,"ts":"...","type":"run_finished","runId":"run-2","outcome":"success","tokens":{"input":1234,"output":32},"durationMs":1200}
```

### 5.5 Example: Tool-Use Turn

```json
{"v":1,"ts":"...","type":"turn_started","runId":"run-1","turn":0,"model":"claude-sonnet-4-6","provider":"anthropic"}
{"v":1,"ts":"...","type":"assistant_message","runId":"run-1","turn":0,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"main.go"}}]}}
{"v":1,"ts":"...","type":"tool_result","runId":"run-1","turn":0,"toolUseId":"toolu_1","toolName":"read_file","result":{"tool_use_id":"toolu_1","content":"package main\n","is_error":false},"durationMs":4}
{"v":1,"ts":"...","type":"turn_committed","runId":"run-1","turn":0,"modelStopReason":"tool_use","turnOutcome":"success","tokens":{"input":900,"output":25},"toolResultCount":1}
```

On reconstruction, the committed tool result becomes one user-role message
containing one or more `tool_result` content blocks, preserving the order in
which `tool_result` events appeared.

### 5.6 Example: Crashed Partial Turn

```json
{"v":1,"ts":"...","type":"user_message","runId":"run-3","source":"followup_prompt","message":{"role":"user","content":[{"type":"text","text":"Now run tests."}]}}
{"v":1,"ts":"...","type":"turn_started","runId":"run-3","turn":0,"model":"claude-sonnet-4-6","provider":"anthropic"}
{"v":1,"ts":"...","type":"assistant_message","runId":"run-3","turn":0,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_2","name":"run_command","input":{"cmd":"go test ./..."}}]}}
```

If the process crashes here, resume keeps the durable `user_message` and
drops the uncommitted assistant/tool output for `run-3` turn 0. This
preserves valid provider alternation. It does not make side effects
transactional; if a tool completed before the crash but the turn did not
commit, the workspace may still reflect that side effect.

---

## 6. Reconstruction Algorithm

On `--continue`:

1. Resolve `<session-id-or-path>` to a file. A bare ID resolves under the
   default session directory.
2. Acquire the exclusive writer lock before reading, because this process
   will append to the same file.
3. Read line by line and record the byte offset after the last complete
   line. Ignore one partial final line. A malformed complete line is an
   error.
4. Require a first-line `header` event. Reject unsupported schema versions
   with a clear upgrade message.
5. Reconstruct canonical `[]types.Message`:
   - Append every `user_message` immediately.
   - If a `user_message` would be adjacent to a previous user message with
     no committed assistant turn between them, coalesce it into the prior
     user message by appending its content blocks in order. This preserves
     valid provider alternation after crash recovery while keeping all
     durable user text.
   - Buffer `assistant_message` by `(runId, turn)`.
   - Buffer `tool_result` events by `(runId, turn)` in file order.
   - When `turn_committed` appears, append the buffered assistant message.
     If buffered tool results exist, append one user message containing
     those `tool_result` blocks.
   - Ignore `verification_result`, `run_started`, `run_finished`, and
     `writer_closed` for message reconstruction.
6. Drop any buffered assistant/tool events that did not receive
   `turn_committed`.
7. Validate the reconstructed history:
   - roles alternate in a provider-acceptable way after user-message
     coalescing;
   - every committed `tool_result` refers to a `tool_use` ID from the
     immediately preceding assistant message;
   - no committed assistant `tool_use` is missing a matching tool result;
   - message JSON is valid for the current `types.Message` schema.
8. Build the effective `RunConfig` for the new run using the policy in
   section 8.
9. If a partial final line was found, truncate the file to the recorded
   complete-line offset before appending new events.

Unknown additive event types are skipped after logging a warning. Unknown
required fields inside known v1 events are errors.

If the reconstructed history ends with an unanswered user message from an
incomplete run, the next `--continue --prompt "..."` does not replace or
drop that durable text. The new prompt is coalesced into the same trailing
user message for the next provider call and recorded as another
`user_message` event.

---

## 7. Loop Integration

Add a new package:

```go
// harness/internal/session/session.go
package session

type Recorder interface {
    Header(h Header) error
    RunStarted(runID string, cfg types.RunConfig) error
    UserMessage(runID string, source string, msg types.Message) error
    TurnStarted(runID string, turn int, model string, provider string) error
    AssistantMessage(runID string, turn int, msg types.Message) error
    ToolResult(runID string, turn int, toolName string, result types.ToolResult, durationMs int64) error
    TurnCommitted(runID string, turn int, commit TurnCommit) error
    VerificationResult(runID string, attempt int, result types.VerificationResult) error
    RunFinished(runID string, outcome string, totals types.TokenUsage, durationMs int64) error
    Close(reason string) error
}
```

Implementations:

- `JSONLRecorder`: production writer and reader support.
- `NoopRecorder`: disabled sessions, used by default. Its hot path should
  be allocation-free.

`AgenticLoop` gets a `Session session.Recorder` field. `BuildLoop` wires a
`JSONLRecorder` only when the config asks for session recording or the CLI
is continuing an existing session. Otherwise it wires `NoopRecorder`.

Refactor `Run` into an entrypoint that can operate on existing messages and
optionally append the user message that starts this run:

```go
type UserAppend struct {
    Source  string
    Message types.Message
}

func (l *AgenticLoop) RunWithMessages(
    ctx context.Context,
    config *types.RunConfig,
    messages []types.Message,
    appendUser *UserAppend,
) ([]types.Message, *types.RunTrace, error)
```

`Run` remains the fresh-run convenience wrapper: it builds the initial user
message from `config.Prompt` and calls `RunWithMessages` with
`appendUser.Source="initial_prompt"`.

`RunWithMessages` records events in this order:

1. `RunStarted`.
2. If `appendUser` is non-nil, append the user message to canonical
   history and record `UserMessage`. If canonical history already ends in a
   user message, append the new content blocks to that trailing message
   rather than creating an adjacent user message.
3. Run the existing verifier + inner-loop flow from that message slice.

Recorder call sites:

- At the start of each invocation: `RunStarted`.
- When appending the run-starting prompt through `appendUser`, or later
  verifier feedback inside the outer verification loop: `UserMessage`.
- After model selection and before provider streaming: `TurnStarted`.
- After a successful provider stream and `appendAssistantContent`:
  `AssistantMessage`.
- If `modelStopReason == "end_turn"`: `TurnCommitted` immediately with
  `toolResultCount=0`.
- If `modelStopReason == "tool_use"`: record each `ToolResult` after the
  tool returns (in dispatch order, which writers must preserve so the
  Anthropic API's required `tool_result`-block ordering is honoured),
  then call `TurnCommitted` once every `tool_use` block in the assistant
  message has a matching `tool_result` appended.
- If a stall, tool failure cap, or budget cutoff fires after every
  `tool_use` block in the assistant message has a matching `tool_result`
  (e.g., the third tool call's result triggered a budget cap), call
  `TurnCommitted` with `modelStopReason="tool_use"` and `turnOutcome` set
  to the terminating outcome. The turn is durable and the run terminates.
- If a stall, tool failure cap, or budget cutoff interrupts the dispatch
  loop partway through a turn — leaving one or more `tool_use` blocks
  without a matching `tool_result` — do **not** commit that turn.
  Reconstruction (§6 step 7) would reject mismatched turns. The
  terminating outcome is reflected in `run_finished`; the assistant
  message and any partial tool-result events remain on disk for forensics
  but are dropped on resume just like a crashed turn.
- If provider streaming fails before assistant content is appended: do not
  commit that turn.
- After each verifier execution: `VerificationResult`. If verifier feedback
  is appended for another attempt, also record that appended feedback as a
  `UserMessage`.
- When `RunWithMessages` finishes: `RunFinished`.
- `AgenticLoop.Close`: `Session.Close("done")`, or a more specific reason
  when the caller knows it (`signal`, `timeout`, `error`).

### gRPC Follow-Up Flow

`RunFollowUpLoop` keeps the message slice returned by the primary
`RunWithMessages` and tracks the active `runId`/`sessionId`. On each
`user_response`:

1. Validate that the inbound event correlates to the active session/run
   (e.g., its `runId` field, if present, matches the most recent run we
   sent). Mismatched events are logged and discarded rather than appended
   — a misrouted or replayed `user_response` from an unrelated run must
   not be folded into this session's history.
2. Generate a new `runId` for the follow-up turn.
3. Call `RunWithMessages` with the current slice and `appendUser` set to
   the response message with `source="grpc_user_response"`.
4. Store the returned message slice for the next follow-up.

This preserves observable gRPC behaviour while making follow-ups true
conversation continuations.

---

## 8. CLI and Config Semantics

### 8.1 CLI Surface

```text
stirrup harness [flags] [prompt]
  --session-record             Opt in to recording a fresh session.
  --session-file <path>        Fresh-run output path. Implies --session-record.
  --continue <session-id|path> Continue and append to an existing session.
  --allow-config-drift         Permit otherwise rejected resume-time config changes.
```

Recording is off by default. `--session-file` is explicit opt-in. There is
no automatic "record every run" mode in v1.

`--continue` requires a value. Continuing the most recent session without
an explicit ID/path is out of scope for v1 because it requires a session
index and makes shell parsing more ambiguous.

`--continue` requires a new prompt for v1, supplied either by `--prompt` or
the positional prompt. If neither is present, the CLI errors:

```text
prompt is required when using --continue: pass an argument or --prompt
```

### 8.2 Fresh Run Behaviour

A fresh run with `--session-record`:

1. Creates a new session file with `0600`, or accepts an existing empty
   file.
2. Writes `header`.
3. Writes `run_started`.
4. Writes the initial `user_message`.
5. Appends all committed events as the run progresses.
6. Prints `Session: <path>` to stderr at the end of the normal run summary.

If `--session-file` points at an existing non-empty file, the CLI refuses
to treat it as a fresh run unless a future explicit fork/branch flag is
introduced. Only `--continue` appends to an existing session file.

### 8.3 Continue Behaviour

`--continue <session-id-or-path> --prompt "..."`

1. Loads and reconstructs committed history from the session file.
2. Builds an effective `RunConfig` for the new run.
3. Builds the loop and recorder.
4. Calls `RunWithMessages` with the reconstructed history and
   `appendUser.Source="followup_prompt"`.
5. Appends to the same session file.

Recovery from a crashed run is implicit. Uncommitted assistant/tool events
remain on disk for forensics, but are ignored by reconstruction.

### 8.4 Config Merge Policy on Resume

The session header records the non-secret execution shape from the original
run. The merge for a continued run is allowlist-driven so that a tampered
or attacker-supplied header cannot inject free-text content (e.g., a
malicious `SystemPromptOverride` or `PromptBuilder.Template`) the user did
not author. For each continued run:

1. Start with a fresh default `RunConfig`.
2. Apply structural fields from the header config that match the safe
   allowlist below: type selectors (provider, model router, executor, edit
   strategy, verifier, permission policy, git strategy, prompt builder,
   context strategy, transport), numeric limits, model identity, non-text
   router config, and `Executor.Workspace`. Free-text fields
   (`SystemPromptOverride`, `PromptBuilder.Template`, verifier criteria
   strings, `DynamicContext`) are never copied from the header — they must
   come from `--config` or explicit flags on resume if the user wants
   them.
3. Generate a fresh `RunID`.
4. Set `Prompt` from `--prompt` or positional prompt.
5. Apply external credential bindings from explicit CLI flags, `--config`,
   and environment-backed defaults.
6. Apply safe run-scoped overrides (see allowlist below).
7. Validate the final `RunConfig`.

Allowed without `--allow-config-drift`:

- `Prompt`
- `RunID`
- `LogLevel`
- `Timeout`
- `MaxTurns`, only if increasing. Decreasing `MaxTurns` mid-conversation
  can truncate an in-progress reasoning chain in ways that are hard to
  diagnose; users who genuinely want a tighter cap can pass
  `--allow-config-drift` and acknowledge the change. The token and cost
  budgets below are symmetric because budget exhaustion produces a clean
  termination outcome regardless of direction.
- `MaxTokenBudget`
- `MaxCostBudget`
- `TraceEmitter.*`
- `Executor.Workspace`
- credential binding fields listed by `requiredSecretFields`

Rejected without `--allow-config-drift`:

- `Mode`
- `Provider.Type`
- `Providers` shape, except credential bindings
- `ModelRouter` type/provider mappings/model choices
- `PromptBuilder`
- `ContextStrategy`
- `Executor.Type`
- `EditStrategy`
- `Verifier`
- `PermissionPolicy`
- `GitStrategy`
- `Transport`
- `Tools.BuiltIn`
- `Tools.MCPServers` shape, except credential bindings
- `SystemPromptOverride`
- `DynamicContext`

With `--allow-config-drift`, the CLI may allow these changes after printing
a warning that the continued run may not behave like the original
conversation. The final config must still pass `types.ValidateRunConfig`.
Security invariants are never bypassed.

When `--allow-config-drift` permits a change, the resulting `run_started`
event sets `configDrift=true` and `driftedFields=[...]` listing each
config path whose value differs from the header. Workspace-only changes
via `--workspace` (sec 8.5) are not considered drift and do not trigger
this flag. The audit field gives readers a clear signal that the
continued run diverged structurally from the recorded conversation, which
matters for forensics and any future tooling that walks the log.

### 8.5 Workspace Path on Teleport

If the header workspace path does not exist on the current host, `--continue`
requires an explicit `--workspace`. Changing only `Executor.Workspace` is a
safe resume override because teleporting is a first-class goal.

Changing executor type, for example `container` to `local`, is config drift
and requires `--allow-config-drift`.

### 8.6 Provider Secrets on Resume

Session files do not persist provider secret values or provider secret
references. The user must provide credential bindings at resume time by one
of these routes:

- default CLI flags, such as `--api-key-ref secret://ANTHROPIC_API_KEY`;
- explicit CLI flags, such as a different `--api-key-ref`;
- a `--config` file that supplies the required secret reference fields;
- provider credential modes that need no secret reference, such as a
  Bedrock `aws-default` environment.

If the provider cannot be built after merging these inputs, resume fails
before `run_started` or the follow-up `user_message` is appended.

---

## 9. Context Compaction Semantics

The session log stores canonical message history: the same logical history
that `runInnerLoop` mutates with `appendAssistantContent`,
`appendToolResults`, and verifier feedback.

It does not store:

- provider stream text deltas;
- the `preparedMessages` slice after sliding-window truncation;
- generated summary text from `summarise`, except when that summary is
  actually inserted into canonical history in a future implementation;
- offloaded file payload metadata beyond the resulting tool output text in
  canonical history.

On resume, the configured `ContextStrategy` prepares the reconstructed
canonical messages for the next provider call. This is sufficient for
conversation continuation, but not for exact replay/eval. Deriving
`types.RunRecording` from a session file is future work and would not be
lossless without additional provider-input events.

---

## 10. Interaction With Existing Components

| Component | Change |
|---|---|
| `core.AgenticLoop` | Add `Session session.Recorder`; call it at message mutation and run lifecycle sites. |
| `core.Run` | Becomes a wrapper over `RunWithMessages` for fresh prompts. |
| `core.RunWithMessages` | New shared execution path that accepts and returns canonical message history. |
| `core.RunFollowUpLoop` | Maintains returned message history across gRPC follow-ups. |
| `transport.StdioTransport` | Unchanged for live events. Local `--continue` is a single-shot CLI invocation, not a stdin read loop. |
| `types.RunConfig` | Add `Session SessionConfig`; mirror in `proto/harness/v1/harness.proto`. |
| `types.RunRecording` | Unchanged. Sessions and replay/eval recordings remain separate concerns. |
| `lakehouse.FileStore` | Unchanged. Lakehouse stores completed traces/recordings, not local session state. |
| Sub-agents | Do not inherit the parent recorder in v1. The parent records only the `spawn_agent` tool call/result. |

Proposed `SessionConfig`:

```go
type SessionConfig struct {
    Type              string `json:"type,omitempty"` // "none" | "jsonl"
    FilePath          string `json:"filePath,omitempty"`
    Continue          bool   `json:"continue,omitempty"`
    AllowConfigDrift  bool   `json:"allowConfigDrift,omitempty"`
}
```

`SessionConfig` is runtime control state. It is valid in the effective
`RunConfig` used to build the loop, but it is omitted from the session
header and `run_started.config` snapshots.

---

## 11. Security and Privacy

Session files are intentionally more sensitive than traces. They can contain
full prompts, model responses, tool results, command output, file contents,
dynamic context, workspace paths, and secret-looking strings found in the
workspace.

v1 security decisions:

- Sessions are opt-in only.
- The session directory is created with mode `0700`.
- Files are written `0600` using `O_CREAT|O_EXCL|O_NOFOLLOW`; a
  pre-existing path causes a fresh recording to fail rather than be
  reused. The descriptor's mode is set via `fchmod` rather than a
  path-based chmod to avoid TOCTOU.
- Credential values are never written.
- Provider secret references are not written; they are supplied on resume.
- Tool outputs are written verbatim to preserve resumability (see §12).
- Session paths are local filesystem paths only.
- The session writer uses file locking to prevent concurrent corruption.
  `flock` is advisory: it binds cooperating writers but does not prevent
  a hostile non-cooperating process from appending. The v1 threat model
  assumes single-user file ownership and trusts that the file has not
  been modified by a third party between writes — the same assumption
  that underpins workspace file integrity. Tamper-resistant session files
  (HMAC, signatures, encryption) are out of scope for v1.
- Sessions are supported on Linux and Darwin in v1. Unsupported platforms
  fail before writing a session file.

This means a session file should be handled like a local workspace snapshot.
Encrypted session files, remote stores, keychain integration, and content
addressed external payload storage are out of scope for v1.

**Disposal.** Session files accumulate; they are not garbage-collected.
Operators should periodically remove old sessions from the default
directory. For sensitive sessions (anything that touched production
secrets, customer data, or PII), prefer `shred` (Linux) or `rm -P`
(macOS) over plain `rm`. A retention window of 30 days is a reasonable
default for sessions kept for resume; older sessions should be removed.
A future `stirrup session rm <id>` command may automate this.

---

## 12. Tool Output Bloat

Tool outputs can be large. Existing executor/tool caps still limit single
outputs, but a long session can produce a large JSONL file.

v1 stores committed tool outputs verbatim because exact continuation is the
goal. Future work may add content-addressed payload files or a compaction
command, but v1 should not introduce partial redaction/truncation that makes
conversation state diverge from what the model already saw.

---

## 13. Schema Versioning

Readers support v1. Unknown `v` fails with a clear message:

```text
unsupported session schema vN; upgrade stirrup or export the session with a compatible version
```

Forward-compatible changes must be additive, with a distinction between
event types that older readers may safely skip and those they must not:

- Telemetry-only event types (e.g., new metrics, lifecycle hints) can be
  skipped by older readers without affecting message reconstruction.
- Optional fields on existing events can be ignored.
- New event types that contribute to canonical message reconstruction
  must carry `"reconstructionCritical": true` in their payload. An older
  reader that encounters a reconstruction-critical event type it does not
  recognise must fail with a clear "session contains
  reconstruction-critical events from a newer schema" error rather than
  silently skipping. This lets future features — compaction summaries
  inserted into canonical history (see the `compaction_summary` source
  in §5.2 and the future-work note in §14), sub-agent capture, payload
  offload — ship as additive events without a schema bump while
  preventing older readers from producing a wrong reconstruction.
- Changes to the meaning of existing v1 events, or to the reconstruction
  algorithm in ways that affect existing event types, require `v=2`.

---

## 14. Out of Scope for v1

- Cross-conversation merging or branch/fork semantics.
- Continuing the most recent session without an explicit ID/path.
- Interactive `--continue` that waits for stdin when no prompt is passed.
- A web/IDE UI for browsing sessions.
- External session stores such as S3 or Postgres.
- Sub-agent session capture.
- Session compaction or log rotation.
- Encrypted session files.
- Exact provider-input replay/eval from session files.
- Redacting or truncating committed message history.

---

## 15. Acceptance Checklist

- [ ] `session.Recorder`, `JSONLRecorder`, and `NoopRecorder`.
- [ ] Session reader reconstructs canonical messages from committed events.
- [ ] Crash-safety tests cover partial final line, uncommitted assistant
      message, uncommitted tool result, and committed no-tool `end_turn`.
- [ ] Continuing writer truncates only the partial final line before
      appending new events.
- [ ] Session ID policy is implemented: first run ID becomes `sessionId`;
      continued runs get fresh run IDs and inherit that session ID.
- [ ] `turn_committed` is written for no-tool turns and tool-use turns.
- [ ] Stall/tool-failure/budget exits commit only when every assistant
      `tool_use` block has a matching `tool_result`; otherwise the turn
      is treated as uncommitted and dropped on resume.
- [ ] `tool_result` events are written in the same order as the
      corresponding `tool_use` blocks in the assistant message.
- [ ] `writer_closed` is emitted only for abnormal exits without a
      preceding `run_finished`.
- [ ] gRPC `user_response` events that fail run/session correlation are
      logged and discarded rather than appended to message history.
- [ ] Adjacent durable user messages are coalesced during reconstruction and
      when appending a follow-up to an unanswered trailing user message.
- [ ] Verifier feedback is not duplicated: `verification_result` is telemetry,
      and fed-back text is replayed only through `user_message`.
- [ ] Tool-use/tool-result ID validation catches missing, mismatched, or
      out-of-order committed results.
- [ ] `RunWithMessages` returns updated messages; `Run` and
      `RunFollowUpLoop` both use it.
- [ ] gRPC follow-up test proves the second run sees primary-run history.
- [ ] CLI flags: `--session-record`, `--session-file`, `--continue`, and
      `--allow-config-drift`.
- [ ] `--continue` requires an explicit session ID/path and a prompt in v1.
- [ ] Fresh recording is opt-in only; no default session file is written when
      no session flag is present.
- [ ] Fresh recording refuses to write to a non-empty existing file; only
      `--continue` appends to an existing session.
- [ ] Header/run-start config snapshots omit `RunID`, `Prompt`, `Session`,
      `DynamicContext`, `SystemPromptOverride`, `PromptBuilder.Template`,
      and secret binding fields. URL fields (`BaseURL`, `Executor.Proxy`,
      `Tools.MCPServers[*].URI`) strip userinfo before serialisation and
      appear in `requiredSecretFields` when stripped. Required secret
      fields are supplied externally on resume.
- [ ] `requiredSecretFields` from a tampered header cannot bypass
      credential prompting; the authoritative set is re-derived from the
      merged effective config.
- [ ] Missing required credential bindings fail before the provider is built.
- [ ] Credential binding cases cover static provider API keys, Bedrock
      `aws-default`, Bedrock web identity file/env token sources, API
      executor tokens, and MCP API keys.
- [ ] Config merge for continued runs starts from a fresh default
      `RunConfig` and copies only the safe structural allowlist from the
      header; free-text fields (`SystemPromptOverride`, `PromptBuilder.Template`,
      verifier criteria, `DynamicContext`) are never carried over from a
      session file automatically.
- [ ] Config-drift validator names offending fields and honours the safe
      allowlist.
- [ ] When `--allow-config-drift` permits a rejected change, the
      `run_started` event sets `configDrift=true` and lists the changed
      paths in `driftedFields`. Workspace-only changes via `--workspace`
      do not trigger the flag.
- [ ] Teleport test: session recorded under one workspace path resumes with an
      explicit different `--workspace`.
- [ ] File locking prevents two writers from appending concurrently.
- [ ] Unsupported session-locking platforms fail clearly before writing.
- [ ] Session files are created with `O_CREAT|O_EXCL|O_NOFOLLOW` and mode
      `0600` via `fchmod`; pre-existing paths cause a fresh recording to
      fail.
- [ ] Session directory is created with mode `0700`.
- [ ] `--continue` opens the file with `O_NOFOLLOW` and refuses symlinks
      at the target path.
- [ ] Unknown schema version errors. Unknown telemetry-only event types
      are skipped with a warning. Unknown event types carrying
      `reconstructionCritical: true` cause the reader to fail with a
      clear upgrade message.
- [ ] `RunConfig.Session` and proto mirror are added.
- [ ] Documentation is promoted to `docs/sessions.md` and CLI flag tables are
      updated.
