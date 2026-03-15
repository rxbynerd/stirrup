require 'http'
require 'json'

MAX_TURNS = 20

# ---------------------------------------------------------------------------
# Tool — pairs an API-facing tool definition with its local handler.
# ---------------------------------------------------------------------------
class Tool
  attr_reader :name, :description, :input_schema

  def initialize(name:, description:, input_schema:, &handler)
    @name         = name
    @description  = description
    @input_schema = input_schema
    @handler      = handler
  end

  # Returns a hash suitable for the Anthropic tools array.
  def to_api
    { name: @name, description: @description, input_schema: @input_schema }
  end

  # Invokes the handler and returns a tool_result content block.
  def call(tool_use_id, input)
    content = @handler.call(input)
    { type: "tool_result", tool_use_id: tool_use_id, content: content }
  rescue => e
    { type: "tool_result", tool_use_id: tool_use_id, content: e.message, is_error: true }
  end
end

# ---------------------------------------------------------------------------
# ToolCall — represents one model-requested tool invocation.
# ---------------------------------------------------------------------------
class ToolCall
  attr_reader :id, :tool_name, :input

  def initialize(id:, tool_name:, input:)
    @id        = id
    @tool_name = tool_name
    @input     = input
  end

  # Resolves and executes the tool, returning the tool_result content block.
  def execute(tools_by_name)
    tool = tools_by_name[@tool_name] or raise "unknown tool: #{@tool_name}"
    tool.call(@id, @input)
  end
end

# ---------------------------------------------------------------------------
# Conversation — owns message history and drives the agentic loop.
# ---------------------------------------------------------------------------
class Conversation
  def initialize(
    http:,
    model:,
    system:,
    tools:,
    max_tokens: 64000,
    temperature: 0.1,
    tool_choice: { type: "auto" }
  )
    @http          = http
    @tools_by_name = tools.to_h { |t| [t.name, t] }
    @request       = {
      model:       model,
      system:      system,
      max_tokens:  max_tokens,
      temperature: temperature,
      tool_choice: tool_choice,
      tools:       tools.map(&:to_api),
      messages:    [],
    }
  end

  # Appends a user message and runs the agentic loop, yielding typed event
  # hashes to the caller as they occur (text_delta, tool_call, tool_result,
  # done, error).
  def say(content, &event_callback)
    @request[:messages] << { role: "user", content: content }
    run_loop(&event_callback)
  end

  private

  def run_loop(&event_callback)
    turn = 0
    loop do
      raise "exceeded max turns (#{MAX_TURNS})" if (turn += 1) > MAX_TURNS

      stop_reason = stream_turn(&event_callback)

      case stop_reason
      when "tool_use"
        next  # tool results already appended in stream_turn; continue loop
      else
        event_callback&.call(type: "done", stop_reason: stop_reason)
        break
      end
    end
  end

  # Streams one API call, accumulates content blocks, appends assistant
  # message and (if tool_use) tool results to @request[:messages].
  # Returns the stop_reason string.
  def stream_turn(&event_callback)
    response = @http.post('/v1/messages', json: @request.merge(stream: true))

    buffer = ""
    blocks = {}   # Integer index → block hash
    state  = { stop_reason: nil }

    response.body.each do |chunk|
      buffer += chunk.force_encoding('UTF-8')

      while (boundary = buffer.index("\n\n"))
        event_str = buffer.slice!(0, boundary + 2)
        handle_sse_event(event_str, blocks, state, &event_callback)
      end
    end

    # Reconstruct assistant content array from accumulated blocks.
    content = blocks.sort_by { |k, _| k }.map do |_, block|
      if block[:type] == "text"
        { "type" => "text", "text" => block[:text_buf] }
      else
        input_json = block[:json_buf].empty? ? "{}" : block[:json_buf]
        { "type" => "tool_use", "id" => block[:id], "name" => block[:name],
          "input" => JSON.parse(input_json) }
      end
    end

    @request[:messages] << { role: "assistant", content: content }

    if state[:stop_reason] == "tool_use"
      tool_results = dispatch_tool_calls(content, &event_callback)
      @request[:messages] << { role: "user", content: tool_results }
    end

    state[:stop_reason]
  end

  # Parses a single SSE event string and updates blocks/state accordingly.
  def handle_sse_event(event_str, blocks, state, &event_callback)
    lines     = event_str.split("\n")
    data_line = lines.find { |l| l.start_with?("data:") }
    return unless data_line

    data_str = data_line.sub(/^data:\s*/, "")
    return if data_str.empty? || data_str == "[DONE]"

    data = JSON.parse(data_str)

    case data["type"]
    when "content_block_start"
      idx = data["index"]
      cb  = data["content_block"]
      blocks[idx] = case cb["type"]
                    when "text"     then { type: "text",     text_buf: "" }
                    when "tool_use" then { type: "tool_use", id: cb["id"], name: cb["name"], json_buf: "" }
                    end

    when "content_block_delta"
      block = blocks[data["index"]]
      return unless block
      delta = data["delta"]

      case delta["type"]
      when "text_delta"
        block[:text_buf] += delta["text"]
        event_callback&.call(type: "text_delta", text: delta["text"])
      when "input_json_delta"
        block[:json_buf] += delta["partial_json"]
      end

    when "content_block_stop"
      block = blocks[data["index"]]
      return unless block&.[](:type) == "tool_use"

      input = JSON.parse(block[:json_buf].empty? ? "{}" : block[:json_buf])
      event_callback&.call(type: "tool_call", id: block[:id], name: block[:name], input: input)

    when "message_delta"
      state[:stop_reason] = data.dig("delta", "stop_reason")
    end
  rescue JSON::ParserError
    # skip malformed SSE events
  end

  # Executes all tool_use blocks in content, emits tool_result events, and
  # returns an array of tool_result content blocks ready to append as a user
  # message.
  def dispatch_tool_calls(content, &event_callback)
    content.filter_map do |block|
      next unless block["type"] == "tool_use"

      result = ToolCall.new(id: block["id"], tool_name: block["name"], input: block["input"])
                       .execute(@tools_by_name)

      event_callback&.call(
        type:        "tool_result",
        tool_use_id: result[:tool_use_id],
        content:     result[:content],
      )

      result
    end
  end
end
