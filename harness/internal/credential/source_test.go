package credential

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// mockSecretStore implements security.SecretStore for testing. It tracks
// the number of Resolve calls so closure-caching tests can assert that the
// SecretStore is hit exactly once per Source.Resolve regardless of how many
// times the resulting BearerToken closure is invoked.
type mockSecretStore struct {
	values map[string]string
	err    error
	calls  int
}

func (m *mockSecretStore) Resolve(_ context.Context, ref string) (string, error) {
	m.calls++
	if m.err != nil {
		return "", m.err
	}
	val, ok := m.values[ref]
	if !ok {
		return "", fmt.Errorf("secret not found: %s", ref)
	}
	return val, nil
}

// Verify interface compliance at compile time.
var _ security.SecretStore = (*mockSecretStore)(nil)

func TestStaticSource_Resolve(t *testing.T) {
	secrets := &mockSecretStore{values: map[string]string{
		"secret://MY_KEY": "sk-test-123",
	}}
	src := &StaticSource{secrets: secrets, ref: "secret://MY_KEY"}

	cred, err := src.Resolve(context.Background())
	require.NoError(t, err)
	require.NotNil(t, cred.BearerToken, "BearerToken closure should be non-nil for static source")

	tok, err := cred.BearerToken(context.Background())
	require.NoError(t, err)
	require.Equal(t, "sk-test-123", tok)
	require.Nil(t, cred.AWSCredentials, "AWSCredentials should be nil for static source")
}

// TestStaticSource_ClosureUsesCachedValue verifies that the BearerToken
// closure does not re-resolve the SecretStore on every call: a static
// secret is fetched once at Source.Resolve time and reused thereafter.
// Adapters call the closure on every provider request, so this avoids a
// SecretStore round-trip per request.
func TestStaticSource_ClosureUsesCachedValue(t *testing.T) {
	secrets := &mockSecretStore{values: map[string]string{
		"secret://MY_KEY": "sk-test-123",
	}}
	src := &StaticSource{secrets: secrets, ref: "secret://MY_KEY"}

	cred, err := src.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, secrets.calls, "after Resolve")
	require.NotNil(t, cred.BearerToken)

	for i := 0; i < 10; i++ {
		tok, err := cred.BearerToken(context.Background())
		require.NoError(t, err, "closure call %d", i)
		require.Equal(t, "sk-test-123", tok, "closure call %d", i)
	}

	require.Equal(t, 1, secrets.calls, "after 10 closure calls; closure must reuse cached value")
}

func TestStaticSource_ResolveError(t *testing.T) {
	secrets := &mockSecretStore{err: fmt.Errorf("vault unreachable")}
	src := &StaticSource{secrets: secrets, ref: "secret://MISSING"}

	_, err := src.Resolve(context.Background())
	require.Error(t, err)
}

func TestAWSDefaultSource_Resolve(t *testing.T) {
	src := &AWSDefaultSource{}

	cred, err := src.Resolve(context.Background())
	require.NoError(t, err)
	require.Nil(t, cred.AWSCredentials, "nil AWSCredentials signals use of the SDK credential chain")
	require.Nil(t, cred.BearerToken, "BearerToken closure should be nil for AWS default source")
}

func TestBuildSource_InfersBedrock(t *testing.T) {
	cfg := types.ProviderConfig{Type: "bedrock"}

	src, err := BuildSource(cfg, nil)
	require.NoError(t, err)
	require.IsType(t, (*AWSDefaultSource)(nil), src)
}

func TestBuildSource_InfersStatic(t *testing.T) {
	secrets := &mockSecretStore{values: map[string]string{
		"secret://KEY": "value",
	}}
	cfg := types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://KEY"}

	src, err := BuildSource(cfg, secrets)
	require.NoError(t, err)
	require.IsType(t, (*StaticSource)(nil), src)
}

