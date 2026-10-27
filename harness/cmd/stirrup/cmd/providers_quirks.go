package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// quirksStaleness mirrors the 180-day window used by
// quirks_test.go::TestRuleStaleness. The CLI surfaces it in the
// `stale` boolean on each contributing rule so operators reading the
// JSON output can spot a rule overdue for re-verification without
// computing dates themselves.
const quirksStaleness = 180 * 24 * time.Hour

// providersCmd is the root of the `stirrup providers …` subcommand
// family. The subcommands inspect provider-adapter configuration
// without booting the agentic loop.
var providersCmd = &cobra.Command{
	Use:   "providers",
	Short: "Inspect provider-adapter configuration",
	Long: `Inspect provider-adapter configuration without running a turn.

Subcommands:
  quirks  Resolve the per-(provider, model) quirks registry and print
          the result as JSON. Side-effect-free; safe to invoke from
          shell completion or CI checks.`,
}

// providersQuirksCmd is the `stirrup providers quirks` introspection
// surface for the quirks registry. It prints the resolved
// ProviderQuirks as JSON, along with the Description, LastVerified,
// and staleness flag of every contributing rule, so operators can
// debug a divergence between what they expected the adapter to send
// and what a rule actually applied.
var providersQuirksCmd = &cobra.Command{
	Use:   "quirks",
	Short: "Print the resolved provider quirks for a (provider, model) pair",
	Long: `Resolve the built-in provider quirks registry for the supplied
(--provider, --model) pair and emit the result as JSON. The output
carries the merged ProviderQuirks value plus a list of every
contributing rule (description, last-verified date, staleness flag).

For an openai-compatible or openai-responses provider, --base-url and
--endpoint-profile select the endpoint profile the way a run does: a
declared --endpoint-profile wins, otherwise the profile is derived from
--base-url (openai for OpenAI's own API or no --base-url, generic for
any other URL).

The empty-registry case (no rule matched) is not an error: the output
carries an empty appliedRules list and a ProviderQuirks value at its
zero-after-init shape.`,
	Args: cobra.NoArgs,
	RunE: runProvidersQuirks,
}

func init() {
	rootCmd.AddCommand(providersCmd)
	providersCmd.AddCommand(providersQuirksCmd)
	addProvidersQuirksFlags(providersQuirksCmd)
}

// addProvidersQuirksFlags registers the providers-quirks flag surface on
// cmd.
func addProvidersQuirksFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("provider", "", "Provider type to resolve (e.g. openai-compatible, gemini, anthropic).")
	f.String("model", "", "Model identifier to resolve (e.g. gpt-5-nano, gemini-3.1-pro-preview).")
	f.String("base-url", "", "Base URL of an openai-compatible or openai-responses provider; derives the endpoint profile as a run does when --endpoint-profile is unset.")
	f.String("endpoint-profile", "", "Endpoint profile of an openai-compatible or openai-responses provider (openai, azure-openai, bedrock-mantle, generic). Empty derives it from --base-url.")
	_ = cmd.RegisterFlagCompletionFunc("endpoint-profile", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return types.ValidEndpointProfileValues(), cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.MarkFlagRequired("provider")
	_ = cmd.MarkFlagRequired("model")
}

// quirksCLIOutput is the wire shape printed by `stirrup providers
// quirks`. JSON tags are stable: scripts consuming this output should
// be able to grep on the field names.
type quirksCLIOutput struct {
	Provider        string                 `json:"provider"`
	Model           string                 `json:"model"`
	EndpointProfile string                 `json:"endpointProfile"`
	Quirks          quirks.ProviderQuirks  `json:"quirks"`
	AppliedRules    []appliedRuleCLIOutput `json:"appliedRules"`
}

// appliedRuleCLIOutput summarises one contributing rule. Stale rules
// are flagged so operators can prioritise re-verification work
// without computing date arithmetic.
type appliedRuleCLIOutput struct {
	Description  string `json:"description"`
	LastVerified string `json:"lastVerified"`
	Stale        bool   `json:"stale"`
}

func runProvidersQuirks(cmd *cobra.Command, _ []string) error {
	return runProvidersQuirksWithIO(cmd, os.Stdout)
}

// runProvidersQuirksWithIO is the testable seam: it accepts an
// explicit writer so unit tests can capture the JSON output without
// touching the process-global stdout. Required flag enforcement runs
// before this entry point (via MarkFlagRequired in init).
func runProvidersQuirksWithIO(cmd *cobra.Command, stdout io.Writer) error {
	providerType, _ := cmd.Flags().GetString("provider")
	model, _ := cmd.Flags().GetString("model")
	declared, _ := cmd.Flags().GetString("endpoint-profile")
	baseURL, _ := cmd.Flags().GetString("base-url")

	if baseURL != "" && !types.IsOpenAIWireProviderType(providerType) {
		return fmt.Errorf("--base-url only applies to openai-compatible and openai-responses providers, got %q", providerType)
	}
	if declared != "" {
		if !slices.Contains(types.ValidEndpointProfileValues(), declared) {
			return fmt.Errorf("--endpoint-profile %q is not a recognised endpoint profile; legal values: %s",
				declared, strings.Join(types.ValidEndpointProfileValues(), ", "))
		}
		if !types.IsOpenAIWireProviderType(providerType) {
			return fmt.Errorf("--endpoint-profile only applies to openai-compatible and openai-responses providers, got %q", providerType)
		}
	}
	endpoint := provider.EndpointProfileFor(types.ProviderConfig{Type: providerType, BaseURL: baseURL, EndpointProfile: declared})

	reg := quirks.DefaultRegistry()
	resolved, applied := reg.ResolveWithRules(quirks.Target{ProviderType: providerType, Endpoint: endpoint, Model: model})

	out := quirksCLIOutput{
		Provider:        providerType,
		Model:           model,
		EndpointProfile: endpoint,
		Quirks:          resolved,
		AppliedRules:    formatAppliedRules(applied),
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode quirks output: %w", err)
	}
	return nil
}

// formatAppliedRules projects the rule list returned by
// ResolveWithRules into the CLI-facing summary shape. The registry
// orders the input the same way Resolve applies the rules — glob
// length ascending with declaration order as the tiebreaker — so the
// last entry is the rule whose writes won on overlapping keys.
//
// Returns a non-nil empty slice when no rule matched so the JSON
// output is `[]` rather than `null` — easier to script against.
func formatAppliedRules(rules []quirks.Rule) []appliedRuleCLIOutput {
	out := make([]appliedRuleCLIOutput, 0, len(rules))
	cutoff := time.Now().Add(-quirksStaleness)
	for _, r := range rules {
		lastVerified := ""
		if !r.LastVerified.IsZero() {
			lastVerified = r.LastVerified.Format("2006-01-02")
		}
		out = append(out, appliedRuleCLIOutput{
			Description:  r.Description,
			LastVerified: lastVerified,
			Stale:        !r.LastVerified.IsZero() && r.LastVerified.Before(cutoff),
		})
	}
	return out
}
