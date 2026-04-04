package credential

import (
	"context"
	"fmt"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// mockSecretStore implements security.SecretStore for testing.
type mockSecretStore struct {
	values map[string]string
	err    error
}

func (m *mockSecretStore) Resolve(_ context.Context, ref string) (string, error) {
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
	if cred.BearerToken != "sk-test-123" {
		t.Errorf("BearerToken = %q, want %q", cred.BearerToken, "sk-test-123")
	}
	if cred.AWSCredentials != nil {
		t.Error("AWSCredentials should be nil for static source")
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
	if cred.BearerToken != "" {
		t.Error("BearerToken should be empty for AWS default source")
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
	_, err := BuildTokenSource(&types.TokenSourceConfig{Type: "azure-imds"})
	if err == nil {
		t.Fatal("expected error for unsupported token source type")
	}
}
