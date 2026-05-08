package credential

import (
	"context"
	"fmt"
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
