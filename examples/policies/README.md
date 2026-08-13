# Cedar policy starter set

This directory contains starter [Cedar](https://www.cedarpolicy.com/) policies
for the `policy-engine` permission policy. See
[`docs/safety-rings.md`](../../docs/safety-rings.md) for the
operator-facing context.

## Loading a policy file

### From the CLI

```sh
stirrup harness --permission-policy-file examples/policies/destructive-shell.cedar ...
```

The `--permission-policy-file` flag is a convenience shortcut: it sets
`permissionPolicy.policyFile` and (when the type is unset elsewhere)
also implies `permissionPolicy.type=policy-engine`. The fallback policy
defaults to `deny-side-effects` when not specified.

### From a RunConfig file

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

`fallback` must be one of `allow-all`, `deny-all`,
`deny-side-effects`, or `ask-upstream`. Chained policy engines are
intentionally rejected to avoid no-decision loops.

The recommended mental model: **the policy file grants; the fallback
handles the rest**. A permit-based allow-list paired with `deny-all`
denies everything the file does not name — including non-mutating
tools such as `web_fetch` that `deny-side-effects` would allow.
`forbid` files are defence-in-depth backstops layered over a
permissive fallback.

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
| `destructive-shell.cedar` | `forbid` | Blocks `run_command` calls whose `command` matches `*rm -rf*`, `*chmod -R*`, `*git push --force*`, `*mkfs*`, etc. Defence-in-depth against unintended history rewrites or filesystem-wide destruction. Pair with an `allow-all` fallback when it is the only gate on `run_command` — a `deny-side-effects` fallback already denies every `run_command` on no-match. |
| `github-only-fetch.cedar` | `permit` | Permits `web_fetch` only to `github.com`, `api.github.com`, `raw.githubusercontent.com`, and `docs.python.org`. Pair with a fallback of `deny-all` to deny everything else — `deny-side-effects` does not deny non-mutating `web_fetch`. |
| `no-secret-in-input.cedar` | `forbid` | Forbids any tool whose input contains common leaked-secret patterns (`sk-*`, `ghp_*`, `github_pat_*`, `aws_secret_*`) in the `command`, `content`, or `url` fields. Structural backstop for the LogScrubber. |
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

- **Key `context.input` clauses on the exact field names the tool's
  JSON Schema declares** (`command` for `run_command`, `content` for
  `write_file`, `url` for `web_fetch` — see
  `harness/internal/tool/builtins/`). The schemas set
  `additionalProperties: false`, so an input with any other field
  name is rejected before Cedar runs: a clause keyed on an undeclared
  name parses cleanly and never fires.
  `harness/internal/tool/builtins/starter_policies_test.go` pins the
  shipped starters against the real schemas.
- **Anchor URL patterns as full `https://<host>/*` literals.**
  Cedar's `like` wildcard matches every character including `/` and
  `@`, so `https://*.github.com/*` also matches
  `https://evil.example/x.github.com/y`. Never place a wildcard
  before or inside the host.
- Use `like` for prefix / suffix / substring matches (`*` is the only
  wildcard; `?` is not supported by Cedar's `like`).
- Guard `context.input` field accesses with `has` — tools have wildly
  different input schemas and unconditional access on a missing field
  surfaces as a Cedar error in the diagnostic.
- Match on `Action::"tool:<name>"` AND `Tool::"<name>"` for clarity even
  though one would suffice — readers grepping by tool name find both.
- Keep one concern per file. Composition is via concatenation (or a
  future loader that accepts a directory). Note the asymmetry:
  `permit` statements union across files (a broad permit swallows a
  narrower allow-list), while `forbid` statements compose safely (any
  match denies).
