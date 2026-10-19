package credential

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// mockTokenSource returns a fixed token for testing.
type mockTokenSource struct {
	token []byte
	err   error
}

func (m *mockTokenSource) Token(_ context.Context) ([]byte, error) {
	return m.token, m.err
}

func TestWebIdentityAWSSource_Resolve_ReturnsAWSCredentials(t *testing.T) {
	ts := &mockTokenSource{token: []byte("eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.test")}
	src := NewWebIdentityAWSSource(ts, "us-east-1", "arn:aws:iam::123456789012:role/test", "stirrup-test")

	cred, err := src.Resolve(context.Background())
	require.NoError(t, err)
	require.NotNil(t, cred.AWSCredentials, "web identity source must return AWS credentials")
	require.Nil(t, cred.BearerToken, "AWS credential source must not return a bearer token")
}

func TestWebIdentityAWSSource_EmptySessionNameDefaultsToStirrup(t *testing.T) {
	ts := &mockTokenSource{token: []byte("test-token")}
	src := NewWebIdentityAWSSource(ts, "eu-west-1", "arn:aws:iam::123456789012:role/test", "")

	t.Run("session name defaults to stirrup", func(t *testing.T) {
		require.Equal(t, "stirrup", src.sessionName)
	})

	t.Run("resolve returns AWS credentials", func(t *testing.T) {
		cred, err := src.Resolve(context.Background())
		require.NoError(t, err)
		require.NotNil(t, cred.AWSCredentials)
	})
}

func TestTokenSourceAdapter_GetIdentityToken_ReturnsSourceToken(t *testing.T) {
	expected := []byte("test-oidc-token")
	adapter := &tokenSourceAdapter{ts: &mockTokenSource{token: expected}}

	token, err := adapter.GetIdentityToken()
	require.NoError(t, err)
	require.Equal(t, expected, token)
}

func TestTokenSourceAdapter_GetIdentityToken_PropagatesSourceError(t *testing.T) {
	adapter := &tokenSourceAdapter{ts: &mockTokenSource{err: context.DeadlineExceeded}}

	_, err := adapter.GetIdentityToken()
	require.Error(t, err)
}
