// Package credential provides cross-cloud credential federation for provider
// adapters. It bridges the gap between "where am I running" (token sources)
// and "what service am I calling" (credential sources), enabling scenarios
// like GKE Workload Identity → AWS STS → Bedrock.
//
// The package introduces two composable layers:
//
//	TokenSource  →  Source  →  Resolved  →  Provider adapter
//
// TokenSource fetches identity proofs (OIDC/JWT tokens) from the runtime
// environment. Source exchanges those proofs (or resolves static secrets)
// into provider-specific credentials. Both are interface-based and
// constructed from RunConfig by BuildSource.
//
// Bearer credentials are surfaced through Resolved.BearerToken, a closure
// invoked by provider adapters at request time. The closure contract is:
//
//   - Safe for concurrent use: the closure may be called from multiple
//     goroutines (e.g. concurrent Stream calls on the same adapter).
//   - Internally caches and refreshes: static sources capture a resolved
//     value once and return it on every call with no IO; federation sources
//     implement an OAuth2-style cache + refresh internally (typically via
//     oauth2.ReuseTokenSource).
//   - Called per provider request: adapters MUST NOT cache the returned
//     string across requests, because the cache/refresh logic lives behind
//     the closure. A nil BearerToken signals "this source produces no
//     bearer" (e.g. AWSDefaultSource — the AWS SDK consumes AWSCredentials
//     instead).
package credential

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// TokenSource provides identity tokens from the runtime environment.
// Implementations fetch tokens that prove "who am I" and can be exchanged
// for target-service credentials.
type TokenSource interface {
	Token(ctx context.Context) ([]byte, error)
}

// Source resolves credentials for a provider adapter.
type Source interface {
	Resolve(ctx context.Context) (*Resolved, error)
}

// BearerTokenFunc returns the current bearer credential for a provider
// request. See the package doc comment for the closure contract (concurrency
// safety, internal cache/refresh, per-request invocation).
type BearerTokenFunc = func(ctx context.Context) (string, error)

// Resolved holds authentication material produced by a Source.
type Resolved struct {
	// BearerToken returns the current bearer credential. Static sources
	// return a closure that yields a cached value with no IO. Federation
	// sources implement OAuth2-style cache + refresh internally (e.g. via
	// oauth2.ReuseTokenSource). The closure is called by adapters at
	// request time. Nil means the source produced no bearer (e.g.
	// AWSDefaultSource).
	BearerToken BearerTokenFunc

	// AWSCredentials for AWS-based providers (Bedrock).
	// nil signals "use the SDK default credential chain."
	// When set, the SDK's CredentialsCache handles automatic refresh.
	AWSCredentials aws.CredentialsProvider
}

