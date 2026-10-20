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
	must := require.New(t)
	ts := &mockTokenSource{token: []byte("eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.test")}
	src := NewWebIdentityAWSSource(ts, "us-east-1", "arn:aws:iam::123456789012:role/test", "stirrup-test")

	cred, err := src.Resolve(context.Background())
	must.NoError(err)
	must.NotNil(cred.AWSCredentials, "web identity source must return AWS credentials")
	must.Nil(cred.BearerToken, "AWS credential source must not return a bearer token")
}

func TestWebIdentityAWSSource_EmptySessionNameDefaultsToStirrup(t *testing.T) {
	ts := &mockTokenSource{token: []byte("test-token")}
	src := NewWebIdentityAWSSource(ts, "eu-west-1", "arn:aws:iam::123456789012:role/test", "")

	t.Run("session name defaults to stirrup", func(t *testing.T) {
		must := require.New(t)
		must.Equal("stirrup", src.sessionName)
	})

	t.Run("resolve returns AWS credentials", func(t *testing.T) {
		must := require.New(t)
		cred, err := src.Resolve(context.Background())
		must.NoError(err)
		must.NotNil(cred.AWSCredentials)
	})
}

func TestTokenSourceAdapter_GetIdentityToken_ReturnsSourceToken(t *testing.T) {
	must := require.New(t)
	expected := []byte("test-oidc-token")
	adapter := &tokenSourceAdapter{ts: &mockTokenSource{token: expected}}

	token, err := adapter.GetIdentityToken()
	must.NoError(err)
	must.Equal(expected, token)
}

func TestTokenSourceAdapter_GetIdentityToken_PropagatesSourceError(t *testing.T) {
	must := require.New(t)
	adapter := &tokenSourceAdapter{ts: &mockTokenSource{err: context.DeadlineExceeded}}

	_, err := adapter.GetIdentityToken()
	must.Error(err)
}