func TestBuildSource_ExplicitStatic(t *testing.T) {
	secrets := &mockSecretStore{values: map[string]string{
		"secret://KEY": "value",
	}}
	cfg := types.ProviderConfig{
		Type:      "anthropic",
		APIKeyRef: "secret://KEY",
		Credential: &types.CredentialConfig{
			Type: "static",
		},
	}

	src, err := BuildSource(cfg, secrets)
	require.NoError(t, err)
	require.IsType(t, (*StaticSource)(nil), src)
}

func TestBuildSource_ExplicitAWSDefault(t *testing.T) {
	cfg := types.ProviderConfig{
		Type: "bedrock",
		Credential: &types.CredentialConfig{
			Type: "aws-default",
		},
	}

	src, err := BuildSource(cfg, nil)
	require.NoError(t, err)
	require.IsType(t, (*AWSDefaultSource)(nil), src)
}

func TestBuildSource_WebIdentity(t *testing.T) {
	cfg := types.ProviderConfig{
		Type:   "bedrock",
		Region: "us-east-1",
		Credential: &types.CredentialConfig{
			Type:    "web-identity",
			RoleARN: "arn:aws:iam::123456789012:role/test",
			TokenSource: &types.TokenSourceConfig{
				Type:   "env",
				EnvVar: "TEST_TOKEN",
			},
		},
	}

	src, err := BuildSource(cfg, nil)
	require.NoError(t, err)
	require.IsType(t, (*WebIdentityAWSSource)(nil), src)
}

// TestBuildSource_AzureWorkloadIdentity_BadTokenSourceType verifies that
// an unsupported inner TokenSource type wraps the BuildTokenSource
// failure with "build token source" rather than silently falling
// through to a malformed or nil source.
func TestBuildSource_AzureWorkloadIdentity_BadTokenSourceType(t *testing.T) {
	cfg := types.ProviderConfig{
		Type: "openai-compatible",
		Credential: &types.CredentialConfig{
			Type:          "azure-workload-identity",
			AzureTenantID: "11111111-2222-3333-4444-555555555555",
			AzureClientID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			TokenSource: &types.TokenSourceConfig{
				Type: "unsupported-type",
			},
		},
	}

	_, err := BuildSource(cfg, nil)
	require.ErrorContains(t, err, "build token source")
}

func TestBuildSource_UnsupportedType(t *testing.T) {
	cfg := types.ProviderConfig{
		Type: "anthropic",
		Credential: &types.CredentialConfig{
			Type: "kerberos",
		},
	}

	_, err := BuildSource(cfg, nil)
	require.Error(t, err)
}

func TestBuildTokenSource_GKEMetadata(t *testing.T) {
	ts, err := BuildTokenSource(&types.TokenSourceConfig{
		Type:     "gke-metadata",
		Audience: "sts.amazonaws.com",
	})
	require.NoError(t, err)
	require.IsType(t, (*GKEMetadataTokenSource)(nil), ts)
}

func TestBuildTokenSource_File(t *testing.T) {
	ts, err := BuildTokenSource(&types.TokenSourceConfig{
		Type: "file",
		Path: "/tmp/token",
	})
	require.NoError(t, err)
	require.IsType(t, (*FileTokenSource)(nil), ts)
}

func TestBuildTokenSource_Env(t *testing.T) {
	ts, err := BuildTokenSource(&types.TokenSourceConfig{
		Type:   "env",
		EnvVar: "TOKEN",
	})
	require.NoError(t, err)
	require.IsType(t, (*EnvTokenSource)(nil), ts)
}

func TestBuildTokenSource_NilConfigErrors(t *testing.T) {
	_, err := BuildTokenSource(nil)
	require.Error(t, err)
}

func TestBuildTokenSource_UnsupportedType(t *testing.T) {
	_, err := BuildTokenSource(&types.TokenSourceConfig{Type: "unknown-source"})
	require.Error(t, err)
}

func TestBuildTokenSource_AWSIRSA(t *testing.T) {
	ts, err := BuildTokenSource(&types.TokenSourceConfig{Type: "aws-irsa"})
	require.NoError(t, err)
	require.IsType(t, (*AWSIRSATokenSource)(nil), ts)
}

