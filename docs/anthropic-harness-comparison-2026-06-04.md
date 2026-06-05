# Stirrup ← Anthropic Defending-Code Reference Harness: Comparative Analysis

## Executive Summary

Both projects run autonomous LLM agents against untrusted input and both treat that as inherently dangerous, but they sit at opposite points on the generality axis: Anthropic's harness is a narrow, unmaintained **reference** for one task — finding memory bugs in hostile C/C++ via a multi-agent recon→find→grade→judge→dedup→report→patch pipeline gated by an ASAN executable oracle — while Stirrup is a **general-purpose, pre-1.0** harness built around a single agentic loop, a factory, and a `RunConfig`. The strongest philosophical alignment is already shared: *constraints belong in code and configuration, not prompts* — Stirrup's five deterministic safety rings, FQDN-allowlisting egress proxy, `secret://` indirection + `Redact`, Cedar policy engine, and read-only-mode invariant are the general-purpose embodiment of exactly the posture Anthropic validates publicly (A:docs/blog-post.md:60). Stirrup is therefore *ahead* on secrets, permission policy, and K8s NetworkPolicy egress, and has no need for the vuln-specific machinery. The three biggest transferable ideas are: **(1) runtime-truth isolation verification** — re-inspect the started container's effective runtime and fail loud on mismatch, and probe runtime-registration at dry-run (today Stirrup plumbs `Runtime` but never re-reads it); **(2) executable-oracle integrity** — give the runtime verifier a fresh-workspace re-verification mode and make LLM-judge verdicts advisory-by-default so an agent cannot talk its own gate into passing; and **(3) deterministic untrusted-data fencing** — unconditionally fence `web_fetch`/MCP output with a per-result nonce, independent of any (default-Noop) GuardRail. The find→grade→ASAN pipeline itself is a non-goal to copy; its *kernel* (independent adversarial verification, two-pass dedup, setup-then-freeze egress) is what ports.

---

## What Each Harness Is

| | Anthropic "Defending Code Reference Harness" | Stirrup |
|---|---|---|
| **Purpose** | Narrow: find/verify/patch memory-safety bugs in C/C++ | General-purpose coding-agent harness |
| **Shape** | Multi-agent **runtime** pipeline: recon → find → grade → judge → dedup → report → patch (A:README.md:177-204) | Single agentic loop (`harness/internal/core/loop.go`) + offline `eval/` suite |
| **Verification** | Executable oracle (ASAN crash / build exit / PoC re-run), LLM tier advisory-only (A:docs/patching.md:73-90) | Pluggable `Verifier` (test-runner, llm-judge, composite) invoked inside the loop |
| **Isolation** | gVisor sandbox, refuse-to-start otherwise (A:README.md:48), `--dangerously-no-sandbox` opt-out | container + K8s Pod-per-run executors; `local` executor runs `sh -c` on host |
| **Maintenance** | "Reference, not a product"; unmaintained, not accepting contributions (A:README.md:12,36) | Live, pre-1.0, contributions-driven |
| **Composition** | Forked/ported via `/customize`; C/C++/ASAN isolated to named files | `RunConfig` + factory; `harness/harnessapi/` public surface |

