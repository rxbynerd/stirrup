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
`slog.Handler` and runs `security.Scrub()` on the log message and every
attribute value before delegation. Nineteen regex patterns are scrubbed,
covering:

- Anthropic API keys
- OpenAI and Stripe API keys
- GitHub PATs and app tokens
- AWS access key IDs and key/value-anchored AWS secret access keys
- Slack tokens
- GCP API keys and OAuth2 access tokens
- Key/value-anchored Azure storage keys
- Bearer tokens including JWTs (Authorization headers). A value that is
  one of a short list of prose words (`token`, `tokens`, `auth`,
  `authentication`, `authorization`, `scheme`, `header`, `credentials`,
  `value`, `string`, `prefix`, `type`, `format`, and a few function
  words), compared case-insensitively, is left alone, so "bearer token"
  and "the Bearer authentication scheme" stay readable; any other value,
  `Bearer changeme` included, is redacted
- Basic-auth credentials (`Basic <base64>`), redacted only when the
  payload decodes to bytes containing the `user:password` colon, so
  prose such as "basic documentation" is left intact. A payload cut off
  mid-token is still redacted. A payload with no colon, such as a bare
  vendor API key sent as the whole Basic value, is not recognised
- PEM and PGP armoured key blocks, private and public, from the `BEGIN`
  line through the `END` line. The body may hold only base64 lines,
  armour header lines (`Proc-Type:`, `Version:`, `Comment:`) directly
  after `BEGIN` followed by a blank line, a PGP `=XXXX` checksum, and the
  harness's own mid-result truncation marker (`[...truncated N
  bytes...]`). A line of prose ends the match at the header, so text
  placed between armour lines stays readable. Lines may be raw,
  line-numbered (`read_file`, `cat -n` and `grep -n` output, the
  `path:line:` prefix of `grep -rn` and `search_files`, with spaces in
  the path, and grep's `path-line-` context lines), `\n`-escaped
  inside a JSON string or `\\n`-escaped inside JSON held in a JSON
  string, separate JSON array elements (the shape of `read_file`'s
  structured payload) or single-quoted list elements, or string literals
  joined with `+`. A header with no `END` line
  is redacted with the whole base64 lines after it and stops before the
  first line that is not base64 to its end, so that line, its
  indentation and its first word are kept. In `\n`-escaped text a line's
  end is not visible, so there the next line's leading base64 run is
  redacted with the key. Any other truncation marker inside a key ends
  the match, and the body lines after it are not redacted. Body lines in
  other shapes are not redacted either: blockquoted (`> `), with
  `cat -A` `$` line ends, joined with `<br>` or with spaces on one line,
  or a line followed by other text, such as a `...` the model appends,
  which with the lines after it reaches the sinks. A `path:line:` prefix
  whose path has more than eight space-separated parts, starts with `-`,
  or holds a quote or shell syntax is not recognised either
- API-key header values
- Key/value-anchored 32-character hex secrets
- `secret://` references themselves, through the reference syntax the
  secret stores accept (letters, digits and `_./:@+~%=-`), except in a
  guard's copy (see [below](#scrubbed-content-is-fenced))

The key/value patterns (AWS secret access keys, Azure storage keys and
hex secrets) take a value on the same line as the key or, as YAML
allows, on the next line when it is indented. No pattern's match holds
shell syntax (`;`, `|`, `&`, `$`, a backtick, parentheses, `<` or `>`),
and none spans a line break except the PEM pattern and that one
key/value separator, so a redaction cannot hide a command placed after a
credential-shaped string from a guard;
`TestSecretPatterns_MatchesHoldNoShellSyntax` pins this.

Because the scrubber sits at the `slog.Handler` boundary, it is
defence in depth against recognised credential formats — it runs
before any handler (JSON, text, OTel log exporter) sees the
attribute. A secret with no recognisable format is not redacted.

The handler covers:

- the log message and the value of every attribute, resolved first so
  that a `slog.LogValuer` cannot defer its content past the handler;
- errors, `fmt.Stringer` values and byte slices, which are rendered to
  text before scrubbing;
- group attributes, member by member, and any other composite value
  (map, slice or struct), which is rendered as one JSON string;
- both sinks of a run's logger, the stderr JSON line and the OTLP log
  exporter, which sit below one shared scrub layer;
- the process-default logger, which the CLI installs before any
  subcommand runs, so code that logs through `slog.Default()` or the
  standard `log` package is scrubbed the same way.

It does not cover:

- a secret with no recognised format;
- security audit events, which `security.SecurityLogger` writes as JSON
  lines of their own and scrubs with `security.ScrubMap`, a function
  that handles string values, nested `map[string]any` and `[]any` and
  leaves other typed values, such as a `[]string`, as given;
- text written without `slog`, such as the `Error:` line the CLI prints
  when a command fails.

Error text recorded on OTel spans does not pass through the log
handler. The agentic loop's span helpers scrub it before they add an
exception event or set a span status, and the OTel trace emitter scrubs
the error text of the spans it writes unless redaction is disabled with
`--debug`.

Composite attribute values change shape under scrubbing. A map, slice or
struct appears as a single JSON string instead of a nested value, and a
Stringer or byte slice appears as plain text. Strings, numbers,
booleans, times and durations keep their type. The process-default
logger writes slog text (`time=… level=… msg=… key=value`) to stderr.
The loggers built for a run write JSON lines to stderr.

