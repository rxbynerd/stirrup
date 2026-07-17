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
- **Prompt templates** (`promptBuilder.template`) are syntax-checked
  at validation and trial-rendered at component construction. The
  template data surface is plain strings and pure matching methods —
  no filesystem, environment, or network reach — so an
  operator-supplied template is string interpolation only.
  `systemPromptOverride` is never template-parsed. Combining the
  override with `promptBuilder.template` or
  `promptBuilder.promptModel` is rejected rather than resolved by
  precedence.

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

## SSRF protection (`web_fetch` and MCP)

The `web_fetch` tool layers four checks before any HTTP request goes
out:

1. **Scheme allowlist:** only `http` and `https`. No `file://`, no
   `gopher://`, no other URL schemes.
2. **DNS resolution:** the hostname is resolved before the request is
   issued; the resolved IP is checked against the blocklist.
3. **IP block list:** RFC 1918 private ranges, RFC 6598 carrier-grade
   NAT (`100.64.0.0/10`), loopback (127/8), link-local (169.254/16),
   and multicast (224/4 and similar) are rejected. This prevents
   cloud-metadata exfiltration (`169.254.169.254`) and lateral
   movement to internal services — including deployment targets that
   route the CGNAT range to internal subnets.
4. **Response cap:** 100 KB. Larger responses are truncated; the
   truncation is visible to the model.

Checks 1–3 live in one place — the `security.ValidatePublicHost`
guard — and are reused by every component that dials a
caller-supplied host. The MCP client shares the same guard so there
is a single SSRF blocklist to audit, not one per consumer.

The transport `DialContext` re-runs the host check at connect time
(`security.SafeDialContext` for `web_fetch`,
`security.LoopbackAwareDialContext` for MCP), so a hostname that
passes the resolve-time check but whose DNS answer later flips to a
private address — a DNS-rebinding attack — is still refused when the
socket is actually opened. The rebinding guard is best-effort across
the resolve→dial gap; it does not pin a specific IP for the lifetime
of a long-lived connection.

### MCP server trust model

An MCP server is untrusted: it supplies tool definitions and tool
results the harness threads into the model's context and, for
write-capable tools, into actions. Two operator controls bound what
a compromised or misconfigured server can do (`MCPServerConfig`):

- **`uri` scheme and host:** the URI must be `http`/`https`. A remote
  (non-loopback) host must use `https`, so credentials and tool-call
  payloads are not sent in clear; plain `http` is permitted only for
  `localhost`/loopback during local development. The host is then run
  through the shared SSRF guard above, so a server URI resolving to a
  private or reserved address is refused at connect. `ValidateRunConfig`
  rejects the malformed-config cases (missing name/uri, bad scheme,
  non-`https` remote, malformed `allowedMCPHosts`) before a run starts.
- **`allowedTools`:** an optional per-server allowlist matched against
  the server-reported tool name. When set, any advertised tool not on
  the list is refused at registration with a logged reason, so a
  server cannot smuggle in tools beyond the set it was trusted for.
  The filter runs before the per-server tool-count cap. An unset list
  registers every advertised tool (the historical, backward-compatible
  behaviour).
- **`allowedMCPHosts`:** an optional host pin. When set, the URI host
  must appear in the list (exact, case-insensitive) in addition to
  passing the SSRF guard — a defence against a server URI being
  repointed at an unexpected host.

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
  only the project's own `ghcr.io/rxbynerd/*` images and Docker Hub
  official `docker.io/library/*` images; any other reference is
  rejected before a container is created. Operators widen or
  replace the set via `executor.registryAllowlist` (a list of
  globs over the normalised `host/repo` reference, tag/digest
  stripped, with the `index.docker.io` / `registry-1.docker.io`
  pull aliases folded to `docker.io`). Globs follow `path.Match`
  semantics, so `*` matches one path segment and does not cross
  `/`: `ghcr.io/rxbynerd/*` admits `ghcr.io/rxbynerd/base` but not
  `ghcr.io/rxbynerd/team/base` (use `ghcr.io/rxbynerd/*/*` for the
  deeper namespace). An explicit list *replaces* the default rather
  than extending it. Digest-pinned references (`@sha256:…`) are
  accepted and preferred; cryptographic verification of the digest
  (cosign/Sigstore) is a deferred follow-up. This allowlist governs
  only the container executor's own image pulls (via the Docker
  Engine API); it is not consulted for the `k8s`/`k8s-sandbox`
  executors, whose Pod image is scheduled by the cluster. A
  deployment pulling container-executor images from Google Cloud
  Artifact Registry (a `*.pkg.dev` host, including a GAR
  remote/standard repository) must add that host to
  `executor.registryAllowlist` explicitly — the default does not
  admit any Artifact Registry host.
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

