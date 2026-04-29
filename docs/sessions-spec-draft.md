# Sessions: append-only persistence and `--continue`

**Status:** Draft 1. To be refined before implementation.
**Scope:** local CLI follow-ups (stdio transport) and session "teleporting"
between environments.

---

## 1. Motivation

Today, when `stirrup harness` finishes its first user prompt the process
exits and the conversation is lost. There is no way to follow up
locally — the existing `--followup-grace` flag keeps the gRPC transport
open, but only the gRPC control plane can deliver follow-up
`user_response` events, and even then `RunFollowUpLoop`
(`harness/internal/core/loop.go:584`) discards prior history and starts
a fresh run for each follow-up (see §2).

We want two things:

1. **Local follow-ups.** After a run finishes, `stirrup harness
   --continue <session-id-or-path> --prompt "…"` resumes the same
   conversation with full message history.
2. **Teleporting.** A session captured in environment A (e.g. a remote
   sandbox) can be moved to environment B (e.g. a laptop) and resumed
   there, as long as B can satisfy the same `RunConfig` (provider, tools,
   workspace).

Both fall out of the same primitive: a continuous, append-only,
on-disk log of session events written as the run progresses.

---

## 2. Current state (codebase assessment)

| Concern | Today |
|---|---|
| Message history lifetime | Local variable in `runInnerLoop`; gone when `Run` returns. |
| Existing `--trace` JSONL | One summary line at end (token totals, tool-call summaries). **Not** a session log — does not contain message content. |
| `--followup-grace` (`RunFollowUpLoop`) | Calls `loop.Run(ctx, config)` per follow-up, which calls `buildMessages(config.Prompt)` (`loop.go:107`). Prior turns are dropped. Despite the name, this is not a continuation. |
| Persistent session schema | `types.RunRecording` + `[]types.TurnRecord` already exist (`types/runtrace.go:64,89`) and are consumed by `ReplayProvider`, `ReplayExecutor`, and `lakehouse.FileStore`. They are written post-hoc, not populated by the live loop. |
| Secret hygiene | `RunConfig.Redact()` exists; `LogScrubber`/`StdioTransport.Emit` already scrub strings before write. |
| Tool-use correlation | `ToolUseID` couples `tool_use` and `tool_result` blocks. Provider APIs (Anthropic, OpenAI) reject mismatched IDs, so replay must round-trip them verbatim. |
| Sub-agents | Run synchronously inside a tool call; have isolated message history; do not need to appear in the parent session log (only the parent's `tool_use`/`tool_result` for `spawn_agent` does). |

**Implication:** the schema and replay machinery already exist. The
missing piece is a writer that emits session events *during* the live
run, plus a reader that can hydrate state on `--continue`.

---

## 3. Proposed approach

### 3.1 Session log file

A session is a single append-only newline-delimited JSON file. Each
line is a self-describing event. The file is written on the local
filesystem at `~/.stirrup/sessions/<session-id>.jsonl` by default;
`--session-file <path>` overrides the location.

Properties:

- **Append-only.** Writers only append. Readers tolerate truncation at
  any record boundary (last line may be partial after a crash).
- **Self-describing.** First line is a `header` event carrying schema
  version, session ID, redacted RunConfig, and harness version. No
  out-of-band metadata files.
- **Resumable mid-run, not just post-run.** Even if the harness
  crashes mid-turn, a partial session can be loaded; the
  reconstruction logic stops at the last fully-committed turn.
- **Crash-safe writes.** Each event is marshalled, a single `write`
  with a trailing `\n` is issued, and the file handle is `Sync()`-ed
  after `header` and after each `turn_committed` event. Mid-turn
  `text_delta` events are not synced (volume too high).

### 3.2 Event schema (v1)

All events share:

```json
{ "v": 1, "ts": "<RFC3339Nano>", "type": "<eventType>", ... }
```

Event types (proposed):

| Type | Purpose | Payload |
|---|---|---|
| `header` | First line. Identifies the session. | `sessionId`, `runId`, `harnessVersion`, `config` (redacted), `parentSessionId?`, `createdAt` |
| `user_message` | A user prompt was added to history (initial or follow-up). | `message: types.Message` (role=user, single text block) |
| `turn_started` | A model turn began. | `turn`, `model`, `provider` |
| `assistant_message` | The assistant's response for this turn (text + tool_use blocks). | `turn`, `message: types.Message` (role=assistant) |
| `tool_result` | A tool call result was appended to history. | `turn`, `toolUseId`, `toolName`, `output`, `isError`, `durationMs` |
| `turn_committed` | All blocks for this turn are now in history; safe checkpoint. | `turn`, `stopReason`, `tokens` |
| `verifier_feedback` | Verifier emitted feedback that was fed back as a user message. | `attempt`, `feedback`, `passed` |
| `run_finished` | The run completed (success/error/max_turns/etc). | `outcome`, `tokens` (total), `durationMs` |
| `session_closed` | Final entry; the writer is shutting down cleanly. | `reason` ("done"\|"signal"\|"timeout") |

A "turn" is bracketed by `turn_started` … `turn_committed`. Anything
between those that does not reach `turn_committed` is considered
incomplete and is discarded on resume.

### 3.3 Reconstruction algorithm

On `--continue`:

1. Open file; require a `header` on line 1; check `v` is supported.
2. Reconstruct `[]types.Message` by replaying events in order:
   - `user_message` → append.
   - `assistant_message` → append.
   - `tool_result` → buffer until the corresponding `turn_committed`,
     then append as a `tool_result` block in a user message.
   - `verifier_feedback` (when `passed=false`) → append the feedback
     as a user text message.
3. **Discard incomplete tail.** Any events after the last
   `turn_committed` that did not themselves complete a turn are
   dropped. This guarantees the resumed history is a valid
   user/assistant alternation that the provider API will accept.
4. Validate the redacted `RunConfig` from the header against the
   user-supplied flags (or reuse the header's config when no
   conflicting flags are passed). On conflict, fail loudly — see §6.4.

### 3.4 Wiring inside the loop

**One new component, behind an interface, integrated where messages
are mutated.**

```go
// harness/internal/session/session.go (proposed)
package session

type Recorder interface {
    Header(h Header) error
    UserMessage(turn int, msg types.Message) error
    TurnStarted(turn int, sel router.ModelSelection) error
    AssistantMessage(turn int, msg types.Message) error
    ToolResult(turn int, r types.ToolResult, name string, durationMs int64) error
    TurnCommitted(turn int, stopReason string, tokens types.TokenUsage) error
    VerifierFeedback(attempt int, feedback string, passed bool) error
    RunFinished(outcome string, totals types.TokenUsage, durationMs int64) error
    Close(reason string) error
}
```

Implementations:

- `JSONLRecorder` — the production writer described above.
- `NoopRecorder` — used when sessions are disabled (preserves the loop's
  pure-interface property and keeps tests trivial).

The recorder is set on `AgenticLoop` (new field `Session
session.Recorder`) and called at exactly the points where `messages`
is mutated in `runInnerLoop` and `Run`:

- `Run` after `buildMessages` → `UserMessage(0, …)`
- After `appendAssistantContent` → `AssistantMessage(turn, …)`
- After each tool call result is computed (loop body) →
  `ToolResult(turn, …)`
- After `appendToolResults` → `TurnCommitted(turn, …)`
- The verifier-feedback append → `UserMessage(…, feedback)` plus
  `VerifierFeedback(…)`
- After `Run` finishes → `RunFinished(outcome, …)`

`AgenticLoop.Close` calls `Session.Close("done")`.

### 3.5 Follow-up flow (local + gRPC, unified)

The existing `RunFollowUpLoop` is the right shape but mis-implements
continuation: it overwrites `config.Prompt` and calls `Run` again,
which rebuilds `messages` from scratch.

We restructure as follows:

- `Run` accepts an *initial messages* argument (or `Run` reads the
  initial state from `loop.Session` if continuing). The simplest
  refactor is to add a sibling method `RunWithMessages(ctx, config,
  []types.Message) (*RunTrace, error)` and have `Run` call it after
  building from `config.Prompt`.
- `RunFollowUpLoop` keeps the existing message buffer between calls
  and appends each `user_response` as a new user message before
  calling `RunWithMessages`.
- For the local CLI follow-up case (no gRPC control plane), see §4.

This unifies stdio and gRPC: both paths now extend an in-memory
message slice and persist it via the same recorder.

### 3.6 Teleporting

Teleport = "transfer a session file from machine A to machine B and
resume." This works as long as B's environment satisfies the session's
`RunConfig`: provider credentials are resolvable, the workspace path
exists (or is overridden), and any container/api executor backing
state is reachable.

The session file alone is enough — the redacted `RunConfig` in the
`header` event is the source of truth for "how was this run
configured." The user re-supplies secrets (via `secret://` refs as
normal); they are never written to the session file.

