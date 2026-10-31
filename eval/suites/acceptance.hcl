// Acceptance suite: realistic tasks on real repositories at pinned commits.
//
// Every task clones a small public Go repository at a full commit SHA
// (the runner fetches the SHA directly, so it need not be a branch tip)
// and is judged by a deterministic command or trace check, never a
// model. Four small Go modules with no third-party dependencies are used:
//
//   - github.com/mitchellh/go-wordwrap (MIT)
//   - github.com/rs/xid                (MIT)
//   - github.com/google/shlex          (Apache-2.0)
//   - github.com/dustin/go-humanize    (MIT)
//
// Bug-fix tasks pin the parent of the upstream fix (or a bug that still
// exists upstream) and seed a failing test that reproduces it. Judges
// that need a hidden check write their own test file at judge time and
// remove it afterwards, so the agent never sees it.
//
// The `add-function-with-verifier-retry` task also exercises recovery:
// its verifier rejects the first submission unconditionally, then runs
// the real tests, and the judge confirms the verifier ran at least twice.
// The two `*-under-post-edit-check` tasks fail a suite-level post-edit
// check on their first edit, so A/B runs of feedback features have tasks
// a first attempt does not pass.
// A per-task verifier needs a suite baseline, so the suite-level
// run_config below pins Anthropic Sonnet 5 and a Go-only tool set
// without web_fetch (the agent must not read the upstream fix). The
// provider, model and key flags of `stirrup-eval run` retarget it.
//
// Needs a Go toolchain and network access to github.com. Opt-in, no
// baseline, not run by CI. Run with:
//   stirrup-eval run \
//     --suite eval/suites/acceptance.hcl \
//     --output eval/results/acceptance \
//     --trials 3
// Cost and the failure cases deliberately not covered are in
// eval/suites/README.md.