func TestBuildTokenSource_AzureIMDS(t *testing.T) {
	ts, err := BuildTokenSource(&types.TokenSourceConfig{
		Type:     "azure-imds",
		Resource: "https://management.azure.com/",
	})
	require.NoError(t, err)
	require.IsType(t, (*AzureIMDSTokenSource)(nil), ts)
}

func TestBuildTokenSource_AzureIMDS_MissingResource(t *testing.T) {
	_, err := BuildTokenSource(&types.TokenSourceConfig{Type: "azure-imds"})
	require.Error(t, err)
}

// TestBuildTokenSource_GitHubActionsOIDC needs the env var set before
// BuildTokenSource runs: the constructor reads the issuance URL once and
// keeps it for the source's lifetime.
func TestBuildTokenSource_GitHubActionsOIDC(t *testing.T) {
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "https://token.actions.githubusercontent.com/?api-version=2.0")

	ts, err := BuildTokenSource(&types.TokenSourceConfig{
		Type:     "github-actions-oidc",
		Audience: "sts.amazonaws.com",
	})
	require.NoError(t, err)
	require.IsType(t, (*GitHubActionsOIDCTokenSource)(nil), ts)
}

func TestBuildTokenSource_GitHubActionsOIDC_MissingURLEnv(t *testing.T) {
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")

	_, err := BuildTokenSource(&types.TokenSourceConfig{
		Type:     "github-actions-oidc",
		Audience: "sts.amazonaws.com",
	})
	require.ErrorContains(t, err, "ACTIONS_ID_TOKEN_REQUEST_URL")
}

func TestBuildTokenSource_GitHubActionsOIDC_MissingAudience(t *testing.T) {
	_, err := BuildTokenSource(&types.TokenSourceConfig{Type: "github-actions-oidc"})
	require.Error(t, err)
}

func TestBuildSource_GCPWorkloadIdentityFederation(t *testing.T) {
	cfg := types.ProviderConfig{
		Type: "gemini",
		Credential: &types.CredentialConfig{
			Type:           "gcp-workload-identity-federation",
			Audience:       "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/aws-pool/providers/aws-provider",
			ServiceAccount: "vertex@my-project.iam.gserviceaccount.com",
			TokenSource: &types.TokenSourceConfig{
				Type: "aws-irsa",
			},
		},
	}

	src, err := BuildSource(cfg, nil)
	require.NoError(t, err)
	wif, ok := src.(*GCPWorkloadIdentityFederationSource)
	require.True(t, ok, "expected *GCPWorkloadIdentityFederationSource, got %T", src)

	assert.Equal(t, cfg.Credential.Audience, wif.audience)
	assert.Equal(t, cfg.Credential.ServiceAccount, wif.serviceAccount)
	assert.IsType(t, (*AWSIRSATokenSource)(nil), wif.tokenSource, "wrapped token source")
}

func TestBuildSource_GCPWorkloadIdentityFederation_MissingAudience(t *testing.T) {
	cfg := types.ProviderConfig{
		Type: "gemini",
		Credential: &types.CredentialConfig{
			Type: "gcp-workload-identity-federation",
			TokenSource: &types.TokenSourceConfig{
				Type: "aws-irsa",
			},
		},
	}

	_, err := BuildSource(cfg, nil)
	require.Error(t, err)
}

func TestBuildSource_GCPWorkloadIdentityFederation_MissingTokenSource(t *testing.T) {
	cfg := types.ProviderConfig{
		Type: "gemini",
		Credential: &types.CredentialConfig{
			Type:     "gcp-workload-identity-federation",
			Audience: "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/q",
		},
	}

	_, err := BuildSource(cfg, nil)
	require.Error(t, err)
}

