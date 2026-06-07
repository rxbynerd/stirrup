# Eval suites

Each `.hcl` file in this directory is an `EvalSuite` that the
`stirrup-eval run` subcommand executes against a built `stirrup`
binary. The CI workflow at `.github/workflows/ci.yml::eval-gate`
runs every suite here on every push to `main`, compares the result
to the matching baseline in `../baselines/`, and fails the gate
on a regression.

For the suite schema and the per-task contract see
[`docs/eval.md`](../../docs/eval.md).

## Current suites

| Suite | Source | Notes |
|---|---|---|
| `dogfood-seed.hcl` | Hand-authored (#13) | Starter suite for the v0.1 eval-gate. Targets harness behaviours stirrup's maintainers actually rely on; judges are deterministic. Replace with the mined output once the dogfood recording loop is established. |
| `guardrail.hcl` | Hand-authored (#43) | Red-team suite for the GuardRail component. Requires a vLLM endpoint with Granite Guardian loaded. |
| `openai-responses-empty-tool-output.hcl` | Hand-authored | Regression pin for a provider edge case. |
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

To compare models, run the suite once per model — swap the
`model_router` model, or layer a per-task `run_config_overrides`
`model_router` block — and diff the `result.json` files with
`stirrup-eval compare`. Live-provider runs are slow and spend credits;
they are an explicit opt-in and are not part of default CI. No baseline
ships for this suite, so the eval-gate's `compare` step skips it until
an operator promotes one (see "Promoting a mined suite" for the
baseline workflow).

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
   state.

The seed suite (`dogfood-seed.hcl`) exists to give the eval-gate
non-empty work while the dogfood corpus matures. When the mined
suite supersedes it for coverage, the seed can be removed in the
same PR that lands the replacement.

## Provider credentials

The eval-gate's "Run eval suite" step needs `ANTHROPIC_API_KEY` as
a GitHub Actions secret to drive the harness. Suites that bundle
a `diff-review` judge ALSO read that key at judge-evaluation
time. Without the secret, the eval-gate's `Run eval suite` step
no-ops gracefully (the harness invocation fails per-task, the
`compare` step still runs against the resulting result.json).
