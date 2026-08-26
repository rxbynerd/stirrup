# API executor (`api`)

The `api` executor serves read-only runs — review, research, planning,
triage — **without any sandbox or checkout at all**. File reads and
directory listings are fulfilled by the GitHub REST API over HTTPS;
there is no workspace, no shell, and no write surface. `WriteFile` and
`Exec` return errors unconditionally, so even a misconfigured run
cannot mutate anything.

The executor implementation is
[`harness/internal/executor/api.go`](../../harness/internal/executor/api.go);
factory wiring is `buildExecutor`'s `"api"` case in
[`harness/internal/core/factory.go`](../../harness/internal/core/factory.go).
Every behaviour documented here is cross-checked against those files,
and the tool matrix below was verified against live runs on
2026-08-27 (harness at `84eb9a97`, repo `rxbynerd/stirrup`,
`mode: review`).

## Contents

- [When to use it](#when-to-use-it)
- [Architecture: where reads actually happen](#architecture-where-reads-actually-happen)
- [Configuration](#configuration)
- [Validation and mode interactions](#validation-and-mode-interactions)
- [Built-in tool matrix](#built-in-tool-matrix)
- [Limits and rate-limit behaviour](#limits-and-rate-limit-behaviour)
- [Known issues](#known-issues)

## When to use it

| Executor | Filesystem surface | Pick when |
|---|---|---|
| `api` | Remote VCS reads only (GitHub contents API) | The run needs repository *content* but no shell, no diffing against a working tree, and no sandbox — code review of a ref, repository Q&A, research over a codebase too large or too sensitive to clone onto the harness host. |
| `none` | None at all | The run is MCP-only / server-side-tool-only and must not read anything, local or remote. See [`examples/runconfig/none-mcp-only.json`](../../examples/runconfig/none-mcp-only.json). |
| `container` / `k8s` with a checkout | Full read (and optionally write) of a cloned working tree | The run needs `git_*` tools, content search (`grep_files` / `find_files`), cross-file analysis at scale, or any command execution. A read-only *mode* on a sandboxed executor gives the same safety invariants with a far more capable tool surface. |

The honest summary of the current state: the `api` executor is a
minimal, working read/list surface, not a full read-only workspace.
Until the [known issues](#known-issues) are fixed, runs that lean on
search or git tooling belong on a sandboxed executor with a checkout
in a read-only mode.

## Architecture: where reads actually happen

**The harness process calls the VCS HTTP API directly. Nothing
round-trips over the gRPC control stream.**

Each `ReadFile` / `ListDirectory` is an HTTPS request from the harness
process to `https://api.github.com/repos/{owner}/{repo}/contents/{path}`
(30 s client timeout), with the resolved token as a `Bearer` header
and `ref` passed verbatim as a query parameter. The executor is
constructed in-process by the factory; the transport never sees the
reads except as ordinary `tool_call` / `tool_result` events after the
fact.

For control-plane authors this means:

- The control plane does **not** serve file content. The only
  mechanism by which a tool result can be fulfilled over the control
  stream is the async-tool `tool_result_request` / `tool_result_response`
  pair (see the [integration guide](../integration-guide.md)), and no
  shipped built-in tool uses it — every built-in resolves in-process.
- The harness therefore needs direct egress to `api.github.com` and
  the VCS token resolvable in *its own* environment (the `secret://`
  reference resolves where the harness runs, e.g. the Cloud Run job's
  env), exactly as for provider credentials.

Other properties, from the implementation:

- **`ref` resolution** is delegated to GitHub: branch name, tag, or
  commit SHA all work; an empty `ref` means the repository's default
  branch.
- **No caching**: every `read_file` call re-fetches the entire file,
  then slices `start_line` / `limit` harness-side. Paging through a
  large file re-downloads it per call.
- **No retry or rate-limit handling**: a non-200 response surfaces to
  the model as a tool error of the form
  `api executor: read file "x": HTTP 403`.
- **`read_file` on a directory path** does not error: GitHub returns
  the JSON listing with HTTP 200 even under the raw media type, so
  the model receives raw JSON metadata instead of file content
  (verified against the live API).

## Configuration

`executor.vcsBackend` is the whole configuration surface:

| Field | Required | Meaning |
|---|---|---|
| `type` | — | Nominally `"github"` \| `"gitlab"`, **but the field is currently ignored and only GitHub is implemented** — `"gitlab"` silently reads from the GitHub API ([#556](https://github.com/rxbynerd/stirrup/issues/556)). Set `"github"`. |
| `apiKeyRef` | Yes (in practice) | `secret://` reference to a GitHub token. The executor itself supports token-less public-repo access, but the factory resolves this field unconditionally, so omitting it fails at boot ([#559](https://github.com/rxbynerd/stirrup/issues/559)). |
| `repo` | Yes | `owner/repo` (exactly one `/`). |
| `ref` | No | Branch, tag, or commit SHA; empty = default branch. |

There are no `--vcs-*` CLI flags; the `api` executor is configured via
a `--config` file, stdin, or the gRPC `task_assignment`. A bare
`--executor api` fails at boot with
`build executor: api executor requires vcsBackend configuration`.

A minimal working RunConfig (this exact shape completed a live
`review`-mode run against `rxbynerd/stirrup`):

```json
{
  "runId": "review-repo-layout",
  "mode": "review",
  "prompt": "Summarise the repository layout from its top-level directories and README.",
  "provider": { "type": "anthropic", "apiKeyRef": "secret://ANTHROPIC_API_KEY" },
  "modelRouter": { "type": "static", "provider": "anthropic", "model": "claude-haiku-4-5" },
  "executor": {
    "type": "api",
    "vcsBackend": {
      "type": "github",
      "apiKeyRef": "secret://GITHUB_TOKEN",
      "repo": "rxbynerd/stirrup",
      "ref": "main"
    }
  },
  "permissionPolicy": { "type": "deny-side-effects" },
  "tools": { "builtIn": ["read_file", "list_directory", "web_fetch"] },
  "gitStrategy": { "type": "none" },
  "maxTurns": 14,
  "timeout": 420
}
```

The explicit `tools.builtIn` above is deliberate — see the
[tool matrix](#built-in-tool-matrix) for why the read-only *default*
list is not recommended on this executor yet.

## Validation and mode interactions

- **Capabilities**: `CanRead` and `CanNetwork` only. `CanWrite` and
  `CanExec` are false, so the factory never registers `write_file`,
  the edit tool, or `run_command` regardless of mode.
- **Mode defaults**: the CLI defaults `--mode` to `planning`, so a
  bare invocation gets `deny-side-effects` and the full read-only
  default tool list. `execution` mode with the `api` executor is
  accepted by validation, but mutating tools silently drop from the
  registry — there is nothing for them to act on.
- **Rejected cross-fields**: `executor.workspaceExportTo` (no
  workspace to export) and `hooks.preRun` / `hooks.postRun` (hooks
  need an exec-capable executor; `ExecutorCanExec("api")` is false)
  fail `ValidateRunConfig`.
- **Ignored fields**: `workspace`, `image`, `network`, `resources`
  and the `k8s*` fields are meaningless for `api` — there is no
  sandbox to apply them to. Unlike the `none` executor, they are
  currently ignored rather than rejected, so a leftover `image` from
  a copied config does not error.
- **`vcsBackend` presence** is enforced at factory time, not by
  `ValidateRunConfig` — the error appears at harness boot rather than
  config load.

## Built-in tool matrix

Status of each tool in the read-only default list
(`DefaultReadOnlyBuiltInTools`) on the `api` executor. "Live" means
observed in the 2026-08-27 verification runs; "traced" means the
conclusion is read off the code path but was not executed.

| Tool | Status | Evidence and notes |
|---|---|---|
| `read_file` | **Works** (live) | Line-numbered output, `start_line` / `limit` honoured (sliced harness-side after a full-file fetch). Nested paths work. A directory path returns raw JSON, not an error. |
| `list_directory` | **Degraded** (live) | Returns entry names, but directory entries carry no trailing `/` (the tool description promises one), so files and directories are indistinguishable, and `recursive: true` never descends — recursive output was byte-identical to non-recursive. [#558](https://github.com/rxbynerd/stirrup/issues/558). |
| `grep_files` | **Broken — do not enable** (live) | The Go-native walker searches the harness host's working directory, not the repo: a canary file placed in the harness cwd was returned to the model, while a symbol present in the repo was not found. Wrong results plus host-filesystem disclosure. [#557](https://github.com/rxbynerd/stirrup/issues/557). |
| `find_files` | **Broken — do not enable** (live) | Same host-walk: `*.go` against a repo with hundreds of Go files returned "No matches found." from an empty harness cwd. [#557](https://github.com/rxbynerd/stirrup/issues/557). |
| `git_status`, `git_changed_files`, `git_diff`, `git_show` | **Non-functional** (traced) | Registered (they are read-only), but every call shells out via `Exec`, which returns `api executor: command execution not supported`, surfaced as `git is not available: …`. There is no local clone for git to inspect. Live exercise was pre-empted by the zero-argument tool-input bug [#560](https://github.com/rxbynerd/stirrup/issues/560), but the error path is unconditional. |
| `web_fetch` | **Works** (traced) | Registered without any capability gate and executes in the harness process; independent of the executor. |
| `spawn_agent` | **Works** (traced) | Sub-agents share the parent's tool registry and executor, so they inherit the same read-only VCS surface (and the same broken tools). |
| `run_command`, `write_file`, edit tools | **Never registered** | Capability-gated off. |

Practical consequence: until [#557](https://github.com/rxbynerd/stirrup/issues/557)
and [#558](https://github.com/rxbynerd/stirrup/issues/558) are fixed,
set an explicit `tools.builtIn` of `read_file`, `list_directory`, and
optionally `web_fetch` / `spawn_agent`, rather than relying on the
read-only default list — the default enables `grep_files` /
`find_files` (host-walking) and four git tools that can only error.
The model compensates for the missing search tools with
`list_directory` + `read_file` navigation, which the live runs showed
working smoothly.

## Limits and rate-limit behaviour

All reads are the GitHub
[contents API](https://docs.github.com/en/rest/repos/contents), so its
documented limits apply directly:

- **Directory listings** return at most 1,000 entries and are not
  paginated by the executor; larger directories are silently
  truncated by GitHub.
- **File reads** use the raw media type, which GitHub supports for
  files up to 100 MB. The whole file lands in harness memory per
  `read_file` call before line-slicing.
- **Rate limits**
  ([reference](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api)):
  5,000 requests/hour for a normal authenticated token. The executor
  performs no client-side throttling, caching, or `Retry-After`
  handling — exhaustion surfaces to the model as `HTTP 403` /
  `HTTP 429` tool errors, and the model's own retries burn further
  budget. Budget roughly one request per `read_file` and one per
  directory per `list_directory` level when sizing `maxTurns`.

## Known issues

Filed from the verification pass on 2026-08-27:

- [#556](https://github.com/rxbynerd/stirrup/issues/556) —
  `vcsBackend.type` is ignored; `"gitlab"` silently reads from the
  GitHub API.
- [#557](https://github.com/rxbynerd/stirrup/issues/557) —
  `grep_files` / `find_files` walk the harness host filesystem, not
  the repo (wrong results + host disclosure).
- [#558](https://github.com/rxbynerd/stirrup/issues/558) —
  `ListDirectory` drops directory markers; recursive listing never
  descends.
- [#559](https://github.com/rxbynerd/stirrup/issues/559) — empty
  `apiKeyRef` fails at boot with a secret-scheme error, foreclosing
  unauthenticated public-repo access the executor itself supports.
- [#560](https://github.com/rxbynerd/stirrup/issues/560) — (not
  api-specific) zero-argument tool calls arrive as `null` input on
  the Anthropic adapter; the schema gate rejects them and the replay
  400s the run. On this executor it is triggered by the parameterless
  git tools — a further reason to exclude them from `tools.builtIn`.
