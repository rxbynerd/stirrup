// Live wire-protocol suite for the OpenAI reasoning-class models
// served through OpenRouter, at Terra and Sol grade.
//
// WHY THIS EXISTS
//
// The quirks registry decides, per (provider, model), which request
// fields the harness may send. Those decisions are unit-tested in
// harness/internal/provider/quirks — but a unit test pins *our*
// resolution, never the provider's parser. Every wire regression this
// project has shipped had green unit tests: the openai-responses
// `output`-key bug (#172) marshalled fine and was rejected with HTTP
// 400 by the real API, and the gateway-prefix gap this suite was
// written alongside resolved cleanly to the WRONG rule set, because
// path.Match's `*` does not cross `/` and `gpt-5*` therefore never
// matched `openai/gpt-5.6-terra`. A reasoning-class model was being
// treated as zero-value. Nothing offline could see it.
//
// So this suite runs real requests against the live API. It is the
// end-to-end half of the contract; the unit tests remain the per-PR
// gate.
//
// ASSERT ON THE WIRE CONTRACT, NOT ON TOOL CHOICE
//
// The first revision of this suite asserted `write_file` call counts.
// Four of five tasks failed on their first live run while every
// filesystem assertion passed — the models had used `edit_file`, or
// satisfied a whole three-file task with a single `run_command`. The
// assertions were measuring which tool a model happens to prefer,
// which is not a harness contract and varies by model, by grade, and
// by prompt phrasing.
//
// The rule this suite now follows:
//
//   - The correct END STATE is the primary assertion. Under a wire
//     regression the run dies mid-loop and the artifact never lands,
//     so the artifact's presence and exact bytes prove the round trip.
//   - Assert on a specific tool only where the task's PREMISE requires
//     that tool to have run (terra-empty-tool-output-round-trip needs
//     a run_command to have produced the empty result it is testing),
//     and then on min_calls rather than success — the shell-escape
//     guard legitimately rejects some command shapes, and a rejected
//     attempt the model recovers from is correct behaviour, not a
//     defect.
//   - Assert ORDER only where the prompt itself imposes it across
//     distinct tools (sol-reasoning-class-tool-chain reads before it
//     verifies).
//
// A judge that fails when the harness is working is worse than no
// judge: it trains readers to ignore the suite.
//
// WHY IT PINS ITS OWN PROVIDER
//
// The suite-level run_config below fixes provider, base URL, and model.
// That is deliberate, for the same reason
// openai-responses-empty-tool-output.hcl does it: a quirk test whose
// model can be swapped from the command line tests nothing. If CI
// passed `--model` on top, every task would silently collapse into a
// run of whatever the cheap gate happens to use, still reporting green.
//
// ci.yml::eval-gate therefore invokes `provider-quirks-*` suites with
// NO --model/--provider/--base-url override. The only thing it supplies
// is the OPENROUTER_API_KEY environment variable that the
// `secret://OPENROUTER_API_KEY` reference below resolves against.
//
// COST
//
// Terra is ~$1/M input and ~$6/M output; Sol is ~$5/M and ~$30/M —
// roughly 50x the per-token output cost of the Luna gate. This suite is
// consequently NOT part of the per-main-push gate. It runs on demand:
//
//   gh workflow run ci.yml --ref <branch> -f run_quirks_suites=true
//
// Sol tasks are held to the two behaviours where the extra grade
// actually buys signal (a longer tool chain, and strict schema
// adherence under multi-constraint instructions); everything cheaper to
// observe is asserted at Terra grade.
//
// Run locally with:
//   OPENROUTER_API_KEY=... ./stirrup-eval run \
//     --suite eval/suites/provider-quirks-openai.hcl \
//     --harness "$PWD/stirrup" \
//     --output results/

