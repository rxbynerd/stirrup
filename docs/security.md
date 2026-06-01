# Security foundations

The harness holds API keys and runs code that an LLM can be coerced
into producing. Two layers of controls compose to bound the blast
radius.

The **safety rings** (operator-facing, configurable) catch attacks at
the boundaries of a run — pre-flight, per tool call, in-sandbox, and
post-edit. They are documented separately in
[`safety-rings.md`](safety-rings.md). LLM-based content classification
sits in front of the rings via the `GuardRail` component
([`guardrails.md`](guardrails.md)).

The **foundations** below are the in-harness controls that are always
on. They are not configurable knobs; they are structural properties
of the codebase that make whole classes of vulnerability (secret
leakage, path traversal, SSRF, prototype pollution, unbounded
memory) hard or impossible to reintroduce.

## Secrets

### `SecretStore`

API keys are never stored in `RunConfig`. The `RunConfig` carries
`secret://` references that resolve at runtime through
`harness/internal/security/SecretStore`. Three backends ship,
auto-routed by URL scheme:

| Reference | Backend |
|---|---|
| `secret://NAME` | Environment variable `NAME` |
| `secret://file:///abs/path` | File contents at `/abs/path` |
| `secret://ssm:///param-name` | AWS SSM Parameter Store (initialised lazily) |

`AutoSecretStore` only initialises the SSM client when a config
reference requires it, so plain env-var deployments do not pay the
AWS-SDK initialisation cost.

### `RunConfig.Redact()`

Before any trace, recording, or persistent log writes the
`RunConfig`, it is run through `Redact()` which strips every
`secret://` reference, normalises provider and MCP URLs to their
origin, redacts provider API-key header names, and redacts
non-allowlisted provider query parameters. The persisted artifact
never contains anything that could be replayed against a provider.

### `LogScrubber`

The `harness/internal/observability/ScrubHandler` wraps any
`slog.Handler` and runs `security.Scrub()` on every string attribute
value before delegation. Nineteen regex patterns are scrubbed,
covering:

- Anthropic API keys
- OpenAI and Stripe API keys
- GitHub PATs and app tokens
- AWS access key IDs and key/value-anchored AWS secret access keys
- Slack tokens
- GCP API keys and OAuth2 access tokens
- Key/value-anchored Azure storage keys
- Bearer tokens including JWTs (Authorization headers; Basic auth)
- PEM-encoded private keys
- API-key header values
- Key/value-anchored 32-character hex secrets
- `secret://` references themselves

Because the scrubber sits at the `slog.Handler` boundary, secret
leakage through a misformatted log line is structurally impossible —
the scrubber runs before any handler (JSON, text, OTel log
exporter) sees the attribute.

## Input validation

All tool inputs are validated against a JSON Schema before the tool
function runs. The validator
(`harness/internal/security/inputvalidator.go`) uses
`santhosh-tekuri/jsonschema/v6` for full Draft 2020-12 support
(inline `$ref`/`$defs`, `oneOf`/`anyOf`/`allOf`, `format`, `enum`,
`pattern`, numeric bounds, array item validation).

Two hardenings apply on top of standard schema validation:

- **External schema loading is disabled.** A hostile MCP server
  cannot induce a local file read or SSRF by referencing a
  `$ref: file:///etc/passwd` or `$ref: http://attacker/...`.
- **Prototype-pollution keys are stripped.** `__proto__`,
  `constructor`, and similar are removed from the input object before
  validation, preventing schema-conforming inputs that exploit
  JS-style prototype-chain mutation in downstream tooling.

## `RunConfig` validation

`types.ValidateRunConfig` enforces hard invariants before any
component is constructed. Anything that fails validation is rejected
at config-load time, not at runtime.

The most security-relevant invariants:

- **Read-only modes** (`planning`, `review`, `research`, `toil`) must
  exclude `write_file`, `run_command`, and `edit_file` from
  `tools.builtIn`, and must not use `permissionPolicy.type=allow-all`.
- **Bounded budgets:** `maxTurns` ≤ 100, `timeout` ≤ 3600 s,
  `followUpGrace` ≤ 3600 s, `maxCostBudget` ≤ $100,
  `maxTokenBudget` ≤ 50 M.
- **Mutually exclusive credentials:** `apiKeyRef` and
  `credential.type` cannot both be set on the same provider.
- **Cedar policy file paths** reject `..` traversal segments;
  workspace-relative paths are resolved against `executor.workspace`.