**Key architectural difference:** Anthropic achieves integrity through *process separation across many agents in fresh containers* (the grader never sees the finder's reasoning or filesystem — A:docs/pipeline.md:63-65). Stirrup achieves integrity through *deterministic controls around one loop* plus a CI-side `eval/` judge. The transferable pattern is to import Anthropic's separation discipline *into Stirrup's single-loop verifier seam* — not to adopt its agent topology.

---

## At-a-Glance Comparison

| Dimension | Anthropic approach | Stirrup today | Gap |
|---|---|---|---|
| **Sandbox / isolation** | `docker inspect` re-verifies effective runtime, raises on mismatch (A:docs/docker_ops.py:69-82); `sandbox.require()` checks runtime registration (A:harness/sandbox.py:119-127); refuse-to-start | Sends `hostConfig.Runtime` on create, never re-reads it (S:harness/internal/executor/container.go:194); dry-run probe ignores `cfg.Runtime` (S:harness/internal/executor/container.go:135-150); `local` = no boundary | **Runtime truth never verified**; no require-sandbox gate; no adversarial negative probes |
| **Egress control** | `docker network --internal` = proxy is the only route out (A:scripts/setup_sandbox.sh:88); default allowlist = `api.anthropic.com:443` (A:scripts/egress_proxy.py:23-26) | Bridge network + `HTTP_PROXY` env; in-code TODO: raw-TCP bypasses proxy (S:harness/internal/executor/container.go:237-245); empty allowlist denies all | **Container egress not fail-closed**; no auto-include of provider host; `egress_blocked` dies at stderr |
| **Verification oracle** | Fresh-container re-verify; LLM advisory-only; ~60% patches pass build-and-reproduce but <15% survive re-attack (A:docs/patching.md:94-96) | Verifier shares the agent's mutated executor/workspace (S:harness/internal/core/loop.go:243); llm-judge sees full transcript and gates (S:harness/internal/verifier/llmjudge.go:76) | **Agent can influence its own gate**; LLM verdict is load-bearing |
| **Prompt injection** | Patch agent *always* fences target text with per-call nonce, regardless of classifier (A:harness/prompts/patch_prompt.py:66-68,137) | Fencing fires only via GuardRail `VerdictAllowSpot`; default GuardRail is Noop, no shipped adapter emits spotlight | **No fence on stock build** for `web_fetch`/MCP |
| **Permission model** | `permission_mode()` derived from `runtime()` (A:harness/sandbox.py:46-55) | Read-only-mode invariant strips mutating tools; but no isolation→policy coupling | **Permission floor not derived from isolation tier**; sub-agents inherit full toolset |
| **Secrets** | Never mount `~/.aws`/`.env`; credential-proxy injects outside boundary | `secret://` refs + `Redact()`; executors get no provider key; `local` env filtered to 27-key allowlist | **Strong already**; gaps are docs + task-scoped credential surface |
| **Skills / customization** | `/customize` port guide; `--extra`/`--fp-rules` tuning files | 5 compiled-in modes; `SystemPromptOverride` is all-or-nothing | **No recipe registry, no additive prompt seam, no public tool-registration hook** |
| **Checkpoint / resume** | Long-lived session + `--resume`; durable per-stage transcripts (A:docs/pipeline.md:164-173) | Per-turn git checkpoint only; GCS trace is summary-only no-op (S:harness/internal/trace/gcs.go:157-162) | **No conversation resume; serverless transcript thinnest where most needed** |
| **Threat-model / grading** | THREAT_MODEL.md as first-class scope; severity rubric; `--votes` majority | Freeform `DynamicContext`; binary verdicts; single-pass judging | **No structured scope artifact; no vote/quorum; no confidence/severity field** |

---

## Per-Theme Analysis

### 1. Sandbox & Isolation Model

**What Anthropic does.** Re-inspects the started container and raises on an image/runtime mismatch (A:harness/docker_ops.py:69-82 — verified: `docker inspect ... {{.HostConfig.Runtime}}` then `if runtime and actual_runtime != runtime: raise`). `sandbox.require()` performs a two-part env-AND-`docker info` check that the requested runtime is registered (A:harness/sandbox.py:119-127); `bin/vp-sandboxed` greps `docker info ... Runtimes` for `runsc` (A:bin/vp-sandboxed:15-17). `setup_sandbox.sh` runs *adversarial negative probes*: guest-kernel ≠ host-kernel, `example.com` and direct `8.8.8.8:53` blocked, host `/tmp` sentinel unreadable (A:scripts/setup_sandbox.sh:160-194 — verified).

**What Stirrup does today.** The container path sends `hostConfig.Runtime` on create (S:harness/internal/executor/container.go:194) but never re-reads the started container's actual runtime — there is no `GET /containers/{id}/json` anywhere in the executor (the only `/json` endpoints are image-presence and exec-inspect, S:harness/internal/executor/container_api.go:200-231). The dry-run probe pings the engine and checks image presence but ignores `cfg.Runtime` entirely (S:harness/internal/executor/container.go:135-150), even though `containerProbeConfig` *populates* the field (S:harness/internal/core/preflight.go:421-428) — it is plumbed but dead. K8s warns on empty `RuntimeClass` (S:harness/internal/executor/k8s.go:192-197); the container path emits no equivalent warning. `local` runs `sh -c` directly on the host with only env filtering (S:harness/internal/executor/local.go:207-209) and refuses only `network.mode=allowlist`, not `none`/nil.

**Recommendations.**

| ID | Title | Status | Effort |
|---|---|---|---|
| sandbox-isolation-1 | Re-inspect effective runtime post-launch via `docker inspect`, fail loud on mismatch (+ K8s `RuntimeClassName` re-read) | **valid** | low |
| sandbox-isolation-2 | Extend dry-run preflight with a `GET /info` probe asserting the requested OCI runtime is registered | **valid** | low |
| sandbox-isolation-3 | Opt-in `--require-sandbox` preflight: fail closed unless `container`/`k8s` AND a non-runc isolating runtime | **valid** | medium |
| sandbox-isolation-4 | Isolation smoke-test with adversarial negative probes (guest≠host kernel, host FS unreachable, non-allowlisted egress denied) | **partial** (positive path exists in kind smoke test) | medium |
| sandbox-isolation-5 | Document setup/attack two-phase contract + add an `executor.readonlyWorkspace` (`:ro`) bind option | **partial** (k8s already avoids host bind; mechanism for two-phase exists) | low |

All five reuse the existing stdlib-HTTP `containerAPIClient` helpers and respect the no-vendor-SDK invariant. sandbox-isolation-1 and -2 pair naturally and are the cleanest wins.

---

### 2. Network Egress Control

**What Anthropic does.** Structural deny-by-default: `docker network create --internal` means the sidecar proxy is the **only** route out (A:scripts/setup_sandbox.sh:88; A:scripts/egress_proxy.py documents "this proxy is their only path out"). Default allowlist is exactly `api.anthropic.com:443` (A:scripts/egress_proxy.py:23-26 — verified: `os.environ.get("VP_EGRESS_ALLOW") or "api.anthropic.com:443"`), so the model is reachable with zero config.

**What Stirrup does today.** In allowlist mode the container executor sets `NetworkMode="bridge"` (unrestricted egress) and relies on the in-container client honouring `HTTP_PROXY`/`HTTPS_PROXY` (S:harness/internal/executor/container.go:230-236). The in-code TODO is explicit and honest that a raw-TCP / own-DNS client bypasses the proxy and that full fail-closed needs a host iptables/nftables drop, which is privilege-sensitive and not portable to macOS Docker Desktop (S:harness/internal/executor/container.go:237-245 — verified verbatim). `egress_blocked` is emitted only to `*security.SecurityLogger`, whose sink is `os.Stderr` (S:harness/internal/core/factory.go:65) plus an optional OTel counter — it never becomes a transport `HarnessEvent` or `RunResult` field. There is no default allowlist; an empty allowlist denies every request, so an operator who forgets to list the provider host fails the model call itself.

**Recommendations.**

| ID | Title | Status | Effort |
|---|---|---|---|
| egress-control-1 | Host-firewall (nftables) drop / `--internal` user network so bridge egress is deny-by-default except the proxy (Linux-only, behind a flag) | **valid** | high |
| egress-control-2 | Surface `egress_blocked` as a transport `HarnessEvent` + `RunResult` warning (stall-detection already warns the transport; egress does not) | **valid** | medium |
| egress-control-3 | Auto-include resolved provider/MCP hosts in the allowlist (opt-out) + a config-load probe that the provider host is present-and-resolvable | **valid** | medium |
| egress-control-4 | General `validateNetwork` in `ValidateRunConfig`: reject unknown modes / empty-allowlist-in-allowlist-mode for **all** executors (today k8s-only) | **partial** | low |
| egress-control-5 | Document the setup-then-freeze + deny-by-topology egress recipe in `docs/security.md` (caveats currently live only in code comments) | **valid** | low |

A Docker `--internal` user network is arguably the simpler structural fix than a host firewall and is worth scoping inside egress-control-1.

---

### 3. Multi-Agent Pipeline & Executable Verification Oracle

**What Anthropic does.** A finding/patch is valid only when a deterministic check decides (ASAN crash, build exit, PoC re-run, test suite) — "No patch fails based on model judgment" (A:docs/patching.md:73-76). The verification ladder gates on build → reproduce → regress → re-attack exit codes; the style/LLM tier is "advisory only, never gates" (A:docs/patching.md:80-90). The grader runs in a **fresh container with no shared FS or conversation history** — "the only thing that crosses is the PoC bytes" (A:docs/pipeline.md:63-65), framed adversarially ("guilty until proven innocent", A:docs/pipeline.md:156-157). Justified by evals: ~60% of model patches pass build-and-reproduce but <15% survive re-attack (A:docs/patching.md:94-96).

**What Stirrup does today.** The runtime `Verifier` is invoked with `Executor: l.Executor` — the *exact* executor/workspace the agent just mutated (S:harness/internal/core/loop.go:243) — so an agent that edits the test or plants a stub influences its own gate. `LLMJudgeVerifier.buildUserMessage` serializes every non-synthetic message — full transcript, the agent's own reasoning, `tool_use`/`tool_result` — into the judge prompt (S:harness/internal/verifier/llmjudge.go:76), and its boolean `Passed` short-circuits the outer loop to success (S:harness/internal/core/loop.go:255) with no executable-vs-LLM distinction. `TestRunnerVerifier` calls `exec.Exec` exactly once (S:harness/internal/verifier/testrunner.go:60), with no repeat/threshold and no distinction of timeout/OOM (124/137) from a genuine failure. `CompositeVerifier` merges details under index-only `verifier_%d` keys (S:harness/internal/verifier/composite.go:42) — no named tiers. After a verifier passes, nothing re-attacks the fix; `SpawnSubAgent` gives the child `NewNoneVerifier` and shares the parent's `Executor` (S:harness/internal/core/subagent.go:148,150) — a delegation helper, not an adversary. The only "resume" is eval-side, judge-only `ReplayRecording` (S:eval/runner/replay.go:18). **Crucially, the artifact-only judging primitive already exists in eval** — `diffreview.go` feeds only `git diff` to the model (S:eval/judge/diffreview.go:79) — and is exactly the pattern to generalise into the runtime verifier.

**Recommendations (all valid).**

| ID | Title | Status | Effort | Severity |
|---|---|---|---|---|
| pipeline-verification-1 | Opt-in **fresh-workspace independent verification** mode for the runtime verifier | **valid** | high | highest-integrity gap |
| pipeline-verification-2 | Stop feeding the agent transcript to llm-judge; make LLM verdicts **advisory-by-default**, only executable oracles gate | **valid** | medium | high |
| pipeline-verification-3 | N-of-N repeat-and-consistency execution in test-runner; classify 124/137 distinctly | **valid** | low | reliability |
| pipeline-verification-4 | Opt-in adversarial re-verification stage (bounded `spawn_agent` + fresh executable oracle, gate on oracle not child self-report) | **valid** | medium | speculative |
| pipeline-verification-5 | Ordered, **named tiers** in `CompositeVerifier` driving structured feedback | **valid** | medium | quality |
| pipeline-verification-6 | Durable per-stage checkpoint+resume for long runtime runs, keyed by idempotence key | **valid** | high | serverless |

pipeline-verification-1 and -2 are the highest-value pair: together they close the "agent grades its own homework" loophole, and -2 can be drafted by generalising the existing `diffreview` artifact-only primitive.

---

### 4. Prompt-Injection & Untrusted-Data Handling

**What Anthropic does.** The patch agent **always** fences target-derived text (ASAN traces, exploitability reports, build output) with a per-call `secrets.token_hex(4)` random delimiter and a co-located data-not-instructions note, *regardless of any classifier* — "prompt-level fencing is a mitigation, not a guarantee" (A:harness/prompts/patch_prompt.py:66-77,137). It applies blast-radius asymmetry: only the patch agent (output becomes a real diff) gets the fence; read-only find/report agents do not (A:docs/security.md:78-89).

**What Stirrup does today.** `web_fetch` returns the raw response body straight into the `tool_result` with no wrapper (S:harness/internal/tool/builtins/webfetch.go:116-119). The only mechanism that wraps tool output is `spotlightUntrustedChunks`, which fires **only** when a GuardRail returns `VerdictAllowSpot` (S:harness/internal/core/loop.go:1432-1448). But the default GuardRail is **Noop** (always `VerdictAllow`, S:harness/internal/guard/none.go:20; factory returns `NewNoop` when none configured, S:harness/internal/core/factory.go:1248), and **no shipped adapter ever emits `VerdictAllowSpot`** — Granite Guardian and cloud-judge are both binary (S:harness/internal/guard/graniteguardian.go:435-442; S:harness/internal/guard/cloudjudge.go:205-211). So on a stock build, **no fence/delimiter/note is applied to `web_fetch`/MCP output at all**. The `dynamicContext` wrapper is the static string `<untrusted_context name=%q>` with no nonce (S:harness/internal/prompt/default.go:50), mitigated by a *denylist* regex (`SanitizeDynamicContext`, S:harness/internal/security/dynamiccontext.go:7,31) that entity-encoded close-delimiters can evade. The data-not-instructions note exists **only** in the prompt fragment (S:harness/internal/prompt/default.go:41), never in any tool Description.

**Recommendations (all valid).**

| ID | Title | Status | Effort |
|---|---|---|---|
| prompt-injection-1 | **Unconditionally** fence `web_fetch`/MCP output with a per-result random-nonce delimiter, independent of any GuardRail (at the tool/dispatch layer, a pure string transform — respects loop purity) | **valid** | medium |
| prompt-injection-3 | Ship a **deterministic always-spotlight GuardRail** (no model call) so spotlighting is reachable from a stock build — doubles as the engine for prompt-injection-1 | **valid** | medium |
| prompt-injection-2 | Add a per-render `crypto/rand` nonce to the `<untrusted_context>` delimiter | **valid** | low |
| prompt-injection-4 | Escalate fencing on the high-blast-radius path (execution mode / side-effecting output), keying off the existing read-only-vs-execution invariant | **valid** | medium |
| prompt-injection-5 | Carry a standing data-not-instructions note in built-in untrusted-source tool Descriptions (an explicit exception to the impersonal-voice doc rule) | **valid** | low |

prompt-injection-1 + -3 are the structural pair; -2/-4/-5 are cheap defense-in-depth that pair with them (a note without a fence is weak). This dimension aligns directly with safety-rings doctrine that deterministic controls are preferred over LLM guards (S:docs/safety-rings.md:55-60).

---

### 5. Permission Model & Read-Only/Execution Separation

**What Anthropic does.** Derives `permission_mode()` from `runtime()` (A:harness/sandbox.py:46-55) — the permission posture is a *function of the actual isolation tier*, not a declared intent.

**What Stirrup does today.** `validateExecutorRuntime` only checks the runtime name is in a closed set per `Type` (S:types/runconfig.go:3912-3930); it never derives an isolation tier nor couples it to permission strictness. A config with `mode=execution` + `executor.type=local` + `permissionPolicy.type=allow-all` + `run_command` **passes validation with no error** — the read-only/allow-all rejection does not fire (execution is not read-only) and Rule of Two does not trip unless all three legs hold. The dry-run probe never confirms the requested runtime is registered (S:harness/internal/executor/container.go:135-150), so a config requesting `runtime=runsc` on a host where it is unregistered proceeds *as if isolated*. Sub-agents inherit the full parent toolset minus only `spawn_agent` (S:harness/internal/core/subagent.go:82) and reuse `parent.Executor` wholesale (S:harness/internal/core/subagent.go:148); Cedar `principal.capabilities` is a **dead seam** — `factory.go` leaves it unset with an explicit "reserved for a future wave" comment (S:harness/internal/core/factory.go:1384-1387), documented as "compiles but never matches in v1" (S:docs/safety-rings.md:336-342).

**Recommendations.**

| ID | Title | Status | Effort |
|---|---|---|---|
| permission-isolation-coupling-1 | Derive a **minimum permission-policy floor** from the executor's isolation tier (Anthropic's most transferable idea) | **valid** | medium |
| permission-readonly-runtime-verify-1 | Verify the requested OCI runtime / `RuntimeClass` is actually registered before granting a permissive posture | **valid** | low |
| permission-subagent-downgrade-1 | Let `spawn_agent` **narrow** the child's capability/tool set; populate the dead Cedar `principal.capabilities` seam | **valid** | medium |
| permission-execution-sandbox-gate-1 | Opt-in "require sandbox for execution" invariant with one audited override (model on the existing `ruleOfTwo.enforce:false` pattern, S:types/runconfig.go:2110-2114) | **valid** | low |
| permission-judge-scoping-1 | Read-scope + prose-withhold the llm-judge / judging sub-agent for untrusted-repo runs | **partial** (GuardRail-inheritance backstop exists; path-scoping + prose-withholding absent) | medium |