suite "provider-quirks-openai" {
  description = "Live wire-protocol regressions for OpenAI reasoning-class models via OpenRouter, at Terra and Sol grade. Pins its own provider posture so the scenario cannot be nullified by a --model override. Opt-in: costs materially more than the Luna gate, so it runs on demand rather than per main push."

  // Suite-level baseline. Every task inherits this; Sol tasks replace
  // the model_router wholesale in run_config_overrides.
  //
  // The model id carries the `openai/` vendor prefix because that is
  // what OpenRouter serves and therefore what the quirks registry must
  // cope with. Using the bare `gpt-5.6-*` form here would resolve a
  // different rule set and quietly stop testing the gateway path.
  run_config {
    // A suite-level run_config is the COMPLETE baseline RunConfig, not
    // an overlay on the harness defaults, so it must satisfy
    // ValidateRunConfig on its own — mode and max_turns included. The
    // per-task `mode` attribute reaches the harness as a --mode flag and
    // does not help here, because validation runs against the merged
    // config before any flag is applied.
    mode      = "execution"
    max_turns = 20

    provider {
      type        = "openai-compatible"
      base_url    = "https://openrouter.ai/api/v1"
      api_key_ref = "secret://OPENROUTER_API_KEY"
    }

    model_router {
      type     = "static"
      provider = "openai-compatible"
      model    = "openai/gpt-5.6-terra"
    }
  }

  task "terra-reasoning-class-tool-loop" {
    description = "Baseline contract at Terra grade: a gateway-prefixed reasoning-class model completes a prompt -> tool_use -> tool_result -> answer loop. This is the task that fails outright if the harness sends sampling parameters to a model class that rejects them — the exact exposure the */gpt-5* quirk rule closes. The correct end state IS the assertion: under a rejected request the run dies before anything is written, so the file's presence is the proof the loop completed. Deliberately makes no claim about WHICH tool did the writing — see the note on tool-choice assertions at the top of this file."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      Write the literal text `ok` (two characters, lowercase, no newline, no quotes, no surrounding whitespace) to a file named `terra-loop.txt` in the workspace root.

      Do not create any other files. When it is done, stop.
    EOT

    judge {
      type    = "file-contains"
      path    = "terra-loop.txt"
      pattern = "^ok$"
    }
  }

  task "terra-empty-tool-output-round-trip" {
    description = "Chat Completions analogue of the openai-responses #172 regression. A tool result with empty content must survive being replayed into the next request. Under an adapter that drops or nulls the empty result, the follow-up turn is rejected and the sentinel write never happens — so the sentinel's presence, not the command's exit status, is the signal. The run_command expectation is min_calls only: it confirms the empty-output path was actually exercised rather than the model skipping straight to the write, without asserting success, because the shell-escape guard legitimately rejects some command shapes and a rejected attempt is not a harness defect."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      Perform the following two steps exactly, in order, using the tools available to you. Do not run any other commands or write any other files.

      Step 1: Run the shell command `true`. It will exit with status 0 and produce no output. That is expected.

      Step 2: Write the literal text `ok` (two characters, lowercase, no newline, no quotes, no surrounding whitespace) to a file named `terra-empty.txt` in the workspace root.

      When both steps are done, stop.
    EOT

    judge {
      type    = "composite"
      require = "all"

      judge {
        type    = "file-contains"
        path    = "terra-empty.txt"
        pattern = "^ok$"
      }

      judge {
        type = "tool-trace"

        tool_trace {
          call "run_command" {
            min_calls = 1
          }
        }
      }
    }
  }

  task "terra-multi-tool-turn" {
    description = "Multi-artifact correctness: three distinct outputs must all land with exactly the right bytes. A truncated or mis-attributed tool result typically produces some files and not others, or the right count with swapped contents, which the per-file assertions catch. Makes no claim about the number of tool calls — an earlier revision asserted three write_file calls and failed against a model that satisfied the whole task with one shell command, which is a model choice, not a harness contract. Genuine multi-turn threading is covered by sol-reasoning-class-tool-chain, which the prompt forces across several turns."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      Create exactly three files in the workspace root, each containing only the single lowercase word shown, with no newline and no other content:

      - `one.txt` containing `alpha`
      - `two.txt` containing `beta`
      - `three.txt` containing `gamma`

      Do not create any other files. When all three exist, stop.
    EOT

    judge {
      type    = "composite"
      require = "all"

      judge {
        type  = "file-exists"
        paths = ["one.txt", "two.txt", "three.txt"]
      }

      judge {
        type    = "file-contains"
        path    = "one.txt"
        pattern = "^alpha$"
      }

      judge {
        type    = "file-contains"
        path    = "two.txt"
        pattern = "^beta$"
      }

      judge {
        type    = "file-contains"
        path    = "three.txt"
        pattern = "^gamma$"
      }
    }
  }

  task "sol-reasoning-class-tool-chain" {
    description = "Sol grade. A read -> edit -> verify chain over several turns, which keeps a longer tool history in the request than the Terra tasks do. Threading defects that a two-call task cannot reach — a dropped or reordered tool_result partway through the history — surface as the chain failing to complete. Sol rather than Terra because the task must not fail for want of instruction-following: the point is to test the wire, so the model needs to be capable enough that a failure implicates the harness."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      The workspace contains a file `counter.txt`.

      Read it, then rewrite it so that it contains the single number that is one greater than the number currently in it, with no newline and no other content. Then run `cat counter.txt` to confirm the new value.

      Do not create any other files. When done, stop.
    EOT

    file "counter.txt" {
      content = "41"
    }

    // Whole-struct replace, not a field merge: mergeOverrides assigns
    // *overlay.ModelRouter over the baseline, so type and provider must
    // be restated or they would be zeroed.
    run_config_overrides {
      model_router {
        type     = "static"
        provider = "openai-compatible"
        model    = "openai/gpt-5.6-sol"
      }
    }

    judge {
      type    = "composite"
      require = "all"

      judge {
        type    = "file-contains"
        path    = "counter.txt"
        pattern = "^42$"
      }

      judge {
        type = "tool-trace"

        tool_trace {
          sequence = ["read_file", "run_command"]
        }
      }
    }
  }

  task "sol-strict-schema-adherence" {
    description = "Sol grade. The gpt-5 family resolves strict-mode structured outputs, which constrains tool arguments to the declared JSON schema. This task drives a tool call whose arguments must carry an exact, awkward literal — leading zeros, embedded punctuation, brackets — through the strict-mode encoder. A schema or encoder defect mangles the argument in transit, so the file lands with the wrong bytes while every call still reports success. That is why the byte-exact, both-ends-anchored content check is the whole assertion: a tool-call-count check would pass in exactly the case this task exists to catch."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      Write the following text to a file named `strict.txt` in the workspace root. Reproduce it exactly, as a single line with no trailing newline, no quotes around it, and no other content:

      id=007;name=a-b_c.d;flags=[x,y]

      Do not create any other files. When it is done, stop.
    EOT

    run_config_overrides {
      model_router {
        type     = "static"
        provider = "openai-compatible"
        model    = "openai/gpt-5.6-sol"
      }
    }

    judge {
      type    = "file-contains"
      path    = "strict.txt"
      pattern = "^id=007;name=a-b_c\\.d;flags=\\[x,y\\]$"
    }
  }
}
