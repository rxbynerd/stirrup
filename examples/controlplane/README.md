# Minimal control plane

A runnable control plane for `stirrup job`. It serves the
`HarnessService.RunTask` stream for exactly one session: it waits for the
harness to connect, assigns a `RunConfig` read from a file, answers permission
requests, prints events as they arrive, optionally starts follow-up runs, and
exits with a status that reflects the outcome of the runs. The wire contract is documented in
[`docs/integration-guide.md`](../../docs/integration-guide.md).

The transport is **plaintext and unauthenticated** unless the `-tls-*` flags
are set (see [TLS](#tls)). The program is a demonstration of the contract, not
a deployable service: without TLS, run it on loopback or a private network
only. Mapping a client identity to a session, multiple concurrent jobs,
durable state and the sandbox identity token flow are out of scope.

The module depends on the standard library, gRPC and the generated stubs in
[`gen/`](../../gen) only.

## Running it

```sh
go run ./examples/controlplane -config examples/runconfig/qwen3.6-lmstudio.json
```

In a second terminal, point a harness at it:

```sh
CONTROL_PLANE_ADDR=127.0.0.1:50051 ./stirrup job
```

| Flag | Default | Meaning |
|---|---|---|
| `-config` | required | `RunConfig` JSON file to assign; the format accepted by `stirrup harness --config`. |
| `-listen` | `127.0.0.1:50051` | Address to serve on. |
| `-permission` | `deny` | How to answer `permission_request` events: `allow`, `deny`, or `ask` (prompts on the terminal; anything but `y` or `yes` denies). |
| `-addr-file` | none | Write the bound address to this file once listening, for use with `-listen 127.0.0.1:0`. |
| `-followup` | none | A prompt to send as a `user_response` after a run's `done`, which starts a follow-up run. Repeat the flag for more runs; they are sent in order, one per `done`. Requires `followUpGrace` in the `RunConfig`. |
| `-tls-cert`, `-tls-key` | none | PEM server certificate and key. Set together, they serve TLS 1.3 instead of plaintext. |
| `-tls-client-ca` | none | PEM CA bundle. Requires every client to present a certificate issued by it (mutual TLS). Needs `-tls-cert` and `-tls-key`. |

A `RunConfig` delivered over the wire skips the CLI's convenience defaults, so
the file has to name every component explicitly. Every file under
[`examples/runconfig/`](../runconfig) is accepted. The `transport` key is
ignored, since a job always reports over the stream it is connected to; any
other field the wire format does not know is rejected.

Exit status:

| Status | Meaning |
|---|---|
| 0 | Every run's `done` event reported outcome `success`. |
| 1 | A run finished with another outcome, or the stream closed before the last run's `done` event. |
| 2 | Usage or setup error (bad flag, unreadable config, address in use). |

The model's answer text goes to stdout. Everything else goes to stderr as a
live transcript: the user-role messages the harness appends, turn boundaries
with token usage (including turns reported as abandoned, with the reason),
reasoning text, each tool call and result with its outcome and duration, and,
under lazy tool loading, each search that loads tools as
`[tools_loaded] <source> <search tool> <tool IDs>`.

Model and tool output is untrusted, so control characters, ANSI escape
sequences and bidirectional formatting characters are printed as escapes
such as `\x1b` or `\u202e` rather than written to the terminal raw. Streamed
answer and reasoning text keeps its newlines and tabs; single-line fields
escape those too.
Tool input in a `tool_call` line or a permission prompt is shown as the
tool decodes it: escaped characters appear decoded, and a key repeated in
the JSON shows only the value the tool will use.

## Follow-up runs

With `followUpGrace` set, the harness keeps the stream open after a run's
`done`, and a `user_response` starts another run on it. `-followup` drives
that from the command line:

```sh
go run ./examples/controlplane -config run.json -followup "Now add tests." -followup "Summarise the change."
```

Set `followUpHistory` to `"continue"` in the same file to have each follow-up
continue the previous run's conversation rather than start from its prompt
alone; see
[`docs/integration-guide.md#follow-up-turns`](../../docs/integration-guide.md#follow-up-turns).
After the last follow-up's `done`, the control plane closes the stream and
exits.

Events that need a reply this example does not implement (`tool_result_request`,
the `batch_*` events, `sandbox_token_request`) are reported on stderr and left
unanswered. A `RunConfig` that opts into them stalls until the harness times
out the request.

## TLS

With `-tls-cert` and `-tls-key` the server presents that certificate and
accepts TLS 1.3 only; a plaintext client fails before any event is exchanged.
Adding `-tls-client-ca` makes it mutual TLS: a client without a certificate
issued by that CA is refused during the handshake. The harness side is
configured with the `CONTROL_PLANE_TLS_*` environment variables:

```sh
go run ./examples/controlplane -config run.json -listen 127.0.0.1:50051 \
    -tls-cert server.pem -tls-key server-key.pem -tls-client-ca ca.pem

CONTROL_PLANE_ADDR=127.0.0.1:50051 \
CONTROL_PLANE_TLS_CA_FILE=ca.pem \
CONTROL_PLANE_TLS_CERT_FILE=client.pem \
CONTROL_PLANE_TLS_KEY_FILE=client-key.pem \
    ./stirrup job
```

The server certificate must name the address the harness dials (here an IP
SAN for `127.0.0.1`). The example authenticates the client certificate but
does not bind it to a session. See
[`docs/transport-security.md`](../../docs/transport-security.md) for
certificate generation and the Kubernetes recipes.

## Smoke test

`smoke.sh` runs tasks end to end with no container engine, network access
or API key: the control plane, a scripted OpenAI-compatible provider
([`stubprovider`](stubprovider)) and the locally built `stirrup job` with the
`local` executor, configured by [`smoke-runconfig.json`](smoke-runconfig.json).

```sh
just controlplane-smoke          # success, followup and abandoned-turn; each exits 0
just controlplane-smoke-failure  # scripted provider error; passes only if the run exits non-zero
```

| Scenario | What it checks |
|---|---|
| `success` | One run with a scripted final message ends successfully. |
| `followup` | A follow-up run started by `-followup` on the same stream, with `followUpHistory: "continue"`: two `run_started` events and a `followup_prompt` user message. |
| `abandoned-turn` | The provider returns an empty message, and the harness reports `turn_abandoned` with reason `empty_response`. |
| `provider-error` | A scripted provider failure exits non-zero. |

CI runs every scenario.
