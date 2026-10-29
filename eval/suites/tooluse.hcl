# Tool-use reliability suite.
#
# This suite gives the built-in tool surface end-to-end regression
# coverage: read_file line ranges, the grep_files/find_files split, the
# edit_file operation enum, tool-choice escalation, structured results
# and the toolset-profile aliases. Each task seeds a small synthetic
# workspace, exercises ONE behaviour, and judges BOTH the final workspace
# state AND the tool-call trace the run produced (the "tool-trace" judge
# inspects RunTrace.ToolCalls).
#
# Default (no-credential) path:
#   Every task names a recording under fixtures/tooluse/ and runs it
#   through the replay provider, so the real `stirrup harness` binary
#   drives the real tool registry and a real LocalExecutor over the seeded
#   workspace with no provider credential and no network (the
#   streaming-split-args-* tasks below serve a canned stream instead):
#
#     ./stirrup-eval run --suite eval/suites/tooluse.hcl --output results/tooluse
#
#   A recording is a JSONL trace whose turn_record events carry the model
#   output of each turn in order; the fixtures here are hand-written to
#   the shapes harness/internal/core/tooluse_replay_test.go pins in
#   process. That Go test remains the unit-level gate for the same
#   behaviours; this suite is the same gate at the binary and RunConfig
#   level.
#
# Live-provider path (opt-in, NOT default CI):
#   Pass a provider on the command line. The harness drops the recording
#   path when the provider is switched, so the same tasks run against a
#   live model with their seeded workspaces and judges unchanged. The
#   judges accept any correct path to the end state, so a model that
#   needs no recovery still passes the recovery tasks:
#
#     ANTHROPIC_API_KEY=... ./stirrup-eval run \
#       --suite eval/suites/tooluse.hcl \
#       --provider anthropic --api-key-ref secret://ANTHROPIC_API_KEY \
#       --model claude-sonnet-5 \
#       --output results/tooluse-live
#
#   To compare models, run the suite once per model and diff the
#   result.json files with `stirrup-eval compare`.
#
# Streaming tool-call parsing is a wire-level property of each adapter
# that a replay bypasses by design, so the streaming-split-args-* tasks
# run the real openai-compatible and openai-responses adapters against a
# loopback fixture_server that plays back a canned SSE stream (under
# fixtures/tooluse/) whose tool-call arguments arrive split across
# deltas. They are keyless and unaffected by the --provider/--model
# live flags, which retarget only the replayed tasks. The gemini adapter
# has no base URL a RunConfig can set and needs Google credentials, so
# its parsing stays pinned in process.
#
# MCP name normalization needs a tool registered under an MCP-style name.
# The mcp-name-normalization task gets one from a loopback MCP stub (a
# fixture_server in mode "mcp") that advertises read-file-stub, and
# replays a recording that calls it by the underscore name the replay
# provider's strictest naming policy shows the model.
#
# See eval/suites/README.md for the run/compare/baseline workflow.

