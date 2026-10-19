package cmd

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// failingGoogleCredentials points Application Default Credentials at a
// service-account key whose token endpoint always fails, so a GCS
// upload fails at token acquisition without any request leaving the
// host.
func failingGoogleCredentials(t *testing.T) {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"backend_error"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(tokenSrv.Close)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err, "generate key")
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err, "marshal key")
	keyJSON, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "stirrup-test",
		"private_key_id": "test-key",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email":   "trace@stirrup-test.iam.gserviceaccount.com",
		"client_id":      "1",
		"token_uri":      tokenSrv.URL,
	})
	require.NoError(t, err, "marshal key file")
	path := filepath.Join(t.TempDir(), "sa.json")
	require.NoError(t, os.WriteFile(path, keyJSON, 0o600), "write key file")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
}

func TestRunWithConfig_GCSTraceUploadFailureStillEmitsRunResult(t *testing.T) {
	failingGoogleCredentials(t)
	srv := openAICompatibleSuccessServer(t)
	defer srv.Close()

	config := runOutcomeTestConfig(t, srv.URL)
	config.TraceEmitter = types.TraceEmitterConfig{
		Type:       "gcs",
		Bucket:     "stirrup-test-traces",
		Credential: &types.CredentialConfig{Type: "gcp-default"},
	}

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout := stdoutDone()

	require.Error(t, runErr, "run error should report the trace upload failure")
	require.ErrorContains(t, runErr, "finish trace")
	require.ErrorContains(t, runErr, "upload trace")

	result := decodeStirrupResult(t, stdout)
	require.Equal(t, "success", result.Outcome, "a trace upload failure does not change the outcome")
}