---

## 4. CLI surface

```
stirrup harness [flags] [prompt]
  --session-record               Enable session recording (off by default; auto-on
                                 with --session-file).
  --session-file <path>          Path to write/read the session log
                                 (default: ~/.stirrup/sessions/<runId>.jsonl).
  --continue [<session-id|path>] Continue an existing session. With no argument,
                                 resume the most recent session in the default dir.
                                 Mutually exclusive with --session-record on a fresh
                                 run, but composes with --session-file (which then
                                 specifies an explicit path to continue).
```

Behaviour:

- A fresh run with `--session-record` writes the header, then events,
  prints `Session: <path>` to stderr at the end of the summary so the
  user can copy/paste it for `--continue`.
- `--continue` without `--prompt`: loads history, starts the
  conversation in "waiting for prompt" mode. For stdio that means
  reading one user prompt from stdin or rejecting (TBD — see §6.2).
- `--continue --prompt "…"`: loads history, appends the prompt as a
  new `user_message`, runs one outer loop iteration, persists, exits.
  This is the canonical "ask a follow-up locally" workflow.
- `--continue` always *appends* to the existing file. Recovery from a
  crashed run is implicit: stale incomplete events at the tail are
  discarded on read but kept on disk for forensic purposes.

Precedence with `--config`:

