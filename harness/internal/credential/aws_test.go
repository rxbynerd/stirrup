package credential

import (
	"context"
	"testing"
)

// mockTokenSource returns a fixed token for testing.
type mockTokenSource struct {
	token []byte
	err   error
}

func (m *mockTokenSource) Token(_ context.Context) ([]byte, error) {
	return m.token, m.err
}

func TestWebIdentityAWSSource_Resolve(t *testing.T) {
	ts := &mockTokenSource{token: []byte("eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.test")}
	src := NewWebIdentityAWSSource(ts, "us-east-1", "arn:aws:iam::123456789012:role/test", "stirrup-test")

	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.AWSCredentials == nil {
		t.Fatal("AWSCredentials should be non-nil for web identity source")
	}
	if cred.BearerToken != "" {
		t.Error("BearerToken should be empty for AWS credential source")
	}
}

func TestWebIdentityAWSSource_DefaultSessionName(t *testing.T) {
	ts := &mockTokenSource{token: []byte("test-token")}
	src := NewWebIdentityAWSSource(ts, "eu-west-1", "arn:aws:iam::123456789012:role/test", "")

	if src.sessionName != "stirrup" {
		t.Errorf("sessionName = %q, want %q", src.sessionName, "stirrup")
	}

	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.AWSCredentials == nil {
		t.Fatal("AWSCredentials should be non-nil")
	}
}

func TestTokenSourceAdapter_GetIdentityToken(t *testing.T) {
	expected := []byte("test-oidc-token")
	ts := &mockTokenSource{token: expected}
	adapter := &tokenSourceAdapter{ts: ts}

	token, err := adapter.GetIdentityToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(token) != string(expected) {
		t.Errorf("token = %q, want %q", string(token), string(expected))
	}
}

func TestTokenSourceAdapter_Error(t *testing.T) {
	ts := &mockTokenSource{err: context.DeadlineExceeded}
	adapter := &tokenSourceAdapter{ts: ts}

	_, err := adapter.GetIdentityToken()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
