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

// Resolved holds authentication material produced by a Source.
// Exactly one field is meaningful, determined by the provider type.
type Resolved struct {
	// BearerToken for API-key-based providers (Anthropic, OpenAI).
	// For providers that need continuous token refresh (e.g. Azure AD),
	// the adapter should hold a Source reference and call Resolve()
	// per-request rather than caching this value.
	BearerToken string

	// AWSCredentials for AWS-based providers (Bedrock).
	// nil signals "use the SDK default credential chain."
	// When set, the SDK's CredentialsCache handles automatic refresh.
	AWSCredentials aws.CredentialsProvider
}

// BuildSource returns a Source for the given provider config.
// When cfg.Credential is nil, the source type is inferred:
//   - bedrock → AWSDefaultSource (SDK default credential chain)
//   - all others → StaticSource (resolve APIKeyRef via SecretStore)
func BuildSource(cfg types.ProviderConfig, secrets security.SecretStore) (Source, error) {
	if cfg.Credential == nil {
		switch cfg.Type {
		case "bedrock":
			return &AWSDefaultSource{}, nil
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
	default:
		return nil, fmt.Errorf("unsupported token source type: %q", cfg.Type)
	}
}