permission-isolation-coupling-1, permission-readonly-runtime-verify-1, and permission-execution-sandbox-gate-1 overlap heavily with sandbox-isolation-1/2/3 — **implement them together** as one "isolation truth → capability floor" workstream to avoid competing knobs.

---

### 6. Secret & Credential Handling

**What Anthropic does.** Never mount `~/.aws`/`.env`/`~/.ssh` into agent reach (A:docs/security.md:43-44); the secure-deployment guide's stronger form is a proxy that **injects** credentials outside the agent boundary so the agent never holds the token. For nested CLI agents, passes `--setting-sources ""` / `--strict-mcp-config` to block host-config ingestion (A:harness/agent.py:253-254).

**What Stirrup does today (already strong).** Executors construct env from a hardcoded literal list and never read `os.Environ()` (S:harness/internal/executor/container.go:232-236; S:harness/internal/executor/k8s_netpol.go:195-214), so "no host secret in executor env" holds by construction. `local` `run_command` env is filtered to a 27-key allowlist (S:harness/internal/executor/local.go:254-299). Container `none` mode is tested for fully-empty env (S:harness/internal/executor/container_test.go:983-985).

**Recommendations.**

| ID | Title | Status | Effort |
|---|---|---|---|
| secrets-credentials-1 | Add an explicit invariant + test that allowlist-mode executor env is a **subset of `{HTTP_PROXY,HTTPS_PROXY,NO_PROXY}`** and contains no `*_API_KEY`/`*_TOKEN` (today only checks proxy keys are *present*, not exclusive) | **partial** | low |
| secrets-credentials-2 | Extend the env-allowlist model to **scoped task credentials** (registry/dependency tokens) bound to a named non-model phase, excluded from `run_command` | **valid** | high |
| secrets-credentials-4 | Tighten container egress to fail-closed (no-default-route / host firewall) so task credentials cannot leak via a proxy-ignoring client | **valid** | high (duplicates egress-control-1; most acute only once secrets-credentials-2 exists) |
| secrets-credentials-3 | Document the local-vs-container executor choice as a credential/blast-radius tradeoff in `docs/security.md` | **partial** | low |
| ~~secrets-credentials-5~~ | Disable host agent-config/MCP-config ingestion for nested CLI tools | **not_applicable** — Stirrup runs its own loop, not a nested `claude -p`; the git builtin already inherits `filteredCommandEnv` (S:harness/internal/tool/builtins/git.go:102-108); MCP is HTTP-only (S:types/runconfig.go:582). Keep as a one-line forward note only. |

