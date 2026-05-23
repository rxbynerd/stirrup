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
