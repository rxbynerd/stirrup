# stirrup

A Ruby coding agent server. Exposes a WebSocket endpoint that accepts user messages and streams back responses from Claude (claude-sonnet-4-6) via the Anthropic API, with tool use support.

## Architecture

- `stirrup.rb` — Core library. Defines `Tool`, `ToolCall`, and `Conversation`. `Conversation` drives the agentic loop: it streams one API turn at a time, dispatches tool calls, appends results, and continues until a non-`tool_use` stop reason.
- `server.rb` — Sinatra app (`StirrupApp`). Defines the five workspace tools, the system prompt, and the WebSocket handler. One `Conversation` and one persistent HTTP client are created per connection.

## Running

```
bundle exec ruby server.rb
```

Requires a `.env` file with:

```
ANTHROPIC_API_KEY=sk-ant-...
WORKSPACE=/path/to/workspace   # optional, defaults to cwd
```

## WebSocket Protocol

Connect to `ws://localhost:4567/`.

**Client → server:**
```json
{ "type": "message", "content": "your prompt here" }
```

**Server → client (streamed events):**

| `type` | Fields | Description |
|---|---|---|
| `text_delta` | `text` | Incremental assistant text |
| `tool_call` | `id`, `name`, `input` | Tool invocation by the model |
| `tool_result` | `tool_use_id`, `content` | Result returned to the model |
| `done` | `stop_reason` | Turn complete |
| `error` | `message` | Error occurred |

Concurrent turns on the same connection are rejected with an `error` event.

## Tools Available to the Agent

All file/directory tools are sandboxed to `WORKSPACE` via `workspace_path()` in `server.rb`.

| Tool | Description |
|---|---|
| `read_file` | Read a file from the workspace |
| `write_file` | Write content to a file (creates parent dirs) |
| `list_directory` | List a directory's contents |
| `search_files` | Grep file contents (regex) or find files (glob) |
| `run_shell_command` | Run a command in the workspace; 30s timeout, output capped at 10,000 chars |

`run_shell_command` applies a blocklist (`rm -rf`, `sudo`, etc.) and rejects shell metacharacters (`|`, `;`, `&`, `<`, `>`, `` ` ``, `$`, `\`). This is best-effort only, not a security boundary.

## Key Constants

- `MAX_TURNS = 20` — maximum agentic loop iterations per `Conversation#say` call (in `stirrup.rb`)
- Model: `claude-sonnet-4-6`
- `max_tokens: 64000`, `temperature: 0.1`

## Dependencies

Ruby 4.0.1 (see `.ruby-version`). Install gems with `bundle install`.

| Gem | Purpose |
|---|---|
| `http` | HTTP client for Anthropic API (streaming SSE) |
| `sinatra` | Web framework |
| `puma` | Web server |
| `faye-websocket` | WebSocket support for Rack/Puma |
| `dotenv` | Load `.env` into `ENV` |
| `pry` | Debug REPL |

## Development Notes

- The `Conversation` class in `stirrup.rb` is framework-agnostic — it only needs an `http` client and a list of `Tool` objects. It can be used independently of the Sinatra server.
- SSE parsing happens manually in `stream_turn` / `handle_sse_event`. Malformed SSE events are silently skipped.
- `.env` is gitignored. Never commit API keys.