---

### 7. Skills, Agent-SDK Composition & Customization/Porting

**What Anthropic does.** `/customize` port guide isolates C/C++/ASAN specifics to named files; orchestration is "generic plumbing" (A:docs/customizing.md:44-62). Tuning via version-controlled `--extra` and `--fp-rules` files.

**What Stirrup does today.** The 5 modes are compiled-in (`readOnlyModes` map + embedded `systemprompts/*.md`); no file-discoverable named preset (S:types/runconfig.go:1759-1761; S:harness/internal/prompt/modes.go:26-53). `SystemPromptOverride` is all-or-nothing — a non-empty value short-circuits to `NewOverridePromptBuilder`, which replaces `ModeFragment` with a single `StaticFragment`, **silently dropping the mode's security/grounding preamble** (S:harness/internal/prompt/override.go:8-17). No additive prompt seam; no non-overridable safety layer. The public embedding API is exactly `BuildLoopWithTransport + Run + Close` (S:harness/harnessapi/harnessapi.go:29-45) — `tool.Registry.Register` and `RegisterBuiltins` are internal-only, so an embedder cannot register a custom tool/verifier/policy without editing `internal/`. A full preflight (`--dry-run`) **already ships** (S:harness/cmd/stirrup/cmd/harness.go:803-809).