## Lifecycle hooks

`hooks.preRun` / `hooks.postRun` (issue #461) run operator-authored
shell commands around the agentic session — outside the model's
turn-by-turn control entirely, not through the tool layer the rest of
this document gates. That is a different trust boundary from
everything else in this document, and worth being explicit about:

- **Hook commands are operator config, not agent output.** They come
  from `RunConfig` — a file, stdin, or the control plane's
  `task_assignment` — the same trust level as `verifier.command` or
  the `deterministic` git strategy's branch-naming logic. The model
  never sees hook commands and cannot construct or influence one; the
  per-tool-call gates (input validator, `PermissionPolicy`, `GuardRail`
  pre-tool) do not apply because hooks never reach the tool dispatch
  path.
- **Hook output is trace-only.** `HookExecution.OutputTail` (scrubbed,
  capped at 4 KB, tail not head) and `Error` are recorded for
  operator/control-plane visibility and never appended to the model's
  message history. A hook cannot smuggle content into the
  conversation the way a tool result can.
- **Hooks share the run's existing egress posture.** They dispatch
  through the same `Executor` as every agent tool call, so a `preRun`
  clone inherits the same network mode, allowlist, and egress proxy
  the rest of the run uses — there is no separate hook-specific
  network surface to configure or audit.
- **`secret://` is structurally rejected in `command`.**
  `ValidateRunConfig` rejects any hook `command` containing a
  `secret://` reference before the harness boots, so a credential can
  never be resolved and inlined into a hook's `sh -c` invocation the
  way `apiKeyRef` is for a provider. Clone/deploy credentials belong
  in control-plane runtime bindings (e.g. a pre-provisioned SSH agent
  or short-lived deploy token injected into the workspace before the
  hook runs), never in `RunConfig` — issue #516 ships a concrete
  instance of that binding for git specifically; see [Sandbox
  identity tokens](#sandbox-identity-tokens-git-proxy-credential-binding),
  below.

  This pattern moves the credential to a different trust boundary, not
  out of the agent's reach: anything a hook leaves on disk as a side
  effect — an SSH private key, a `.netrc`, a deploy token embedded in
  `.git/config` — is readable by every agent tool for the remainder of
  the run, `run_command` in particular, which is not confined to the
  workspace-relative path containment `read_file` / `write_file`
  enforce. "Trace-only output" above covers hook stdout/stderr, not
  files a hook writes. Prefer a short-lived, narrowly-scoped credential
  (so exposure is bounded even if the agent reads it) and have the
  hook clean up the material before returning (e.g. `rm` the key,
  `git config --unset` the embedded token) rather than relying on
  workspace boundaries to hide it.
- **Hooks do not interact with the Rule of Two.** The invariant bounds
  what the *agent* can simultaneously hold (untrusted input, sensitive
  data, external communication); hooks add none of those — they run
  outside the conversation, their output is trace-only, and they share
  rather than expand the run's egress posture. `HooksConfig` is
  therefore absent from all three Rule-of-Two legs.
- **Read-only modes allow hooks.** The read-only invariant
  (`planning`, `review`, `research`, `toil` reject `write_file`,
  `run_command`, `edit_file`) bounds *agent-reachable* tools;
  operator-authored, reviewable shell commands outside the tool surface
  already have precedent there (the test-runner verifier's command, the
  `deterministic` git strategy). See [`configuration.md` "Lifecycle
  hooks"](configuration.md#lifecycle-hooks) for the full schema and
  failure semantics.

**Heartbeat caveat:** the agentic loop's `heartbeat` transport event
(every 30 s, see [`architecture.md`
"Heartbeat and health probes"](architecture.md#heartbeat-and-health-probes))
is emitted on `runCtx` — the run's own wall-clock-bounded context — and
stops the instant that context is done, at the run's `timeout` deadline
or a control-plane cancel. `postRun` hooks execute afterwards on a
*detached* context (`context.WithoutCancel`) specifically so they can
outlive that deadline. This means a run whose `postRun` hooks are still
uploading an artifact after wall-clock expiry emits **no heartbeats**
for up to the detached budget (sum of configured `postRun` timeouts
plus a 30 s margin, capped at 1830 s). A control plane that treats a
heartbeat gap as "orphaned and safe to reap" may kill the process
mid-upload. Operators relying on heartbeat-based liveness for runs with
`postRun` hooks should account for this gap in their reap timeout;
re-arming heartbeats for the detached post-hook phase is tracked as a
follow-up if this proves disruptive in practice.

## Sandbox identity tokens (git-proxy credential binding)

Issue #516 ships a concrete instance of the control-plane runtime
binding named above, scoped to cloning and pushing private
repositories. The control plane issues a short-lived **sandbox
identity token** — a signed JWT — to the harness over the existing
gRPC control stream, fail-closed with a 60-second wait and a 16 KiB
cap on the returned token (`sandboxidentity.MaxTokenBytes`): a control
plane that is slow, silent, or declines to issue aborts the run before
any sandbox is created, rather than leaving a partially-provisioned,
tokenless sandbox behind. The harness then injects the token, plus
non-secret `GIT_CONFIG_*` environment variables that rewrite `git`
remote URLs, into the sandbox environment at creation time.

Git operations inside the sandbox are routed through a
git-credential proxy such as
[haybale](https://github.com/rxbynerd/haybale), which holds the real
GitHub App credential *outside* the sandbox entirely and authenticates
each proxied operation using the run's token, presented as the HTTP
Basic-auth password on the rewritten request. The invariant this
preserves: the raw git credential never enters the sandbox or
`RunConfig` — the sandbox only ever holds the run-scoped token, and
the proxy is the only component that ever sees the long-lived
credential. The token itself never touches trace, transcript, or log
output: the sandbox-identity exchange code path never logs, traces, or
persists it, independent of the `oidc_jwt` `LogScrubber` pattern above
that would otherwise backstop an accidental leak.

See [`deployment.md`'s "Sandbox identity token
issuance"](deployment.md#sandbox-identity-token-issuance-control-plane-implementers)
for the gRPC wire contract a control plane implements to answer the
request, and [`configuration.md`'s "Sandbox identity and git-proxy
wiring"](configuration.md#sandbox-identity-and-git-proxy-wiring) for
the `executor.sandboxIdentity` / `executor.gitProxy` `RunConfig`
fields that request it.

## Where each control sits

```text
Controls layered from outside in:

  ── RunConfig load ────────────────────────────────────────
   ValidateRunConfig: bounded budgets, read-only invariants,
   credential consistency, Cedar policy path checks, hook
   command bounds + secret:// rejection.

  ── Pre-session (outside the turn loop) ───────────────────
   preRun hooks: operator exec via the run's own Executor,
   before GitStrategy.Setup. Trace-only output; never reaches
   the model. Fatal failure -> outcome "setup_failed", zero
   turns.

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

  ── Post-session (outside the turn loop) ──────────────────
   postRun hooks: operator exec via the run's own Executor,
   after GitStrategy.Finalise, on a detached context so
   wall-clock expiry does not cut them off. Trace-only
   output. Fatal failure overrides outcome to "hook_failed"
   only when the run would otherwise report success.

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
