# Eval suites

Each `.hcl` file in this directory is an `EvalSuite` that the
`stirrup-eval run` subcommand executes against a built `stirrup`
binary. Two CI surfaces execute the suites that have a matching
baseline in `../baselines/`:

- **Main-push gate** — `.github/workflows/ci.yml::eval-gate` runs
  the baselined suites on pushes to `main` (and on manual
  `workflow_dispatch`), pinned to a cheap model (GPT-5.6 Luna over
  OpenRouter, selected by `stirrup-eval run`'s `--provider` /
  `--base-url` / `--api-key-ref` / `--model` flags), compares each
  result to its baseline, and fails the gate on a regression.

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
  command line tests nothing.
- **Release sweep** — `.github/workflows/release.yml::eval-extended`
  re-runs the same baselined suites against stronger models
  (Claude Sonnet 5 and Claude Opus 4.8) on every release tag. The
  sweep is non-blocking-but-visible: a regression turns the matrix
  cell red without holding the release.

Suites without a baseline are never executed in CI — they are
opt-in local runs (see the per-suite notes below).

For the suite schema and the per-task contract see
[`docs/eval.md`](../../docs/eval.md).

## Current suites

| Suite | Source | Notes |
|---|---|---|
| `dogfood-seed.hcl` | Hand-authored (#13) | Starter suite for the v0.1 eval-gate. Targets harness behaviours stirrup's maintainers actually rely on; judges are deterministic. Replace with the mined output once the dogfood recording loop is established. |
| `guardrail.hcl` | Hand-authored (#43) | Red-team suite for the GuardRail component. Requires a vLLM endpoint with Granite Guardian loaded. |
| `openai-responses-empty-tool-output.hcl` | Hand-authored | Regression pin for a provider edge case. Self-pinned (`openai-responses`, `secret://OPENAI_KEY`); opt-in local run. |
| `provider-quirks-openai.hcl` | Hand-authored | Live wire-protocol suite for OpenAI reasoning-class models via OpenRouter, at Terra and Sol grade. Self-pinned; requires `OPENROUTER_API_KEY`. On-demand only — see the dispatch recipe above. |
| `tooluse.hcl` | Hand-authored (#233) | Tool-use reliability regression for the Wave 1-5 tool redesign. Judges check both workspace state and tool-call trace. See below for the no-credential gate. |
| `ruleoftwo.hcl` | Hand-authored | Deterministic suite for [Ring 4's runtime sensitive-data classifier](../../docs/safety-rings.md#the-runtime-classifier) under the default enforcing `block-external` action: a secret in a tool result and a Luhn-valid PAN in the prompt each revoke egress; canonical AWS example keys must not over-block. No vLLM/guard dependency. |
| `ruleoftwo-observe.hcl` | Hand-authored | Companion to `ruleoftwo.hcl` for the `ruleOfTwo.enforce: false` observe-only escape hatch (egress survives while detection still latches). A separate file because `LoadSuiteHCL` takes one suite per file and `rule_of_two` is not a per-task override. |

## Tool-use reliability suite (`tooluse.hcl`)

`tooluse.hcl` gives the tool redesign (schema redesign, MCP name
normalization, tool-choice escalation, structured results, toolset
profiles) end-to-end regression coverage. Each task is a small
synthetic repo exercising one behaviour, judged on both the final
workspace state (`file-exists` / `file-contains`) and the tool-call
path (`tool-trace`, documented in [`docs/eval.md`](../../docs/eval.md)).

### Running without provider credentials (the default gate)

The acceptance criterion is that the suite runs locally with no live
provider and no network. `stirrup-eval run` spawns the real `stirrup
harness` binary, which has no replay-provider path, so that subcommand
is the live-provider form. The no-credential gate is instead the
in-process replay regression at
`harness/internal/core/tooluse_replay_test.go`: it drives the same
behaviours through the agentic loop with a `ReplayProvider` and a real
`LocalExecutor` over synthetic workspaces, asserting the same workspace
state and tool-call traces the HCL judges check. It runs under:

```sh
go test ./harness/internal/core/ -run TestToolUse
```

No `ANTHROPIC_API_KEY`, no network, deterministic.

### Running against a live provider (opt-in)

To measure a real model, pin the provider/model with a suite-level
`run_config` block (or a `--config` baseline) supplying the credential
as a `secret://` reference, then:

```sh
ANTHROPIC_API_KEY=... ./stirrup-eval run \
    --suite eval/suites/tooluse.hcl \
    --output results/tooluse
```

To compare models, run the suite once per model — pass `--model` to
`stirrup-eval run` (it overrides both the harness default and any
suite-pinned `model_router`), or layer a per-task
`run_config_overrides` `model_router` block — and diff the
`result.json` files with `stirrup-eval compare`. Live-provider runs
are slow and spend credits; they are an explicit opt-in and are not
part of default CI. No baseline ships for this suite, so the eval
gate neither runs nor compares it until an operator promotes one
(see "Promoting a mined suite" for the baseline workflow).

## Authoring a self-pinned suite

Two constraints catch every author writing one for the first time.

**A suite-level `run_config` is the complete baseline RunConfig, not
an overlay on the harness defaults.** It must satisfy
`ValidateRunConfig` by itself, which means `mode` and `max_turns` are
required even though a task's own `mode` attribute reaches the harness
as a `--mode` flag — validation runs against the merged config before
any flag is applied. Omitting them fails every task with `mode type is
required; maxTurns must be positive` before a single request is sent.

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
the same merge and validation path without issuing a request:

```sh
./stirrup-eval run --suite eval/suites/<name>.hcl --dry-run --output /tmp/dry
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
   as `eval/baselines/mined.json` once you're satisfied with the
   coverage and the baseline reflects an intentional reference
   state. Generate the baseline with the provider and model the
   per-push gate runs (see [Provider credentials](#provider-credentials)
   for the exact invocation) so the committed expectations match what
   CI actually executes; committing a baseline auto-enrols the suite in
   both CI eval surfaces.

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
  --provider openai-compatible \
  --base-url https://openrouter.ai/api/v1 \
  --api-key-ref secret://OPENROUTER_API_KEY \
  --model openai/gpt-5.6-luna
```

The **release sweep** authenticates via Anthropic Workload Identity
Federation (the four non-secret `--anthropic-*` identifiers plus the
GitHub Actions OIDC token); no static `ANTHROPIC_API_KEY` secret is
required. Suites that bundle a `diff-review` judge ALSO read an
Anthropic key at judge-evaluation time.

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
