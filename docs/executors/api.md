# API executor (`api`)

The `api` executor serves read-only runs — review, research, planning,
triage — **without any sandbox or checkout at all**. File reads,
directory listings, and content search are fulfilled by a Git host's
REST API over HTTPS; there is no workspace, no shell, and no write
surface. `WriteFile` and `Exec` return errors unconditionally, so even
a misconfigured run cannot mutate anything.

Two forges are supported, selected by `executor.vcsBackend.type`:
`github` and `gitlab`. They differ in tool coverage — see the
[tool matrix](#built-in-tool-matrix).

The implementations are
[`harness/internal/executor/api.go`](../../harness/internal/executor/api.go)
(GitHub) and
[`harness/internal/executor/apigitlab.go`](../../harness/internal/executor/apigitlab.go)
(GitLab); dispatch is `buildVcsExecutor` in
[`harness/internal/core/factory.go`](../../harness/internal/core/factory.go).
Every behaviour documented here is cross-checked against those files,
and the tool matrix was verified against live runs on 2026-08-27
against `rxbynerd/stirrup` (GitHub) and `gitlab-org/gitlab-runner`
(GitLab) in `mode: review`.

## Contents

- [When to use it](#when-to-use-it)
- [Architecture: where reads actually happen](#architecture-where-reads-actually-happen)
- [Configuration](#configuration)
- [Validation and mode interactions](#validation-and-mode-interactions)
- [Built-in tool matrix](#built-in-tool-matrix)
- [Limits and rate-limit behaviour](#limits-and-rate-limit-behaviour)
- [Remaining gaps](#remaining-gaps)

## When to use it

| Executor | Filesystem surface | Pick when |
|---|---|---|
| `api` | Remote VCS reads only | The run needs repository *content* but no shell, no diffing against a working tree, and no sandbox — code review of a ref, repository Q&A, research over a codebase too large or too sensitive to clone onto the harness host. |
| `none` | None at all | The run is MCP-only / server-side-tool-only and must not read anything, local or remote. See [`examples/runconfig/none-mcp-only.json`](../../examples/runconfig/none-mcp-only.json). |
| `container` / `k8s` with a checkout | Full read (and optionally write) of a cloned working tree | The run needs `git_*` tools, unbounded content search, or any command execution. A read-only *mode* on a sandboxed executor gives the same safety invariants with a fuller tool surface. |

The trade the `api` executor makes is coverage for setup cost: no
image, no clone, no egress policy to write — but search is bounded
(see [limits](#limits-and-rate-limit-behaviour)) and the `git_*` tools
cannot work at all. Runs whose value depends on exhaustive search or
on git history belong on a sandboxed executor with a checkout, in a
read-only mode.

## Architecture: where reads actually happen

**The harness process calls the VCS HTTP API directly. Nothing
round-trips over the gRPC control stream.**

Each tool call is an HTTPS request from the harness process to the
forge (30 s client timeout), carrying the resolved token — a `Bearer`
header on GitHub, `PRIVATE-TOKEN` on GitLab — and the configured `ref`
as a query parameter. The executor is constructed in-process by the
factory; the transport never sees the reads except as ordinary
`tool_call` / `tool_result` events after the fact.

| Operation | GitHub endpoint | GitLab endpoint |
|---|---|---|
| `read_file` | `/repos/{owner}/{repo}/contents/{path}` (raw media type) | `/projects/{id}/repository/files/{path}/raw` |
| `list_directory` | `/repos/{owner}/{repo}/contents/{path}` (JSON media type) | `/projects/{id}/repository/tree` (paginated) |
| whole-tree enumeration for `grep_files` / `find_files` | `/repos/{owner}/{repo}/git/trees/{ref}?recursive=1` | *not implemented* |

For control-plane authors this means:

- The control plane does **not** serve file content. The only
  mechanism by which a tool result can be fulfilled over the control
  stream is the async-tool `tool_result_request` /
  `tool_result_response` pair (see the
  [integration guide](../integration-guide.md)), and no shipped
  built-in tool uses it — every built-in resolves in-process.
- The harness therefore needs direct egress to `api.github.com` or
  `gitlab.com`, and the VCS token resolvable in *its own* environment
  (the `secret://` reference resolves where the harness runs, e.g. the
  Cloud Run job's env), exactly as for provider credentials.

Other properties, from the implementation:

- **`ref` resolution** is delegated to the forge: branch name, tag, or
  commit SHA all work; an empty `ref` means the repository's default
  branch.
- **No caching**: every `read_file` call re-fetches the whole file,
  then slices `start_line` / `limit` harness-side. Paging through a
  large file re-downloads it per call.
- **No retry or rate-limit handling**: a non-200 response surfaces to
  the model as a tool error of the form
  `api executor: read file "x": HTTP 403`.
- **`read_file` on a directory path** does not error on GitHub: the
  contents API returns the JSON listing with HTTP 200 even under the
  raw media type, so the model receives raw JSON metadata instead of
  file content. GitLab's raw-file endpoint 404s instead.

## Configuration

`executor.vcsBackend` is the whole configuration surface:

| Field | Required | Meaning |
|---|---|---|
| `type` | Yes | `"github"` or `"gitlab"`. `ValidateRunConfig` enforces the closed set, and an unrecognised type is a factory error rather than a fallback — reading a same-named repository from the wrong forge would otherwise go unnoticed. |
| `apiKeyRef` | No | `secret://` reference to a forge token. Omit it for unauthenticated access to a public repository, which both forges allow at a reduced rate limit. A literal key is rejected by validation. |
| `repo` | Yes | `owner/repo` on GitHub; the full project path (`group/subgroup/project`) on GitLab. |
| `ref` | No | Branch, tag, or commit SHA; empty = default branch. |

There are no `--vcs-*` CLI flags; the `api` executor is configured via
a `--config` file, stdin, or the gRPC `task_assignment`
(`VcsBackendConfig` is mirrored field-for-field on the proto). A bare
`--executor api` fails at boot with
`build executor: api executor requires vcsBackend configuration`.

A minimal working RunConfig:

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
  "tools": { "builtIn": ["read_file", "list_directory", "grep_files", "find_files"] },
  "gitStrategy": { "type": "none" },
  "maxTurns": 14,
  "timeout": 420
}
```

Dropping `apiKeyRef` turns the same config into an unauthenticated
public-repository run. The explicit `tools.builtIn` omits the four
`git_*` tools that the read-only default list would otherwise enable;
see the [tool matrix](#built-in-tool-matrix).

## Validation and mode interactions

- **Capabilities**: `CanRead` and `CanNetwork` only. `CanWrite` and
  `CanExec` are false, so the factory never registers `write_file`,
  the edit tools, or `run_command` regardless of mode.
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
  config load. Its `type` is validated at config load.

## Built-in tool matrix

Status of each tool in the read-only default list
(`DefaultReadOnlyBuiltInTools`). "Live" means observed in the
2026-08-27 verification runs; "traced" means the conclusion is read
off the code path but was not executed.

| Tool | GitHub | GitLab | Notes |
|---|---|---|---|
| `read_file` | **Works** (live) | **Works** (traced) | Line-numbered output, `start_line` / `limit` honoured (sliced harness-side after a full-file fetch). Nested paths work. On GitHub a directory path returns raw JSON rather than an error. |
| `list_directory` | **Works** (live) | **Works** (live) | Directory entries carry a trailing `/`, so `recursive: true` descends correctly and files are distinguishable from directories. |
| `grep_files` | **Works, bounded** (live) | **Refused** (live) | GitHub enumerates the tree in one `git/trees` call, then fetches matching candidates 8-at-a-time; caps below. GitLab implements no tree enumeration, so the tool errors with `cannot search ".": this executor does not expose a searchable workspace` — a refusal, not a silent host walk. |
| `find_files` | **Works** (live) | **Refused** (live) | Same tree enumeration as `grep_files`, but no file content is fetched, so a whole-repo name search costs one API call. |
| `git_status`, `git_changed_files`, `git_diff`, `git_show` | **Non-functional** (live) | **Non-functional** (traced) | Registered (they are read-only), but every call shells out via `Exec`, which returns `api executor: command execution not supported`, surfaced to the model as `git is not available: …`. There is no local clone for git to inspect. The call fails cleanly and the run continues. |
| `web_fetch` | **Works** (traced) | **Works** (traced) | Registered without any capability gate and executes in the harness process; independent of the executor. |
| `spawn_agent` | **Works** (traced) | **Works** (traced) | Sub-agents share the parent's tool registry and executor, so they inherit the same read-only VCS surface. |
| `run_command`, `write_file`, edit tools | **Never registered** | **Never registered** | Capability-gated off. |

Practical consequence: set an explicit `tools.builtIn` rather than
relying on the read-only default list, which enables four `git_*`
tools that can only error. On GitHub that is `read_file`,
`list_directory`, `grep_files`, `find_files`, plus optionally
`web_fetch` / `spawn_agent`; on GitLab, drop the two search tools.
Without search, the model navigates with `list_directory` +
`read_file`, which the live runs showed working, at the cost of more
turns.

## Limits and rate-limit behaviour

Search is deliberately bounded so one tool call cannot exhaust the
caller's rate limit
([`searchtree.go`](../../harness/internal/tool/builtins/searchtree.go)):

- **200 files** per `grep_files` call, and files larger than **1 MiB**
  are skipped. When either bound bites — or when the forge truncated
  the tree — the rendering carries a
  `[search incomplete: …]` notice so a partial result is not read as
  exhaustive. Narrow the scan with `path` or `include` for full
  coverage.
- **8 concurrent** file fetches, inside the search tool's own 30 s
  timeout.
- `find_files` fetches no content, so it is bounded only by the tree
  enumeration itself.

Forge limits apply on top:

- **GitHub tree enumeration** uses
  [`git/trees?recursive=1`](https://docs.github.com/en/rest/git/trees),
  which GitHub truncates past 100,000 entries or a 7 MB response and
  flags with `truncated: true`; the executor propagates that flag into
  the incomplete notice.
- **GitHub directory listings** return at most 1,000 entries from the
  [contents API](https://docs.github.com/en/rest/repos/contents) and
  are not paginated by the executor. **File reads** use the raw media
  type, supported for files up to 100 MB; the whole file lands in
  harness memory per `read_file` call before line-slicing.
- **GitLab directory listings** are paginated by the executor at 100
  entries per page for up to 10 pages, matching GitHub's 1,000-entry
  ceiling.
- **Rate limits**
  ([GitHub](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api),
  [GitLab](https://docs.gitlab.com/user/gitlab_com/)):
  GitHub allows 5,000 requests/hour for a normal authenticated token
  but only 60/hour unauthenticated, so unauthenticated runs are for
  light exploration only. The executor performs no client-side
  throttling, caching, or `Retry-After` handling — exhaustion surfaces
  to the model as `HTTP 403` / `HTTP 429` tool errors, and the model's
  own retries burn further budget. When sizing `maxTurns`, budget one
  request per `read_file`, one per `list_directory` level, one per
  `find_files` call, and up to 201 per `grep_files` call.

## Remaining gaps

- **GitLab has no content or name search.** `GitLabExecutor` does not
  implement `executor.TreeLister`, so `grep_files` and `find_files`
  refuse rather than work. Omit them from `tools.builtIn` for a GitLab
  run.
- **The `git_*` tools are dead weight** on both backends and are in
  the read-only default tool list, so a config that does not set
  `tools.builtIn` explicitly hands the model four tools that can only
  error.
- **No caching, retries, or rate-limit backoff.** Repeated reads of
  the same file cost repeated requests, and a 403/429 is passed
  straight to the model.
- **`read_file` on a directory path** returns raw GitHub JSON with
  HTTP 200 instead of an error.