- Header config + `--continue` is the base.
- Explicit flags override individual fields exactly as in the existing
  flag-vs-config logic (`harness/cmd/stirrup/cmd/harness.go:241`).
- Conflicts where the override would invalidate the persisted history
  (e.g. switching mode from `execution` to `research`, which changes
  permitted tools) fail validation — see §6.4.

---

## 5. Interaction with existing components

| Component | Change |
|---|---|
| `core.AgenticLoop` | New `Session session.Recorder` field; `BuildLoop` wires `JSONLRecorder` when configured, otherwise `NoopRecorder`. |
| `core.Run` | Calls recorder at message-append sites. Accepts initial messages when continuing. |
| `core.RunFollowUpLoop` | Refactored to maintain the running message slice across follow-ups and call `RunWithMessages`. |
| `transport.StdioTransport` | Unchanged for live runs. Local-follow-up path operates without a transport read loop (single shot per `--continue` invocation). |
| `types.RunConfig` | Add `Session SessionConfig` (Type=`jsonl`/`none`, FilePath, etc.). Mirror in `proto/harness/v1/harness.proto`. |
| `types.RunRecording` | Unchanged. Optionally we can derive a `RunRecording` from a session file post-hoc (one-line tool, future work). |
| `lakehouse.FileStore` | Unchanged. Sessions and lakehouse recordings are separate concerns: sessions are conversational state; recordings are post-hoc artefacts for replay/eval. |
| Sub-agents (`SpawnSubAgent`) | Sub-agent does **not** inherit the parent's recorder. Sub-agents stay ephemeral — only the `spawn_agent` `tool_use`/`tool_result` round-trip is captured (which is already in the parent's history). |

---

## 6. Open questions / decisions to make before implementing

### 6.1 File location and naming

Default path: `~/.stirrup/sessions/<runId>.jsonl`. Alternatives:

- Per-workspace: `<workspace>/.stirrup/sessions/...` — keeps sessions
  with their workspace, makes teleport ambiguous when workspace path
  differs across machines.
- XDG: `${XDG_STATE_HOME:-~/.local/state}/stirrup/sessions/`.

**Recommendation:** XDG, with `~/.stirrup/sessions/` fallback when
`$HOME` resolves but no XDG var is set. File mode `0600`.

### 6.2 `--continue` without `--prompt`

Should the CLI prompt for input on stdin, or refuse? Refusing is
simpler and consistent with the existing "prompt is required" rule.
Reading from stdin would be a small UX win for interactive shells.

**Recommendation:** require `--prompt` for v1; revisit if users ask.

### 6.3 Session ID vs run ID

A session may span multiple runs (each follow-up is a new `runId`).
Today the CLI generates one `runId` per invocation. We need a stable
**session ID** that is distinct from `runId`.

**Recommendation:** `sessionId = first runId of the session`. New
runs in the same session keep their own `runId` for tracing but
inherit `sessionId` from the header. Each run boundary is marked by
`run_finished` so the file remains parseable.

### 6.4 Config drift on resume

If `--continue` is given alongside flags that contradict the header
config (different model, different mode, different tool set), what do
we do?

Options:

1. Strict: any change rejected — user must pass `--allow-config-drift`.
2. Lenient: silently apply overrides (current `applyOverrides` shape).
3. Hybrid: allow non-load-bearing changes (model bump, log level)
   silently; reject changes that would invalidate history (mode swap
   to read-only when history contains write tool calls; permission
   policy tightening that would have rejected past tool calls).

**Recommendation:** hybrid. Implementer should produce a small
allowlist of "safe to override on continue" fields and reject the
rest unless `--allow-config-drift` is passed. Concrete proposed
allowlist: `LogLevel`, `Timeout`, `MaxTurns` (only if increasing),
`MaxTokenBudget`, `MaxCostBudget`, `TraceEmitter.*`. Everything else
requires the flag.

