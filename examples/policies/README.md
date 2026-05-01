# Cedar policy starter set

This directory contains starter [Cedar](https://www.cedarpolicy.com/) policies
for the `policy-engine` permission policy (issue #42, Wave 2.3 — B3).

## Loading a policy file

Configure `RunConfig.permissionPolicy` in your run config JSON:

```json
{
  "permissionPolicy": {
    "type": "policy-engine",
    "policyFile": "examples/policies/destructive-shell.cedar",
    "fallback": "deny-side-effects"
  }
}
```

`policyFile` must point to a single `.cedar` file. To compose multiple
files, concatenate them — `cedar-go` accepts any number of `permit` /
`forbid` statements per document.

## Entity model (Cedar schema v1)

Every authorisation request is built with the following entities:

| Component | Shape | Notes |
|-----------|-------|-------|
| `principal` | `User::"<runId>"` | Parent: `User::"any"` so policies may match all runs with `principal in User::"any"`. Attributes: `runId` (String), `mode` (String), `parentRunId` (String, only on sub-agents), `capabilities` (Set\<String>). |
| `action` | `Action::"tool:<toolName>"` | One action per tool name, e.g. `Action::"tool:run_command"`. |
| `resource` | `Tool::"<toolName>"` | Mirror of the action for symmetry. |
| `context` | Record | `input` (Record — recursively translated tool input), `workspace` (String — absolute workspace path), `dynamicContext` (Record — string keys to string values). |

JSON tool input is converted to Cedar values recursively: strings stay
strings, integers become `Long`, booleans become `Boolean`, arrays become
`Set`, objects become `Record`. Floats and JSON `null` are handled
defensively — floats lose precision and become String; nulls are dropped.

The schema version is pinned in `harness/internal/permission/policyengine.go`
as `CedarSchemaVersion`. Bump it whenever the entity layout changes.

## Starter files

| File | Effect | Purpose |
|------|--------|---------|
| `destructive-shell.cedar` | `forbid` | Blocks `run_command` calls whose `cmd` matches `*rm -rf*`, `*chmod -R*`, `*git push --force*`, `*mkfs*`, etc. Defence-in-depth against unintended history rewrites or filesystem-wide destruction. |
| `github-only-fetch.cedar` | `permit` | Permits `web_fetch` only to `*.github.com`, `github.com`, `raw.githubusercontent.com`, and `docs.python.org`. Pair with a fallback of `deny-side-effects` to deny everything else. |
| `no-secret-in-input.cedar` | `forbid` | Forbids any tool whose input contains common leaked-secret patterns (`sk-*`, `ghp_*`, `github_pat_*`, `aws_secret_*`) in the `cmd`, `content`, or `url` fields. Structural backstop for the LogScrubber. |
| `subagent-capability-cap.cedar` | `forbid` | Forbids `run_command` when `principal.parentRunId` is set, i.e. the caller is a sub-agent. Limits blast radius of `spawn_agent`. |

## Decision rules

The `policy-engine` evaluator returns:

- **Allow** when at least one `permit` matches and no `forbid` matches.
- **Deny** when at least one `forbid` matches (denial reason includes
  the matched policy IDs).
- **No decision** when no policy matches — the configured `fallback`
  (default `deny-side-effects`) is consulted instead.

Every decision is emitted as a `policy_decision` (allow / no-match) or
`policy_denied` (forbid) security event for audit.

## Authoring conventions

- Use `like` for prefix / suffix / substring matches (`*` is the only
  wildcard; `?` is not supported by Cedar's `like`).
- Guard `context.input` field accesses with `has` — tools have wildly
  different input schemas and unconditional access on a missing field
  surfaces as a Cedar error in the diagnostic.
- Match on `Action::"tool:<name>"` AND `Tool::"<name>"` for clarity even
  though one would suffice — readers grepping by tool name find both.
- Keep one concern per file. Composition is via concatenation (or a
  future loader that accepts a directory).