The `user:password` check on Basic-auth matches runs in the log
scrubber, which also covers captured OTel span content, and in the
Rule-of-Two detector. The code scanner's hardcoded-secret rules reuse
the same pattern set without that check, so they still match prose such
as "basic documentation" in a written file (issue #773).

### Command-output scrub-on-write

`run_command` capture redacts each stream on the way to its spool file
rather than at command completion. `security.ScrubWriter` applies the
same pattern set the log scrubber uses, and the spool file is also the
archive member, so a crash or SIGKILL leaves behind the same scrubbed
bytes the archive would have carried.

Redacting a stream in chunks requires holding a carry window back from
the sink so a secret split across two writes is still matched as one
span, and every cut is scanned so that no match is emitted in halves.
The window is sized above every pattern's largest realistic match. What
it cannot cover — a span longer than the window, and anything nested
inside one — along with the age-gated sweep that reclaims spool roots a
crashed run leaves behind, is described in
[`configuration.md#command-output-capture`](configuration.md#command-output-capture).

### Recorded verification results

The loop records one entry per verification attempt on the run trace
(`verificationResults`). Every recorded string (the feedback, the
error and each string in `details` at any depth) is scrubbed with
the log patterns and then capped at 4 KiB, so a secret that straddles
the cap is still redacted. A `details` value of any other type is
normalised through a JSON round trip first, so a typed value cannot
carry an unscrubbed string into the trace. The text of a verifier error
is scrubbed the same way before it reaches the log and the `warning`
event on the control stream.

The persisted `RunConfig` is the output of `RunConfig.Redact()`, which
strips secret references and provider URL detail but leaves command text
as written. A literal value in `verifier.command` is therefore stored in
the trace in clear text, and operators must not put a secret there. The same
holds for the other command fields, such as hook and post-edit
commands, which validation additionally rejects when they contain a
`secret://` reference.

### Recorded system prompt

The JSONL recording holds the run's system prompt in a `system_prompt`
line. The text is scrubbed like other recorded content, and
`systemPromptSha256` is the digest of the prompt as it was sent, before
scrubbing. The prompt includes dynamic context and, when
`repositoryGuidance` is configured, the guidance files, so repository
content that is not secret-shaped appears in the recording unchanged. A
debug build records the prompt verbatim.

### Control-stream scrubbing

The stdio and gRPC transports run `security.Scrub()` over the `text`,
`content`, `message` and `input` fields of every event before sending
it, and the gRPC transport then caps `tool_call` input, `tool_result`
content, `user_message` text, `thinking_delta` text and `message` at
256 KiB, and every name and ID field and `stop_reason` at 1 KiB;
`text_delta` is not capped. Scrubbing is per event.

Model output and reasoning arrive from the provider as many small
fragments, and a secret the model writes out can be split between two
of them. The loop therefore passes `text_delta` and `thinking_delta`
text through a carry (`security.StreamCarry`) that scrubs it as one
stream before any transport sees it, an embedder's `harnessapi.Transport`
included. The carry holds back only text a secret pattern could still
match: everything from the earliest point at which the remaining text
could begin or continue a match, such as an `s` that could start `sk-`,
a `Bearer` keyword and the whitespace after it, or an open PEM header
followed only by base64 lines, together with any complete match that
crosses that point. Which text counts is derived from the pattern set,
so a new pattern needs no change to the carry. Everything before that
point is released, scrubbed, as it arrives. For each pattern the carry
redacts, the loop raises the `secret_redacted_in_output` security event
with the location `core.stream.<event type>`; the transports' scrub
stays as defence in depth.

Held text is released as soon as it can no longer begin a match, before
the next event of the model's response (a `tool_call`, a thinking block,
the end of the turn), when the stream switches between text and
reasoning, and on every way the provider stream ends: completion, a
provider error, a stream that closes early, and cancellation. The order
of the response's events is unchanged and no text is lost, but delta
boundaries no longer match the provider's: one event can join several
provider deltas, and one delta can be split. `heartbeat` events and
`warning` events raised outside the response, such as one for a dropped
`user_response`, come from other goroutines and are not ordered against
held text. A thinking block that arrives whole is scrubbed in one pass
and sent as one event.

While text is held, the cut also stops at each pattern's matches in the
text as written, not only in the text earlier patterns rewrote, so a
match that an earlier pattern's placeholder hides cannot surface later
over text already released. Except where the cap below forces a
release, the streamed text equals whole-stream `Scrub`, which
`TestStreamCarry_InterleavedPatternsMatchWholeStreamScrub` and the
`FuzzStreamCarry` target check.

The latency this adds to ordinary prose is a few bytes that could begin
a keyword, such as a trailing `s` or `Bea`, waiting for the next
fragment. While a possible secret is being written (a token, a
key/value pair, a key block) the stream trails by that text; a held
span longer than 64 bytes is rescanned only after it grows by a
quarter, so its release can lag by that much more. There is no timer:
held text waits for the next fragment or event, so a control plane
detects a stall with `heartbeat`, not with delta silence.

The hold is capped at 8 KiB, whitespace inside a match included. When
one possible match outgrows the cap, the text before it is released
scrubbed, and the text from its start is released as one `[REDACTED]`,
whether or not the scrubber matches it yet: a JWT whose header and
payload pass 8 KiB before the second dot has no match until the dot
arrives. Nothing more of that match is sent. The carry keeps holding
its text, rescanning after each quarter of growth, until a scan shows
where the match ends, and releases only what follows it. A possible
match that ends without matching is redacted from its start all the
same, such as base64 running on past the 40 characters
`aws_secret_access_key=` takes. A match still open after 1 MiB is
withheld rather than held: the carry keeps its last 64 KiB and the
patterns that could still be matching, and sends nothing until a rune
arrives that none of those patterns can contain, which no match can
cross; it releases the stream from there. That point can come later
than the match's end (after a long `Bearer` run, spaces still count as
possible match text), never earlier. When the carry starts withholding,
the loop sends a `warning` on the control stream, raises the
`stream_text_withheld` security event and logs a warning.
`TestStreamCarry_SpanOverCapIsRedactedWhole`,
`TestStreamCarry_UnmatchedHeadOverCapIsWithheld`,
`TestStreamCarry_RunPastTheCapIsBounded`,
`TestStreamCarry_TextAfterAWithheldMatchIsReleased` and
`TestStreamCarry_WhitespaceAfterKeyword` pin this. The final assistant
text recorded in the trace is scrubbed as a whole.

