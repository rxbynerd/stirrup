require 'sinatra/base'
require 'faye/websocket'
require 'json'
require 'fileutils'
require 'shellwords'
require 'open3'
require 'timeout'
require 'dotenv/load'

require_relative 'stirrup'

WORKSPACE = File.expand_path(ENV.fetch('WORKSPACE', '.')).freeze

SYSTEM_PROMPT = <<~PROMPT.freeze
  You are a coding agent helping software engineers with their day-to-day tasks,
  from research to design to implementation. You have a variety of tools at your
  disposal to assist users by responding to their questions and asks with accurate
  answers, cited where possible.
PROMPT

# ---------------------------------------------------------------------------
# Shared workspace path guard — raises if path escapes the workspace root.
# ---------------------------------------------------------------------------
def workspace_path(path)
  abs = File.expand_path(path, WORKSPACE)
  # Use WORKSPACE + "/" to prevent "/workspace-foo" matching "/workspace".
  unless abs == WORKSPACE || abs.start_with?(WORKSPACE + "/")
    raise "path must be within workspace"
  end
  abs
end

# ---------------------------------------------------------------------------
# Tool definitions
# ---------------------------------------------------------------------------

read_file_tool = Tool.new(
  name:        "read_file",
  description: "Read a file from the workspace.",
  input_schema: {
    type:       "object",
    properties: { path: { type: "string", description: "Path to the file to read" } },
    required:   ["path"],
  }
) do |input|
  abs = workspace_path(input["path"])
  File.read(abs)
rescue Errno::ENOENT
  raise "file not found: #{input["path"]}"
end

write_file_tool = Tool.new(
  name:        "write_file",
  description: "Write content to a file in the workspace. Creates parent directories as needed.",
  input_schema: {
    type:       "object",
    properties: {
      path:    { type: "string", description: "Path to the file to write" },
      content: { type: "string", description: "Content to write to the file" },
    },
    required: ["path", "content"],
  }
) do |input|
  abs = workspace_path(input["path"])
  FileUtils.mkdir_p(File.dirname(abs))
  File.write(abs, input["content"])
  "wrote #{input["content"].bytesize} bytes to #{input["path"]}"
end

list_directory_tool = Tool.new(
  name:        "list_directory",
  description: "List files and directories in the workspace.",
  input_schema: {
    type:       "object",
    properties: {
      path: { type: "string", description: "Directory path relative to workspace root (defaults to workspace root)" },
    },
  }
) do |input|
  dir = workspace_path(input.fetch("path", "."))
  entries = Dir.children(dir).sort.map do |name|
    full = File.join(dir, name)
    File.directory?(full) ? "[dir]  #{name}" : "[file] #{name}"
  end
  entries.empty? ? "(empty directory)" : entries.join("\n")
rescue Errno::ENOENT
  raise "directory not found: #{input.fetch("path", ".")}"
end

search_files_tool = Tool.new(
  name:        "search_files",
  description: "Search for files by glob pattern, or search file contents for a regex pattern. Results are capped at 200.",
  input_schema: {
    type:       "object",
    properties: {
      pattern: { type: "string", description: "Regex to search in file contents, or glob pattern when glob is true" },
      path:    { type: "string", description: "Directory to search in (defaults to workspace root)" },
      glob:    { type: "boolean", description: "If true, treat pattern as a filename glob instead of a content regex" },
    },
    required: ["pattern"],
  }
) do |input|
  search_dir = input["path"] ? workspace_path(input["path"]) : WORKSPACE
  pattern    = input["pattern"]
  results    = []

  if input["glob"]
    Dir.glob(pattern, base: search_dir).first(200).each { |rel| results << rel }
  else
    regex = Regexp.new(pattern)
    Dir.glob("**/*", base: search_dir).each do |rel|
      full = File.join(search_dir, rel)
      next if File.directory?(full)
      begin
        File.foreach(full).with_index(1) do |line, lineno|
          if line.match?(regex)
            results << "#{rel}:#{lineno}: #{line.chomp}"
          end
          break if results.size >= 200
        end
      rescue Encoding::InvalidByteSequenceError, ArgumentError
        next  # skip binary/unreadable files
      end
      break if results.size >= 200
    end
  end

  results.empty? ? "no matches found" : results.join("\n")
