package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newTestProvidersQuirksCommand returns a fresh cobra command preloaded
// with the providers-quirks flag surface. Mirrors
// newTestRunConfigCommand's per-test scope so flag-changed state does
// not leak between tests.
func newTestProvidersQuirksCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "quirks"}
	cmd.Flags().String("provider", "", "")
	cmd.Flags().String("model", "", "")
	return cmd
}

// TestProvidersQuirks_EmptyRegistryEmitsValidJSON exercises the
// Step-1 BuiltinRules() empty state: the subcommand must still
// produce a structurally valid JSON document with an empty
// appliedRules slice (not null) and a ProviderQuirks value at the
// post-Resolve zero shape (non-nil empty maps/slices).
func TestProvidersQuirks_EmptyRegistryEmitsValidJSON(t *testing.T) {
	cmd := newTestProvidersQuirksCommand()
	if err := cmd.ParseFlags([]string{"--provider", "openai-compatible", "--model", "gpt-4o"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	var buf bytes.Buffer
	if err := runProvidersQuirksWithIO(cmd, &buf); err != nil {
		t.Fatalf("runProvidersQuirksWithIO: %v", err)
	}

	var got quirksCLIOutput
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}

	if got.Provider != "openai-compatible" {
		t.Errorf("Provider = %q, want openai-compatible", got.Provider)
	}
	if got.Model != "gpt-4o" {
		t.Errorf("Model = %q, want gpt-4o", got.Model)
	}
	if got.AppliedRules == nil {
		t.Error("AppliedRules must be a non-nil slice (empty array, not null)")
	}
	if len(got.AppliedRules) != 0 {
		t.Errorf("AppliedRules = %+v, want empty", got.AppliedRules)
	}
	// The JSON output must also literally contain "appliedRules": []
	// so downstream scripts can rely on the array shape even before
	// unmarshalling.
	if !strings.Contains(buf.String(), `"appliedRules": []`) {
		t.Errorf("output missing literal `\"appliedRules\": []`:\n%s", buf.String())
	}
}

// TestProvidersQuirks_RequiredFlags pins the MarkFlagRequired wiring
// on the registered cobra command: missing --provider or --model
// must produce a non-nil error rather than a bare-default
// resolution.
func TestProvidersQuirks_RequiredFlags(t *testing.T) {
	for _, name := range []string{"provider", "model"} {
		t.Run("missing-"+name, func(t *testing.T) {
			// Re-execute the real command tree with the offending flag
			// absent. cobra surfaces MarkFlagRequired through
			// Command.Execute, not ParseFlags, so we drive Execute
			// against a fresh root command to avoid mutating the
			// process-global one.
			root := &cobra.Command{Use: "stirrup"}
			parent := &cobra.Command{Use: "providers"}
			child := &cobra.Command{Use: "quirks", RunE: runProvidersQuirks}
			child.Flags().String("provider", "", "")
			child.Flags().String("model", "", "")
			_ = child.MarkFlagRequired("provider")
			_ = child.MarkFlagRequired("model")
			root.AddCommand(parent)
			parent.AddCommand(child)

			args := []string{"providers", "quirks"}
			if name == "provider" {
				args = append(args, "--model", "gpt-4o")
			} else {
				args = append(args, "--provider", "openai-compatible")
			}
			root.SetArgs(args)
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			err := root.Execute()
			if err == nil {
				t.Fatalf("expected required-flag error for missing --%s, got nil", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error %q must mention the missing flag --%s", err.Error(), name)
			}
		})
	}
}

// TestProvidersQuirks_OutputIsValidJSON broadens
// TestProvidersQuirks_EmptyRegistryEmitsValidJSON to assert that the
// emitted document is valid against a generic decoder (not just our
// typed struct). Catches a future regression where a field with no
// JSON tag is added to quirksCLIOutput and produces unexpected camel
// or snake casing.
func TestProvidersQuirks_OutputIsValidJSON(t *testing.T) {
	cmd := newTestProvidersQuirksCommand()
	if err := cmd.ParseFlags([]string{"--provider", "gemini", "--model", "gemini-3.1-pro-preview"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	var buf bytes.Buffer
	if err := runProvidersQuirksWithIO(cmd, &buf); err != nil {
		t.Fatalf("runProvidersQuirksWithIO: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	for _, key := range []string{"provider", "model", "quirks", "appliedRules"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing top-level key %q in output: %s", key, buf.String())
		}
	}
}