- **Path-only fields** (workspace, trace, policy file, semgrep
  config) are validated for traversal and absolute-path constraints
  appropriate to their use.

## HTTP client hardening

Every provider adapter and the MCP client uses an explicit
`*http.Client` with timeouts:

| Client | Timeout |
|---|---|
| Provider streaming (Anthropic, OpenAI, OpenAI Responses, Bedrock, Gemini) | 120 s |
| MCP client | 30 s |
| Web fetch tool | 30 s |

`http.DefaultClient` is never used in production code. Error
response bodies are bounded with `io.LimitReader` to avoid
unbounded memory consumption when a provider returns an unexpectedly
large error payload.

## SSRF protection (`web_fetch`)

The `web_fetch` tool layers four checks before any HTTP request goes
out:

1. **Scheme allowlist:** only `http` and `https`. No `file://`, no
   `gopher://`, no other URL schemes.
2. **DNS resolution:** the hostname is resolved before the request is
   issued; the resolved IP is checked against the blocklist.
3. **IP block list:** RFC 1918 private ranges, loopback (127/8),
   link-local (169.254/16), and multicast (224/4 and similar) are
   rejected. This prevents cloud-metadata exfiltration
   (`169.254.169.254`) and lateral movement to internal services.
4. **Response cap:** 100 KB. Larger responses are truncated; the
   truncation is visible to the model.

## Path traversal prevention

All three `Executor` implementations enforce workspace containment:

- **`local`** uses `filepath.EvalSymlinks` to resolve symlinks
  before the path is checked, so symlink-based escapes are caught.
- **`container`** is structurally sandboxed by the container
  itself; the workspace is bind-mounted at a fixed path.
- **`api`** validates workspace-relative paths and uses
  `url.PathEscape` on every path segment before constructing the
  GitHub Contents API URL.

The `grep_files` and `find_files` tools call `ResolvePath` on the
search root before any directory walk begins, so a workspace-relative
path that escapes the workspace is rejected before the walker sees
it. `grep_files` additionally uses `shellQuote()` on every value
interpolated into the `rg` invocation. Tested against
`../../../etc/passwd`, symlink escapes, and absolute paths.

## Environment filtering

When the local executor runs a shell command, the child process's
environment is filtered to a 27-key allowlist of safe variables
(`HOME`, `PATH`, `LANG`, `LC_*`, `SHELL`, language-specific build
vars, etc.). Everything else is dropped.

The filter blocks:

- All `*_API_KEY` and `*_TOKEN` variables.
- All AWS, GCP, and Azure credential variables
  (`AWS_*`, `GOOGLE_*`, `AZURE_*`).
- The `secret://` variables that the harness itself reads.

A successful `run_command` therefore cannot leak the harness's
own credentials, even by accident, even if the model asks the
shell to print them.

## Container hardening

The container executor applies these defaults regardless of what
`RunConfig` says:

- `CapDrop: ["ALL"]`
- `SecurityOpt: ["no-new-privileges"]`
- `ReadonlyRootfs: true` — the container's root filesystem is
  immutable. All writable scratch is confined to the workspace
  bind and the explicit tmpfs mounts below, so a compromised run
  cannot persist a payload outside the paths it actually needs.
- `User: "65534:65534"` (nobody:nogroup) — the main process and
  every `run_command` exec run unprivileged, so a container escape
  lands on a non-root identity rather than uid 0.
- A 256 MiB `/tmp` tmpfs and a 64 MiB `/dev/shm`, each mounted
  `nosuid,nodev,noexec`. These provide the writable, non-executable
  scratch a read-only rootfs otherwise denies; `noexec` stops a
  dropped binary from being run out of scratch.
- `PidsLimit` from `resources.pids` (fork-bomb containment),
  alongside the CPU and memory limits from `resources`.
- `NetworkMode: "none"` (overridden to `"bridge"` only when
  `network.mode == "allowlist"`, in which case the egress proxy
  enforces FQDN allowlisting on the way out)
- A registry allowlist on `executor.image`. The default admits
  only the project's own `ghcr.io/stirrup/*` images and Docker Hub
  official `docker.io/library/*` images; any other reference is
  rejected before a container is created. Operators widen or
  replace the set via `executor.registryAllowlist` (a list of
  globs over the normalised `host/repo` reference, tag/digest
  stripped, with the `index.docker.io` / `registry-1.docker.io`
  pull aliases folded to `docker.io`). Globs follow `path.Match`
  semantics, so `*` matches one path segment and does not cross
  `/`: `ghcr.io/stirrup/*` admits `ghcr.io/stirrup/base` but not
  `ghcr.io/stirrup/team/base` (use `ghcr.io/stirrup/*/*` for the
  deeper namespace). An explicit list *replaces* the default rather
  than extending it. Digest-pinned references (`@sha256:…`) are
  accepted and preferred; cryptographic verification of the digest
  (cosign/Sigstore) is a deferred follow-up.
