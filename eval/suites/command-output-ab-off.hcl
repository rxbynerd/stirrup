// Command-output capture A/B suite — "off" arm (issue: PR #495).
//
// This suite and command-output-ab-on.hcl carry byte-identical tasks;
// the only difference is the suite-level tools.command_output block.
// This arm disables capture, reverting run_command to the legacy
// inline path: the full command output lands in the model's context.
// See command-output-ab-on.hcl for the full A/B recipe.

suite "command-output-ab-off" {
  description = "A/B arm with command-output capture disabled (legacy inline run_command). Pair with command-output-ab-on; identical tasks, differing only in tools.command_output."

  run_config {
    mode      = "execution"
    max_turns = 20

    provider {
      type        = "anthropic"
      api_key_ref = "secret://ANTHROPIC_API_KEY"
    }

    model_router {
      type     = "static"
      provider = "anthropic"
      model    = "claude-haiku-4-5"
    }

    tools {
      command_output {
        enabled = false
      }
    }
  }

  task "needle-in-verbose-output" {
    description = "A ~200 KB build log streams to stdout with a single ERROR line buried mid-stream, outside any spill preview. The agent must identify the failing test name. Measures whether targeted reads beat inlining the whole stream."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      The workspace contains a script called noisy.sh. Run `sh noisy.sh` and find the single ERROR line in its output. Write the name of the failing test (just the test name, e.g. test_something_123) to a file called answer.txt.
    EOT

    file "noisy.sh" {
      content = <<-EOT
        #!/bin/sh
        i=1
        while [ $i -le 3000 ]; do
          echo "INFO 2026-07-12T00:00:00Z module-$i initialized in $${i}ms status=ok build=stable"
          if [ $i -eq 2100 ]; then
            echo "ERROR test_alpha_7731 failed: assertion mismatch (exit 3)"
          fi
          i=$((i+1))
        done
      EOT
    }

    judge {
      type    = "file-contains"
      path    = "answer.txt"
      pattern = "test_alpha_7731"
    }
  }

  task "two-needles-require-paging" {
    description = "Two numeric tokens sit ~170 KB apart in one command's stdout — no single preview or page contains both. The agent must sum them, forcing either paged reads or full-stream ingestion."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      The workspace contains a script called emit.sh. Run `sh emit.sh`. Its output contains exactly one line starting with "TOKEN-A:" and exactly one line starting with "TOKEN-B:", each followed by an integer. Add the two integers together and write the sum (digits only) to a file called answer.txt.
    EOT

    file "emit.sh" {
      content = <<-EOT
        #!/bin/sh
        i=1
        while [ $i -le 3000 ]; do
          echo "DEBUG 2026-07-12T00:00:00Z worker-$i heartbeat rss=$${i}kb queue=0 state=idle"
          if [ $i -eq 200 ]; then
            echo "TOKEN-A: 4217"
          fi
          if [ $i -eq 2600 ]; then
            echo "TOKEN-B: 9038"
          fi
          i=$((i+1))
        done
      EOT
    }

    judge {
      type    = "file-contains"
      path    = "answer.txt"
      pattern = "13255"
    }
  }

  task "small-output-stays-simple" {
    description = "Control task: a command with tiny output. Both arms should behave identically — guards against the capture pipeline regressing the common case."
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      Run the shell command `echo ready-42`. Write the exact text it printed to a file called answer.txt.
    EOT

    judge {
      type    = "file-contains"
      path    = "answer.txt"
      pattern = "ready-42"
    }
  }
}
