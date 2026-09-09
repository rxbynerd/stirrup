# Minimal suite fixture for end-to-end CLI tests.
#
# Used by TestCmdRun_JUnitFlag to drive `eval run --junit <path>`
# through the run() dispatcher, against a fake harness binary, and
# assert the --junit flag is wired to writeJUnit.
#
# Keep this fixture tiny and deterministic. New tests that need
# different shapes should add their own fixture rather than expand
# this one — orthogonal test inputs are easier to maintain.
suite "cmdrun-junit-fixture" {
  description = "minimal suite for cmdRun --junit dispatch test"

  task "marker" {
    mode   = "execution"
    prompt = "create marker.txt"

    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