- API keys and `secret://` references are resolved on the *host*
  before tool dispatch; they never enter the container's
  environment.

The workspace bind is *not* mounted `nosuid,nodev,noexec`. The
legacy `Binds` string form can express those options to the kernel,
so this is a functional choice, not an Engine-API limitation: the
run must be able to execute tooling it writes into `/workspace`
(build outputs, test binaries, vendored scripts), and `noexec`
there would break that. The compensating controls cover the gap a
missing `nosuid` would otherwise leave — `CapDrop: ["ALL"]` removes
`CAP_SETUID` and `CAP_FOWNER`, and `no-new-privileges` blocks the
setuid bit from elevating, so a setuid binary planted in
`/workspace` cannot escalate even though the bind permits suid
mounts.

Optional kernel-isolation runtime selection (`runc`, `runsc`, `kata*`)
is documented in [`safety-rings.md`](safety-rings.md).

## Untrusted context

Anything the harness receives over the network (tool outputs, web
fetch responses, MCP server replies, `dynamicContext` injected by
the control plane) is wrapped in `<untrusted_context>` tags before
being shown to the model. The mode-specific system prompt instructs
the model to treat content inside those tags as data, not
instructions. This is a probabilistic mitigation against prompt
injection; the deterministic ring is the egress allowlist (Ring 2)
and the Cedar policy engine (Ring 3) which catch the *consequences*
of a successful injection.

The `dynamicContext` map further distinguishes sensitive entries
(`sensitive: true`) so the validator can reason about Rule of Two
exposure and the loop can apply tighter handling rules.

## Stall detection

The agentic loop terminates after:

- **3 consecutive identical tool calls** (same name + same canonical
  input) — `stop_reason: "stalled"`.
- **5 consecutive tool failures** — `stop_reason: "tool_failures"`.

This bounds the damage of a model that has lost the plot — without
stall detection, a coerced or stuck model can burn through the
turn budget executing the same destructive call until something
else trips. The loop also surfaces a warning to the transport so
the control plane can react.

## Where each control sits

```text
Controls layered from outside in:

  ── RunConfig load ────────────────────────────────────────
   ValidateRunConfig: bounded budgets, read-only invariants,
   credential consistency, Cedar policy path checks.

  ── Provider boundary ─────────────────────────────────────
   SecretStore + LogScrubber: secrets resolve lazily and never
   appear in logs or trace artifacts.

  ── Per-turn ──────────────────────────────────────────────
   GuardRail (pre-turn): LLM classifier on untrusted text
   before it enters context.

  ── Per tool call ─────────────────────────────────────────
   Input validator: JSON Schema + prototype-pollution strip.
   GuardRail (pre-tool): LLM classifier on the proposed call.
   PermissionPolicy: structural deny / Cedar policy / ask
   upstream.

  ── In sandbox ────────────────────────────────────────────
   Executor hardening: caps, no-new-privileges, network none,
   path containment, env filtering.
   Egress proxy (when allowlist mode): FQDN allowlist + SNI.

  ── After tool result ─────────────────────────────────────
   GuardRail (post-turn): LLM classifier on assistant text
   (catches secret-shaped output, hallucinated tool calls).
   Code scanner (post-edit): pattern + optional semgrep,
   block findings roll back the write.

  ── Run lifetime ──────────────────────────────────────────
   Stall detection: bounded consecutive identical calls /
   failures.

  ── Persistence ──────────────────────────────────────────
   RunConfig.Redact(): strips secret refs from anything
   written to disk or a trace store.
```

## See also

- Operator-facing guide to the five rings:
  [`safety-rings.md`](safety-rings.md).
- LLM-based safety classifier configuration:
  [`guardrails.md`](guardrails.md).
- Disclosure policy: [`SECURITY.md`](../SECURITY.md).
- Credential federation walkthroughs:
  [`credential-federation.md`](credential-federation.md),
  [`anthropic-wif.md`](anthropic-wif.md),
  [`azure-workload-identity.md`](azure-workload-identity.md).