// TestBuildSource_AzureWorkloadIdentity verifies that the dispatch
// constructs an *AzureWorkloadIdentitySource with the configured tenant,
// client, scope, and token source. Tenant/client UUIDs and scope are
// validated upstream (in types.ValidateRunConfig), so the dispatch
// itself only needs to wire fields.
func TestBuildSource_AzureWorkloadIdentity(t *testing.T) {
	cfg := types.ProviderConfig{
		Type: "openai-compatible",
		Credential: &types.CredentialConfig{
			Type:          "azure-workload-identity",
			AzureTenantID: "11111111-1111-1111-1111-111111111111",
			AzureClientID: "22222222-2222-2222-2222-222222222222",
			AzureScope:    "https://cognitiveservices.azure.com/.default",
			TokenSource: &types.TokenSourceConfig{
				Type: "file",
				Path: "/var/run/secrets/azure/tokens/azure-identity-token",
			},
		},
	}

	src, err := BuildSource(cfg, nil)
	require.NoError(t, err)
	azure, ok := src.(*AzureWorkloadIdentitySource)
	require.True(t, ok, "expected *AzureWorkloadIdentitySource, got %T", src)

	assert.Equal(t, cfg.Credential.AzureTenantID, azure.tenantID)
	assert.Equal(t, cfg.Credential.AzureClientID, azure.clientID)
	assert.Equal(t, cfg.Credential.AzureScope, azure.scope)
	assert.IsType(t, (*FileTokenSource)(nil), azure.tokenSource, "wrapped token source")
}

// TestBuildSource_AzureWorkloadIdentity_DefaultsScope verifies that an
// empty AzureScope yields the Cognitive Services default scope.
func TestBuildSource_AzureWorkloadIdentity_DefaultsScope(t *testing.T) {
	cfg := types.ProviderConfig{
		Type: "openai-responses",
		Credential: &types.CredentialConfig{
			Type:          "azure-workload-identity",
			AzureTenantID: "11111111-1111-1111-1111-111111111111",
			AzureClientID: "22222222-2222-2222-2222-222222222222",
			TokenSource: &types.TokenSourceConfig{
				Type: "file",
				Path: "/var/run/secrets/azure/tokens/azure-identity-token",
			},
		},
	}

	src, err := BuildSource(cfg, nil)
	require.NoError(t, err)
	azure, ok := src.(*AzureWorkloadIdentitySource)
	require.True(t, ok, "expected *AzureWorkloadIdentitySource, got %T", src)
	require.Equal(t, "https://cognitiveservices.azure.com/.default", azure.scope)
}

func TestBuildSource_AzureWorkloadIdentity_MissingTenant(t *testing.T) {
	cfg := types.ProviderConfig{
		Type: "openai-compatible",
		Credential: &types.CredentialConfig{
			Type:          "azure-workload-identity",
			AzureClientID: "22222222-2222-2222-2222-222222222222",
			TokenSource: &types.TokenSourceConfig{
				Type: "file",
				Path: "/var/run/secrets/azure/tokens/azure-identity-token",
			},
		},
	}

	_, err := BuildSource(cfg, nil)
	require.ErrorContains(t, err, "azureTenantId")
}

func TestBuildSource_AzureWorkloadIdentity_MissingClient(t *testing.T) {
	cfg := types.ProviderConfig{
		Type: "openai-compatible",
		Credential: &types.CredentialConfig{
			Type:          "azure-workload-identity",
			AzureTenantID: "11111111-1111-1111-1111-111111111111",
			TokenSource: &types.TokenSourceConfig{
				Type: "file",
				Path: "/var/run/secrets/azure/tokens/azure-identity-token",
			},
		},
	}

	_, err := BuildSource(cfg, nil)
	require.ErrorContains(t, err, "azureClientId")
}

func TestBuildSource_AzureWorkloadIdentity_MissingTokenSource(t *testing.T) {
	cfg := types.ProviderConfig{
		Type: "openai-compatible",
		Credential: &types.CredentialConfig{
			Type:          "azure-workload-identity",
			AzureTenantID: "11111111-1111-1111-1111-111111111111",
			AzureClientID: "22222222-2222-2222-2222-222222222222",
		},
	}

	_, err := BuildSource(cfg, nil)
	require.ErrorContains(t, err, "tokenSource")
}
