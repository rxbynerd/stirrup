# Sandbox & deterministic safety rings

This document covers the deterministic safety controls in stirrup that the
agent cannot talk its way around: container kernel isolation, egress
allowlisting, the Cedar-backed policy engine, the Rule-of-Two structural
invariant, and the post-edit code scanner. Each is opt-in but composes
into a layered defence: a single failure does not unlock the next ring.

The five controls below correspond to issue #42 sub-tasks **B1** through
**B5**. They are *deterministic* — the agent cannot prompt them to
behave differently. LLM-based guards (prompt injection classifiers,
content policy filters) are tracked separately and explicitly do **not**
substitute for these rings.

## 1. Container runtimeClass (B1)

The container executor accepts an optional `Runtime` field that selects
which OCI runtime the host Docker/Podman daemon should use to start the
sandbox container. Plain Linux user-namespace caps (the default) drop
all capabilities and apply `no-new-privileges`, but they share a kernel
with the host. A kernel-level isolation tier raises the bar for a kernel
exploit found by an agent.

### Supported values

| Value | Implementation | Host setup required |
|-------|----------------|---------------------|
| `""` (default) | engine default — usually `runc` | none |
| `runc` | vanilla runc | none |
| `runsc` | [gVisor](https://gvisor.dev) — user-space kernel | install `runsc` and register with the daemon |
| `kata` | [Kata Containers](https://katacontainers.io) (default flavour) | install `kata-runtime` and register |
| `kata-qemu` | Kata backed by QEMU | as above |
| `kata-fc` | Kata backed by Firecracker | as above |

### Host setup

- **gVisor**: install `runsc` from the [Google releases](https://gvisor.dev/docs/user_guide/install/), then register it with Docker by adding `"runsc": { "path": "/usr/local/bin/runsc" }` under `runtimes` in `/etc/docker/daemon.json` and restarting the daemon. Verify with `docker info | grep -A1 Runtimes`.
- **Kata Containers**: install via your distribution package or the upstream installer; register `kata-runtime` similarly. The `kata-qemu` and `kata-fc` flavours are aliases the daemon expects to map onto distinct runtime entries.

### Selection

```sh
stirrup harness --container-runtime runsc --executor container ...
```

Or in the RunConfig file:

```json
{
  "executor": {
    "type": "container",
    "image": "ghcr.io/rxbynerd/stirrup:latest",
    "runtime": "runsc"
  }
}
```

`ValidateRunConfig` rejects values outside the closed set above.

### Kubernetes note

On Kubernetes you would set `runtimeClassName` on the pod spec rather
than threading the runtime through stirrup. Use `--container-runtime`
only with the local Docker/Podman container executor.

## 2. Egress allowlist proxy (B2)

When `network.mode == "allowlist"`, the container executor starts an
in-process forward proxy on the host network namespace and configures
the container to use it via `HTTP_PROXY` / `HTTPS_PROXY`. The proxy
resolves the destination FQDN, checks it against `network.allowlist`,
and only forwards the request when it matches.

### FQDN matching

| Allowlist entry | Matches |
|-----------------|---------|
| `example.com` | `example.com:443` only |
| `*.example.com` | any `<sub>.example.com:443` (one wildcard label only) |
| `example.com:80` | `example.com:80` only |

The proxy verifies the TLS SNI matches the requested host so a tampered
HTTP `Host` header cannot mismatch the allowlisted FQDN. Default port is
443 unless explicitly suffixed.

### Cooperation model

The proxy intercepts well-behaved HTTP/HTTPS clients that honour
`HTTP_PROXY` / `HTTPS_PROXY`: curl, git over HTTPS, the language stdlib
HTTP client. The container is wired with `NO_PROXY=localhost,127.0.0.1,::1`
so loopback is unaffected.

### v1 limitation: cooperating clients only

> **Honest limitation**: in v1 the iptables drop is **deferred**. Fail-
> closed depends on the in-container client honouring the proxy env
> vars. A misbehaving client (raw TCP, custom DNS, stripped env) inside
> the container can still dial the bridge gateway directly because the
> bridge network has unrestricted egress.
>
> The full fail-closed posture requires an iptables/nftables drop on the
> host that whitelists only the proxy's listen address. That drop is
> privilege-sensitive and not portable to macOS Docker Desktop, so it
> ships as a follow-up. Track via `TODO: file follow-up` in
> `harness/internal/executor/container.go` (search for "iptables /
> nftables drop").

The cooperative model still defeats the agent attempting to talk to a
disallowed host through the harness's tools (web_fetch, run_command
calling curl, etc.) — those go through the proxy.

### Security events

Each request through the proxy emits one of:

- `egress_allowed` — host matched, request forwarded.
- `egress_blocked` — host did not match the allowlist; request dropped
  with a 403 to the client.

These are JSON-line `SecurityEvent`s in stderr and are also tagged onto
the OTel `SecurityEvents` counter.

### Local executor refusal

The local executor refuses `network.mode: allowlist` at construction
time — the blueprint is clear that egress controls require a sandbox
boundary, and the local executor is one. Use `executor.type: container`
for allowlist mode.

## 3. Cedar policy engine (B3)

A fourth `PermissionPolicy` type, `policy-engine`, evaluates each tool
call against a Cedar policy file. It returns:

- **Allow** when at least one `permit` matches and no `forbid` matches.
- **Deny** when at least one `forbid` matches.
- **No decision** when no policy matches — falls back to the configured
  fallback policy (default: `deny-side-effects`).

### Entity model (schema v1)

Every tool call is an authorisation request with these entities:

| Component | Shape | Notes |
|-----------|-------|-------|
| `principal` | `User::"<runId>"` | Parents: `User::"any"`. Attributes: `runId` (String), `mode` (String), `parentRunId` (String, only on sub-agents), `capabilities` (Set\<String>). |
| `action` | `Action::"tool:<toolName>"` | One per tool. |
| `resource` | `Tool::"<toolName>"` | Mirror of action for symmetry. |
| `context` | Record | `input` (Record — recursively translated tool input), `workspace` (String), `dynamicContext` (Record — string keys to string values). |

JSON tool input is converted recursively: strings stay strings, integers
become `Long`, booleans become `Boolean`, arrays become `Set`, objects
become `Record`. Floats become String (lose precision); JSON `null`
values are dropped.

The schema version is pinned in
`harness/internal/permission/policyengine.go::CedarSchemaVersion`. Bump
it whenever the entity layout changes.

### Authoring policies

Starter policies under `examples/policies/`:

| File | Effect | Purpose |
|------|--------|---------|
| `destructive-shell.cedar` | `forbid` | Blocks `run_command` whose `cmd` matches `*rm -rf*`, `*chmod -R*`, `*git push --force*`, `*mkfs*`. |
| `github-only-fetch.cedar` | `permit` | Permits `web_fetch` only to `*.github.com`, `github.com`, `raw.githubusercontent.com`, `docs.python.org`. |
| `no-secret-in-input.cedar` | `forbid` | Forbids any tool whose input contains common leaked-secret patterns (`sk-*`, `ghp_*`, etc.). |
| `subagent-capability-cap.cedar` | `forbid` | Forbids `run_command` when `principal.parentRunId` is set (the caller is a sub-agent). Limits blast radius of `spawn_agent`. |

To compose multiple policies, concatenate them — Cedar accepts any
number of `permit` / `forbid` statements per document.

### Loading

```sh
stirrup harness --permission-policy-file examples/policies/destructive-shell.cedar ...
```

The flag is a convenience shortcut: it sets `permissionPolicy.policyFile`
and (when type is unset elsewhere) bumps `permissionPolicy.type` to
`policy-engine`.

In the RunConfig file:

```json
{
  "permissionPolicy": {
    "type": "policy-engine",
    "policyFile": "examples/policies/destructive-shell.cedar",
    "fallback": "deny-side-effects"
  }
}
```

`fallback` must be one of `allow-all`, `deny-side-effects`, or
`ask-upstream` — chained policy engines are intentionally rejected.

### Decisions are audited

Every Cedar decision emits one of:

- `policy_decision` (level `info`) on Allow or no-match (with the
  fallback outcome included).
- `policy_denied` (level `warn`) on Forbid (with matched policy IDs).

## 4. Rule of Two (B4)

The Meta "Agents Rule of Two" is a structural invariant: a single run
must not simultaneously hold all three of the following.

| Flag | True when |
|------|-----------|
| `holdsUntrustedInput` | `dynamicContext` populated, `web_fetch` enabled, OR any MCP server configured. |
| `holdsSensitiveData` | The provider/VCS/MCP `apiKeyRef` matches the secret-name heuristic (`*KEY*`, `*TOKEN*`, `*SECRET*`, `*PASSWORD*`, case-insensitive) or any reference uses `secret://ssm:///...`. |
| `canCommunicateExternally` | `run_command` enabled, `web_fetch` enabled, any MCP server configured, OR the executor has a non-`none` network mode. |

### Enforcement

`ValidateRunConfig` rejects the all-three case with:

```
all three of {untrusted-input, sensitive-data, external-communication}
cannot simultaneously hold without the ask-upstream permission policy
(Rule of Two)
```

The `ask-upstream` permission policy is the documented exception: it
gates every sensitive tool call on operator approval, which is the
"human in the loop" the rule prescribes.

### Override

For explicit operator override there is `ruleOfTwo.enforce: false` in
the RunConfig:

```json
{
  "ruleOfTwo": {
    "enforce": false
  }
}
```

There is **no CLI flag** for this — the override must live in the
RunConfig file so it is reviewable in pull requests.

When set, the validator passes the all-three case, but the harness emits
a `rule_of_two_disabled` security event at run start with the three flag
states for audit.

### Two-of-three warning

When exactly two of the three flags hold, the run is legal but the
harness emits a structural `rule_of_two_warning` event so reviewers
can spot capability creep one step before the invariant trips.

## 5. Code scanner (B5)

A post-edit static-analysis pass runs on every successful
`EditStrategy.Apply`. Findings of severity `block` roll back the edit
(restoring the prior file content) and surface as a tool failure.
Findings of severity `warn` log and emit `code_scan_warning` but the
edit succeeds.

### Scanner types

| Type | Implementation | Default availability |
|------|----------------|----------------------|
| `none` | no-op | always |
| `patterns` | pure-Go regex pack covering hardcoded secrets + eval/exec sinks | always — default for execution mode |
| `semgrep` | shells out to `semgrep --config auto --json` | requires `semgrep` on `$PATH` |
| `composite` | runs all configured child scanners and unions findings | requires `codeScanner.scanners` list |

### Configuration

```json
{
  "codeScanner": {
    "type": "patterns",
    "blockOnWarn": false
  }
}
```

`blockOnWarn` promotes warn findings to block. Use it when you want
warn-level rules to fail the edit for production runs while keeping
the same rule pack across environments.

For composite, the child scanner list must be supplied (each entry must
be a non-composite type — composite-of-composite is rejected):

```json
{
  "codeScanner": {
    "type": "composite",
    "scanners": ["patterns", "semgrep"]
  }
}
```

### Mode-aware default

`ValidateRunConfig` applies these defaults when `codeScanner` is unset:

- Execution mode: `{"type": "patterns"}`.
- Read-only modes (planning, review, research, toil): `{"type": "none"}`
  — there are no edits to scan.

### Security events

- `code_scan_warning` (level `warn`) — warn finding, edit applied.
- The edit-strategy error path surfaces blocking findings as tool
  errors with `rule@line: message` pairs.

## Four canonical configurations

These four configs cover the common operating points; each is a
runnable RunConfig snippet that validates against `ValidateRunConfig`.

### Dev — local iteration, no isolation

```json
{
  "runId": "dev-run",
  "mode": "execution",
  "prompt": "...",
  "provider": { "type": "anthropic", "apiKeyRef": "secret://ANTHROPIC_API_KEY" },
  "modelRouter": { "type": "static", "provider": "anthropic", "model": "claude-sonnet-4-6" },
  "promptBuilder": { "type": "default" },
  "contextStrategy": { "type": "sliding-window", "maxTokens": 200000 },
  "executor": { "type": "container", "image": "ghcr.io/rxbynerd/stirrup:latest", "runtime": "runc", "network": { "mode": "none" } },
  "editStrategy": { "type": "multi" },
  "verifier": { "type": "none" },
  "permissionPolicy": { "type": "allow-all" },
  "gitStrategy": { "type": "none" },
  "transport": { "type": "stdio" },
  "traceEmitter": { "type": "jsonl" },
  "tools": { "builtIn": ["read_file", "list_directory", "search_files", "edit_file", "run_command"] },
  "codeScanner": { "type": "none" },
  "ruleOfTwo": { "enforce": false },
  "maxTurns": 20,
  "timeout": 600
}
```

`allow-all` + `runc` + `network.mode: none` + `codeScanner: none`. Fast,
permissive; not for shared workloads. The `ruleOfTwo.enforce: false`
override is required because the `secret://ANTHROPIC_API_KEY` ref
combined with `run_command` and a populated tool set may otherwise hit
the all-three case.

### Defaults — the recommended baseline

```json
{
  "runId": "default-run",
  "mode": "execution",
  "prompt": "...",
  "provider": { "type": "anthropic", "apiKeyRef": "secret://ANTHROPIC_API_KEY" },
  "modelRouter": { "type": "static", "provider": "anthropic", "model": "claude-sonnet-4-6" },
  "promptBuilder": { "type": "default" },
  "contextStrategy": { "type": "sliding-window", "maxTokens": 200000 },
  "executor": { "type": "container", "image": "ghcr.io/rxbynerd/stirrup:latest", "runtime": "runc", "network": { "mode": "none" } },
  "editStrategy": { "type": "multi" },
  "verifier": { "type": "none" },
  "permissionPolicy": { "type": "deny-side-effects" },
  "gitStrategy": { "type": "deterministic" },
  "transport": { "type": "stdio" },
  "traceEmitter": { "type": "jsonl" },
  "tools": { "builtIn": ["read_file", "list_directory", "search_files", "edit_file", "run_command"] },
  "codeScanner": { "type": "patterns" },
  "ruleOfTwo": { "enforce": false },
  "maxTurns": 20,
  "timeout": 600
}
```

`deny-side-effects` + `runc` + `network.mode: none` + `codeScanner:
patterns`. The recommended starting point. Workspace mutation goes
through the policy; the patterns scanner blocks obvious secret/eval
patterns.

### Hardened — kernel isolation, allowlisted egress, Cedar + composite scanner

```json
{
  "runId": "hardened-run",
  "mode": "execution",
  "prompt": "...",
  "provider": { "type": "anthropic", "apiKeyRef": "secret://ANTHROPIC_API_KEY" },
  "modelRouter": { "type": "static", "provider": "anthropic", "model": "claude-sonnet-4-6" },
  "promptBuilder": { "type": "default" },
  "contextStrategy": { "type": "sliding-window", "maxTokens": 200000 },
  "executor": {
    "type": "container",
    "image": "ghcr.io/rxbynerd/stirrup:latest",
    "runtime": "runsc",
    "network": { "mode": "allowlist", "allowlist": ["api.github.com", "*.githubusercontent.com"] }
  },
  "editStrategy": { "type": "multi" },
  "verifier": { "type": "none" },
  "permissionPolicy": {
    "type": "policy-engine",
    "policyFile": "examples/policies/destructive-shell.cedar",
    "fallback": "deny-side-effects"
  },
  "gitStrategy": { "type": "deterministic" },
  "transport": { "type": "stdio" },
  "traceEmitter": { "type": "otel", "endpoint": "localhost:4317" },
  "tools": { "builtIn": ["read_file", "list_directory", "search_files", "edit_file", "run_command", "web_fetch"] },
  "codeScanner": { "type": "composite", "scanners": ["patterns", "semgrep"] },
  "ruleOfTwo": { "enforce": false },
  "maxTurns": 20,
  "timeout": 600
}
```

`policy-engine` + `runsc` + `network.mode: allowlist` + `codeScanner:
composite`. Production posture. Cedar gates destructive shell commands;
gVisor isolates the kernel; egress is FQDN-restricted; both pattern and
semgrep scanners run on every edit.

### Read-only — research / planning, no writes

```json
{
  "runId": "readonly-run",
  "mode": "research",
  "prompt": "...",
  "provider": { "type": "anthropic", "apiKeyRef": "secret://ANTHROPIC_API_KEY" },
  "modelRouter": { "type": "static", "provider": "anthropic", "model": "claude-sonnet-4-6" },
  "promptBuilder": { "type": "default" },
  "contextStrategy": { "type": "sliding-window", "maxTokens": 200000 },
  "executor": { "type": "container", "image": "ghcr.io/rxbynerd/stirrup:latest", "runtime": "runc", "network": { "mode": "none" } },
  "editStrategy": { "type": "multi" },
  "verifier": { "type": "none" },
  "permissionPolicy": { "type": "ask-upstream", "timeout": 60 },
  "gitStrategy": { "type": "none" },
  "transport": { "type": "stdio" },
  "traceEmitter": { "type": "jsonl" },
  "tools": { "builtIn": ["read_file", "list_directory", "search_files", "web_fetch", "spawn_agent"] },
  "codeScanner": { "type": "none" },
  "maxTurns": 20,
  "timeout": 600
}
```

`ask-upstream` + `runc` + `network.mode: none` + no scanner. Read-only
modes (`planning`, `review`, `research`, `toil`) cannot enable
write-capable tools (enforced by `ValidateRunConfig`); the scanner is
unused because no edits happen. `ask-upstream` is the documented Rule-
of-Two-compatible policy when web_fetch + secret-named API key + MCP
servers may all be present.