**Recommendations.**

| ID | Title | Status | Effort |
|---|---|---|---|
| skills-customization-2 | Non-overridable **safety preamble** + an **additive** prompt-tuning seam (the `--extra` analog) — closes the silent-preamble-drop hole | **valid** | medium |
| skills-customization-5 | Open a narrow custom-tool/verifier/policy **registration hook** on `harnessapi` (export a minimal interface, not the registry type, to preserve the internal-is-private invariant) | **valid** | high |
| skills-customization-1 | Discoverable **recipe registry**: named partial-`RunConfig` presets loaded from a directory, selectable via `--recipe`; migrate modes to seed recipes | **valid** | high |
| skills-customization-4 | Document the generic-vs-customizable boundary as a per-area "customization map" (`docs/customizing.md`) classifying each extension point by effort tier | **valid** | low |
| skills-customization-3 | Add a `--explain` resolved-component-**graph** view + a canary validation fixture | **partial** (the preflight spine already ships as `--dry-run`; only `--explain` and the canary are missing) | medium |

---

### 8. Checkpointing, Resume, Streaming, Artifacts & Observability

**What Anthropic does.** Each agent is one long-lived `claude -p` session; 429/5xx retried in the CLI, then by the pipeline's own backoff via `--resume <session_id>`, up to 20 times; a killed batch resumes with `run --resume <results-dir>` skipping finished runs (A:docs/pipeline.md:164-173). Results/transcripts written to disk "the moment they're produced" (A:docs/pipeline.md:97).

**What Stirrup does today.** A finished Draft 2 session spec exists (`docs/sessions-spec-draft.md`) but there is **no session package**, no `SessionConfig`, no `--continue`/`--resume` flag — the only `SessionName` is a non-injected human label and the AWS web-identity field. The JSONL trace calls `writer.Write` with no `Sync()` (S:harness/internal/trace/jsonl.go:70-79) — durable against process SIGKILL (page cache) but not host crash / serverless preemption. `GCSTraceEmitter.RecordTurnRecord` is a **documented no-op** and the summary `RunTrace` is a single PUT at `Finish` (S:harness/internal/trace/gcs.go:157-162 — verified), so a serverless run gets only a summary, and a run killed before `Finish` uploads nothing. `gcp-pubsub`/`gcs` result sinks are reserved-but-rejected (S:harness/internal/resultsink/resultsink.go:141-142 — verified). The eval runner fans out but writes per-task artifacts to `OutputDir` (S:eval/runner/runner.go:41) and never re-scans on re-entry to skip completed runs.

**Recommendations (all valid).**

| ID | Title | Status | Effort |
|---|---|---|---|
| observability-resume-1 | Implement the drafted append-only session log + `--continue` (conversation-level resume) | **valid** | high |
| observability-resume-4 | Stream the full transcript to GCS, not just the summary (reuse the hand-rolled `gcs.UploadObject` REST path) | **valid** | high |
| observability-resume-6 | Implement the reserved `gcs` result sink + atomic tmp+rename for any file-backed sink | **valid** | medium |
| observability-resume-3 | Terminal-vs-retryable outcome taxonomy + skip-completed batch resume in the eval runner | **valid** | medium |
| observability-resume-2 | `fsync` the JSONL trace at durability checkpoints (`run_started`/`run_finished`, opt-in) | **valid** | low |
| observability-resume-5 | Document + wire a "watch a remote run / stop without losing work" story for Cloud Run + K8s | **valid** | low |

These cluster: -2, -4, -5 should land together so the "SIGTERM leaves a complete remote record" guarantee statement is actually true.

---

### 9. Threat-Model Artifact, Grading, Novelty & Severity Calibration

**What Anthropic does.** THREAT_MODEL.md is a first-class, checked-in config artifact used twice — as discovery scope and triage filter (A:docs/blog-post.md:50). Two-pass dedup (cheap deterministic same-file/same-category/±10-lines, then a semantic model pass; A:docs/blog-post.md:126). Majority-vote verification via `--votes` (A:docs/triage.md). Severity rubric.

**What Stirrup does today.** The only per-run scope surface is freeform `DynamicContext` strings (no parse contract) + global per-mode prompts; the only typed `Scope` is `AzureScope` OAuth (S:types/runconfig.go:819-824). Every judging path is single-pass — no vote/quorum anywhere; `CompositeVerifier` is conjunction of *heterogeneous* criteria, not N samples of one verdict (S:harness/internal/verifier/composite.go:27-60). `VerificationResult` is `{Passed, Feedback, Details}` (S:types/runtrace.go:75-79) and `JudgeVerdict` is `{Passed, Reason, Details}` (S:eval/eval.go:23-27) — binary, with the runner deriving only a pass rate. The deterministic-vs-LLM split exists at the *rings* layer ("both must agree", S:docs/guardrails.md:39-49) and as standalone deterministic judges (tool-trace, S:types/eval.go:115-153), but a host-computed fact cannot be injected *into* an LLM judge's prompt — each `Verify` gets only `VerifyContext` with no channel for a prior verifier's result.