suite "acceptance" {
  description = "Opt-in acceptance suite: ten realistic tasks on pinned revisions of small public Go repositories, including a verifier that fails once before it passes and two tasks whose first edit fails a post-edit check."

  run_config {
    mode      = "execution"
    max_turns = 30

    provider {
      type        = "anthropic"
      api_key_ref = "secret://ANTHROPIC_API_KEY"
    }

    model_router {
      type     = "static"
      provider = "anthropic"
      model    = "claude-sonnet-5"
    }

    permission_policy {
      type = "allow-all"
    }

    tools {
      built_in = ["read_file", "write_file", "edit_file", "list_directory", "grep_files", "find_files", "run_command"]

      // Matches only the files the two *-under-post-edit-check tasks
      // create. Each check needs both of its task's files and a hidden
      // conformance test, so the first edit fails it by construction.
      post_edit {
        glob            = "*percent*.go"
        timeout_seconds = 120
        command         = <<-EOT
          if ! grep -q '^func ExamplePercent()' example_percent_test.go 2>/dev/null; then
            echo "example_percent_test.go must define ExamplePercent with an // Output: comment"
            exit 1
          fi
          trap 'rm -f zz_percent_check_test.go' EXIT
          cat > zz_percent_check_test.go <<'CHECK_EOF'
          package humanize

          import (
            "math"
            "testing"
          )

          func TestPostEditCheckPercent(t *testing.T) {
            cases := []struct {
              part, whole float64
              decimals    int
              want        string
            }{
              {1, 3, 1, "33.3%"},
              {2, 3, 0, "67%"},
              {1, 2, 2, "50.00%"},
              {3, 2, 0, "150%"},
              {0, 5, 1, "0.0%"},
              {1, 8, 0, "13%"},
              {1, 16, 1, "6.3%"},
              {-1, 16, 1, "-6.3%"},
              {-1, 4, 0, "-25%"},
              {-1, 1000, 0, "0%"},
              {1, 3, -2, "33%"},
              {1, 0, 1, "n/a"},
              {math.NaN(), 1, 1, "n/a"},
              {1, math.Inf(1), 1, "n/a"},
              {math.Inf(-1), 1, 1, "n/a"},
            }
            for _, tc := range cases {
              if got := Percent(tc.part, tc.whole, tc.decimals); got != tc.want {
                t.Errorf("Percent(%v, %v, %d) = %q, want %q", tc.part, tc.whole, tc.decimals, got, tc.want)
              }
            }
          }
          CHECK_EOF
          go vet ./... && go test -run 'Percent' ./...
        EOT
      }

      post_edit {
        glob            = "*lenient*.go"
        timeout_seconds = 120
        command         = <<-EOT
          if ! grep -q '^func ExampleParseLenient()' example_lenient_test.go 2>/dev/null; then
            echo "example_lenient_test.go must define ExampleParseLenient with an // Output: comment"
            exit 1
          fi
          trap 'rm -f zz_lenient_check_test.go' EXIT
          cat > zz_lenient_check_test.go <<'CHECK_EOF'
          package xid

          import (
            "errors"
            "fmt"
            "testing"
          )

          func TestPostEditCheckParseLenient(t *testing.T) {
            const canonical = "9m4e2mr0ui3e8a215n4g"
            want, err := FromString(canonical)
            if err != nil {
              t.Fatal(err)
            }
            for _, in := range []string{
              canonical,
              "9M4E2MR0UI3E8A215N4G",
              "  9m4e2mr0ui3e8a215n4g\n",
              "xid:9m4e2mr0ui3e8a215n4g",
              "XID:9m4e2mr0ui3e8a215n4g",
              "Xid:9M4E2MR0UI3E8A215N4G",
              "\txid:9m4e2mr0ui3e8a215n4g ",
            } {
              got, err := ParseLenient(in)
              if err != nil || got != want {
                t.Errorf("ParseLenient(%q) = %v, %v; want %v, nil", in, got, err, want)
              }
            }
            for _, in := range []string{
              "",
              "   ",
              "xid:",
              "XID:  ",
              "xid:xid:9m4e2mr0ui3e8a215n4g",
              "xid: 9m4e2mr0ui3e8a215n4g",
              "9m4e2mr0ui3e 8a215n4g",
              "id:9m4e2mr0ui3e8a215n4g",
              "9m4e2mr0ui3e8a215n4",
              "9m4e2mr0ui3e8a215n4!",
            } {
              got, err := ParseLenient(in)
              if !errors.Is(err, ErrInvalidID) {
                t.Errorf("ParseLenient(%q) err = %v, want one wrapping ErrInvalidID", in, err)
                continue
              }
              if wantMsg := fmt.Sprintf("xid: invalid ID: %q", in); err.Error() != wantMsg {
                t.Errorf("ParseLenient(%q) err = %q, want %q", in, err.Error(), wantMsg)
              }
              if got != (ID{}) {
                t.Errorf("ParseLenient(%q) = %v, want the zero ID on error", in, got)
              }
            }
          }
          CHECK_EOF
          go vet ./... && go test -run 'Lenient' ./...
        EOT
      }
    }
  }

  task "fix-multibyte-wrap" {
    description = "Fix a failing test: WrapString counts bytes instead of characters for multi-byte text. Pinned to the parent of the upstream fix; the failing test is seeded."
    repo        = "https://github.com/mitchellh/go-wordwrap"
    ref         = "7256b9c0a84f21c609b5c38205e57f0cf4c3981b"
    mode        = "execution"
    prompt      = <<-EOT
      The new test TestWrapStringCountsCharactersNotBytes in multibyte_test.go fails: WrapString breaks lines too early for text containing multi-byte characters because it measures width in bytes instead of characters. Fix WrapString in wordwrap.go so the width limit counts characters, keep the existing tests passing, and do not edit multibyte_test.go. Make sure `go test ./...` passes.
    EOT

    file "multibyte_test.go" {
      content = <<-EOT
        package wordwrap

        import (
          "strings"
          "testing"
        )

        func TestWrapStringCountsCharactersNotBytes(t *testing.T) {
          cases := []struct {
            name, input, want string
            lim               uint
          }{
            {
              name:  "block characters",
              input: strings.Repeat("▄ ", 4),
              want:  "▄ ▄\n▄ ▄ ",
              lim:   4,
            },
            {
              name:  "accented words",
              input: "café crème brûlée",
              want:  "café\ncrème\nbrûlée",
              lim:   6,
            },
            {
              name:  "ideographs",
              input: "你好 世界 再见",
              want:  "你好 世界\n再见",
              lim:   5,
            },
          }
          for _, tc := range cases {
            t.Run(tc.name, func(t *testing.T) {
              if got := WrapString(tc.input, tc.lim); got != tc.want {
                t.Errorf("WrapString(%q, %d) = %q, want %q", tc.input, tc.lim, got, tc.want)
              }
            })
          }
        }
      EOT
    }

    judge {
      type = "test-command"
      command = <<-EOT
        test "$(grep -c 'name:' multibyte_test.go)" -eq 3 && go vet ./... && go test ./...
      EOT
    }
  }

  task "fix-json-unmarshal-panic" {
    description = "Fix a failing test: ID.UnmarshalJSON panics on a one-byte JSON value. Pinned to the parent of the upstream fix; the failing test is seeded."
    repo        = "https://github.com/rs/xid"
    ref         = "efa678f304ab65d6d57eedcb086798381ae22206"
    mode        = "execution"
    prompt      = <<-EOT
      The new test TestUnmarshalJSONRejectsMalformedValuesWithoutPanicking in unmarshal_json_test.go panics: decoding a JSON document such as {"ID":1} into a struct with an ID field crashes inside ID.UnmarshalJSON instead of returning ErrInvalidID. Fix the bug in id.go without changing behaviour for valid input, and do not edit unmarshal_json_test.go. Make sure `go test ./...` passes.
    EOT

    file "unmarshal_json_test.go" {
      content = <<-EOT
        package xid

        import (
          "encoding/json"
          "errors"
          "testing"
        )

        func TestUnmarshalJSONRejectsMalformedValuesWithoutPanicking(t *testing.T) {
          for _, input := range []string{
            `{"ID":1}`,
            `{"ID":9}`,
            `{"ID":[]}`,
            `{"ID":true}`,
            `{"ID":"short"}`,
          } {
            t.Run(input, func(t *testing.T) {
              var v struct{ ID ID }
              err := json.Unmarshal([]byte(input), &v)
              if !errors.Is(err, ErrInvalidID) {
                t.Errorf("json.Unmarshal(%s) err = %v, want %v", input, err, ErrInvalidID)
              }
            })
          }
        }
      EOT
    }

    judge {
      type = "test-command"
      command = <<-EOT
        test "$(grep -c '"ID"' unmarshal_json_test.go)" -ge 5 && go vet ./... && go test ./...
      EOT
    }
  }

  task "fix-negative-ordinal-suffix" {
    description = "Fix a failing test in a larger package: Ordinal gives negative numbers the wrong suffix. Pinned to a commit that still has the bug; the failing test is seeded."
    repo        = "https://github.com/dustin/go-humanize"
    ref         = "a1b4e66b9a6d890e9e15e7091cf16c8032367d6e"
    mode        = "execution"
    prompt      = <<-EOT
      The new test TestOrdinalNegativeNumbersUseTheSameSuffixRules in ordinals_negative_test.go fails: Ordinal(-1) returns "-1th" instead of "-1st". Negative numbers should take the suffix their absolute value would take, including the 11, 12 and 13 exceptions. Fix Ordinal, keep the existing tests passing, and do not edit ordinals_negative_test.go. Make sure `go test ./...` passes.
    EOT

    file "ordinals_negative_test.go" {
      content = <<-EOT
        package humanize

        import "testing"

        func TestOrdinalNegativeNumbersUseTheSameSuffixRules(t *testing.T) {
          for in, want := range map[int]string{
            -1:   "-1st",
            -2:   "-2nd",
            -3:   "-3rd",
            -4:   "-4th",
            -11:  "-11th",
            -12:  "-12th",
            -13:  "-13th",
            -21:  "-21st",
            -101: "-101st",
            -111: "-111th",
          } {
            if got := Ordinal(in); got != want {
              t.Errorf("Ordinal(%d) = %q, want %q", in, got, want)
            }
          }
        }
      EOT
    }

    judge {
      type = "test-command"
      command = <<-EOT
        test "$(grep -c -- '-111' ordinals_negative_test.go)" -ge 1 && go vet ./... && go test ./...
      EOT
    }
  }

  task "add-quote-function-with-test" {
    description = "Add a new exported function and its own test to a small library. The judge round-trips a set of awkward strings through the agent's Quote and Join with a hidden test."
    repo        = "https://github.com/google/shlex"
    ref         = "e7afc7fbc51079733e9468cdfd1efcd7d196cd1d"
    mode        = "execution"
    prompt      = <<-EOT
      Add two exported functions to this package, in a new file quote.go:

      - `Quote(s string) string` returns s quoted so that `Split(Quote(s))` yields exactly `[]string{s}` for any s, including the empty string, strings with spaces, quote characters, backslashes, `#` and newlines.
      - `Join(words []string) string` quotes each word and joins them with single spaces so that `Split(Join(words))` yields words again.

      Add tests for both in a new file quote_test.go. Make sure `go test ./...` passes.
    EOT

    judge {
      type    = "composite"
      require = "all"

      judge {
        type  = "file-exists"
        paths = ["quote.go", "quote_test.go"]
      }

      judge {
        type = "test-command"
        command = <<-EOT
          cat > zz_judge_test.go <<'JUDGE_EOF'
          package shlex

          import (
            "reflect"
            "testing"
          )

          func TestJudgeQuoteRoundTrip(t *testing.T) {
            words := []string{"plain", "", "two words", "it's", `say "hi"`, `back\slash`, "#hash", "tab\there", "line\nbreak", "café 你好", `\`, `"`, "'", "a'b\"c"}
            for _, w := range words {
              got, err := Split(Quote(w))
              if err != nil || !reflect.DeepEqual(got, []string{w}) {
                t.Errorf("Split(Quote(%q)) = %q, %v", w, got, err)
              }
            }
            got, err := Split(Join(words))
            if err != nil || !reflect.DeepEqual(got, words) {
              t.Errorf("Split(Join(words)) = %q, %v", got, err)
            }
            if got := Join(nil); got != "" {
              t.Errorf("Join(nil) = %q, want empty", got)
            }
          }
          JUDGE_EOF
          trap 'rm -f zz_judge_test.go' EXIT
          go vet ./... && go test ./...
        EOT
      }
    }
  }

  task "add-function-with-verifier-retry" {
    description = "Add a method and a test while a test-runner verifier rejects the first submission unconditionally and runs the real tests from then on. Judge confirms the verifier ran at least twice, i.e. the run recovered from a failed verification."
    repo        = "https://github.com/rs/xid"
    ref         = "1b855c3d62f5df92838d8ab14261cc5dbf2fd046"
    mode        = "execution"
    prompt      = <<-EOT
      Add a method `IsZero() bool` to the ID type in id.go that reports whether the ID is the nil ID, as an alias of IsNil, with a doc comment. Add a test for it to id_test.go. Make sure `go test ./...` passes. The project verifier runs when you finish; if it reports a failure, act on its feedback and finish again.
    EOT

    run_config_overrides {
      verifier {
        type    = "test-runner"
        timeout = 120
        command = <<-EOT
          echo run >> .git/stirrup-verifier-runs
          if [ "$(wc -l < .git/stirrup-verifier-runs)" -le 1 ]; then
            echo "Verifier rejected the first submission. Re-check the change and the tests, then finish again."
            exit 1
          fi
          go test ./...
        EOT
      }
    }

    judge {
      type    = "composite"
      require = "all"

      judge {
        type    = "file-contains"
        path    = "id_test.go"
        pattern = "IsZero"
      }

      judge {
        type    = "test-command"
        command = "test \"$(wc -l < .git/stirrup-verifier-runs)\" -ge 2"
      }

      judge {
        type = "test-command"
        command = <<-EOT
          cat > zz_judge_test.go <<'JUDGE_EOF'
          package xid

          import "testing"

          func TestJudgeIsZero(t *testing.T) {
            var zero ID
            if !zero.IsZero() || !NilID().IsZero() {
              t.Error("the zero ID must report IsZero")
            }
            if New().IsZero() {
              t.Error("a generated ID must not report IsZero")
            }
            id, err := FromString("9m4e2mr0ui3e8a215n4g")
            if err != nil || id.IsZero() {
              t.Errorf("FromString: %v, IsZero=%v", err, id.IsZero())
            }
          }
          JUDGE_EOF
          trap 'rm -f zz_judge_test.go' EXIT
          go vet ./... && go test ./...
        EOT
      }
    }
  }

  task "move-shared-helper-to-own-file" {
    description = "Multi-file edit: logn is defined in bytes.go but also used by si.go. Move it to a new file without changing behaviour. Judge checks the new file, the other files compile and pass, and that exactly two files changed."
    repo        = "https://github.com/dustin/go-humanize"
    ref         = "a1b4e66b9a6d890e9e15e7091cf16c8032367d6e"
    mode        = "execution"
    prompt      = <<-EOT
      The helper `logn` is defined in bytes.go, but si.go uses it as well. Move `logn` into a new file `mathutil.go` in the same package, with no change in behaviour, and leave every other function where it is. Make sure `go vet ./...` and `go test ./...` pass.
    EOT

    judge {
      type = "test-command"
      command = <<-EOT
        test -f mathutil.go && grep -q '^func logn(' mathutil.go && ! grep -q 'func logn' bytes.go && ! grep -q 'func logn' si.go && go vet ./... && go test ./... && test "$(git status --porcelain | awk '{print $2}' | grep -vx runconfig.json | sort | tr '\n' ' ')" = "bytes.go mathutil.go "
      EOT
    }
  }

  task "comment-only-edit-leaves-code-alone" {
    description = "Narrow edit in a real repository: extend one doc comment and touch nothing else. Judge reads git status and the diff, so any code change or any second file fails."
    repo        = "https://github.com/dustin/go-humanize"
    ref         = "a1b4e66b9a6d890e9e15e7091cf16c8032367d6e"
    mode        = "execution"
    prompt      = <<-EOT
      In ordinals.go, extend the doc comment on Ordinal with two more examples after the existing one, in the same style: `Ordinal(11) -> 11th` and `Ordinal(22) -> 22nd`. Change nothing else in the repository.
    EOT

    judge {
      type    = "composite"
      require = "all"

      judge {
        type    = "file-contains"
        path    = "ordinals.go"
        pattern = "Ordinal\\(11\\) -> 11th[\\s\\S]*Ordinal\\(22\\) -> 22nd"
      }

      judge {
        type = "test-command"
        command = <<-EOT
          test "$(git status --porcelain | grep -v ' runconfig.json$')" = " M ordinals.go" && test "$(git diff -U0 -- ordinals.go | grep -E '^[+-]' | grep -vE '^(\+\+\+|---) ' | grep -vcE '^[+-][[:space:]]*//')" -eq 0 && go build ./... && go test ./...
        EOT
      }
    }
  }

  task "answer-question-from-source" {
    description = "Navigate an unfamiliar repository read-only and answer a question from its source. Judge checks the answer file for the facts, git status for no other change, and the trace for no edit_file calls."
    repo        = "https://github.com/rs/xid"
    ref         = "34d39051ca0d70f40a8cd8c5491ef835e624b71d"
    mode        = "execution"
    prompt      = <<-EOT
      Without changing any existing file, answer these questions about this repository from its source and write the answers to a new file answer.txt:

      1. Which environment variable overrides the machine id?
      2. Which function reads it?
      3. What is the largest value it accepts?
      4. What happens when the value is not a number?
    EOT

    judge {
      type    = "composite"
      require = "all"

      judge {
        type    = "file-contains"
        path    = "answer.txt"
        pattern = "XID_MACHINE_ID"
      }

      judge {
        type    = "file-contains"
        path    = "answer.txt"
        pattern = "readMachineIDFromEnv"
      }

      judge {
        type    = "file-contains"
        path    = "answer.txt"
        pattern = "(?i)(16777215|0xFFFFFF|2\\^24)"
      }

      judge {
        type    = "file-contains"
        path    = "answer.txt"
        pattern = "(?i)panic"
      }

      judge {
        type    = "test-command"
        command = "test \"$(git status --porcelain | grep -v ' runconfig.json$')\" = \"?? answer.txt\""
      }

      judge {
        type = "tool-trace"
        tool_trace {
          forbid_unknown = true
          call "edit_file" {
            max_calls = 0
          }
        }
      }
    }
  }

  task "add-percent-formatter-under-post-edit-check" {
    description = "Add a formatting function with exact rounding and edge-case rules, plus an example test. The suite's post-edit check on these files needs both of them and runs a hidden conformance test, so the first edit fails it; the judge reruns the hidden test."
    repo        = "https://github.com/dustin/go-humanize"
    ref         = "a1b4e66b9a6d890e9e15e7091cf16c8032367d6e"
    mode        = "execution"
    prompt      = <<-EOT
      Add an exported function `Percent(part, whole float64, decimals int) string` to this package in a new file percent.go, and an example function `ExamplePercent` with an `// Output:` comment in a new file example_percent_test.go.

      Percent formats part/whole as a percentage with exactly `decimals` digits after the decimal point, followed by `%`: `Percent(1, 3, 1)` is `33.3%` and `Percent(1, 2, 2)` is `50.00%`. It rounds half away from zero (`Percent(1, 8, 0)` is `13%`), treats a negative `decimals` as 0, never prints a negative zero (`Percent(-1, 1000, 0)` is `0%`), and returns `n/a` when whole is 0 or either argument is NaN or infinite.

      Checks may run after your edits; act on any failure they report. Make sure `go vet ./...` and `go test ./...` pass.
    EOT

    judge {
      type = "test-command"
      command = <<-EOT
        grep -q '^func ExamplePercent()' example_percent_test.go || exit 1
        trap 'rm -f zz_judge_test.go' EXIT
        cat > zz_judge_test.go <<'JUDGE_EOF'
        package humanize

        import (
          "math"
          "testing"
        )

        func TestJudgePercent(t *testing.T) {
          cases := []struct {
            part, whole float64
            decimals    int
            want        string
          }{
            {1, 3, 1, "33.3%"},
            {2, 3, 0, "67%"},
            {1, 2, 2, "50.00%"},
            {3, 2, 0, "150%"},
            {0, 5, 1, "0.0%"},
            {1, 8, 0, "13%"},
            {1, 16, 1, "6.3%"},
            {-1, 16, 1, "-6.3%"},
            {-1, 4, 0, "-25%"},
            {-1, 1000, 0, "0%"},
            {1, 3, -2, "33%"},
            {1, 0, 1, "n/a"},
            {math.NaN(), 1, 1, "n/a"},
            {1, math.Inf(1), 1, "n/a"},
            {math.Inf(-1), 1, 1, "n/a"},
          }
          for _, tc := range cases {
            if got := Percent(tc.part, tc.whole, tc.decimals); got != tc.want {
              t.Errorf("Percent(%v, %v, %d) = %q, want %q", tc.part, tc.whole, tc.decimals, got, tc.want)
            }
          }
        }
        JUDGE_EOF
        go vet ./... && go test ./...
      EOT
    }
  }

  task "add-lenient-parser-under-post-edit-check" {
    description = "Add a lenient ID parser with exact acceptance and error-message rules, plus an example test. The suite's post-edit check on these files needs both of them and runs a hidden conformance test, so the first edit fails it; the judge reruns the hidden test."
    repo        = "https://github.com/rs/xid"
    ref         = "1b855c3d62f5df92838d8ab14261cc5dbf2fd046"
    mode        = "execution"
    prompt      = <<-EOT
      Add an exported function `ParseLenient(s string) (ID, error)` to this package in a new file lenient.go, and an example function `ExampleParseLenient` with an `// Output:` comment in a new file example_lenient_test.go.

      ParseLenient accepts what FromString accepts, and also: surrounding whitespace, trimmed once before anything else; one optional `xid:` prefix in any letter case; and the ID's characters in upper case. Whitespace anywhere else, a repeated prefix, or anything else FromString would reject is invalid. On invalid input it returns the zero ID and an error that wraps ErrInvalidID (so `errors.Is(err, ErrInvalidID)` holds) and whose message is `xid: invalid ID: ` followed by the original, untrimmed input quoted as with `%q`.

      Checks may run after your edits; act on any failure they report. Make sure `go vet ./...` and `go test ./...` pass.
    EOT

    judge {
      type = "test-command"
      command = <<-EOT
        grep -q '^func ExampleParseLenient()' example_lenient_test.go || exit 1
        trap 'rm -f zz_judge_test.go' EXIT
        cat > zz_judge_test.go <<'JUDGE_EOF'
        package xid

        import (
          "errors"
          "fmt"
          "testing"
        )

        func TestJudgeParseLenient(t *testing.T) {
          const canonical = "9m4e2mr0ui3e8a215n4g"
          want, err := FromString(canonical)
          if err != nil {
            t.Fatal(err)
          }
          for _, in := range []string{
            canonical,
            "9M4E2MR0UI3E8A215N4G",
            "  9m4e2mr0ui3e8a215n4g\n",
            "xid:9m4e2mr0ui3e8a215n4g",
            "XID:9m4e2mr0ui3e8a215n4g",
            "Xid:9M4E2MR0UI3E8A215N4G",
            "\txid:9m4e2mr0ui3e8a215n4g ",
          } {
            got, err := ParseLenient(in)
            if err != nil || got != want {
              t.Errorf("ParseLenient(%q) = %v, %v; want %v, nil", in, got, err, want)
            }
          }
          for _, in := range []string{
            "",
            "   ",
            "xid:",
            "XID:  ",
            "xid:xid:9m4e2mr0ui3e8a215n4g",
            "xid: 9m4e2mr0ui3e8a215n4g",
            "9m4e2mr0ui3e 8a215n4g",
            "id:9m4e2mr0ui3e8a215n4g",
            "9m4e2mr0ui3e8a215n4",
            "9m4e2mr0ui3e8a215n4!",
          } {
            got, err := ParseLenient(in)
            if !errors.Is(err, ErrInvalidID) {
              t.Errorf("ParseLenient(%q) err = %v, want one wrapping ErrInvalidID", in, err)
              continue
            }
            if wantMsg := fmt.Sprintf("xid: invalid ID: %q", in); err.Error() != wantMsg {
              t.Errorf("ParseLenient(%q) err = %q, want %q", in, err.Error(), wantMsg)
            }
            if got != (ID{}) {
              t.Errorf("ParseLenient(%q) = %v, want the zero ID on error", in, got)
            }
          }
        }
        JUDGE_EOF
        go vet ./... && go test ./...
      EOT
    }
  }
}