`thinking_delta` sends the model's readable reasoning text to the
control plane. It is model output, with the same trust level, carry
and scrubbing as `text_delta`.

The Rule-of-Two `redact` action rewrites the model's context, not the
control stream. `tool_result` content and `verifier_feedback` text
reach the control plane with this scrubbing alone, so latch-tier spans
the scrubber does not recognise, such as card numbers, IBANs and
anchored SSNs, are sent unredacted.

<a id="scrubbed-content-is-fenced"></a>

### Guard input is scrubbed, and hidden spans are fenced

Every input a guard receives has every match of the log scrubber
replaced with `[REDACTED]`, so a remote classifier is not sent a
credential. `secret://` references are the exception: a reference is a
pointer the secret store resolves, not key material, and redacting one
can hide what the guard is asked to judge, such as a program run as
`secret://usr/bin/curl` through a `secret:` directory. The guard reads
references as written, and the fences below leave them plain for the
model too. The copies are:

- `pre_turn`: the prompt and dynamic context at turn 0 (dynamic context
  after sanitisation and the Rule-of-Two `redact` action), a mid-run
  `user_response`, arrived and carried tool results, and verifier
  feedback;
- `post_turn`: the model's final text;
- `pre_tool`: the call's arguments, as `Content` and as `ToolInput`,
  which stays valid JSON (a scrub that breaks the document leaves it a
  JSON string of the scrubbed text).