**Recommendations.**

| ID | Title | Status | Effort |
|---|---|---|---|
| threat-model-evaluation-1 | Optional structured, schema-validated `RunConfig.Scope` artifact (`in_scope_paths`, `out_of_scope`, `success_criteria`, `trust_boundary`), consumed by prompt builder, Cedar context, verifier | **valid** | high |
| threat-model-evaluation-2 | Voting verifier: N independent llm-judge passes with isolated context, operator-set quorum (opt-in; cost = N model calls) | **valid** | medium |
| threat-model-evaluation-3 | Extend `VerificationResult`/`JudgeVerdict` with an optional **confidence/severity** field (data prerequisite for -2 and -5; keep domain-neutral, not the vuln-severity ladder) | **valid** | low |
| threat-model-evaluation-4 | Document + enforce a **fresh-context invariant** for the LLM judge (the eval `diffreview` judge already satisfies it; the runtime `LLMJudgeVerifier` violates it) | **partial** (transcript-isolation half is cheap; separate-key half needs a 2nd adapter) | medium |
| threat-model-evaluation-5 | Support a **deterministic host-fact input** to the LLM verdict (judgment-vs-fact split) — plumb a computed fact into the judge prompt | **partial** (split exists at rings layer; the plumbing-into-prompt is the gap) | medium |

threat-model-evaluation-1, -3, and -5 chain naturally: scope-boundary checks become the *host fact* (-5) carried in the verdict (-3).

> **Caveat (per the implementer note):** `RunConfig.Scope` must remain *trusted operator config*. Any field populated from a fetched artifact must still flow through the existing `DynamicContext` untrusted-wrapping path (S:types/runconfig.go:276-280) — do not let `Scope` become a bypass.

---

## Prioritized Roadmap (valid + partial only)

Ranked by impact/effort. **Top five are framed as candidate GitHub issues.**

### Tier 1 — high impact, low/medium effort (do first)

1. **[Issue] Verify effective container/k8s runtime post-launch and at dry-run; couple permission floor to isolation truth.**
   Bundles `sandbox-isolation-1` + `sandbox-isolation-2` + `permission-readonly-runtime-verify-1` + `permission-isolation-coupling-1`. *Rationale:* today `Runtime` is plumbed but dead — a config requesting `runsc` silently runs under `runc` and can still get `allow-all`. *Effort:* low–medium. *Packages:* `harness/internal/executor/container*.go`, `harness/internal/core/preflight.go`, `types/runconfig.go`.

2. **[Issue] Unconditionally fence `web_fetch`/MCP output with a per-result nonce + ship a deterministic always-spotlight GuardRail.**
   Bundles `prompt-injection-1` + `prompt-injection-3` (+ cheap `prompt-injection-2`, `-5`). *Rationale:* on a stock build, raw external content reaches the model with zero framing because the default GuardRail is Noop and no adapter emits spotlight. *Effort:* medium. *Packages:* `harness/internal/tool/builtins/`, `harness/internal/guard/`, `harness/internal/prompt/`.

3. **[Issue] Make LLM-judge verdicts advisory-by-default; only executable oracles gate; stop feeding the judge the agent transcript.**
   `pipeline-verification-2`. *Rationale:* a gating judge that reads the agent's own persuasive reasoning is the classic rubber-stamp; generalise the existing artifact-only `eval/judge/diffreview.go` primitive. *Effort:* medium. *Package:* `harness/internal/verifier/`, `harness/internal/core/loop.go`.

4. **[Issue] Surface `egress_blocked` as a transport `HarnessEvent` + `RunResult` warning, and complete `validateNetwork` for all executors.**
   `egress-control-2` + `egress-control-4`. *Rationale:* a security-relevant signal currently dies at stderr; container network mis-config passes config-load. *Effort:* low–medium. *Packages:* `harness/internal/security/`, `harness/internal/core/factory.go`, `types/runconfig.go`.

5. **[Issue] Opt-in `--require-sandbox` (execution) invariant with one audited override + auto-include provider/MCP hosts in the allowlist.**
   `sandbox-isolation-3` + `permission-execution-sandbox-gate-1` + `egress-control-3`. *Rationale:* fail-closed for multi-tenant operators without breaking the documented local-dev posture; stop allowlist mode silently breaking the model call. *Effort:* low–medium. *Packages:* `types/runconfig.go`, `harness/internal/core/preflight.go`, `harness/internal/executor/egressproxy/`.

### Tier 2 — medium impact, medium effort

6. Fresh-workspace independent verification mode for the runtime verifier (`pipeline-verification-1`) — *high effort, highest integrity ceiling.* `harness/internal/verifier/`, `harness/internal/core/loop.go`.
7. N-of-N repeat + 124/137 classification in test-runner (`pipeline-verification-3`) — *low effort, reliability.*
8. Named tiers in `CompositeVerifier` (`pipeline-verification-5`).
9. Sub-agent capability/tool narrowing + populate Cedar `principal.capabilities` (`permission-subagent-downgrade-1`).
10. Non-overridable safety preamble + additive prompt seam (`skills-customization-2`).
11. Voting verifier + confidence/severity field (`threat-model-evaluation-2` + `-3`).
12. `fsync` JSONL at checkpoints (`observability-resume-2`) — *low effort.*
13. Implement reserved `gcs` result sink + atomic write (`observability-resume-6`).
14. Eval skip-completed batch resume + outcome taxonomy (`observability-resume-3`).

### Tier 3 — high effort or docs-only (schedule deliberately)

