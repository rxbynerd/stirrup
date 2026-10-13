# Minimal control plane

A runnable control plane for `stirrup job`. It serves the
`HarnessService.RunTask` stream for exactly one run: it waits for the harness
to connect, assigns a `RunConfig` read from a file, answers permission
requests, prints events as they arrive and exits with a status that reflects
the outcome of the run. The wire contract is documented in
[`docs/integration-guide.md`](../../docs/integration-guide.md).

The transport is **plaintext and unauthenticated**. The program is a
demonstration of the contract, not a deployable service: run it on loopback
or a private network only. Authentication, multiple concurrent jobs, durable
state and the sandbox identity token flow are out of scope.

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

A `RunConfig` delivered over the wire skips the CLI's convenience defaults, so
the file has to name every component explicitly. Every file under
[`examples/runconfig/`](../runconfig) is accepted. The `transport` key is
ignored, since a job always reports over the stream it is connected to; any
other field the wire format does not know is rejected.

Exit status:

| Status | Meaning |
|---|---|
| 0 | The `done` event reported outcome `success`. |
| 1 | The run finished with another outcome, or the stream closed without a `done` event. |
| 2 | Usage or setup error (bad flag, unreadable config, address in use). |

Events that need a reply this example does not implement (`tool_result_request`,
the `batch_*` events, `sandbox_token_request`) are reported on stderr and left
unanswered. A `RunConfig` that opts into them stalls until the harness times
out the request.

## Smoke test

`smoke.sh` runs one task end to end with no container engine, network access
or API key: the control plane, a scripted OpenAI-compatible provider
([`stubprovider`](stubprovider)) and the locally built `stirrup job` with the
`local` executor, configured by [`smoke-runconfig.json`](smoke-runconfig.json).

```sh
just controlplane-smoke          # scripted final message; exits 0
just controlplane-smoke-failure  # scripted provider error; passes only if the run exits non-zero
```

`smoke.sh success` exits 0 on a successful run. `smoke.sh provider-error`
scripts a provider failure and exits non-zero. CI runs both.
