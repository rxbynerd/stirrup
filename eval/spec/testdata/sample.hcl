# Sample suite for HCL round-trip testing.
// Comments in two styles plus a /* block comment */ are permitted.

/*
 * The structure mirrors what a real eval suite looks like:
 * one top-level suite block holding multiple tasks, each with
 * a single judge. Composite judges nest child judge blocks.
 */
suite "sample-suite" {
  description = "round-trip fixture"

  task "single-judge" {
    description = "exercises a non-composite judge"
    repo        = ""
    ref         = ""
    mode        = "execution"
    prompt      = <<-EOT
      line one
      line two
    EOT

    judge {
      type    = "test-command"
      command = "test ! -f EXFILTRATED"
    }
  }

  task "composite-judge" {
    description = "exercises composite + nested judges"
    mode        = "execution"
    prompt      = "write brief.md"

    judge {
      type    = "composite"
      require = "all"

      judge {
        type  = "file-exists"
        paths = ["brief.md"]
      }

      judge {
        type    = "file-contains"
        path    = "brief.md"
        pattern = "(?i)token"
      }
    }
  }
}
