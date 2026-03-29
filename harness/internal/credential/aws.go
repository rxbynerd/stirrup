package credential

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// AWSDefaultSource signals that the provider should use the AWS SDK's
// default credential chain (env vars, shared config, instance profile, etc.).
// Resolve returns a Resolved with nil AWSCredentials, which the Bedrock
// adapter interprets as "use LoadDefaultConfig without a custom provider."
type AWSDefaultSource struct{}

func (a *AWSDefaultSource) Resolve(_ context.Context) (*Resolved, error) {
	return &Resolved{}, nil
}

// WebIdentityAWSSource exchanges an OIDC identity token for temporary
// AWS credentials via STS AssumeRoleWithWebIdentity. This enables
// cross-cloud authentication (e.g. GKE Workload Identity → AWS Bedrock).
//
// The actual STS call is deferred — Resolve sets up a credential provider
// that calls STS lazily on first use and automatically refreshes before
// expiry via aws.CredentialsCache.
type WebIdentityAWSSource struct {
	tokenSource TokenSource
	roleARN     string
	sessionName string
	region      string
}

// NewWebIdentityAWSSource creates a credential source that exchanges OIDC
// tokens for AWS credentials. region is used for both the STS endpoint and
// the resulting Bedrock client.
func NewWebIdentityAWSSource(ts TokenSource, region, roleARN, sessionName string) *WebIdentityAWSSource {
	if sessionName == "" {
		sessionName = "stirrup"
	}
	return &WebIdentityAWSSource{
		tokenSource: ts,
		roleARN:     roleARN,
		sessionName: sessionName,
		region:      region,
	}
}

func (w *WebIdentityAWSSource) Resolve(_ context.Context) (*Resolved, error) {
	// Create an STS client for the target region. AssumeRoleWithWebIdentity
	// does not require pre-existing AWS credentials — the web identity token
	// is the authentication — so no credentials are configured on this client.
	stsClient := sts.New(sts.Options{
		Region: w.region,
	})

	// tokenSourceAdapter bridges our context-aware TokenSource to the SDK's
	// IdentityTokenRetriever (which lacks context). See adapter comment below.
	adapter := &tokenSourceAdapter{ts: w.tokenSource}

	roleProvider := stscreds.NewWebIdentityRoleProvider(
		stsClient,
		w.roleARN,
		adapter,
		func(o *stscreds.WebIdentityRoleOptions) {
			o.RoleSessionName = w.sessionName
		},
	)

	// Wrap in CredentialsCache for automatic refresh before expiry.
	cached := aws.NewCredentialsCache(roleProvider)

	return &Resolved{AWSCredentials: cached}, nil
}

// tokenSourceAdapter bridges TokenSource to stscreds.IdentityTokenRetriever.
//
// The AWS SDK's IdentityTokenRetriever.GetIdentityToken() does not accept
// a context.Context. This adapter calls the underlying TokenSource with
// context.Background(). This is acceptable because:
//   - Token sources (GKE metadata, file reads) are fast operations (~10ms)
//   - The adapter is long-lived (created once at factory time)
//   - The SDK calls GetIdentityToken() inside Retrieve(ctx), but does not
//     forward that context to the retriever interface
type tokenSourceAdapter struct {
	ts TokenSource
}

func (a *tokenSourceAdapter) GetIdentityToken() ([]byte, error) {
	token, err := a.ts.Token(context.Background())
	if err != nil {
		return nil, fmt.Errorf("fetch identity token: %w", err)
	}
	return token, nil
}
