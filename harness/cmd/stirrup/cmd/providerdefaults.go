package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/stirrup/types"
)

const (
	defaultAnthropicModel  = "claude-sonnet-4-6"
	defaultAnthropicKeyRef = "secret://ANTHROPIC_API_KEY"
	defaultOpenAIKeyRef    = "secret://OPENAI_API_KEY"
	// defaultReplayModel labels a replayed run whose recording decides
	// every turn; no model is called, so the name is a label wherever
	// it is read.
	defaultReplayModel = "replay"
)

// applyProviderDefaults fills the model and API key reference that the
// resolved provider implies, after the base config and the flag
// overrides have been merged, so a value from --config or an explicit
// flag always wins. The provider is read from cfg, which makes a
// provider named in a --config file drive the defaults the same way
// --provider does.
//
// The key reference is defaulted only when no credential block supplies
// authentication, and an explicit --api-key-ref (including an empty
// one) is never replaced. An openai-* provider that a file or stdin
// document points at a baseUrl is a keyless gateway or local server
// unless the document names a key, so it gets no default reference.
//
// The model is defaulted for the anthropic provider and for replay,
// where no model is called; every other provider has no safe default,
// so a static router without a model is an error that names the
// provider and the flag. Non-static routers carry their own model fields
// and are left to ValidateRunConfig.
func applyProviderDefaults(cmd *cobra.Command, cfg *types.RunConfig, fromDocument bool) error {
	provider := cfg.Provider.Type
	if provider == "" {
		provider = "anthropic"
	}

	if !cmd.Flags().Changed("api-key-ref") && cfg.Provider.APIKeyRef == "" && cfg.Provider.Credential == nil {
		switch provider {
		case "anthropic":
			cfg.Provider.APIKeyRef = defaultAnthropicKeyRef
		case "openai-compatible", "openai-responses":
			if !fromDocument || cfg.Provider.BaseURL == "" {
				cfg.Provider.APIKeyRef = defaultOpenAIKeyRef
			}
		}
	}

	staticRouter := cfg.ModelRouter.Type == "" || cfg.ModelRouter.Type == "static"
	if staticRouter && cfg.ModelRouter.Model == "" {
		switch provider {
		case "anthropic":
			cfg.ModelRouter.Model = defaultAnthropicModel
		case "replay":
			cfg.ModelRouter.Model = defaultReplayModel
		default:
			return validationError(fmt.Errorf(
				"--model is required with --provider=%s: no default model is defined for this provider; "+
					"pass --model <model-id> or set modelRouter.model in the --config file", provider))
		}
	}
	return nil
}