// BuildSource returns a Source for the given provider config.
// When cfg.Credential is nil, the source type is inferred:
//   - bedrock → AWSDefaultSource (SDK default credential chain)
//   - gemini  → GoogleADCSource (Application Default Credentials)
//   - all others → StaticSource (resolve APIKeyRef via SecretStore)
func BuildSource(cfg types.ProviderConfig, secrets security.SecretStore) (Source, error) {
	if cfg.Credential == nil {
		switch cfg.Type {
		case "bedrock":
			return &AWSDefaultSource{}, nil
		case "gemini":
			return &GoogleADCSource{}, nil
		default:
			return &StaticSource{secrets: secrets, ref: cfg.APIKeyRef}, nil
		}
	}

	switch cfg.Credential.Type {
	case "static":
		return &StaticSource{secrets: secrets, ref: cfg.APIKeyRef}, nil
	case "aws-default":
		return &AWSDefaultSource{}, nil
	case "web-identity":
		ts, err := BuildTokenSource(cfg.Credential.TokenSource)
		if err != nil {
			return nil, fmt.Errorf("build token source: %w", err)
		}
		return NewWebIdentityAWSSource(ts, cfg.Region, cfg.Credential.RoleARN, cfg.Credential.SessionName), nil
	case "gcp-default":
		return &GoogleADCSource{}, nil
	case "gcp-service-account":
		return NewServiceAccountKeySource(cfg.GCPCredentialsFile), nil
	case "gcp-workload-identity":
		return NewGoogleWorkloadIdentitySource(), nil
	case "gcp-workload-identity-federation":
		if cfg.Credential.Audience == "" {
			return nil, fmt.Errorf("gcp-workload-identity-federation requires audience")
		}
		if cfg.Credential.TokenSource == nil {
			return nil, fmt.Errorf("gcp-workload-identity-federation requires tokenSource")
		}
		ts, err := BuildTokenSource(cfg.Credential.TokenSource)
		if err != nil {
			return nil, fmt.Errorf("build token source: %w", err)
		}
		return NewGCPWorkloadIdentityFederationSource(
			ts,
			cfg.Credential.Audience,
			cfg.Credential.ServiceAccount,
		), nil
	case "anthropic-wif":
		// Redundant with types.validateCredentialConfig; keeps BuildSource
		// self-contained for callers that bypass full RunConfig validation.
		if cfg.Credential.FederationRuleID == "" || cfg.Credential.OrganizationID == "" || cfg.Credential.ServiceAccountID == "" {
			return nil, fmt.Errorf("anthropic-wif requires federationRuleId, organizationId, and serviceAccountId")
		}
		if cfg.Credential.TokenSource == nil {
			return nil, fmt.Errorf("anthropic-wif requires tokenSource")
		}
		ts, err := BuildTokenSource(cfg.Credential.TokenSource)
		if err != nil {
			return nil, fmt.Errorf("build token source: %w", err)
		}
		return NewAnthropicWIFSource(
			ts,
			cfg.Credential.FederationRuleID,
			cfg.Credential.OrganizationID,
			cfg.Credential.ServiceAccountID,
			cfg.Credential.WorkspaceID,
		), nil
	case "openai-wif":
		// Redundant with types.validateCredentialConfig; keeps BuildSource
		// self-contained for callers that bypass full RunConfig validation.
		if cfg.Credential.OpenAIIdentityProviderID == "" || cfg.Credential.OpenAIServiceAccountID == "" {
			return nil, fmt.Errorf("openai-wif requires openaiIdentityProviderId and openaiServiceAccountId")
		}
		if cfg.Credential.TokenSource == nil {
			return nil, fmt.Errorf("openai-wif requires tokenSource")
		}
		ts, err := BuildTokenSource(cfg.Credential.TokenSource)
		if err != nil {
			return nil, fmt.Errorf("build token source: %w", err)
		}
		return NewOpenAIWIFSource(
			ts,
			cfg.Credential.OpenAIIdentityProviderID,
			cfg.Credential.OpenAIServiceAccountID,
			cfg.Credential.OpenAISubjectTokenType,
		), nil
	case "azure-workload-identity":
		// Redundant with validateRunConfig; keeps BuildSource
		// self-contained for callers that bypass full RunConfig validation.
		if cfg.Credential.AzureTenantID == "" {
			return nil, fmt.Errorf("azure-workload-identity requires azureTenantId")
		}
		if cfg.Credential.AzureClientID == "" {
			return nil, fmt.Errorf("azure-workload-identity requires azureClientId")
		}
		if cfg.Credential.TokenSource == nil {
			return nil, fmt.Errorf("azure-workload-identity requires tokenSource")
		}
		ts, err := BuildTokenSource(cfg.Credential.TokenSource)
		if err != nil {
			return nil, fmt.Errorf("build token source: %w", err)
		}
		return NewAzureWorkloadIdentitySource(
			ts,
			cfg.Credential.AzureTenantID,
			cfg.Credential.AzureClientID,
			cfg.Credential.AzureScope,
			cfg.Credential.AzureTokenURL,
		), nil
	default:
		return nil, fmt.Errorf("unsupported credential type: %q", cfg.Credential.Type)
	}
}

// BuildTokenSource constructs a TokenSource from config.
func BuildTokenSource(cfg *types.TokenSourceConfig) (TokenSource, error) {
	if cfg == nil {
		return nil, fmt.Errorf("tokenSource config is required")
	}
	switch cfg.Type {
	case "gke-metadata":
		return NewGKEMetadataTokenSource(cfg.Audience, ""), nil
	case "file":
		return &FileTokenSource{path: cfg.Path}, nil
	case "env":
		return &EnvTokenSource{envVar: cfg.EnvVar}, nil
	case "aws-irsa":
		return &AWSIRSATokenSource{}, nil
	case "azure-imds":
		if cfg.Resource == "" {
			return nil, fmt.Errorf("azure-imds requires resource")
		}
		return NewAzureIMDSTokenSource(cfg.Resource, cfg.ClientID, ""), nil
	case "github-actions-oidc":
		if cfg.Audience == "" {
			return nil, fmt.Errorf("github-actions-oidc requires audience")
		}
		return NewGitHubActionsOIDCTokenSource(cfg.Audience)
	default:
		return nil, fmt.Errorf("unsupported token source type: %q", cfg.Type)
	}
}