suite "tooluse-reliability" {
  description = "Tool-use reliability regression for the built-in tool surface; replayed from fixtures/tooluse/ by default, live on request. See the file header for the per-behaviour breakdown and the run/compare workflow."

  run_config {
    mode      = "execution"
    max_turns = 8

    # No provider here: every task supplies its own recording below, and a
    # live run supplies the provider from the command line.
    model_router {
      type  = "static"
      model = "replay"
    }

    edit_strategy {
      type = "multi"
    }

    permission_policy {
      type = "allow-all"
    }
  }

  task "read-search-edit-loop" {
    description = "Search for a function, read the file, edit it. Judge confirms the edit landed AND the trace shows grep_files -> read_file -> edit_file in order. Regression-covers the grep_files/read_file/edit_file split and the core coding loop."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      calc.go contains a function Double that currently returns n + n. Find it with a search, read the file to confirm, then edit the body so Double returns n * 2 instead. Do not rewrite the whole file — use a targeted replace.
    EOT

    file "calc.go" {
      content = "package calc\n\nfunc Double(n int) int {\n\treturn n + n\n}\n"
    }

    run_config_overrides {
      provider {
        type        = "replay"
        replay_file = "fixtures/tooluse/read-search-edit-loop.jsonl"
      }
    }

    judge {
      type    = "composite"
      require = "all"

      judge {
        type    = "file-contains"
        path    = "calc.go"
        pattern = "return n \\* 2"
      }

      judge {
        type = "tool-trace"
        tool_trace {
          sequence = ["grep_files", "read_file", "edit_file"]
          call "edit_file" {
            min_calls     = 1
            all_succeeded = true
          }
        }
      }
    }
  }

  task "read-before-edit" {
    description = "Asserts read_file precedes edit_file in the trace. Regression-covers the read-before-edit discipline the edit_file schema encourages."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      greeting.txt contains the line "hello old world". Read it, then change the word "old" to "new" with a targeted edit (not a whole-file rewrite). Leave the rest of the line unchanged.
    EOT

    file "greeting.txt" {
      content = "hello old world\n"
    }

    run_config_overrides {
      provider {
        type        = "replay"
        replay_file = "fixtures/tooluse/read-before-edit.jsonl"
      }
    }

    judge {
      type    = "composite"
      require = "all"

      judge {
        type    = "file-contains"
        path    = "greeting.txt"
        pattern = "hello new world"
      }

      judge {
        type = "tool-trace"
        tool_trace {
          sequence = ["read_file", "edit_file"]
        }
      }
    }
  }

  task "line-range-read" {
    description = "Read a specific line window with read_file start_line/limit. Judge confirms read_file was called and the written window holds exactly the requested lines. Regression-covers line-range reading and the fileExcerpt structured result."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      doc.txt contains 20 numbered lines, "line 1" through "line 20". Read only lines 5 through 7 using a line range, then write those three lines verbatim to window.txt.
    EOT

    file "doc.txt" {
      content = "line 1\nline 2\nline 3\nline 4\nline 5\nline 6\nline 7\nline 8\nline 9\nline 10\nline 11\nline 12\nline 13\nline 14\nline 15\nline 16\nline 17\nline 18\nline 19\nline 20\n"
    }

    run_config_overrides {
      provider {
        type        = "replay"
        replay_file = "fixtures/tooluse/line-range-read.jsonl"
      }
    }

    judge {
      type    = "composite"
      require = "all"

      judge {
        type    = "file-contains"
        path    = "window.txt"
        pattern = "\\Aline 5\\nline 6\\nline 7\\n?\\z"
      }

      judge {
        type = "tool-trace"
        tool_trace {
          call "read_file" {
            min_calls     = 1
            all_succeeded = true
          }
        }
      }
    }
  }

  task "bounded-search" {
    description = "A broad grep with a small max_results must not flood output. Judge confirms grep_files was called and succeeded and that the reported match count is the cap. Regression-covers bounded search and the searchResult truncation flag."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      The workspace has ten files f0.txt..f9.txt each containing the word "needle" twice. Search for "needle" but cap the results at 2 matches. Write the number of matches you were shown to count.txt.
    EOT

    file "f0.txt" {
      content = "needle here\nneedle again\n"
    }
    file "f1.txt" {
      content = "needle here\nneedle again\n"
    }
    file "f2.txt" {
      content = "needle here\nneedle again\n"
    }
    file "f3.txt" {
      content = "needle here\nneedle again\n"
    }
    file "f4.txt" {
      content = "needle here\nneedle again\n"
    }
    file "f5.txt" {
      content = "needle here\nneedle again\n"
    }
    file "f6.txt" {
      content = "needle here\nneedle again\n"
    }
    file "f7.txt" {
      content = "needle here\nneedle again\n"
    }
    file "f8.txt" {
      content = "needle here\nneedle again\n"
    }
    file "f9.txt" {
      content = "needle here\nneedle again\n"
    }

    run_config_overrides {
      provider {
        type        = "replay"
        replay_file = "fixtures/tooluse/bounded-search.jsonl"
      }
    }

    judge {
      type    = "composite"
      require = "all"

      judge {
        type    = "file-contains"
        path    = "count.txt"
        pattern = "\\A\\s*2\\s*\\z"
      }

      judge {
        type = "tool-trace"
        tool_trace {
          call "grep_files" {
            min_calls     = 1
            all_succeeded = true
          }
        }
      }
    }
  }

  task "invalid-arg-recovery" {
    description = "An edit_file 'replace' missing new_string violates the per-operation field contract. Judge confirms the file ended in the corrected state AND that no failed edit_file call was left unrecovered. Regression-covers invalid-argument recovery."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      config.txt contains the single line "mode=off". Change it to "mode=on" with edit_file. If your first attempt is rejected for a missing field, read the error and retry with the corrected arguments.
    EOT

    file "config.txt" {
      content = "mode=off\n"
    }

    run_config_overrides {
      provider {
        type        = "replay"
        replay_file = "fixtures/tooluse/invalid-arg-recovery.jsonl"
      }
    }

    judge {
      type    = "composite"
      require = "all"

      judge {
        type    = "file-contains"
        path    = "config.txt"
        pattern = "mode=on"
      }

      judge {
        type = "tool-trace"
        tool_trace {
          forbid_unknown = true
          call "edit_file" {
            min_calls = 1
          }
        }
      }
    }
  }

  task "ambiguous-edit-recovery" {
    description = "An edit_file 'replace' whose old_string matches two locations is rejected by the search-replace strategy (exactly-one-match); the model recovers with a section-qualified old_string. Judge confirms only section [b] changed AND that no failed edit_file call was left unrecovered. Regression-covers ambiguous-edit recovery."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      settings.ini has two sections, [a] and [b], each containing the line "value = 1". Change the value under [b] to 2 with a targeted edit_file replace and leave [a] unchanged. If an attempt is rejected because the text matches more than one location, retry with an old_string that includes enough context to be unique.
    EOT

    file "settings.ini" {
      content = "[a]\nvalue = 1\n[b]\nvalue = 1\n"
    }

    run_config_overrides {
      provider {
        type        = "replay"
        replay_file = "fixtures/tooluse/ambiguous-edit-recovery.jsonl"
      }
    }

    judge {
      type    = "composite"
      require = "all"

      judge {
        type    = "file-contains"
        path    = "settings.ini"
        pattern = "\\[a\\]\\nvalue = 1\\n\\[b\\]\\nvalue = 2"
      }

      judge {
        type = "tool-trace"
        tool_trace {
          forbid_unknown = true
          call "edit_file" {
            min_calls = 1
          }
        }
      }
    }
  }

  task "renamed-tool-recovery" {
    description = "Calling the retired search_files name yields the directional renamed-tool hint; the model should recover with grep_files. Judge confirms grep_files ultimately succeeded. forbid_unknown is not used here: a retired name is never followed by a successful call to itself, which is the recovery shape that check expects. Regression-covers unknown-tool recovery."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      Find every file containing the string "TODO" in the workspace, then write the count to todo-count.txt.
    EOT

    file "src.go" {
      content = "package src\n// TODO: implement\n"
    }

    run_config_overrides {
      provider {
        type        = "replay"
        replay_file = "fixtures/tooluse/renamed-tool-recovery.jsonl"
      }
    }

    judge {
      type = "tool-trace"
      tool_trace {
        call "grep_files" {
          min_calls     = 1
          all_succeeded = true
        }
      }
    }
  }

  task "no-tool-answer-escalation" {
    description = "In execution mode the model must inspect the workspace before answering; a no-tool answer triggers tool-choice escalation, enabled for this task by its override. The recording answers without a tool on its first turn, so the escalated retry is what produces the read_file call the judge requires. A live model that reads first also passes; the escalation path is then simply not exercised."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      main.go is in the workspace. Confirm which package it declares and write that package name to package.txt. Inspect the file before answering.
    EOT

    file "main.go" {
      content = "package main\n"
    }

    run_config_overrides {
      provider {
        type        = "replay"
        replay_file = "fixtures/tooluse/no-tool-answer-escalation.jsonl"
      }

      tool_choice_escalation {
        enabled = true
      }
    }

    judge {
      type    = "composite"
      require = "all"

      judge {
        type    = "file-contains"
        path    = "package.txt"
        pattern = "main"
      }

      judge {
        type = "tool-trace"
        tool_trace {
          call "read_file" {
            min_calls = 1
          }
        }
      }
    }
  }

  task "multi-tool-turn" {
    description = "A single assistant turn that emits two tool calls (a read and a find) before any tool result, exercising the fan-out/ordered-recording path. Judge asserts both tools were called. Regression-covers multi-tool turns."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      The workspace contains a.go and b.go. In one step, read a.go and list all *.go files. Then write the names of the .go files you found to gofiles.txt.
    EOT

    file "a.go" {
      content = "package a\n"
    }
    file "b.go" {
      content = "package b\n"
    }

    run_config_overrides {
      provider {
        type        = "replay"
        replay_file = "fixtures/tooluse/multi-tool-turn.jsonl"
      }
    }

    judge {
      type    = "composite"
      require = "all"

      judge {
        type  = "file-exists"
        paths = ["gofiles.txt"]
      }

      judge {
        type = "tool-trace"
        tool_trace {
          call "read_file" {
            min_calls = 1
          }
          call "find_files" {
            min_calls = 1
          }
        }
      }
    }
  }

  task "mcp-name-normalization" {
    description = "A tool registered from an MCP server under a hyphenated name is called by the model under the provider-safe name the normalizer shows it (the replay provider is held to the strictest policy, which rewrites hyphens to underscores). Judge confirms the call dispatched under the registry name and succeeded, so the reverse mapping resolved it. The task runs against a loopback MCP fixture server that advertises read-file-stub, so no credential is used. Regression-covers MCP name normalization."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      Read the stub file with the read-file-stub tool from the stub MCP server.
    EOT

    fixture_server {
      mode = "mcp"
    }

    run_config_overrides {
      provider {
        type        = "replay"
        replay_file = "fixtures/tooluse/mcp-name-normalization.jsonl"
      }

      tools {
        mcp_server "stub" {}
      }
    }

    judge {
      type = "tool-trace"
      tool_trace {
        forbid_unknown = true
        call "mcp_stub_read-file-stub" {
          min_calls     = 1
          all_succeeded = true
        }
      }
    }
  }

  task "streaming-split-args-openai-compatible" {
    description = "An openai-compatible tool call whose arguments arrive split across chat-completion deltas is reassembled by the real adapter's stream parser and dispatched. The task runs the openai-compatible adapter against a loopback fixture server that plays back fixtures/tooluse/openai-compatible-split-args.sse, so no credential is used. Judge confirms the edit landed AND the edit_file call succeeded. Regression-covers openai-compatible streamed tool-call parsing, which a replay recording bypasses."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      greeting.txt contains the line "hello old world". Change the word "old" to "new" with a targeted edit.
    EOT

    file "greeting.txt" {
      content = "hello old world\n"
    }

    fixture_server {
      mode        = "sse"
      stream_file = "fixtures/tooluse/openai-compatible-split-args.sse"
    }

    run_config_overrides {
      provider {
        type        = "openai-compatible"
        api_key_ref = ""
      }

      model_router {
        type     = "static"
        provider = "openai-compatible"
        model    = "fixture-model"
      }
    }

    judge {
      type    = "composite"
      require = "all"

      judge {
        type    = "file-contains"
        path    = "greeting.txt"
        pattern = "hello new world"
      }

      judge {
        type = "tool-trace"
        tool_trace {
          forbid_unknown = true
          call "edit_file" {
            min_calls     = 1
            all_succeeded = true
          }
        }
      }
    }
  }

  task "streaming-split-args-openai-responses" {
    description = "An openai-responses tool call whose arguments arrive split across function-call argument deltas is reassembled by the real adapter's stream parser and dispatched. The task runs the openai-responses adapter against a loopback fixture server that plays back fixtures/tooluse/openai-responses-split-args.sse, so no credential is used. Judge confirms the edit landed AND the edit_file call succeeded. Regression-covers openai-responses streamed tool-call parsing, which a replay recording bypasses."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      greeting.txt contains the line "hello old world". Change the word "old" to "new" with a targeted edit.
    EOT

    file "greeting.txt" {
      content = "hello old world\n"
    }

    fixture_server {
      mode        = "sse"
      stream_file = "fixtures/tooluse/openai-responses-split-args.sse"
    }

    run_config_overrides {
      provider {
        type        = "openai-responses"
        api_key_ref = ""
      }

      model_router {
        type     = "static"
        provider = "openai-responses"
        model    = "fixture-model"
      }
    }

    judge {
      type    = "composite"
      require = "all"

      judge {
        type    = "file-contains"
        path    = "greeting.txt"
        pattern = "hello new world"
      }

      judge {
        type = "tool-trace"
        tool_trace {
          forbid_unknown = true
          call "edit_file" {
            min_calls     = 1
            all_succeeded = true
          }
        }
      }
    }
  }
}
