package credential

import (
	"context"
	"fmt"
	"strings"
	"testing"

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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.BearerToken == nil {
		t.Fatal("BearerToken closure should be non-nil for static source")
	}
	tok, err := cred.BearerToken(context.Background())
	if err != nil {
		t.Fatalf("BearerToken closure returned error: %v", err)
	}
	if tok != "sk-test-123" {
		t.Errorf("BearerToken closure value = %q, want %q", tok, "sk-test-123")
	}
	if cred.AWSCredentials != nil {
		t.Error("AWSCredentials should be nil for static source")
	}
}

// TestStaticSource_ClosureUsesCachedValue verifies that the BearerToken
// closure does not re-resolve the SecretStore on every call: a static
// secret is fetched once at Source.Resolve time and reused thereafter.
// This is the critical efficiency property that lets adapters call the
// closure on every provider request without paying for repeated
// SecretStore round-trips.
func TestStaticSource_ClosureUsesCachedValue(t *testing.T) {
	secrets := &mockSecretStore{values: map[string]string{
		"secret://MY_KEY": "sk-test-123",
	}}
	src := &StaticSource{secrets: secrets, ref: "secret://MY_KEY"}

	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if secrets.calls != 1 {
		t.Fatalf("after Resolve, secrets.calls = %d, want 1", secrets.calls)
	}
	if cred.BearerToken == nil {
		t.Fatal("BearerToken closure should be non-nil")
	}

	for i := 0; i < 10; i++ {
		tok, err := cred.BearerToken(context.Background())
		if err != nil {
			t.Fatalf("closure call %d returned error: %v", i, err)
		}
		if tok != "sk-test-123" {
			t.Errorf("closure call %d value = %q, want %q", i, tok, "sk-test-123")
		}
	}

	if secrets.calls != 1 {
		t.Errorf("after 10 closure calls, secrets.calls = %d, want 1 (closure must reuse cached value)", secrets.calls)
	}
}

