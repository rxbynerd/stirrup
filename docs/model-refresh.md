# Model refresh runbook

A model refresh brings the harness up to date with a vendor's new
models: provider quirk rules, prompt tiers, and the docs that describe
them. Vendors change model surfaces weekly, so a refresh is a recurring
task rather than a one-off. This runbook is the procedure; the
[provider quirks layer](provider-quirks.md) is the reference it updates.

## Ground rules

- **Live probes beat docs.** A fact a live request confirmed outranks
  the vendor's documentation, and every claim carries its evidence:
  *probed* (a live request on a named surface, with the date),
  *documented* (vendor docs only, with the date they were read), or
  *inferred* (reasoned from a sibling model or a default). The labels
  follow the [`LastVerified` convention](provider-quirks.md#lastverified),
  which `TestRuleStaleness` enforces on new rules.
- **A gateway probe is not a vendor probe.** OpenRouter and similar
  gateways drop parameters a model does not list as supported and may
  translate others, so a 200 through a gateway says what the gateway
  accepts. Record the surface with every probe.
- **Scoped credentials only.** Read each key from the operator's secret
  store by its exact reference into one dedicated shell session, and run
  every probe there. Never print, log or commit a key, and never search
  a vault or the filesystem for one. A vendor with no credential is
  recorded as unprobed with that reason.
- **Prompt wording is proposed, never applied unreviewed.** A change to
  a system prompt tier block or to tier membership goes to review with
  the eval evidence for it.
- **Keep probes cheap.** A probe is one short request per (model,
  variant): a one-word prompt, a small `max_tokens`, one tool when the
  probe is about tools.

## 1. Sync and inventory

Start from current `main` (`git fetch` and fast-forward), then list
what each vendor serves today:

| Vendor | Models endpoint |
|---|---|
| Anthropic | `GET https://api.anthropic.com/v1/models` |
| OpenAI | `GET https://api.openai.com/v1/models` |
| Gemini (AI Studio) | `GET https://generativelanguage.googleapis.com/v1beta/models` |
| OpenRouter | `GET https://openrouter.ai/api/v1/models`; each entry's `supported_parameters` lists what OpenRouter forwards |

Compare the lists against the model globs in
`harness/internal/provider/quirks/builtin.go`, the compat profiles under
`harness/internal/provider/compat/`, and the tier tables in
`harness/internal/prompt/modeltier.go`. A new id that no glob matches
takes the zero-value behaviour of its provider type, which is safe for
an unknown model but wrong for one that rejects a parameter the harness
sends by default (`temperature`, `tool_choice`, an effort field).

## 2. Research

Research each vendor in parallel. For every new model, collect:

- sampling parameters: accepted, ignored, or a 400;
- the token field name (`max_tokens`, `max_completion_tokens`,
  `max_output_tokens`);
- reasoning or effort controls and the accepted values, including
  whether an unlisted value is rejected or silently mapped;
- tool calling: forced `tool_choice`, parallel calls, strict schemas,
  and any surface on which tools are unavailable;
- reasoning replay: whether prior reasoning must be echoed back, and
  the field it arrives in when streamed;
- stop reasons the loop has not seen before;
- retirement dates of models the repo still names.

Label each fact probed, documented or inferred, with its source.

## 3. Probe

Two scripts under `scripts/dev/model-refresh/` send the probes. Each
reads its key from the environment and hands it to `curl` through a
header file descriptor, so the key never appears in the process list,
and neither prints it. Run them from the session that holds the key.

```sh
# Anthropic Messages: non-default temperature, effort xhigh, forced tool_choice.
ANTHROPIC_API_KEY=... scripts/dev/model-refresh/probe-anthropic.sh claude-sonnet-5-5

# OpenAI-compatible Chat Completions (OpenAI, OpenRouter, Z.ai, DeepSeek):
# one streamed tool request per effort level, then one with temperature.
# Prints the serving host, the delta keys the stream used (where reasoning
# arrives) and the reported reasoning tokens.
API_KEY=... BASE_URL=https://openrouter.ai/api/v1 \
  scripts/dev/model-refresh/probe-chat.sh openai/gpt-6-luna low high max
```

Through OpenRouter, hosts differ in what they return: GLM-5.3 streamed
reasoning from the Z.AI host and none from the Relace host. To pin one,
add `"provider":{"order":["<host>"],"allow_fallbacks":false}` to the
request body. Zero reasoning tokens at every effort level means the
probe shows only that the field was accepted.

Gemini, on both surfaces: send a two-turn tool round trip to
`https://generativelanguage.googleapis.com/v1beta/models/$M:generateContent`
(AI Studio, API key) and to
`https://aiplatform.googleapis.com/v1/projects/$PROJECT/locations/global/publishers/google/models/$M:generateContent`
(Vertex, bearer token). Send the tool result once on `role:"function"`
and once on `role:"user"`, then vary
`generationConfig.thinkingConfig.thinkingLevel` across `MINIMAL`, `LOW`,
`MEDIUM` and `HIGH`. A harness run against Vertex needs a service
account or workload identity: the harness refuses user-mode ADC.

## 4. Map findings to the code

| Finding | Where it goes |
|---|---|
| Wire behaviour of a model (sampling, token field, effort list, tool choice, replay) | A rule in `quirks/builtin.go`, or in a compat profile under `provider/compat/` for compatibility-only vendors. Gateway ids need their own `*/…` rule, because `*` does not cross `/`. |
| Behaviour only on one endpoint | An endpoint-scoped rule ([endpoint profiles](providers.md#endpoint-profiles); `quirks.EndpointBedrock` for Claude on Bedrock). |
| A model that needs leaner or more scaffolded prompting | Tier membership in `prompt/modeltier.go` and the tier blocks in `prompt/systemprompts/*.md`, proposed with eval evidence (section 6). |
| A new request shape | A contract fixture under `provider/testdata/quirks/` (see its `README.md`). |
| A larger output requirement | The loop's response reserve (`defaultReserveForResponse` in `core/loop.go`, 64,000 tokens). |

Then update [provider-quirks.md](provider-quirks.md): the rule table in
section 3, the effort table in section 3.2, and the probe record in
section 3.5, including every model left unprobed and why.

## 5. Before-and-after harness run

A passing build is not verification. Build `main` and the branch, run
the same trivial task against each from an empty scratch directory, and
compare the result and the trace:

```sh
stirrup harness --provider anthropic --model "$M" --mode execution \
  --reasoning-effort high --max-turns 5 \
  --prompt 'Create a file named a.txt containing: ok'
```

## 6. Behaviour checks that need an eval run

These need the eval suite rather than single probes, so they are
budgeted separately from the probes above:

- **Prompt tier fit.** Compare a model's own tier against another with
  `--prompt-model` on `eval/suites/dogfood-seed.hcl` (see
  [eval.md](eval.md)); `stirrup prompt render` shows what each tier
  sends. The `open-weight` tier tells the model to make one tool call
  at a time and to work in a strict edit-test loop; vendors benchmark
  GLM-5.3, DeepSeek V4.1 Flash and Qwen 3.8 in agentic harnesses at
  high effort, so whether that scaffolding still helps them has not
  been measured. The `add-go-file-passing-test` task is known to be
  flaky; re-run it in isolation before attributing a failure to a
  prompt.
- **Early end of turn.** Anthropic's Opus 5.5 guidance reports that in
  unattended runs some progress updates end the turn with text alone.
  The loop treats a text-only `end_turn` as the end of the task and
  hands it to the configured verifier, if any; tool-choice escalation,
  when enabled, re-prompts only turns before the first tool call. Count runs whose final turn is a promise of further work
  before adding a continuation policy.
- **Output limit.** Anthropic recommends `max_tokens` of 64K to 128K for
  long agentic turns on the 5.5 generation, and 128K for Sonnet 5.5; the
  loop sends 64,000, the bottom of that range and half the Sonnet 5.5
  figure. A
  `max_tokens` stop is recovered up to twice per run and reported with
  the `outputLimitRecovery` flag on the turn record, so a rise in those
  flags on a model is the signal to raise the reserve for it.
