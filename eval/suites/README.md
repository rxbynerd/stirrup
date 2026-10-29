# Eval suites

Each `.hcl` file in this directory is an `EvalSuite` that the
`stirrup-eval run` subcommand executes against a built `stirrup`
binary. Two CI surfaces execute the suites that have a matching
baseline in `../baselines/`:

- **Main-push gate** — `.github/workflows/ci.yml::eval-gate` runs
  the baselined suites on pushes to `main` (and on manual
  `workflow_dispatch`), pinned to a cheap model (GPT-6 Luna over
  OpenRouter, selected by `stirrup-eval run`'s `--provider` /
  `--base-url` / `--api-key-ref` / `--model` flags), runs every task
  three times (`--trials 3`), and compares each result to its
  baseline. `compare` reports one of four gate results (see
  [Gate semantics](../../docs/eval.md#gate-semantics)): `block` (a
  task that passed every baseline trial failed or errored every
  current trial, or the paired upper bound on the mean pass-fraction
  change is below zero) fails the gate; `warn` (a mean drop beyond
  0.10 that the data do not confirm, a listed regression, or a
  baseline task missing from the current run) and `inconclusive` (too
  few paired tasks for the interval) are raised as workflow warnings
  without failing it; `pass` is silent.

  The `main`-and-dispatch scoping is a cost control, not an
  oversight: live eval runs spend real tokens per invocation, and a
  per-push trigger multiplies that by the number of active branches
  for a signal `main` produces anyway. To get the signal on a branch
  before merge, run it on demand:

  ```bash
  gh workflow run ci.yml --ref <branch>
  ```

- **On-demand quirk suites** — suites named `provider-quirks-*` pin
  their own provider and model and cost materially more than the
  gate, so they run only when `workflow_dispatch` is given
  `run_quirks_suites=true`:

  ```bash
  gh workflow run ci.yml --ref <branch> -f run_quirks_suites=true
  ```

  CI invokes them with **no** `--model` / `--provider` / `--base-url`
  override, so their inline `run_config` decides the wire posture.
  That is the point: a quirk test whose model can be swapped from the
  command line tests nothing. They also run without `--trials`, one
  trial per task, to keep their cost down.
- **Release sweep** — `.github/workflows/release.yml::eval-extended`
  re-runs the same baselined suites against stronger models
  (Claude Sonnet 5 and Claude Opus 4.8) on every release tag. The
  sweep is non-blocking-but-visible: a blocking comparison turns the
  matrix cell red without holding the release. Every suite is
  compared before the cell fails, each cell publishes a per-suite
  table (pass rate, healthy pass rate, sample size) to the job
  summary, and `provider-quirks-*` suites are skipped because the
  sweep's `--model` would override their pin.

Suites without a baseline are never executed in CI — they are
opt-in local runs (see the per-suite notes below).

For the suite schema and the per-task contract see
[`docs/eval.md`](../../docs/eval.md).

Suites with `diff-review` judges are only as trustworthy as the judge.
Before changing a suite's judge model or provider, or the `--judge-*`
flags CI passes, measure the candidate with `stirrup-eval
judge-calibrate` against `../golden/diff-review-seed.json` (a synthetic
smoke test) or, better, a golden set drawn from the suite's own tasks,
and consider running it as a `shadow` sub-judge first, which records
its verdicts without changing any outcome. See
[Calibrating judges](../../docs/eval.md#calibrating-judges).

## Current suites

| Suite | Source | Notes |
|---|---|---|
| `acceptance.hcl` | Hand-authored | Opt-in acceptance suite: eight realistic tasks on pinned revisions of small public Go repositories, plus a verifier that fails once before it passes. Deterministic judges, live provider, no CI baseline. See [Acceptance suite](#acceptance-suite-acceptancehcl). |
| `command-output-ab-{on,off}.hcl` | Hand-authored (PR #495) | A/B pair for the command-output capture pipeline: byte-identical tasks, differing only in `tools.command_output`. Run both arms, then `stirrup-eval compare -baseline <off>/result.json -current <on>/result.json` — the feature's context-saving claim holds iff the on arm lowers mean tokens at an equal-or-better pass rate. Opt-in, live-provider, no CI baseline. |
| `post-edit-ab-{on,off}.hcl` | Hand-authored | A/B pair for `tools.postEdit`: byte-identical Go signature-change tasks without `run_command`, differing only in whether `go vet ./...` runs after each edit. Run each arm twice and compare pass rate and turns per run. Opt-in, live-provider, needs a host Go toolchain, no CI baseline. |
| `dogfood-seed.hcl` | Hand-authored (#13) | Starter suite for the v0.1 eval-gate. Targets harness behaviours stirrup's maintainers actually rely on; judges are deterministic. Replace with the mined output once the dogfood recording loop is established. |
| `guidance.hcl` | Hand-authored (#686) | Advisory repository guidance (`repositoryGuidance`): one task checks the agent runs the test command an `AGENTS.md` names, the other that it refuses an `AGENTS.md` demand claiming authority the task denies. Deterministic file judges; `eval/runner/guidance_suite_test.go` covers the judge logic with a fake harness. Opt-in, live-provider, no CI baseline. |
| `guardrail.hcl` | Hand-authored (#43) | Red-team suite for the GuardRail component. Requires a vLLM endpoint with Granite Guardian loaded. |
| `openai-responses-empty-tool-output.hcl` | Hand-authored | Regression pin for a provider edge case. Self-pinned (`openai-responses`, `secret://OPENAI_KEY`); opt-in local run. |
| `provider-quirks-openai.hcl` | Hand-authored | Live wire-protocol suite for OpenAI reasoning-class models via OpenRouter, at Terra and Sol grade. Self-pinned; requires `OPENROUTER_API_KEY`. On-demand only — see the dispatch recipe above. |
| `tooluse.hcl` | Hand-authored (#233) | Tool-use reliability regression for the Wave 1-5 tool redesign. Judges check both workspace state and tool-call trace. See below for the no-credential gate. |
| `ruleoftwo.hcl` | Hand-authored | Deterministic suite for [Ring 4's runtime sensitive-data classifier](../../docs/safety-rings.md#the-runtime-classifier) under the default enforcing `block-external` action: a secret in a tool result and a Luhn-valid PAN in the prompt each revoke egress; canonical AWS example keys must not over-block. No vLLM/guard dependency. |
| `ruleoftwo-observe.hcl` | Hand-authored | Companion to `ruleoftwo.hcl` for the `ruleOfTwo.enforce: false` observe-only escape hatch (egress survives while detection still latches). A separate file because `LoadSuiteHCL` takes one suite per file and `rule_of_two` is not a per-task override. |

## Tool-use reliability suite (`tooluse.hcl`)

`tooluse.hcl` gives the tool redesign (schema redesign, tool-choice
escalation, structured results, toolset profiles) end-to-end
regression coverage. Each task seeds a small synthetic workspace and
exercises one behaviour, judged on both the final workspace state
(`file-exists` / `file-contains`) and the tool-call path
(`tool-trace`, documented in [`docs/eval.md`](../../docs/eval.md)).

### Running without provider credentials (the default)

Every task seeds its workspace with `file` blocks and names a
recording under `fixtures/tooluse/` through a `run_config_overrides`
`provider { type = "replay" }` block, so `stirrup-eval run` spawns the
real `stirrup harness` binary and drives its tool registry, a real
`LocalExecutor` and the judges over a scripted conversation:

```sh
./stirrup-eval run --suite eval/suites/tooluse.hcl --output results/tooluse
```

No `ANTHROPIC_API_KEY`, no network, deterministic. The `verify` CI job
runs it on every push. A recording is a JSONL trace whose
`turn_record` events carry the model output of each turn in order
(format in [`docs/providers.md`](../../docs/providers.md#replay)); the
fixtures are hand-written to the shapes
`harness/internal/core/tooluse_replay_test.go` pins in process, and
that Go test remains the unit-level gate for the same behaviours.
Two of its behaviours need a server instead, and each task declares a
loopback [fixture server](../../docs/eval.md#fixture-servers) that the
runner starts for the trial:

- Streaming tool-call parsing is a wire property of each adapter that a
  replay bypasses by design, so the `streaming-split-args-*` tasks run
  the real `openai-compatible` and `openai-responses` adapters against
  a canned SSE stream (`fixtures/tooluse/*.sse`) with the tool-call
  arguments split across deltas. The Gemini adapter has no such task,
  because a RunConfig cannot point it at a loopback server; its parsing
  stays pinned in process.
- MCP name normalization needs a tool registered under a hyphenated MCP
  name, so the `mcp-name-normalization` task registers an MCP stub that
  advertises `read-file-stub` and replays a recording that calls it by
  the underscore name the strictest provider naming policy shows the
  model.

These tasks need no credential, and the live-model flags of an
invocation do not reach them.

### Running against a live provider (opt-in)

To measure a real model, pass the provider on the command line. The
harness drops each task's recording path when its provider is
switched, so the seeded workspaces, prompts and judges are unchanged:

```sh
ANTHROPIC_API_KEY=... ./stirrup-eval run \
    --suite eval/suites/tooluse.hcl \
    --provider anthropic --api-key-ref secret://ANTHROPIC_API_KEY \
    --model claude-sonnet-5 \
    --output results/tooluse-live
```

To compare models, run the suite once per model with a different
`--model` and diff the `result.json` files with `stirrup-eval
compare`. Live-provider runs are slow and spend credits; they are an
explicit opt-in and are not part of default CI. No baseline ships for
this suite, so the eval gate neither runs nor compares it until an
operator promotes one (see "Promoting a mined suite" for the baseline
workflow).

## Acceptance suite (`acceptance.hcl`)

`acceptance.hcl` measures how often an unattended run completes a
realistic task on a real repository, and whether the run recovers when
something around it fails. It is an opt-in local run: it has no
baseline, CI never executes it, and it gates nothing.

Each task clones one of four small public Go modules at a full commit
SHA (`mitchellh/go-wordwrap` and `rs/xid` under MIT, `google/shlex`
under Apache-2.0, `dustin/go-humanize` under MIT) and is judged by a
deterministic command or trace check. Every `ref` must be a 40-character
SHA; `TestSuites_PinClonedReposToAFullCommitSHA` enforces this for every
shipped suite. The runner fetches the SHA directly, so it does not have
to be a branch tip. The tasks:

| Task | Exercises |
|---|---|
| `fix-multibyte-wrap`, `fix-json-unmarshal-panic`, `fix-negative-ordinal-suffix` | Fix a failing test. The test is seeded into the workspace; the first two pin the parent of a real upstream fix. |
| `add-quote-function-with-test` | Add exported functions and tests. A hidden judge-owned test round-trips awkward strings. |
| `add-function-with-verifier-retry` | A `test-runner` verifier rejects the first submission and runs the real tests afterwards. The judge requires at least two verifier runs, so it passes only when the run recovers from a failed verification. |
| `move-shared-helper-to-own-file` | A multi-file edit that must change exactly two files. |
| `comment-only-edit-leaves-code-alone` | A narrow edit; any code change or second file fails. |
| `answer-question-from-source` | Read-only navigation of an unfamiliar repository; the trace must show no `edit_file` call. |

Because a per-task verifier override needs a suite baseline, the suite
pins a complete `run_config`: Sonnet 5, `allow-all`, 30 turns, and a
Go-only tool set without `web_fetch`, so the agent cannot read the
upstream fix. The `--provider`, `--base-url`, `--api-key-ref` and
`--model` flags of `stirrup-eval run` retarget the provider and model.

**Requirements.** A Go toolchain, `git`, network access to
`github.com`, and a provider credential. The harness binary must be
built first (`just build`).

**Cost.** One trial is eight task runs of up to 30 turns each, so
`--trials 3` is 24 task runs. Each task's token usage is in the
`trace` object of `result.json`; take the cost of a run from there
rather than from an estimate.

```bash
ANTHROPIC_API_KEY=... ./stirrup-eval run \
  --suite eval/suites/acceptance.hcl \
  --harness "$PWD/stirrup" \
  --output eval/results/acceptance \
  --trials 3
```

Baselines for this suite should be taken with repeated trials, as above,
so a flaky task records a fractional pass rate. The result reports run
health next to the judge verdict (`healthyPassRate`, `runHealth`).

### Failure-injection coverage

Only the cases the runner can express without new infrastructure are
tasks. The runner drives a real `stirrup` binary with a task prompt, so
it has no fake control plane, artifact store or gRPC stream to cut.

| Case | Status |
|---|---|
| Failed verifier | Task: `add-function-with-verifier-retry`. |
| Remote workspace export | Follow-up. Needs an export destination the eval can inspect. |
| Control-plane restart or stream loss | Follow-up. Needs a fake control plane. |
| Context compaction | Follow-up. No trace field records that a compaction happened, so a judge cannot assert it, and a task cannot force one deterministically. |
| Required MCP server missing | Follow-up, with #663. |
| Budgeted delegation (sub-agent under a small token budget) | Follow-up, with #665. |

## Authoring a self-pinned suite

Two constraints catch every author writing one for the first time.

**A suite-level `run_config` is the complete baseline RunConfig, not
an overlay on the harness defaults.** It must satisfy
`ValidateRunConfig` by itself, which means `mode` and `max_turns` are
required even though a task's own `mode` attribute reaches the harness
as a `--mode` flag — validation runs against the merged config before
any flag is applied. An editable `mode` (`execution`) additionally
requires a `permission_policy` block; the read-only modes default to
`deny-side-effects`. Omitting any of them fails every task before a
single request is sent.

**A per-task `run_config_overrides` block replaces a whole struct, it
does not merge fields.** `mergeOverrides` assigns `*overlay.ModelRouter`
over the baseline wholesale, so an override that sets only `model`
zeroes `type` and `provider`. Restate every field:

```hcl
run_config_overrides {
  model_router {
    type     = "static"
    provider = "openai-compatible"
    model    = "openai/gpt-5.6-sol"
  }
}
```

Validate both with `--dry-run` before spending any credits — it runs
the same merge and validation path without issuing a request, and
writes no artifacts:

```sh
./stirrup-eval run --suite eval/suites/<name>.hcl --dry-run
```

**Lazy tool loading is set in the inline `run_config`.** A `tools`
block takes a `lazy_loading` block that mirrors
`tools.lazyLoading`, so a suite can pair a run with and without lazy
loading without a `run_config_file`. `enabled` defaults to `false`;
`strategy`, `eager` and `max_results` take the values documented in
[Lazy tool loading](../../docs/configuration.md#lazy-tool-loading),
and `ValidateRunConfig` rejects invalid ones. The block belongs to the
suite baseline only; `run_config_overrides` has no `tools` block.

```hcl
run_config {
  # mode, max_turns, provider and the rest of the baseline.
  tools {
    lazy_loading {
      enabled     = true
      strategy    = "harness"
      eager       = ["read_file", "grep_files"]
      max_results = 8
    }
  }
}
```

## Promoting a mined suite

The v0.1 demo narrative (#277) is:

1. `stirrup harness --trace tmp/sessions/*.jsonl` captures real
   coding-agent sessions on this repo (dogfood).
2. `stirrup-eval ingest --trace tmp/sessions/*.jsonl --lakehouse
   var/lakehouse` populates `traces/` and `recordings/`.
3. `stirrup-eval mine-failures --lakehouse var/lakehouse
   --outcome failed --accept-quarantine --output
   eval/suites/mined.hcl` turns a week of failures into a
   regression suite.
4. `stirrup-eval run --suite eval/suites/mined.hcl --output
   results/ --concurrency 8` runs the mined suite at real cadence.
5. Commit `eval/suites/mined.hcl` and the produced `result.json`
   as `eval/baselines/mined.json` once the coverage is sufficient
   and the baseline reflects an intentional reference state.
   Generate the baseline with the provider, model, and
   `--trials 3` the per-push gate runs (see
   [Provider credentials](#provider-credentials) for the exact
   invocation) so the committed expectations match what CI actually
   executes. Three trials record a fractional pass rate for a flaky
   task, so the gate compares like with like instead of against one
   lucky run. This applies to the suites the per-push gate runs:
   `provider-quirks-*` suites run one trial in CI, so their baselines
   are single-run files. A single-run baseline still loads and
   compares as one trial per task. Committing a baseline auto-enrols
   the suite in both CI eval surfaces.

The seed suite (`dogfood-seed.hcl`) exists to give the eval-gate
non-empty work while the dogfood corpus matures. When the mined
suite supersedes it for coverage, the seed can be removed in the
same PR that lands the replacement.

## Provider credentials

The two CI eval surfaces authenticate differently.

The **per-push gate** uses the `OPENROUTER_API_KEY` repository
secret, passed to the harness as the `secret://OPENROUTER_API_KEY`
reference — the key itself never enters a `RunConfig` or a trace.
Reproduce the gate's exact invocation locally with:

```bash
OPENROUTER_API_KEY=... ./stirrup-eval run \
  --suite eval/suites/<name>.hcl \
  --harness "$PWD/stirrup" \
  --trials 3 \
  --provider openai-compatible \
  --base-url https://openrouter.ai/api/v1 \
  --api-key-ref secret://OPENROUTER_API_KEY \
  --model openai/gpt-6-luna
```

The **release sweep** authenticates via Anthropic Workload Identity
Federation (the four non-secret `--anthropic-*` identifiers plus the
GitHub Actions OIDC token); no static `ANTHROPIC_API_KEY` secret is
required. Suites that bundle a `diff-review` judge ALSO need a
judge-model key at judge-evaluation time, resolved by the eval process
rather than the harness. Without `--judge-*` flags or an `llm` block
the judge calls Anthropic with `secret://ANTHROPIC_API_KEY`, a
continuity default; the per-push gate should pass `--judge-provider`,
`--judge-base-url`, `--judge-api-key-ref`, and `--judge-model` so the
judge uses the gate's own provider and credential. See
[The `diff-review` judge](../../docs/eval.md#the-diff-review-judge).
`--judge-cache` and `--judge-cache-dir` record `diff-review` verdicts
and serve them again without a model call; see
[Judge cache](../../docs/eval.md#judge-cache).

Without a usable credential — a fork clone, or a Dependabot-actor
push, neither of which can read this repository's Actions secrets —
the eval jobs skip their live-run steps with a warning instead of
failing.

Both jobs run a `stirrup harness --dry-run` preflight before any task
executes, performing real credential resolution and a provider probe
without spending completion tokens. For the per-push gate a preflight
failure is fatal on every ref: the repository secret is ref-agnostic,
so a probe failure means a revoked, exhausted, or mistyped key rather
than an expected refusal. At release time a refusal fails the matrix
cell. In every case the log names the credential failure explicitly —
a wall of per-task "regressions" is never the correct reading of an
auth problem.
