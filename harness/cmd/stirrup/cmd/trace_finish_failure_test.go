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
	"strings"
	"testing"

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
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyJSON, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "stirrup-test",
		"private_key_id": "test-key",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email":   "trace@stirrup-test.iam.gserviceaccount.com",
		"client_id":      "1",
		"token_uri":      tokenSrv.URL,
	})
	if err != nil {
		t.Fatalf("marshal key file: %v", err)
	}
	path := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(path, keyJSON, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
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

	if runErr == nil {
		t.Fatal("runWithConfig error = nil, want the trace upload failure")
	}
	if !strings.Contains(runErr.Error(), "finish trace") || !strings.Contains(runErr.Error(), "upload trace") {
		t.Errorf("runWithConfig error = %v, want it to name the trace upload failure", runErr)
	}

	result := decodeStirrupResult(t, stdout)
	if result.Outcome != "success" {
		t.Errorf("STIRRUP_RESULT outcome = %q, want success (a trace upload failure does not change the outcome)", result.Outcome)
	}
}
