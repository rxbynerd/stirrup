package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
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
// no-rules-matched state: the subcommand must still produce a
// structurally valid JSON document with an empty appliedRules slice
// (not null) and a ProviderQuirks value at the post-Resolve zero shape
// (non-nil empty maps/slices). It resolves an unknown provider type so
// no builtin rule fires — the first-party providers now all carry a
// cross-provider tool-choice base rule (#230) that matches every model,
// so this assertion needs a provider with no rules at all to still
// observe the empty-appliedRules path.
func TestProvidersQuirks_EmptyRegistryEmitsValidJSON(t *testing.T) {
	cmd := newTestProvidersQuirksCommand()
	if err := cmd.ParseFlags([]string{"--provider", "no-such-provider", "--model", "gpt-4o"}); err != nil {
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

	if got.Provider != "no-such-provider" {
		t.Errorf("Provider = %q, want no-such-provider", got.Provider)
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

// TestCollectAppliedRules_FiltersAndFlagsCorrectly exercises every
// branch of collectAppliedRules using a hand-constructed rule slice.
// Without this test the CLI's primary operator-facing correctness
// surface had zero direct coverage (BuiltinRules() is empty in Step 1
// so no rule ever flowed through the predicate or staleness branches).
//
// Covers:
//   - matching rule (ProviderType + ModelMatch glob both match)
//   - non-matching rule (different ProviderType)
//   - stale-flag branch (LastVerified > 180 days ago)
//   - zero-LastVerified branch (empty lastVerified string; Stale=false)
//   - Apply == nil guard (skipped; mirrors Resolve)
//   - specificity ordering (longer ModelMatch glob runs last and wins)
func TestCollectAppliedRules_FiltersAndFlagsCorrectly(t *testing.T) {
	noop := func(*quirks.ProviderQuirks) {}
	stale := time.Now().Add(-365 * 24 * time.Hour) // 1y ago, well past 180d
	fresh := time.Now().Add(-30 * 24 * time.Hour)  // 30d ago, fresh

	rules := []quirks.Rule{
		{
			// Wildcard rule: matches every model for the provider.
			// Glob length 1, declared first → runs first under sort.
			ProviderType: "openai-compatible",
			ModelMatch:   "*",
			Description:  "wildcard openai-compatible",
			LastVerified: fresh,
			Apply:        noop,
		},
		{
			// Stale rule: LastVerified > 180d ago.
			ProviderType: "openai-compatible",
			ModelMatch:   "gpt-4*",
			Description:  "stale gpt-4 rule",
			LastVerified: stale,
			Apply:        noop,
		},
		{
			// Zero-LastVerified rule: lastVerified field renders empty,
			// Stale flag stays false (zero time is not "before cutoff"
			// in the meaningful sense the operator cares about).
			ProviderType: "openai-compatible",
			ModelMatch:   "gpt-4o",
			Description:  "zero-LastVerified gpt-4o rule",
			LastVerified: time.Time{},
			Apply:        noop,
		},
		{
			// Non-matching: different ProviderType.
			ProviderType: "anthropic",
			ModelMatch:   "claude-*",
			Description:  "anthropic claude rule (should not appear)",
			LastVerified: fresh,
			Apply:        noop,
		},
		{
			// Apply == nil: must be filtered out, matching Resolve's
			// behaviour, even though the predicate matches.
			ProviderType: "openai-compatible",
			ModelMatch:   "gpt-4o",
			Description:  "nil-apply gpt-4o rule (should not appear)",
			LastVerified: fresh,
			Apply:        nil,
		},
	}

	reg := quirks.NewRegistry(rules)
	_, applied := reg.ResolveWithRules("openai-compatible", "gpt-4o")
	got := formatAppliedRules(applied)

	// Expected, in specificity order:
	//   1. "wildcard openai-compatible"            (glob length 1)
	//   2. "stale gpt-4 rule"                      (glob length 5)
	//   3. "zero-LastVerified gpt-4o rule"         (glob length 6)
	// The anthropic rule has the wrong ProviderType; the nil-apply rule
	// is skipped by the guard.
	wantDescriptions := []string{
		"wildcard openai-compatible",
		"stale gpt-4 rule",
		"zero-LastVerified gpt-4o rule",
	}
	if len(got) != len(wantDescriptions) {
		t.Fatalf("got %d applied rules, want %d: %+v", len(got), len(wantDescriptions), got)
	}
	for i, want := range wantDescriptions {
		if got[i].Description != want {
			t.Errorf("applied[%d].Description = %q, want %q", i, got[i].Description, want)
		}
	}

	// Stale flag and lastVerified rendering by index in the sorted output.
	if got[0].Stale {
		t.Errorf("applied[0] (fresh wildcard) Stale = true, want false")
	}
	if got[0].LastVerified != fresh.Format("2006-01-02") {
		t.Errorf("applied[0].LastVerified = %q, want %q", got[0].LastVerified, fresh.Format("2006-01-02"))
	}
	if !got[1].Stale {
		t.Errorf("applied[1] (stale) Stale = false, want true")
	}
	if got[1].LastVerified != stale.Format("2006-01-02") {
		t.Errorf("applied[1].LastVerified = %q, want %q", got[1].LastVerified, stale.Format("2006-01-02"))
	}
	if got[2].Stale {
		t.Errorf("applied[2] (zero-LastVerified) Stale = true, want false (zero time is not flagged)")
	}
	if got[2].LastVerified != "" {
		t.Errorf("applied[2].LastVerified = %q, want empty string for zero time", got[2].LastVerified)
	}
}

// TestCollectAppliedRules_EmptyRules pins the empty-rule-set
// behaviour the JSON output depends on: an empty rule slice returns a
// non-nil empty slice so the encoded JSON is `[]` rather than `null`.
func TestCollectAppliedRules_EmptyRules(t *testing.T) {
	got := formatAppliedRules(nil)
	if got == nil {
		t.Fatal("formatAppliedRules(nil) returned nil; want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("formatAppliedRules(nil) = %+v, want empty", got)
	}
}