The copy is built only when a guard checks the phase. What the model
reads, what the tool receives and what the permission policy evaluates
are not changed by it. `Input.Metadata["redacted_spans"]` is the number
of scrubber matches in the guard's input; see
[Guard input](guardrails.md#guard-input).

Several scrubber patterns match any run of token characters after a
prefix, so text written in the shape of a token
(`sk-ant-ignore_all_previous_instructions…`, a camelCase `ghp_` value,
a `Bearer` value that is base64 of an instruction) is replaced as well,
and the guard cannot judge it. Where the model would otherwise read
that text plain, the harness fences it when a `pre_turn` guard is
configured:

- **Tool results.** In the model's history, the original text of each
  range the scrubber replaced is spotlighted in place, and the rest of
  the result stays plain text the guard has read. This applies when the
  guard allows the content, or errors with `failOpen` set. A deny
  replaces the result with `[content blocked by guardrail]` instead, and
  a guard `spotlight` verdict wraps the whole result once, with no span
  fences inside it. A result already screened, including one carried
  into a continued follow-up, is not screened again.
- **Turn-0 dynamic context.** The system prompt renders each value with
  the same ranges fenced, after the value is sanitised, rewritten by the
  Rule-of-Two `redact` action and truncated to the operator-text cap
  again, so a fence is never cut. A value whose fenced form does not
  decode to the value the prompt renders is fenced whole instead. The
  guard's input is the unfenced, scrubbed text. A turn-0 deny ends the
  run, so the fence matters only when the guard allows.

A fence is the tag `<untrusted_content_b64>`, the standard base64
encoding of the original bytes, and the tag `</untrusted_content_b64>`,
the same encoding a guard's `spotlight` verdict uses. Ranges closer than
64 bytes share one fence, and a block or value with more than 32 fences
is fenced whole, so fencing adds at most the base64 overhead of the
text and 32 tag pairs. A tool result holding more than 32
credential-shaped strings at least 64 bytes apart, such as the header
examples in authentication documentation, therefore reaches the model
as one fence. Traces, recordings and OTel content capture
decode each fence's payload, scrub it and re-encode it before
persisting, so a fenced secret is stored as base64 of `[REDACTED]`.

A tool result with a match in its text or its structured payload has
the structured payload dropped, so adapters send the fenced text rather
than the raw JSON. When only the payload matched, nothing is fenced and
no spotlight is recorded; a result whose text is empty then carries the
payload's scrubbed text, the copy the guard read.

What is not fenced, and why:

- With no `pre_turn` guard (no guard, the `none` adapter, or a guard
  gated to other phases), tool results and dynamic context reach the
  model unchanged.
- Verifier feedback is scrubbed in the model's history in every
  configuration, so the model reads the copy a guard judges. A guard's
  `spotlight` verdict on feedback wraps it like a tool result.
- The prompt (`config.Prompt`) and a mid-run `user_response` reach the
  model as written: they are operator input, and fencing them would mark
  the operator's own instructions as untrusted, so a `spotlight` verdict
  on them is not applied either. When a control plane templates ticket
  text into the prompt or relays end-user chat through `user_response`,
  token-shaped text inside it is neither judged by the guard nor fenced
  for the model. Third-party text belongs in dynamic context, which is
  fenced.
- `post_turn` and `pre_tool` content is not rewritten. A guard cannot
  judge what the scrubber hid, and for `pre_tool` the hidden text can be
  exactly the argument under review. Because no match holds shell
  syntax, a command written after a credential-shaped string stays
  visible to the guard.
- Repository guidance is not part of the turn-0 guard input.

Fencing ranges rather than whole blocks keeps the cost of a false match
small. Benign token-shaped text also matches the scrubber, such as a
literal `Bearer abc123`, a `secret://` reference in a config file or in
these docs, or authentication code that prints a header. Such a string
is base64-wrapped where it stands, for the model and for the
`llm-judge` verifier, which renders the same history, while the rest of
the output stays readable to both, such as the `FAIL` lines of a test
run. The costs that remain are the dropped structured payload, which
leaves the model only the result's text, and that a fenced string
cannot be copied literally, for example into an `edit_file`
`old_string`.

### Transport-error URL unwrapping

When `http.Client.Do` fails, Go wraps the transport error in a
`*url.Error` whose `Error()` string embeds the full request URL,
including any query parameters — Go redacts userinfo but not the
query string. Provider adapters that accept operator-configured
`QueryParams` (for example Azure OpenAI's `api-version` /
`sig`-bearing URLs) therefore risk leaking credentialed URLs into
logs or traces if the raw error is ever surfaced. Pattern-matching
scrubbers only catch known secret shapes, not arbitrary opaque
tokens (Azure SAS signatures, opaque gateway tokens). Provider
adapters and `provider.DoWithRetry` instead unwrap `*url.Error` to
its transport cause at the `Do` call site, so the credentialed URL
never enters the error chain — a guarantee that does not depend on
pattern matching.

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

Validation runs before the `pre_tool` guardrail, so a schema-invalid
or unknown-tool call is rejected without a classifier call, and the
guardrail classifies the stripped input the handler receives. The
full per-call order is in
[`guardrails.md` § Dispatch order](guardrails.md#dispatch-order).

## Judge and classifier prompts

The `cloud-judge` guardrail and the `llm-judge` verifier both put
untrusted text (tool input, tool output, fetched content, transcripts)
in front of a model whose answer gates the run. Both fence that text
with `security.DataFence`: markers carrying a per-call random nonce,
content with every `<<<` run broken so it cannot contain or imitate
a marker, and an explicit statement that fenced text is data, never
instructions.

Both require the verdict object to carry the fence nonce, which the
prompt states after the fenced text (`jsonextract.ObjectWithNonce`).
Content cannot know a nonce drawn after it was written, so a verdict
planted in the content is ignored wherever the model echoes it, before
or after its own answer. A balanced brace pair that is not valid JSON
is skipped whole, so an object quoted inside a malformed reasoning
string is never selected. Two differing objects that carry the nonce
are a conflict, and no matching object is a parse failure; both fail
closed (`cloud-judge` denies unless `failOpen: true`, `llm-judge`
reports a failed verification). Both also discard the verdict of a
stream that did not end at `end_turn` or `stop_sequence`, so a reply
cut by the token cap or a deadline cannot supply a partial answer. A
`cloud-judge` guardrail with `failOpen: true` turns every such failure
into an allow, so neither rule constrains it. Details per surface:
[`guardrails.md` § `cloud-judge`](guardrails.md#cloud-judge) and
[`architecture.md` § Verifiers](architecture.md#verifiers).

The marker grammar (`<<<LABEL_nonce>>>` and `<<<END_LABEL_nonce>>>`),
the notice wording, and the neutralisation rule are pinned by golden
vectors in `harness/internal/security/testdata/datafence_vectors.json`,
and the nonce extraction rule by
`harness/internal/jsonextract/testdata/nonce_extraction_vectors.json`.
The eval module's diff-review judge applies the same rules without
importing the harness, and carries byte-identical copies of both files
in `eval/judge/testdata/`. Neither copy changes without the other.

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
  `maxTokenBudget` ≤ 50 M. These are config-time bounds; of the two
  budgets only `maxTokenBudget` is enforced at runtime
  (`maxCostBudget` is validated and warned about, never enforced — see
  [`configuration.md`](configuration.md#limits-and-budgets)).
  `maxTokenBudget` consumes the provider-reported input where the
  provider reports one, so an endpoint that under-reports its input
  lowers consumption; the harness estimate is not a floor.
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

Every `net/http` provider adapter and the MCP client uses an explicit
`*http.Client` with timeouts:

| Client | Timeout |
|---|---|
| Provider streaming (Anthropic, OpenAI, OpenAI Responses, Gemini) | 30 s dial, 10 s TLS handshake, 30 s response header, 120 s idle read on the streamed body; no total cap |
| Provider streaming (Bedrock, AWS SDK client) | SDK defaults (30 s dial, 10 s TLS handshake) plus a 120 s idle read on the connection, set by the adapter because the SDK applies none to Bedrock Runtime; no total cap |
| MCP client | 30 s |
| Web fetch tool | 30 s |

`stirrup harness` and `stirrup job` run every provider call under the
run context, whose deadline is the RunConfig `timeout`.
`ValidateRunConfig` rejects a missing or zero `timeout` and caps it at
3600 s, so no stream outlives the run. For the `net/http` adapters that
deadline is the only bound on writing a request body; a non-2xx error
body is read through the same 120 s idle-read timeout, and draining a
retried response is capped at 4 KB and 10 s. Embedders of `harnessapi`
supply their own context and so own the run deadline.

`http.DefaultClient` is never used in production code. Error
response bodies are bounded with `io.LimitReader` to avoid
unbounded memory consumption when a provider returns an unexpectedly
large error payload.

## Provider-side prompt caching

The Anthropic adapter requests ephemeral prompt caching by default on
every main-loop turn (see [Prompt caching](providers.md#anthropic)).
The cached prefix is the tool list, the system prompt and the message
history, including tool results, so the provider holds that content
for the cache lifetime (five minutes by default). Anthropic isolates
caches per workspace. Retention of request content, cached or not,
falls under the operator's agreement with the provider; the harness
makes no claim about it. No opt-out exists: caching is a built-in
[quirk](provider-quirks.md), and quirks have no `RunConfig` override.

The OpenAI Responses adapter sends a `prompt_cache_key` derived from a
run ID. It does not send the raw run ID, but the SHA-256 digest is
not an anonymiser: run IDs are not secret, so anyone holding a
candidate run ID can recompute the key and match it.

## Debug builds

Diagnosing a redaction or provider-wire-format bug sometimes requires
seeing exactly what left or arrived at the process, unredacted. The
harness supports this through a compile-time-gated debug build rather
than a runtime flag, so the capability cannot be requested against a
release artifact.

Debug behaviour is gated by the `stirrupdebug` Go build tag
(`harness/internal/debugbuild.DebugBuildEnabled()`), built with:

```sh
go build -tags stirrupdebug ./harness/cmd/stirrup
```

Release artifacts — the `justfile`'s `build` recipe and
[`release.yml`](../.github/workflows/release.yml) — never pass
`-tags`, so every distributed binary has `DebugBuildEnabled()`
hard-coded to `false` at compile time. A debug build's `--version`
output carries a `+debug` suffix so it can never be mistaken for a
release binary, and every run of a debug build prints a one-line
stderr notice at startup.

Two flags exist only on a debug build:

| Flag | Effect |
|---|---|
| `--debug` | Disables `RunConfig.Redact()` and the trace emitter's `security.Scrub` content-scrub chain, so the persisted trace/recording carries unredacted secrets. |
| `--trace-wire` | Installs a wire-tap `http.RoundTripper` on provider HTTP clients (Anthropic, OpenAI-compatible, OpenAI Responses, Gemini) that dumps every raw, unredacted request and response — including streaming SSE frames as they arrive — to stderr. Bedrock is not tapped: it talks to AWS via the `aws-sdk-go-v2` signer, which exposes no raw `*http.Client` seam. |

Both flags are registered directly on the `harness` command's flag
set, independent of `addRunConfigFlags` — they never enter
`BuildRunConfig`, `applyOverrides`, or `types.RunConfig`, and
`TestRunConfig_DoesNotExposeDebugFields` guards this structurally.
They are CLI-only: no serialized `RunConfig` — including one
submitted by a control plane — can request either behaviour.

On a release binary, setting `--debug` or `--trace-wire` hard-errors
(exit code 4, the usage class) before `BuildRunConfig` does any I/O
or credential resolution:

```text
--trace-wire requires a debug build; rebuild with: go build -tags stirrupdebug ./harness/cmd/stirrup
```

The behaviour is additionally gated at its point of effect —
`buildTraceEmitter` for `--debug`, the provider-adapter wiring loop
for `--trace-wire` — against `debugbuild.DebugBuildEnabled()`, not
only against the flag value. A release binary is therefore
physically incapable of disabling redaction or dumping unredacted
wire traffic even via a hypothetical future code path that bypasses
the CLI flag entirely. Neither `RunConfig.Redact()` nor
`security.Scrub` is weakened to support this: the debug path bypasses
them at trace-emitter construction time, not by altering what they do.

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
  non-`https` remote, malformed `allowedMcpHosts`) before a run starts.
- **`allowedTools`:** an optional per-server allowlist matched against
  the server-reported tool name. When set, any advertised tool not on
  the list is refused at registration with a logged reason, so a
  server cannot smuggle in tools beyond the set it was trusted for.
  The filter runs before the per-server tool-count cap. An unset list
  registers every advertised tool (the historical, backward-compatible
  behaviour).
- **`allowedMcpHosts`:** an optional host pin. When set, the URI host
  must appear in the list (exact, case-insensitive) in addition to
  passing the SSRF guard — a defence against a server URI being
  repointed at an unexpected host.

A traced run discloses its trace IDs to every connected MCP server:
each tool call sends the run's `traceparent` and `tracestate` as HTTP
headers and in `tools/call` `_meta`, with no opt-out (see
[Trace context sent to MCP servers](observability-cloud.md#trace-context-sent-to-mcp-servers)).

### MCP result bounding

A `tools/call` response from an MCP server is bounded at three layers,
because the server is untrusted:

- **Response body: 10 MiB.** The client reads at most 10 MiB of a
  response. A larger body is cut, fails to decode, and the call returns
  an error.
- **Structured envelope: 256 KiB.** The harness keeps the server's
  `structuredContent` and a descriptor for each non-text content item
  (images, audio, resource links, embedded resources) as a bounded
  envelope. At most 512 content items are processed, and embedded
  resource text is cut at 8 KiB. Binary blobs are never inlined. An
  oversized `structuredContent` or envelope is replaced by a marker, so
  that "too large" stays distinguishable from "absent".
- **Text: the dispatch budget.** The text content items, joined, are
  bounded like the output of any other tool by `tools.maxResultBytes`
  (100 KiB by default, at most 1 MiB). Text past the budget is cut with
  a marker, and a cut result drops its structured envelope.

## Path traversal prevention

Every `Executor` implementation enforces workspace containment
appropriate to its own boundary:

- **`local`** uses `filepath.EvalSymlinks` to resolve symlinks
  before the path is checked, so symlink-based escapes are caught.
- **`container`** and **`k8s`**/**`k8s-sandbox`** are structurally
  sandboxed by the container/Pod itself; the workspace is bind-mounted
  or provisioned at a fixed path, and `ResolvePath` textually rejects
  anything that doesn't stay under it.
- **`api`** uses `url.PathEscape` on every path segment before
  constructing the GitHub Contents API URL.

The `grep_files` and `find_files` tools call `ResolvePath` on the
search root before any directory walk begins, so a workspace-relative
path that escapes the workspace is rejected before the walker sees
it. Which walker runs is chosen by declared executor capability, not
by inspecting `ResolvePath`'s return shape: a `HostPathWorkspace`
executor (`local`) walks the resolved host path directly; a `CanExec`
executor with no host counterpart (`container`, `k8s`) searches
inside the sandbox via `Exec`; a `TreeLister` executor (`api`)
enumerates through the remote tree. An executor exposing none of
these fails closed with a clear tool error rather than falling back
to the harness host's own filesystem. `grep_files` additionally uses
`shellQuote()` on every value interpolated into the `rg`/`grep`
invocation. Tested against `../../../etc/passwd` and absolute paths
across every dispatch branch. Symlink-escape protection (CWE-59) is
specific to the `HostPathWorkspace` branch's native walker, which
explicitly skips symlink entries before reading file content; a
sandboxed executor's `rg`/`grep`/`find` invocation instead delegates
symlink-following behavior to the tools running inside the sandbox,
bounded by the sandbox's own filesystem namespace rather than a
host-side guard.

The workspace exporter (`harness/internal/workspaceexport`) asks the
run's executor to stream the workspace as a tar archive and builds the
uploaded tarball by re-reading that stream. For the `container`, `k8s`
and `k8s-sandbox` executors the stream is produced by `tar` inside the
sandbox, so the exporter treats it as untrusted and never forwards it
as received. The rules, which also apply to the `local` executor's
stream, are:

- absolute names, `..` components and duplicate names are refused;
- only directories, regular files, symlinks and hard links are kept;
  other entry types, such as devices, FIFOs and sockets, are skipped;
- a symlink with an absolute target is refused, so a workspace that
  holds one, such as a virtual environment that links its interpreter
  by absolute path, fails the export;
- a symlink whose target, resolved through every symlink in the
  archive, leaves the workspace root, loops, or needs more than 40
  hops is refused;
- an entry beneath a symlink entry is refused;
- a hard link to anything other than a regular file in the archive is
  refused;
- ownership is dropped, and only the nine permission bits and the
  modification time are kept.

The link checks run once the whole stream has been read, so entry order
does not matter. Symlinks are never followed while reading the stream.
Any refusal fails the whole export, and extracting the result into an
empty directory cannot create or write a file outside it. The
uncompressed stream and the uploaded tarball are each capped at 1 GiB.

The `local` executor adds a check of its own while walking the
workspace: any path that resolves outside the workspace root, including
via a symlinked parent directory, fails the export, and so does a
dangling symlink (one whose target does not exist, so `EvalSymlinks`
itself errors). A downstream `tar -xzf` without `--no-dereference`
could otherwise be exploited via a symlink-then-overwrite pattern
targeting a path outside the workspace. The sandbox executors have no
such check: a symlink whose target stays inside the root but does not
exist is not detected, because the stream rules compare link targets
with the archive's own entries and do not require the target to exist.
Symlinks that stay inside the workspace are recorded as symlink entries,
not dereferenced.

### Prompt-time workspace reads

The workspace listing and git status in the system prompt are read
through the run's executor. The prompt builder has no filesystem or
git access of its own. Both are bounded because the agent chooses the
names they hold. Each is capped at 200 entries and 8 KiB, with a
truncation marker, and file names containing control characters are
left out of the listing.
Git status runs only when the workspace is the top level of a
repository, so the file names of an enclosing repository never appear.
Each git invocation carries the harness's git hardening flags, which
disable repository hooks and the filesystem monitor.

On the `container`, `k8s` and `k8s-sandbox` executors this means git
runs inside the sandbox, so an agent-written `.git/config` is read by
git in the sandbox and never by a git on the harness host. The `local`
executor runs commands on the harness host by definition, so on that
executor git runs on the host, in the workspace, with the hardening
flags, which are not a security boundary. The `api` executor cannot run commands and
omits the status section.

## Environment filtering

When the local executor runs a shell command, the child process's
environment is filtered to an allowlist of 29 named variables plus any
`LC_*` variable (`HOME`, `PATH`, `LANG`, `SHELL`, language-specific
build vars, etc.). Everything else is dropped.

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
- A bounded lifetime. The container's main process is `sleep` for the
  longer of the run `timeout` and the session budget, plus the `postRun`
  hook budget, plus a 10-minute margin, and the container is created
  with `AutoRemove`. The engine removes it when the sleep ends, so a
  harness that exits uncleanly cannot leave a sandbox running past that
  bound. Each container carries the labels `stirrup.sandbox=true` and
  `stirrup.run-id=<run-id>`.

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

### Egress proxy CONNECT/SNI protocol

The in-process egress proxy (`harness/internal/executor/egressproxy`)
terminates HTTPS `CONNECT` requests and cross-checks the TLS SNI
against the allowlisted host before allowing any bytes through. Its
handler writes the `200 Connection Established` response *before*
peeking the client's TLS ClientHello for SNI, not after: an RFC 7231
§4.3.6-compliant client (curl, browsers, Go `net/http`, git) sends no
tunnel bytes until it has seen a 2xx, so peeking first deadlocks
every compliant client until the read deadline fires. The host
allowlist is already enforced before the hijack (a 403 is returned
before the 200 would be written), so the 200 only promises a tunnel
to an allowlisted *host*.

The SNI cross-check still gates whether any bytes ever reach an
upstream: no upstream connection is dialed and nothing is spliced
until the ClientHello SNI is confirmed to match the allowlisted
host. A bad, absent, or mismatched SNI drops the hijacked connection
before any upstream dial — the client's in-flight TLS handshake
fails, but no egress leak occurs. Sending the 200 early costs only
that a denied client briefly sees "tunnel established"; the audit
event is still emitted.

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

- **A repeated tool-call pattern after one nudge** (3 identical calls,
  or a cycle of 2 to 4 calls repeated 3 times, with the same canonical
  input and results) — `stop_reason: "stalled"`.
- **5 consecutive tool failures** — `stop_reason: "tool_failures"`.

A call is identified by its tool name, its canonical input (key order
and whitespace do not matter), whether it succeeded, and a hash of its
output. The first repetition does not end the run. The harness appends
one message saying that the last calls repeated the same pattern with
the same results and asking the model to change approach or finish with
a final answer, emits a `warning` event, and clears the call window. The
message is merged into the tool-result message and is not marked
synthetic. Calls made in the batch that triggered the nudge do not count
toward the next repetition. A repetition after the nudge ends the run.
The consecutive-failure limit is not deferred by a nudge, so identical
failing calls, which include calls a guard or the permission policy
denied, draw a nudge at the third and end the run at the fifth.

The detector belongs to one inner loop, so a verification retry that
re-enters the loop starts with a fresh window and can draw another
nudge. A loop whose output differs on every call never repeats a
signature and is bounded only by `maxTurns`, the token budget and the
run `timeout`.

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

## Post-edit checks

`tools.postEdit` runs operator-authored shell commands after an edit
tool succeeds on a path matching the check's glob. The field reference
is in [`configuration.md`](configuration.md#post-edit-checks). A check differs from a
lifecycle hook in both trust directions: a hook runs outside the turn
loop and its output never reaches the model, while a check runs inside
an edit tool call and its failure output does.

- **Commands are operator config, not agent output.** They come from
  `RunConfig`, at the same trust level as `verifier.command` and hook
  commands. The model chooses only the edited path. Validation rejects
  the setting in read-only modes, for executors that cannot run
  commands, and for any command containing a `secret://` reference.
- **Checks run outside the permission policy.** They execute through the
  run's `Executor` from inside the edit tool's handler, after the
  input validator, tripwires, `pre_tool` guard and permission policy
  have approved the edit call. Cedar, `PermissionPolicy` and the guard
  never see a check command. The executor's isolation applies as it does
  to every other command: runtime class, network mode and egress proxy,
  and resource limits.
- **The path reaches the shell only as a variable value.** The harness
  prefixes the script with `export STIRRUP_EDITED_PATH='<path>'`, the
  value quoted for POSIX `sh`, and `{path}` in the command expands to a
  reference to that variable. The path text is never parsed as shell
  syntax, and a path starting with `-` is prefixed with `./` so a check
  cannot read it as an option. A path containing a control character is
  not passed at all: the checks are skipped and the model receives a
  warning. Validation rejects `'{path}'` in single quotes, which would
  stop the expansion.
- **Failure output is untrusted data.** When a check exits non-zero,
  its stdout and stderr are scrubbed, any fence markers in them are
  neutralised, and the text is cut to 16 KiB keeping the head. The
  harness wraps the result in a data fence with a per-call random nonce
  and a notice that the fenced text is data, then appends it to the edit
  tool's result. A check that exits zero adds nothing, and a check that
  times out or cannot run adds a warning with no output. The edit is
  never rolled back by a check.
- **The command text is recorded.** Each execution is recorded with its
  scrubbed command, the path and the outcome, so a literal secret in a
  command is stored in the trace's `RunConfig` as written, as for other
  command fields (see [Recorded verification
  results](#recorded-verification-results)).

Because the diagnostics are part of the edit tool's result, they pass
through the same screening as any other tool result: the Rule-of-Two
runtime classifier and a `pre_turn` guard read them before the model
does.

## Structured run results

A run with `outputSchema` returns a validated document, and the checks
on it run in this order:

1. The final message must be exactly one JSON document, bare or in one
   fenced block tagged `json` or untagged.
2. A document containing a `__proto__` or `constructor` key, or a key
   repeated within one object, is rejected. Unlike in tool input, where
   those keys are stripped, the model is asked to correct the document.
3. The document is validated against the schema as the model sent it.
4. Every string literal, keys and values alike, is scrubbed with the log
   patterns, and the result is checked against the size limit.
5. When scrubbing changed the document, it is validated again.

A document that satisfied the schema as sent but fails after scrubbing,
or in which scrubbing made two keys equal, ends the run as
`structured_output_failed` without a correction attempt, because the
model cannot fix a value it never sent. Rejection reasons returned to
the model or recorded as the run error are scrubbed and capped at
2 KiB. The schema validator names its schema with a fixed
`urn:stirrup:schema` resource, so no validation error returned to the
model, for a structured result or for tool input, carries a path from
the harness host.

## Sandbox identity tokens (git-proxy credential binding)

Issue #516 ships a concrete instance of the control-plane runtime
binding named above, scoped to cloning and pushing private
repositories. The control plane issues a short-lived **sandbox
identity token** — a signed JWT — to the harness over the existing
gRPC control stream, fail-closed with a 60-second wait and a 16 KiB
cap on the returned token (`sandboxidentity.MaxTokenBytes`): the
harness aborts the run before any sandbox is created if the control
plane is slow, silent, or declines to issue a token, rather than
leaving a partially-provisioned, tokenless sandbox behind. The
harness then injects the token, plus non-secret `GIT_CONFIG_*`
environment variables that rewrite `git` remote URLs, into the
sandbox environment at creation time, and delivers it again as a
`0600` file on a private memory-backed mount outside the workspace
that the composed credential helper reads. That file is what refresh
rewrites ahead of each `expires_at` the control plane reports, so a
≤15-minute token lifetime no longer has to stretch to the run's
wall-clock budget; each delivery carries the token on exec stdin,
never in argv, so neither the Docker daemon's exec records nor the
Kubernetes API server's audit log see it. See [`configuration.md`'s
"Token file and refresh"](configuration.md#token-file-and-refresh)
for the mount, the schedule, the eight-request cap, and the
fail-loud reporting of any refresh that does not land.

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

This invariant concerns stirrup's own trace, transcript, and log
surfaces. On the `k8s`/`k8s-sandbox` executors the initial token is
delivered as a plaintext Pod env var rather than a `Secret`; see
[`k8s.md`'s exposure note](executors/k8s.md#sandbox-identity-token-exposure)
for the RBAC/etcd delta this implies and the scoping operators should
apply.

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
   SecretStore + LogScrubber: secrets resolve lazily; recognised
   secret formats are redacted from logs and trace artifacts.
   StreamCarry: model text and reasoning scrubbed as one stream
   before any transport sees a delta.

  ── Per-turn ──────────────────────────────────────────────
   GuardRail (pre-turn): LLM classifier on untrusted text
   before it enters context.

  ── Per tool call ─────────────────────────────────────────
   Input validator: unknown-tool rejection, prototype-pollution
   strip, JSON Schema, tool-input tripwires. Deterministic;
   runs before any classifier call.
   GuardRail (pre-tool): LLM classifier on the cleaned call.
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
   Post-edit checks (tools.postEdit): operator commands run
   after a successful edit, outside the permission policy;
   failure output reaches the model scrubbed, capped, fenced.

  ── Run lifetime ──────────────────────────────────────────
   Stall detection: a repeated call pattern draws one nudge,
   a repeat after it ends the run; 5 consecutive failures end
   the run. Output that changes on every call is bounded only
   by maxTurns, the budgets and the run timeout.

  ── Post-session (outside the turn loop) ──────────────────
   postRun hooks: operator exec via the run's own Executor,
   after GitStrategy.Finalise, on a detached context so
   wall-clock expiry does not cut them off. Trace-only
   output. Fatal failure overrides outcome to "hook_failed"
   only when the run would otherwise report success.

  ── Persistence ──────────────────────────────────────────
   RunConfig.Redact(): strips secret refs from anything
   written to disk or a trace store. Command text is kept.
   Verification results and the system prompt are scrubbed
   before they are recorded.
   ScrubWriter: command output is redacted on the way to its
   spool file rather than at command completion.
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