15. Conversation-level `--continue` session resume (`observability-resume-1`) — *high effort; spec already drafted.*
16. Stream full transcript to GCS (`observability-resume-4`) — *high effort; serverless.*
17. Structured `RunConfig.Scope` artifact (`threat-model-evaluation-1`) — *high effort, touches schema + validation + prompt + verifier.*
18. Recipe registry (`skills-customization-1`), public registration hook (`skills-customization-5`) — *high effort, API-contract surface.*
19. Container egress fail-closed via `--internal` network / host firewall (`egress-control-1` ≈ `secrets-credentials-4`) — *high effort, Linux-only.*
20. Scoped task credentials (`secrets-credentials-2`) — *high effort.*
21. Docs-only: setup-then-freeze egress recipe (`egress-control-5`), customization map (`skills-customization-4`), executor credential-tradeoff (`secrets-credentials-3`), remote-watch story (`observability-resume-5`), fresh-context judge invariant (`threat-model-evaluation-4`), env-exclusivity test (`secrets-credentials-1`), adversarial smoke probes (`sandbox-isolation-4`), `:ro` workspace + two-phase doc (`sandbox-isolation-5`), `--explain` + canary (`skills-customization-3`), host-fact-into-judge (`threat-model-evaluation-5`), adversarial re-verification (`pipeline-verification-4`).

---

## Where Stirrup Is Already Ahead

- **Secret indirection + redaction.** `secret://ENV_VAR` / `secret://file:///` resolution (S:harness/internal/security/secretstore.go:35-78), config-load rejection of literal keys (S:types/runconfig.go:2738-2757), lazy SSM init only when an `ssm://` ref is present (S:harness/internal/security/ssm.go:93-107), and `Redact()` rewriting all `apiKeyRef`/`secret://` values before any trace (S:types/runconfig.go:444-514). Anthropic's guidance is "never mount credentials"; Stirrup *structurally cannot* place a literal key in `RunConfig`.
- **Five deterministic, agent-uncircumventable safety rings** — rule-based, evaluated outside the model (S:docs/safety-rings.md:3-17). Anthropic's central lesson ("constraints in code, not prompts", A:docs/blog-post.md:60) is Stirrup's default posture.
- **Cedar policy engine** with bounded value recursion and a `ForChildRun` context-isolation clone carrying `parentRunId` (S:harness/internal/permission/policyengine.go) — a richer, declarative authorization layer than Anthropic's binary `permission_mode()`.
- **K8s NetworkPolicy egress** (`k8s_netpol.go`) backing the egress proxy at the cluster layer — stronger than a single sidecar proxy, with the proxy-url/mode cross-check enforced at config-load for k8s (S:types/runconfig.go:3972-3982).
- **FQDN allowlist matcher** with RFC-6125 wildcard semantics, HTTPS-only default, deliberately no hot-reload (S:harness/internal/executor/egressproxy/matcher.go) — more general than Anthropic's flat `host:port` string set.
- **Read-only-mode invariant** in `ValidateRunConfig` strips `write_file`/`run_command`/`edit_file`/`search_replace`/`apply_diff` and forbids `allow-all` (S:types/runconfig.go:1888-1900) — a hard structural gate, not a prompt instruction; `--mode` defaults to `planning` so a bare invocation is safe-by-default.
- **A full read-only `--dry-run` preflight already ships** (S:harness/cmd/stirrup/cmd/harness.go:803-809) — Anthropic's `setup_sandbox.sh` is a one-shot setup script; Stirrup's preflight is structured, JSON-serialisable, and spends no provider tokens.
- **Hand-rolled stdlib HTTP everywhere** (no Docker/cloud SDK tree) — the same dependency-minimalism Anthropic gets from its thin Python wrappers, but as an enforced invariant.

---

## Do NOT Copy / Non-Goals

| Item | Why it is a non-goal | Stirrup invariant / evidence |
|---|---|---|
| **The find→grade→judge→dedup→report→patch agent topology** | Domain-specific to memory-bug hunting; Stirrup is one general loop + offline eval. Import the *kernel* (independent adversarial verification) into the verifier seam, not the topology. | Agentic-loop-purity: the loop "must not import a concrete component implementation" (CLAUDE.md). |
| **ASAN / PoC-detonation oracle, heap-layout scope sections** | C/C++-specific. `threat-model-evaluation-1` is correctly scoped to *exclude* these. | — |
| **Disabling host agent-config / MCP-config ingestion** (`secrets-credentials-5`) | Verifier verdict: **not_applicable.** Stirrup runs its own loop, not a nested `claude -p`; git builtin already inherits `filteredCommandEnv`; MCP is HTTP-only. No current attack surface. | S:harness/internal/tool/builtins/git.go:102-108; S:types/runconfig.go:582. Keep only as a forward note. |
| **A "raw key" path or any secret in `RunConfig`** | Anthropic mounts credential files; Stirrup must never. Adding a raw-key path is a regression. | "Secrets never in `RunConfig`" (CLAUDE.md). |
| **Vendor SDKs (Docker/cloud) to implement GCS streaming or container inspect** | Reuse the existing hand-rolled `gcs.UploadObject` / `containerAPIClient` REST paths. | "Hand-rolled HTTP over SDKs" (CLAUDE.md). |
| **Reading env / filesystem inside the loop to implement fencing or fresh-workspace verify** | Must go behind an interface injected by the factory; the per-result nonce fence must be a pure string transform at the tool/dispatch layer. | "The agentic loop is a pure function of its interfaces" (CLAUDE.md). |
| **Weakening the read-only-mode invariant** to make a recipe or sub-agent feature easier | Any new mode inherits the tool-exclusion + non-`allow-all` invariant. | S:types/runconfig.go:1888-1900. |
| **Backwards-compat shims** for the removed/renamed concepts above | Project is pre-1.0; clean is preferred. | CLAUDE.md "Things not to do". |

---

## Public Context & Design Philosophy

The two projects share one load-bearing thesis, stated repeatedly in Anthropic's public material and already embodied in Stirrup's architecture:

