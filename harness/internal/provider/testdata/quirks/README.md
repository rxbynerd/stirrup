# Provider contract fixtures

Golden fixtures that pin the outbound request body and inbound streaming
parse for each provider's tool-calling path (issue #224). They are the first
line of defence against silent wire-shape drift: a code change that adds,
drops, or renames a request field fails the matching contract test rather than
breaking end-to-end against a live provider.

## Layout

```
testdata/quirks/<provider-type>/<model>/
    request.json     # golden outbound request body (one tool-enabled turn)
    response.sse      # captured streaming response for parser tests
    replay.json       # captured ReplayFields snapshot (parse-side rules)
```

`<provider-type>` is the `RunConfig` provider type (`anthropic`,
`openai-compatible`, `openai-responses`, `gemini`); `<model>` is the model the
fixture was captured for. Not every provider/model carries all three files —
add only what the test asserts.

## `request.json` format

The first line MAY be a `# synthetic: ...` comment; `quirkstest.LoadFixture`
strips a leading comment block before parsing, and `AssertWireEqual`
canonicalises both sides (unmarshal → re-marshal with sorted keys) before
comparing, so field order and insignificant whitespace do not matter — only
the set of keys and their values.

Mark a fixture `# synthetic` when it was derived from a builder rather than
captured from a live provider, and say why. A live capture is preferred where
one exists (the Gemini 3.x streaming precedent in #191 is why); a synthetic
fixture is acceptable for a shape the harness fully controls.

## Adding a fixture for a new provider/model

1. Build the request through the adapter's builder with representative,
   tool-enabled params — `buildAnthropicRequest`, `buildOpenAIRequest`,
   `buildResponsesRequest`, or `BuildGenerateContentRequest`. Use a synthetic
   tool with **no** `Presentation` unless the fixture is specifically pinning
   the #222 examples fold, so the fixture stays stable across capability
   changes.
2. Marshal the request and write the JSON under the path above, prefixed with a
   `# synthetic:` line describing what it pins.
3. Add a test that rebuilds the same request and calls
   `quirkstest.AssertWireEqual(t, quirkstest.JoinPath("testdata", "quirks",
   <provider>, <model>, "request.json"), body)`.

## Refreshing a fixture after an intentional wire change

When a deliberate code change alters a provider's request shape, the contract
test fails with a diff. Confirm the change is intended, then regenerate the
fixture from the new builder output (the capture step above) and review the
diff in the PR exactly as a code change. A fixture must never be regenerated
blindly to make a red test green — that defeats its purpose.

## Conventions

- **Secrets never appear in fixtures.** Use fake keys/headers only; the
  registry self-tests assert no `secret://` reference reaches a wire body.
- **Negative coverage.** "Reject before send" tests live where the harness
  actually lints a schema: OpenAI strict-mode normalisation
  (`normalizeStrictWithCache`, fail-closed) and the Gemini schema lint
  (`LintGeminiSchema`, e.g. `TestGeminiSchemaLint_FailsClosedOnUnsupportedFeature`).
  The Anthropic adapter forwards `input_schema` verbatim and has no harness-side
  schema lint, so its pre-send validation is the tool-name normalisation layer
  (`tool/toolname`), not a schema check — there is no Anthropic schema-rejection
  fixture by design.
