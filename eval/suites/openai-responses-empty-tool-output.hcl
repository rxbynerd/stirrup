# Live-API regression suite for issue #172 — the openai-responses
# provider adapter dropped the required `output` key from
# function_call_output wire items whenever a tool produced empty
# stdout, causing the next turn to be rejected with HTTP 400
# "Missing required parameter: 'input[N].output'".
#
# This suite is opt-in. It exercises a real round-trip against the
# OpenAI Responses API and therefore requires:
#
#   - A real OpenAI API key set in the environment.
#   - The harness binary configured with
#     `--provider openai-responses --model <model>`.
#
# The unit-level marshal tests in
# harness/internal/provider/openai_responses_test.go (covering
# empty Content, error+empty content, and multiple empty results
# in a row) are the per-PR gate for the fix. They cannot, however,
# observe whether the OpenAI Responses API accepts the resulting
# JSON — the harness's ReplayProvider sidesteps HTTP marshalling
# entirely, and even a contract test against a recorded fixture
# pins our serialisation, not OpenAI's parser. This suite is the
# end-to-end pin: under the buggy adapter the run dies on turn 2
# before the sentinel write happens, so the sentinel file is
# absent and the task fails. Under the fix the run completes and
# the sentinel is present.
#
# Not part of default CI: live provider calls are slow, flaky, and
# spend real credits, and the per-PR signal is already provided by
# the unit tests.
#
# Run with:
#   OPENAI_KEY=... ./stirrup-eval run \
#       --suite eval/suites/openai-responses-empty-tool-output.hcl \
#       --output results/

suite "openai-responses-empty-tool-output-regression" {
  description = "Live-API regression for issue #172: the openai-responses adapter dropped the required `output` key from function_call_output items when a tool produced empty stdout, causing the next turn to be rejected with HTTP 400. Opt-in — requires a real OpenAI API key and the harness configured for --provider openai-responses; the unit tests in openai_responses_test.go are the per-PR gate, this suite is the end-to-end pin against the real Responses API."

  task "empty-stdout-run-command-completes" {
    description = "Drives the agent through at least one run_command whose stdout is empty (`true`), then a write_file that drops a sentinel. Under the buggy adapter, turn 2's request is rejected with HTTP 400 and `completed.txt` is never written. Under the fix, the sentinel is present with the literal text `ok`."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      Perform the following two steps exactly, in order, using the tools available to you. Do not run any other commands or write any other files.

      Step 1: Run the shell command `true`. It will exit with status 0 and produce no output. That is expected.

      Step 2: Write the literal text `ok` (two characters, lowercase, no newline, no quotes, no surrounding whitespace) to a file named `completed.txt` in the workspace root.

      When both steps are done, stop.
    EOT

    judge {
      type    = "composite"
      require = "all"

      judge {
        type  = "file-exists"
        paths = ["completed.txt"]
      }

      judge {
        type    = "file-contains"
        path    = "completed.txt"
        pattern = "^ok$"
      }
    }
  }

  task "multiple-empty-stdout-run-commands-complete" {
    description = "Same regression mechanic as the single-empty case, but exercises two empty-stdout tool results in a row before the sentinel write. The unit-level TestTranslateMessagesResponses_MultipleEmptyToolResultContents pins this path at the marshal layer; this task confirms the real Responses API also accepts repeated empty `output` values end-to-end. Sentinel file differs from the single-empty task so a partial pass is distinguishable in result.json."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      Perform the following three steps exactly, in order, using the tools available to you. Do not run any other commands or write any other files.

      Step 1: Run the shell command `true`. It will exit with status 0 and produce no output. That is expected.

      Step 2: Run the shell command `true` again. It will exit with status 0 and produce no output. That is expected.

      Step 3: Write the literal text `ok` (two characters, lowercase, no newline, no quotes, no surrounding whitespace) to a file named `completed-multi.txt` in the workspace root.

      When all three steps are done, stop.
    EOT

    judge {
      type    = "composite"
      require = "all"

      judge {
        type  = "file-exists"
        paths = ["completed-multi.txt"]
      }

      judge {
        type    = "file-contains"
        path    = "completed-multi.txt"
        pattern = "^ok$"
      }
    }
  }
}