- **Constraints in code and configuration, not prompts.** "One team told the model it had no network access — when it actually did — and the model discovered it could fetch from GitHub anyway" (A:docs/blog-post.md:60); "models will use whatever capabilities they actually have access to, not necessarily just what you tell them they have" (A:docs/security.md:21-24). This is the single strongest alignment: Stirrup's rings are "deterministic: rule-based, evaluated outside the model, so the agent cannot prompt them to behave differently" (S:docs/safety-rings.md:3-17).
- **Trusted deterministic orchestrator vs. untrusted non-deterministic agents — a hard split.** "The orchestration code… is trusted and never runs target code or model-chosen commands… The agents run as `claude -p` processes and can execute arbitrary commands" (A:docs/agent-sandbox.md:7-12). Maps directly to Stirrup's loop-purity + executor boundary.
- **Setup/attack split.** "Give the sandbox network access only while you're setting it up… Then, snapshot the environment and remove its network access. During scanning, allow traffic only to the model API, routed through a local proxy" (A:docs/blog-post.md:64). Stirrup's always-on proxy does not yet model this temporal lifecycle (`egress-control-5`, `sandbox-isolation-5`).
- **Executable oracle over model judgment.** "Each check is an executable oracle… No patch fails based on model judgment"; the style/LLM tier is "advisory only, never gates" (A:docs/patching.md:73-90). Empirical backing: ~60% of patches pass build-and-reproduce, <15% survive re-attack (A:docs/patching.md:94-96). Directly motivates `pipeline-verification-1/-2`.
- **Independent verification, no shared context, framed adversarially.** "Run the verifier in a fresh container without a shared filesystem or conversation history… it may simply agree instead of testing the claim" (A:docs/blog-post.md:106); findings are "guilty until proven innocent" (A:docs/pipeline.md:156-157). Motivates `pipeline-verification-1`, `threat-model-evaluation-2/-4`.
- **Sandbox-by-default with a loudly-named opt-out.** Refuses to run outside gVisor unless `--dangerously-no-sandbox` (A:README.md:48; A:docs/agent-sandbox.md:107-119). Same shape as Stirrup's safe-by-default `--mode=planning` vs. explicit `execution` — motivates `permission-execution-sandbox-gate-1`, `sandbox-isolation-3`.
- **Match isolation strength to the threat model — a spectrum.** "A plain container is fine for an agent that can only read code, while something with stronger isolation (gVisor, Kata, Firecracker) should be used for running the target" (A:docs/security.md:39-42). Argues for a future gVisor/Kata executor tier and the isolation→permission coupling (`permission-isolation-coupling-1`).
- **Two-pass dedup, majority-vote verification, threat model as first-class config** (A:docs/blog-post.md:126; A:docs/triage.md). Generalised into `threat-model-evaluation-1/-2/-3`.
- **Human ownership is non-negotiable.** "while the model can write the patch, a human still needs to own it" (A:docs/blog-post.md:173). Reinforces that Stirrup's strongest fencing remains *mitigation, not guarantee* — keep the "human reviews the diff" framing on any applyable-artifact path.

> **Honest caveat:** Anthropic does **not** use the literal phrase "unsafe by design." The verified framing is stronger-by-inference — autonomous operation *without* enforced isolation is treated as the unsafe default, hence refuse-to-start (A:harness/sandbox.py:108-119). The find→grade pipeline and ASAN oracle are domain-specific; only the *transferable kernels* above port to Stirrup.

---

## Sources

**Stirrup (S:) — cited files**
`types/runconfig.go`, `types/runtrace.go`, `types/result.go`, `types/eval.go` · `harness/internal/core/loop.go`, `factory.go`, `preflight.go`, `subagent.go` · `harness/internal/executor/container.go`, `container_api.go`, `container_test.go`, `local.go`, `k8s.go`, `k8s_netpol.go`, `k8s_unit_test.go`, `egressproxy/matcher.go`, `egressproxy/probe.go`, `egressproxy/proxy.go`, `egressproxy/events.go` · `harness/internal/verifier/llmjudge.go`, `testrunner.go`, `composite.go`, `verifier.go` · `harness/internal/guard/none.go`, `graniteguardian.go`, `cloudjudge.go`, `spotlight.go` · `harness/internal/prompt/default.go`, `composed.go`, `override.go`, `modes.go` · `harness/internal/security/secretstore.go`, `ssm.go`, `securityevent.go`, `dynamiccontext.go` · `harness/internal/permission/policyengine.go`, `denysideeffects.go` · `harness/internal/tool/builtins/webfetch.go`, `git.go`, `register.go` · `harness/internal/tool/registry.go` · `harness/internal/trace/jsonl.go`, `gcs.go` · `harness/internal/resultsink/resultsink.go` · `harness/harnessapi/harnessapi.go` · `harness/cmd/stirrup/cmd/harness.go`, `runconfigflags.go` · `eval/eval.go`, `eval/runner/runner.go`, `replay.go`, `eval/judge/judge.go`, `diffreview.go`, `eval/spec/hcl.go` · `docs/safety-rings.md`, `security.md`, `guardrails.md`, `configuration.md`, `architecture.md`, `observability-cloud.md`, `trace-inspection.md`, `sessions-spec-draft.md`, `anthropic-wif.md` · `examples/runconfig/README.md` · `CLAUDE.md`

**Anthropic (A:) — cited files**
`README.md` · `docs/blog-post.md`, `pipeline.md`, `agent-sandbox.md`, `security.md`, `patching.md`, `triage.md`, `customizing.md` · `harness/docker_ops.py`, `sandbox.py`, `cli.py`, `agent.py` · `harness/prompts/patch_prompt.py` · `scripts/setup_sandbox.sh`, `egress_proxy.py` · `bin/vp-sandboxed` · `.claude/skills/triage/SKILL.md`, `.claude/skills/patch/SKILL.md`

**Public URLs**
- https://claude.com/blog/using-llms-to-secure-source-code — "Using LLMs to secure source code" (Yan & Dattani)
- https://code.claude.com/docs/en/agent-sdk/secure-deployment — "Securely deploying AI agents" (isolation tiers, credential-proxy, defense in depth)
- https://www.anthropic.com/product/security — Claude Security ("Nothing ships without your approval")
- https://www.anthropic.com/glasswing and https://www.anthropic.com/news/expanding-project-glasswing — defensive framing, discovery→verification bottleneck-shift
- https://github.com/anthropics/defending-code-reference-harness — companion repo
- https://github.com/anthropic-experimental/sandbox-runtime — lightweight OS-level isolation primitive referenced by the secure-deployment guide
