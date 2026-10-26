package cmd

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
)

// newTestProvidersQuirksCommand returns a fresh cobra command preloaded
// with the providers-quirks flag surface, scoped per test so
// flag-changed state does not leak between tests.
func newTestProvidersQuirksCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "quirks"}
	cmd.Flags().String("provider", "", "")
	cmd.Flags().String("model", "", "")
	cmd.Flags().String("endpoint-profile", "", "")
	return cmd
}

// TestProvidersQuirks_EmptyRegistryEmitsValidJSON pins the
// no-rules-matched state: the subcommand must still produce valid JSON
// with an empty appliedRules slice (not null). Resolves an unknown
// provider type since every first-party provider now carries a base
// rule matching every model.
func TestProvidersQuirks_EmptyRegistryEmitsValidJSON(t *testing.T) {
	must := require.New(t)
	cmd := newTestProvidersQuirksCommand()
	must.NoError(cmd.ParseFlags([]string{"--provider", "no-such-provider", "--model", "gpt-4o"}))
	var buf bytes.Buffer
	must.NoError(runProvidersQuirksWithIO(cmd, &buf))

	var got quirksCLIOutput
	must.NoError(json.Unmarshal(buf.Bytes(), &got), "output is not valid JSON:\n%s", buf.String())

	must.Equal("no-such-provider", got.Provider)
	must.Equal("gpt-4o", got.Model)
	must.NotNil(got.AppliedRules, "appliedRules must be an empty array, not null")
	must.Empty(got.AppliedRules)

	// Scripts that read the output without unmarshalling depend on the literal shape.
	must.Contains(buf.String(), `"appliedRules": []`)
}

// TestProvidersQuirks_MissingRequiredFlagIsError pins the MarkFlagRequired
// wiring on the registered cobra command: missing --provider or --model
// must produce an error naming the missing flag.
func TestProvidersQuirks_MissingRequiredFlagIsError(t *testing.T) {
	for _, name := range []string{"provider", "model"} {
		t.Run("missing "+name, func(t *testing.T) {
			must := require.New(t)
			// cobra surfaces MarkFlagRequired through Execute, not
			// ParseFlags, so drive a fresh root command instead of the
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
			must.ErrorContains(root.Execute(), name)
		})
	}
}

// TestProvidersQuirks_OutputHasTopLevelKeys asserts the emitted document
// decodes against a generic map, not just the typed struct — catches a
// field with no JSON tag producing unexpected casing.
func TestProvidersQuirks_OutputHasTopLevelKeys(t *testing.T) {
	must := require.New(t)
	cmd := newTestProvidersQuirksCommand()
	must.NoError(cmd.ParseFlags([]string{"--provider", "gemini", "--model", "gemini-3.1-pro-preview"}))
	var buf bytes.Buffer
	must.NoError(runProvidersQuirksWithIO(cmd, &buf))

	var raw map[string]any
	must.NoError(json.Unmarshal(buf.Bytes(), &raw), "output is not valid JSON:\n%s", buf.String())
	for _, key := range []string{"provider", "model", "endpointProfile", "quirks", "appliedRules"} {
		must.Contains(raw, key, "output:\n%s", buf.String())
	}
}

func TestProvidersQuirks_EndpointProfileReportsEffectiveProfile(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"responses default", []string{"--provider", "openai-responses", "--model", "gpt-5.4"}, "openai"},
		{"responses declared generic", []string{"--provider", "openai-responses", "--model", "gpt-5.4", "--endpoint-profile", "generic"}, "generic"},
		{"compatible declared azure", []string{"--provider", "openai-compatible", "--model", "gpt-4o", "--endpoint-profile", "azure-openai"}, "azure-openai"},
		{"anthropic has none", []string{"--provider", "anthropic", "--model", "claude-sonnet-5-5"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cmd := newTestProvidersQuirksCommand()
			must.NoError(cmd.ParseFlags(tc.args))
			var buf bytes.Buffer
			must.NoError(runProvidersQuirksWithIO(cmd, &buf))

			var got quirksCLIOutput
			must.NoError(json.Unmarshal(buf.Bytes(), &got), "output:\n%s", buf.String())
			must.Equal(tc.want, got.EndpointProfile)
		})
	}
}

func TestProvidersQuirks_EndpointProfileRejectsInvalidUse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"unknown profile", []string{"--provider", "openai-responses", "--model", "gpt-5.4", "--endpoint-profile", "azure"}, `--endpoint-profile "azure" is not a recognised endpoint profile`},
		{"non openai wire type", []string{"--provider", "anthropic", "--model", "claude-sonnet-5-5", "--endpoint-profile", "openai"}, "--endpoint-profile only applies to openai-compatible and openai-responses providers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cmd := newTestProvidersQuirksCommand()
			must.NoError(cmd.ParseFlags(tc.args))
			var buf bytes.Buffer
			must.ErrorContains(runProvidersQuirksWithIO(cmd, &buf), tc.wantErr)
			must.Empty(buf.String())
		})
	}
}

// TestCollectAppliedRules_FiltersAndFlagsCorrectly exercises every
// branch of collectAppliedRules using a hand-constructed rule slice.
//
// Covers:
//   - matching rule (ProviderType + ModelMatch glob both match)
//   - non-matching rule (different ProviderType)
//   - stale-flag branch (LastVerified > 180 days ago)
//   - zero-LastVerified branch (empty lastVerified string; Stale=false)
//   - Apply == nil guard (skipped; mirrors Resolve)
//   - specificity ordering (longer ModelMatch glob runs last and wins)
func TestCollectAppliedRules_FiltersAndFlagsCorrectly(t *testing.T) {
	must := require.New(t)
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
	_, applied := reg.ResolveWithRules("openai-compatible", "", "gpt-4o")
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
	must.Len(got, len(wantDescriptions), "applied rules: %+v", got)
	for i, want := range wantDescriptions {
		must.Equal(want, got[i].Description, "applied[%d]", i)
	}

	// Stale flag and lastVerified rendering by index in the sorted output.
	must.False(got[0].Stale, "applied[0] (fresh wildcard) must not be stale")
	must.Equal(fresh.Format("2006-01-02"), got[0].LastVerified)
	must.True(got[1].Stale, "applied[1] (stale gpt-4 rule) must be stale")
	must.Equal(stale.Format("2006-01-02"), got[1].LastVerified)
	must.False(got[2].Stale, "applied[2] (zero-LastVerified) must not be stale: zero time is not flagged")
	must.Empty(got[2].LastVerified, "a zero LastVerified renders as an empty string")
}

// TestCollectAppliedRules_EmptyInputReturnsEmptySlice pins that an empty rule
// slice returns a non-nil empty slice so the encoded JSON is `[]`, not `null`.
func TestCollectAppliedRules_EmptyInputReturnsEmptySlice(t *testing.T) {
	must := require.New(t)
	got := formatAppliedRules(nil)
	must.NotNil(got, "formatAppliedRules(nil) must return a non-nil slice so JSON encodes []")
	must.Empty(got)
}