rescue RegexpError => e
  raise "invalid regex pattern: #{e.message}"
end

# NOTE: run_shell_command provides best-effort sandboxing only — it is not a
# security boundary. The blocklist and metacharacter check reduce accident
# risk but can be bypassed by a determined caller.
SHELL_BLOCKLIST = [
  /rm\s+-[rf]/i,
  /sudo/i,
  /\|\s*sh\b/,
  /chmod\b/,
  /\bdd\b/,
  /mkfs\b/,
  /shutdown\b/,
  /reboot\b/,
].freeze

run_shell_command_tool = Tool.new(
  name:        "run_shell_command",
  description: "Run a command in the workspace directory. Best-effort sandboxing — not a security boundary. Timeout: 30s. Output capped at 10,000 chars.",
  input_schema: {
    type:       "object",
    properties: {
      command: { type: "string", description: "The command to run (no shell metacharacters)" },
    },
    required: ["command"],
  }
) do |input|
  command = input["command"]

  # Reject shell metacharacters to prevent injection via Shellwords.split.
  raise "shell metacharacters not allowed: #{command.scan(/[|;&<>`$\\]/).uniq.join}" \
    if command.match?(/[|;&<>`$\\]/)

  SHELL_BLOCKLIST.each do |pattern|
    raise "command blocked by safety filter (matches '#{pattern.source}')" if command.match?(pattern)
  end

  args = Shellwords.split(command)

  output = Timeout.timeout(30) do
    stdout_and_stderr, status = Open3.capture2e(*args, chdir: WORKSPACE)
    "exit #{status.exitstatus}\n#{stdout_and_stderr.slice(0, 10_000)}"
  end

  output
rescue Timeout::Error
  raise "command timed out after 30 seconds"
rescue Errno::ENOENT => e
  raise "command not found: #{e.message}"
end

TOOLS = [
  read_file_tool,
  write_file_tool,
  list_directory_tool,
  search_files_tool,
  run_shell_command_tool,
].freeze

# ---------------------------------------------------------------------------
# Sinatra application
# ---------------------------------------------------------------------------
class StirrupApp < Sinatra::Base
  set :server, :puma

  get '/' do
    if Faye::WebSocket.websocket?(request.env)
      handle_websocket(request.env)
    else
      [200, {}, ['ok']]
    end
  end

  private

  def handle_websocket(env)
    ws      = Faye::WebSocket.new(env)
    send_mx = Mutex.new   # guards ws.send across threads
    busy_mx = Mutex.new   # guards the busy flag
    busy    = false

    # One persistent HTTP client per WebSocket connection (not shared).
    http = HTTP.headers(
      'User-Agent'        => 'stirrup x@rubynerd.net',
      'x-api-key'         => ENV['ANTHROPIC_API_KEY'],
      'anthropic-version' => '2023-06-01',
    ).persistent("https://api.anthropic.com")

    conversation = Conversation.new(
      http:   http,
      model:  "claude-sonnet-4-6",
      system: SYSTEM_PROMPT,
      tools:  TOOLS,
    )

    ws.on :message do |event|
      msg = JSON.parse(event.data) rescue nil
      unless msg&.[]("type") == "message" && msg["content"].is_a?(String)
        send_mx.synchronize do
          ws.send({ type: "error", message: "expected {\"type\":\"message\",\"content\":\"...\"}" }.to_json)
        end
        next
      end

      # Reject concurrent turns on the same connection.
      acquired = busy_mx.synchronize do
        if busy
          false
        else
          busy = true
          true
        end
      end

      unless acquired
        send_mx.synchronize do
          ws.send({ type: "error", message: "a turn is already in progress" }.to_json)
        end
        next
      end

      Thread.new do
        begin
          conversation.say(msg["content"]) do |evt|
            send_mx.synchronize { ws.send(evt.to_json) }
          end
        rescue => e
          send_mx.synchronize { ws.send({ type: "error", message: e.message }.to_json) }
        ensure
          busy_mx.synchronize { busy = false }
        end
      end
    end

    ws.on :close do |_event|
      http.close rescue nil
    end

    ws.rack_response
  end
end

StirrupApp.run! if $PROGRAM_NAME == __FILE__