### 6.5 Workspace path on teleport

If the session was recorded with `workspace=/home/sandbox/proj` and
the user resumes on a laptop where the path is `/Users/me/proj`, the
header config will be wrong. The user must pass `--workspace`.

**Recommendation:** when `--continue` is in effect and the header's
workspace path does not exist on the current host, require an
explicit `--workspace` flag.

### 6.6 Tool-output bloat

Tool outputs (especially `read_file`) can be large. Capping output at
1 MB (existing limit) means worst-case a single turn writes ~1 MB to
the session file. Sustained over hundreds of turns this becomes
multi-hundred-MB session files.

**Recommendation:** v1 stores tool outputs verbatim. Future work:
content-addressed external storage with the session log holding only
hashes (mirror what `offload-to-file` ContextStrategy does). Out of
scope for the first cut.

### 6.7 Secret leakage

Tool results may contain secrets the user accidentally read with
`read_file` or that surface from `run_command` output. The existing
`security.LogScrubber` only applies to logs and event-stream
emissions, not to in-memory `messages`.

**Recommendation:** run `security.Scrub` on tool result `Content`
before writing to the session log (mirroring what
`StdioTransport.Emit` already does). This is a behaviour change for
session files only; in-memory messages sent to the model remain
unscrubbed since the model needs the real content. **Caveat:** this
means the persisted history diverges from the in-memory history,
which means a perfect replay through `ReplayProvider` is impossible
from a session file alone. That is acceptable for a session log
(which is for follow-ups by the *same* model in the *same*
conversation, and the model already saw the unscrubbed content
upstream). Worth calling out so it's a deliberate decision.

Alternative: write tool outputs verbatim to the session file (mode
0600, local-only) and consider the file as sensitive as the workspace
itself. Document this prominently.

### 6.8 Schema versioning

The header carries `v: 1`. On read, unknown `v` exits with a clear
error suggesting a harness upgrade. Forward compatibility (newer
harness reading older files) is required; we add new event types only
in additive ways and readers must skip unknown event types.

### 6.9 Concurrent writers

Two `stirrup harness --continue` processes pointing at the same
session file would corrupt it.

**Recommendation:** acquire an OS file lock (`flock` /
`LOCK_EX|LOCK_NB`) on open; fail fast with a clear message if held.

### 6.10 `RunFollowUpLoop` deprecation

Once `--continue` exists, the CLI's `--followup-grace` flag for stdio
becomes redundant (gRPC keeps it). We should not silently break gRPC
follow-ups. The refactor in §3.5 keeps gRPC working as before,
because `RunFollowUpLoop` now correctly extends history per call.

---

## 7. Out of scope for v1

- Cross-conversation merging (forking a session into branches).
- A web/IDE UI for browsing sessions.
- External (non-local) session stores (S3, Postgres). Lakehouse is the
  right home for that and remains separate.
- Sub-agent session capture. Sub-agents are isolated; their text
  output already round-trips through the parent `tool_result`.
- Session compaction / log rotation. Tracked under §6.6.
- Encrypted session files. Filesystem permissions + `secret://`
  references for credentials are the v1 perimeter.

---

## 8. Acceptance checklist for the implementer

- [ ] `session.Recorder` interface + `JSONLRecorder` and `NoopRecorder`
      implementations, with crash-safety tests (truncated tail,
      mid-turn crash, partial last line).
- [ ] `AgenticLoop` wired at the four mutation sites; `NoopRecorder`
      is a true no-op (zero allocations on hot path).
- [ ] `core.RunWithMessages` (or equivalent) — `Run` and
      `RunFollowUpLoop` both go through it.
- [ ] `--session-record`, `--session-file`, `--continue` flags +
      precedence rules as in §4.
- [ ] `RunConfig.Session SessionConfig` field; proto mirrored;
      validation rules added.
- [ ] Reconstruction tolerates: missing header (error), unknown `v`
      (error), incomplete trailing turn (drop), unknown event types
      (skip).
- [ ] File locking; file mode `0600`; default location resolution.
- [ ] Config-drift policy (§6.4) implemented with a clear error
      message naming the offending field.
- [ ] Secret scrubbing decision (§6.7) implemented, documented, and
      tested.
- [ ] gRPC follow-up parity test: a `RunFollowUpLoop` round-trip with
      sessions enabled produces the same observable behaviour as
      before, plus a session file that round-trips cleanly through
      `--continue`.
- [ ] Documentation: `docs/sessions.md` (this file, productionised)
      plus a section in the project `CLAUDE.md` and the CLI flag
      table in the project README/CLAUDE.md.
