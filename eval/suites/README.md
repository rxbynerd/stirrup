# Eval suites

Each `.hcl` file in this directory is an `EvalSuite` that the
`stirrup-eval run` subcommand executes against a built `stirrup`
binary. Two CI surfaces execute the suites that have a matching
baseline in `../baselines/`:

- **Per-push gate** — `.github/workflows/ci.yml::eval-gate` runs
  the baselined suites on every push, pinned to a cheap model
  (Claude Haiku 4.5 via `stirrup-eval run --model`), compares each
  result to its baseline, and fails the gate on a regression.
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
| `openai-responses-empty-tool-output.hcl` | Hand-authored | Regression pin for a provider edge case. |
| `tooluse.hcl` | Hand-authored (#233) | Tool-use reliability regression for the Wave 1-5 tool redesign. Judges check both workspace state and tool-call trace. See below for the no-credential gate. |

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
   state. Generate the baseline with the model the per-push gate
   runs (`--model claude-haiku-4-5-20251001`) so the committed
   expectations match what CI actually executes; committing a
   baseline auto-enrols the suite in both CI eval surfaces.

The seed suite (`dogfood-seed.hcl`) exists to give the eval-gate
non-empty work while the dogfood corpus matures. When the mined
suite supersedes it for coverage, the seed can be removed in the
same PR that lands the replacement.

## Provider credentials

Both CI eval surfaces authenticate via Anthropic Workload Identity
Federation (the four non-secret `--anthropic-*` identifiers plus
the GitHub Actions OIDC token); no static `ANTHROPIC_API_KEY`
secret is required in the canonical configuration. If a static key
IS configured as a repository secret, the harness prefers it over
WIF. Suites that bundle a `diff-review` judge ALSO read that key
at judge-evaluation time. Without any usable credential (e.g. a
fork without the federation rule or a static key), the eval jobs
skip their live-run steps gracefully with a warning instead of
failing.

Both jobs run a `stirrup harness --dry-run` preflight before any
task executes, performing the real credential exchange without
spending completion tokens. The Anthropic-side federation rule
constrains which OIDC refs it accepts: a refused exchange on a
non-main ref skips the per-push gate with a warning (widening the
rule in the Anthropic console enables live branch runs, no repo
change needed), a refusal on `main` fails the gate as an
infrastructure regression, and a refusal at release time fails the
matrix cell. In every case the log names the credential failure
explicitly — a wall of per-task "regressions" is never the correct
reading of an auth problem.