func TestStaticSource_ResolveError(t *testing.T) {
	secrets := &mockSecretStore{err: fmt.Errorf("vault unreachable")}
	src := &StaticSource{secrets: secrets, ref: "secret://MISSING"}

	_, err := src.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestAWSDefaultSource_Resolve(t *testing.T) {
	src := &AWSDefaultSource{}
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.AWSCredentials != nil {
		t.Error("AWSCredentials should be nil for default source (signals use SDK chain)")
	}
	if cred.BearerToken != nil {
		t.Error("BearerToken closure should be nil for AWS default source")
	}
}

func TestBuildSource_InfersBedrock(t *testing.T) {
	cfg := types.ProviderConfig{Type: "bedrock"}
	src, err := BuildSource(cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := src.(*AWSDefaultSource); !ok {
		t.Errorf("expected *AWSDefaultSource, got %T", src)
	}
}

func TestBuildSource_InfersStatic(t *testing.T) {
	secrets := &mockSecretStore{values: map[string]string{
		"secret://KEY": "value",
	}}
	cfg := types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://KEY"}
	src, err := BuildSource(cfg, secrets)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := src.(*StaticSource); !ok {
		t.Errorf("expected *StaticSource, got %T", src)
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := src.(*StaticSource); !ok {
		t.Errorf("expected *StaticSource, got %T", src)
	}
}

func TestBuildSource_ExplicitAWSDefault(t *testing.T) {
	cfg := types.ProviderConfig{
		Type: "bedrock",
		Credential: &types.CredentialConfig{
			Type: "aws-default",
		},
	}
	src, err := BuildSource(cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := src.(*AWSDefaultSource); !ok {
		t.Errorf("expected *AWSDefaultSource, got %T", src)
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := src.(*WebIdentityAWSSource); !ok {
		t.Errorf("expected *WebIdentityAWSSource, got %T", src)
	}
}

// TestBuildSource_AzureWorkloadIdentityBadTokenSourceType verifies that
// an unsupported inner TokenSource type wraps the BuildTokenSource
// failure with "build token source" rather than silently falling
// through to a malformed or nil source.
func TestBuildSource_AzureWorkloadIdentityBadTokenSourceType(t *testing.T) {
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
	if err == nil {
		t.Fatal("expected error for unsupported inner token source type, got nil")
	}
	if !strings.Contains(err.Error(), "build token source") {
		t.Errorf("error should wrap with 'build token source', got: %v", err)
	}
}

func TestBuildSource_UnsupportedType(t *testing.T) {
	cfg := types.ProviderConfig{
		Type: "anthropic",
		Credential: &types.CredentialConfig{
			Type: "kerberos",
		},
	}
	_, err := BuildSource(cfg, nil)
	if err == nil {
		t.Fatal("expected error for unsupported credential type")
	}
}

func TestBuildTokenSource_GKEMetadata(t *testing.T) {
	ts, err := BuildTokenSource(&types.TokenSourceConfig{
		Type:     "gke-metadata",
		Audience: "sts.amazonaws.com",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := ts.(*GKEMetadataTokenSource); !ok {
		t.Errorf("expected *GKEMetadataTokenSource, got %T", ts)
	}
}

func TestBuildTokenSource_File(t *testing.T) {
	ts, err := BuildTokenSource(&types.TokenSourceConfig{
		Type: "file",
		Path: "/tmp/token",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := ts.(*FileTokenSource); !ok {
		t.Errorf("expected *FileTokenSource, got %T", ts)
	}
}

func TestBuildTokenSource_Env(t *testing.T) {
	ts, err := BuildTokenSource(&types.TokenSourceConfig{
		Type:   "env",
		EnvVar: "TOKEN",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := ts.(*EnvTokenSource); !ok {
		t.Errorf("expected *EnvTokenSource, got %T", ts)
	}
}

func TestBuildTokenSource_Nil(t *testing.T) {
	_, err := BuildTokenSource(nil)
	if err == nil {
		t.Fatal("expected error for nil token source config")
	}
}

func TestBuildTokenSource_UnsupportedType(t *testing.T) {
	_, err := BuildTokenSource(&types.TokenSourceConfig{Type: "unknown-source"})
	if err == nil {
		t.Fatal("expected error for unsupported token source type")
	}
}

func TestBuildTokenSource_AWSIRSA(t *testing.T) {
	ts, err := BuildTokenSource(&types.TokenSourceConfig{Type: "aws-irsa"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := ts.(*AWSIRSATokenSource); !ok {
		t.Errorf("expected *AWSIRSATokenSource, got %T", ts)
	}
}

func TestBuildTokenSource_AzureIMDS(t *testing.T) {
	ts, err := BuildTokenSource(&types.TokenSourceConfig{
		Type:     "azure-imds",
		Resource: "https://management.azure.com/",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := ts.(*AzureIMDSTokenSource); !ok {
		t.Errorf("expected *AzureIMDSTokenSource, got %T", ts)
	}
}

func TestBuildTokenSource_AzureIMDSMissingResource(t *testing.T) {
	_, err := BuildTokenSource(&types.TokenSourceConfig{Type: "azure-imds"})
	if err == nil {
		t.Fatal("expected error for azure-imds without resource")
	}
}

func TestBuildTokenSource_GitHubActionsOIDC(t *testing.T) {
	// The constructor reads the issuance URL once, freezing it across
	// the source's lifetime, so the env var must be set before
	// BuildTokenSource is invoked.
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "https://token.actions.githubusercontent.com/?api-version=2.0")

	ts, err := BuildTokenSource(&types.TokenSourceConfig{
		Type:     "github-actions-oidc",
		Audience: "sts.amazonaws.com",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := ts.(*GitHubActionsOIDCTokenSource); !ok {
		t.Errorf("expected *GitHubActionsOIDCTokenSource, got %T", ts)
	}
}

func TestBuildTokenSource_GitHubActionsOIDCMissingURLEnv(t *testing.T) {
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")

	_, err := BuildTokenSource(&types.TokenSourceConfig{
		Type:     "github-actions-oidc",
		Audience: "sts.amazonaws.com",
	})
	if err == nil {
		t.Fatal("expected error when ACTIONS_ID_TOKEN_REQUEST_URL is unset at build time")
	}
	if !strings.Contains(err.Error(), "ACTIONS_ID_TOKEN_REQUEST_URL") {
		t.Errorf("error should name the missing env var: %v", err)
	}
}

func TestBuildTokenSource_GitHubActionsOIDCMissingAudience(t *testing.T) {
	_, err := BuildTokenSource(&types.TokenSourceConfig{Type: "github-actions-oidc"})
	if err == nil {
		t.Fatal("expected error for github-actions-oidc without audience")
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wif, ok := src.(*GCPWorkloadIdentityFederationSource)
	if !ok {
		t.Fatalf("expected *GCPWorkloadIdentityFederationSource, got %T", src)
	}
	if wif.audience != cfg.Credential.Audience {
		t.Errorf("audience = %q, want %q", wif.audience, cfg.Credential.Audience)
	}
	if wif.serviceAccount != cfg.Credential.ServiceAccount {
		t.Errorf("serviceAccount = %q, want %q", wif.serviceAccount, cfg.Credential.ServiceAccount)
	}
	if _, ok := wif.tokenSource.(*AWSIRSATokenSource); !ok {
		t.Errorf("expected wrapped AWSIRSATokenSource, got %T", wif.tokenSource)
	}
}

func TestBuildSource_GCPWorkloadIdentityFederationMissingAudience(t *testing.T) {
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
	if err == nil {
		t.Fatal("expected error for missing audience")
	}
}

func TestBuildSource_GCPWorkloadIdentityFederationMissingTokenSource(t *testing.T) {
	cfg := types.ProviderConfig{
		Type: "gemini",
		Credential: &types.CredentialConfig{
			Type:     "gcp-workload-identity-federation",
			Audience: "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/q",
		},
	}
	_, err := BuildSource(cfg, nil)
	if err == nil {
		t.Fatal("expected error for missing token source")
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	azure, ok := src.(*AzureWorkloadIdentitySource)
	if !ok {
		t.Fatalf("expected *AzureWorkloadIdentitySource, got %T", src)
	}
	if azure.tenantID != cfg.Credential.AzureTenantID {
		t.Errorf("tenantID = %q, want %q", azure.tenantID, cfg.Credential.AzureTenantID)
	}
	if azure.clientID != cfg.Credential.AzureClientID {
		t.Errorf("clientID = %q, want %q", azure.clientID, cfg.Credential.AzureClientID)
	}
	if azure.scope != cfg.Credential.AzureScope {
		t.Errorf("scope = %q, want %q", azure.scope, cfg.Credential.AzureScope)
	}
	if _, ok := azure.tokenSource.(*FileTokenSource); !ok {
		t.Errorf("expected wrapped FileTokenSource, got %T", azure.tokenSource)
	}
}

// TestBuildSource_AzureWorkloadIdentityDefaultsScope verifies that an
// empty AzureScope in the RunConfig is left empty at the dispatch layer
// — the constructor itself applies the default
// "https://cognitiveservices.azure.com/.default". This split keeps the
// "what was the operator's input" record honest while still producing a
// usable source.
func TestBuildSource_AzureWorkloadIdentityDefaultsScope(t *testing.T) {
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	azure, ok := src.(*AzureWorkloadIdentitySource)
	if !ok {
		t.Fatalf("expected *AzureWorkloadIdentitySource, got %T", src)
	}
	if azure.scope != "https://cognitiveservices.azure.com/.default" {
		t.Errorf("scope default not applied: got %q", azure.scope)
	}
}

func TestBuildSource_AzureWorkloadIdentityMissingTenant(t *testing.T) {
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
	if err == nil {
		t.Fatal("expected error for missing azureTenantId")
	}
	if !strings.Contains(err.Error(), "azureTenantId") {
		t.Errorf("error should name azureTenantId: %v", err)
	}
}

func TestBuildSource_AzureWorkloadIdentityMissingClient(t *testing.T) {
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
	if err == nil {
		t.Fatal("expected error for missing azureClientId")
	}
	if !strings.Contains(err.Error(), "azureClientId") {
		t.Errorf("error should name azureClientId: %v", err)
	}
}

func TestBuildSource_AzureWorkloadIdentityMissingTokenSource(t *testing.T) {
	cfg := types.ProviderConfig{
		Type: "openai-compatible",
		Credential: &types.CredentialConfig{
			Type:          "azure-workload-identity",
			AzureTenantID: "11111111-1111-1111-1111-111111111111",
			AzureClientID: "22222222-2222-2222-2222-222222222222",
		},
	}
	_, err := BuildSource(cfg, nil)
	if err == nil {
		t.Fatal("expected error for missing token source")
	}
	if !strings.Contains(err.Error(), "tokenSource") {
		t.Errorf("error should name tokenSource: %v", err)
	}
}
