package core

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/resultsink"
	"github.com/rxbynerd/stirrup/types"
)

func TestNewResultSink_BuildsRemoteSinksWithGCPCredentials(t *testing.T) {
	cases := []struct {
		name     string
		cfg      *types.ResultSinkConfig
		wantType any
	}{
		{"gcs default credential", &types.ResultSinkConfig{Type: "gcs", Bucket: "stirrup-results"}, (*resultsink.GCSSink)(nil)},
		{"gcp-pubsub with gcp-default", &types.ResultSinkConfig{
			Type:       "gcp-pubsub",
			Topic:      "projects/acme-prod/topics/stirrup-results",
			Credential: &types.CredentialConfig{Type: "gcp-default"},
		}, (*resultsink.PubSubSink)(nil)},
		{"stdout-json needs no credential", &types.ResultSinkConfig{Type: "stdout-json"}, (*resultsink.StdoutJSONSink)(nil)},
		{"nil config", nil, resultsink.NoneSink{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			sink, err := NewResultSink(tc.cfg)
			must.NoError(err)
			must.IsType(tc.wantType, sink)
		})
	}
}

func TestNewResultSink_RejectsUnsupportedCredential(t *testing.T) {
	for _, credentialType := range []string{"aws-default", "gcp-service-account"} {
		t.Run(credentialType, func(t *testing.T) {
			must := require.New(t)
			_, err := NewResultSink(&types.ResultSinkConfig{
				Type:       "gcs",
				Bucket:     "stirrup-results",
				Credential: &types.CredentialConfig{Type: credentialType},
			})
			must.ErrorContains(err, "resultSink credential")
			must.ErrorContains(err, `credential.type="`+credentialType+`" is not supported`)
		})
	}
}
