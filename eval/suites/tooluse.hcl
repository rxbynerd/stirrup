# Tool-use reliability suite (issue #233).
#
# This suite gives the tool redesign delivered across Waves 1-5 end-to-end
# regression coverage: the #225 schema redesign (read_file line ranges,
# grep_files/find_files split, edit_file operation enum), #223 MCP name
# normalization, #230 tool-choice escalation, #231 structured results, and
# the #234 toolset-profile aliases. Each task is a small synthetic repo that
# exercises ONE behaviour, with judges that check BOTH the final workspace
# state AND the tool-call trace the run produced (the "tool-trace" judge,
# added in this issue, inspects RunTrace.ToolCalls).
#
# Default (no-credential) path:
#   The acceptance criterion — "runs locally without live provider
#   credentials" — is met by the in-process replay regression in
#   harness/internal/core/tooluse_replay_test.go, which drives the SAME
#   behaviours through the agentic loop with a ReplayProvider + a real
#   LocalExecutor over synthetic workspaces. It runs under
#   `go test ./harness/...` with no network and no API key. That test is the
#   gate; this HCL file is its declarative, live-provider-comparable form.
#
# Live-provider path (opt-in, NOT default CI):
#   `stirrup-eval run` spawns the real `stirrup harness` binary, which has
#   no replay-provider path — so running THIS file end-to-end needs a live
#   provider. Pin the provider/model with a suite-level run_config (or a
#   --config baseline) and supply the credential as a secret:// reference,
#   then:
#
#     ANTHROPIC_API_KEY=... ./stirrup-eval run \
#       --suite eval/suites/tooluse.hcl \
#       --output results/tooluse
#
#   To compare models, run the suite once per model (swap the model_router
#   model, or layer a per-task run_config_overrides.model_router block) and
#   diff the resulting result.json files with `stirrup-eval compare`.
#
# See eval/suites/README.md for the run/compare/baseline workflow and the
# credential note.

suite "tooluse-reliability" {
  description = "Tool-use reliability regression for the Wave 1-5 tool redesign (#233). Each task is a small synthetic repo exercising one behaviour — read/search/edit loops, read-before-edit, line-range reads, bounded search, invalid-argument recovery, renamed-tool recovery, no-tool-answer escalation, multi-tool turns, MCP name normalization — with judges asserting both workspace state and tool-call trace. The default no-credential gate is the in-process replay regression in harness/internal/core/tooluse_replay_test.go; this HCL form is the live-provider-comparable suite (opt-in, needs a provider credential since stirrup-eval run spawns the real harness)."

  task "read-search-edit-loop" {
    description = "Search for a function, read the file, edit it. Judge confirms the edit landed AND the trace shows grep_files -> read_file -> edit_file in order. Regression-covers the #225 grep_files/read_file/edit_file split and the core coding loop."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      The workspace contains calc.go with a function Double that currently returns n + n. Find it with a search, read the file to confirm, then edit the body so Double returns n * 2 instead. Do not rewrite the whole file — use a targeted replace. Create calc.go with the original content first if it does not exist.
    EOT

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
    description = "Asserts read_file precedes edit_file in the trace. Regression-covers the read-before-edit discipline #225's schema encourages."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      Create greeting.txt containing the line "hello old world" if it does not exist. Read it, then change the word "old" to "new" with a targeted edit (not a whole-file rewrite). Leave the rest of the line unchanged.
    EOT

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
    description = "Read a specific line window with read_file start_line/limit (#225). Judge confirms read_file was called and the written summary reflects only the requested window. Regression-covers line-range reading and the #231 fileExcerpt structured result."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      doc.txt contains 20 numbered lines (create it with lines "line 1" through "line 20" if absent). Read only lines 5 through 7 using a line range, then write those three lines verbatim to window.txt.
    EOT

    judge {
      type    = "composite"
      require = "all"

      judge {
        type  = "file-exists"
        paths = ["window.txt"]
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
    description = "A broad grep with a small max_results must not flood output (#225 bounded results). Judge confirms grep_files was called and succeeded. Regression-covers bounded search and the #231 searchResult truncation flag."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      The workspace has ten files each containing the word "needle" twice (create them as f0.txt..f9.txt if absent). Search for "needle" but cap the results at 2 matches. Write the number of matches you were shown to count.txt.
    EOT

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

  task "invalid-arg-recovery" {
    description = "An edit_file 'replace' missing new_string violates the per-operation field contract (#225). Judge confirms the file ended in the corrected state AND the trace shows an edit_file failure followed by a successful one. Regression-covers invalid-argument recovery."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      config.txt contains the single line "mode=off" (create it if absent). Change it to "mode=on" with edit_file. If your first attempt is rejected for a missing field, read the error and retry with the corrected arguments.
    EOT

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
          call "edit_file" {
            min_calls = 1
          }
        }
      }
    }
  }

  task "renamed-tool-recovery" {
    description = "Calling the retired search_files name yields the directional renamed-tool hint (#225); the model should recover with grep_files. Judge confirms grep_files ultimately succeeded and no tool call ended as an unresolved unknown-tool miss. Regression-covers unknown-tool recovery."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      Find every file containing the string "TODO" in the workspace (create src.go with a "// TODO: implement" line if the workspace is empty), then write the count to todo-count.txt.
    EOT

    judge {
      type = "tool-trace"
      tool_trace {
        forbid_unknown = true
        call "grep_files" {
          min_calls     = 1
          all_succeeded = true
        }
      }
    }
  }

  task "no-tool-answer-escalation" {
    description = "In execution mode the model must inspect the workspace before answering; a no-tool answer should trigger tool-choice escalation (#230) when the operator opts in. Judge asserts at least one read/search tool was called. NOTE: escalation is OFF by default — the live run must enable RunConfig.ToolChoiceEscalation for this task to exercise the recovery; the in-process gate covers both the fired and OFF cases."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      main.go is in the workspace (create it containing "package main" if absent). Confirm which package it declares and write that package name to package.txt. Inspect the file before answering.
    EOT

    judge {
      type = "tool-trace"
      tool_trace {
        call "read_file" {
          min_calls = 1
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
      The workspace contains a.go and b.go (create them with "package a" and "package b" if absent). In one step, read a.go and list all *.go files. Then write the names of the .go files you found to gofiles.txt.
    EOT

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
