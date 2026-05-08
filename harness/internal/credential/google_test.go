package credential

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// fakeRSAKey is generated lazily once per test process. The 2048-bit
// keygen is the slow part of these tests; sharing one key across all
// tests keeps the suite under a second.
var (
	fakeRSAKeyOnce sync.Once
	fakeRSAKeyPEM  string
)

func testServiceAccountPEM(t *testing.T) string {
	t.Helper()
	fakeRSAKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate RSA key: %v", err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatalf("marshal PKCS8 key: %v", err)
		}
		// google.JWTConfigFromJSON accepts both PKCS#1 and PKCS#8 PEM
		// encodings; PKCS#8 with a "PRIVATE KEY" header is the form the
		// real Google IAM service emits, so we mirror that.
		fakeRSAKeyPEM = string(pem.EncodeToMemory(&pem.Block{
			Type:  "PRIVATE KEY",
			Bytes: der,
		}))
	})
	return fakeRSAKeyPEM
}

// writeServiceAccountJSON writes a syntactically valid service-account
// JSON file with the given fields applied on top of the canonical
// shape. Returns the file path.
func writeServiceAccountJSON(t *testing.T, dir string, overrides map[string]any) string {
	t.Helper()
	doc := map[string]any{
		"type":                        "service_account",
		"project_id":                  "test-project",
		"private_key_id":              "abc123",
		"private_key":                 testServiceAccountPEM(t),
		"client_email":                "test@test-project.iam.gserviceaccount.com",
		"client_id":                   "1234567890",
		"token_uri":                   "https://oauth2.googleapis.com/token",
		"auth_uri":                    "https://accounts.google.com/o/oauth2/auth",
		"auth_provider_x509_cert_url": "https://www.googleapis.com/oauth2/v1/certs",
	}
	for k, v := range overrides {
		if v == nil {
			delete(doc, k)
			continue
		}
		doc[k] = v
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal service account JSON: %v", err)
	}
	path := filepath.Join(dir, "key.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

func TestGoogleADCSource_RejectsAuthorizedUser(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "user.json")
	body := []byte(`{"type":"authorized_user","client_id":"x","client_secret":"y","refresh_token":"z"}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write user creds: %v", err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)

	src := &GoogleADCSource{}
	_, err := src.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected error rejecting user-mode credentials, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"authorized_user", "GOOGLE_APPLICATION_CREDENTIALS", "gcp-service-account", "gcp-workload-identity"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q guidance:\n%s", want, msg)
		}
	}
}

func TestGoogleADCSource_AcceptsServiceAccount(t *testing.T) {
	dir := t.TempDir()
	path := writeServiceAccountJSON(t, dir, nil)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)

	src := &GoogleADCSource{}
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.GoogleTokenSource == nil {
		t.Fatal("GoogleTokenSource should be non-nil for service-account ADC")
	}
	if cred.BearerToken != "" {
		t.Error("BearerToken should be empty for Google credentials")
	}
	if cred.AWSCredentials != nil {
		t.Error("AWSCredentials should be nil for Google credentials")
	}
}

func TestServiceAccountKeySource_AcceptsValidKey(t *testing.T) {
	dir := t.TempDir()
	path := writeServiceAccountJSON(t, dir, nil)

	src := NewServiceAccountKeySource(path)
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.GoogleTokenSource == nil {
		t.Fatal("GoogleTokenSource should be non-nil")
	}
}

func TestServiceAccountKeySource_RejectsMissingFile(t *testing.T) {
	src := NewServiceAccountKeySource(filepath.Join(t.TempDir(), "does-not-exist.json"))
	_, err := src.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "open service account key") {
		t.Errorf("error should mention open failure, got: %v", err)
	}
}

func TestServiceAccountKeySource_RejectsEmptyPath(t *testing.T) {
	src := NewServiceAccountKeySource("")
	_, err := src.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected error for empty path, got nil")
	}
}

func TestServiceAccountKeySource_RejectsOversizedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.json")
	// Write a file just over the cap. Content does not need to be
	// valid JSON; the size check trips first.
	big := make([]byte, maxServiceAccountKeyBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatalf("write huge file: %v", err)
	}

	src := NewServiceAccountKeySource(path)
	_, err := src.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected error for oversized file, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to read files larger than") {
		t.Errorf("error should mention size cap, got: %v", err)
	}
}

func TestServiceAccountKeySource_RejectsAuthorizedUser(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "user.json")
	body := []byte(`{"type":"authorized_user","client_id":"x","client_secret":"y","refresh_token":"z"}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write user creds: %v", err)
	}

	src := NewServiceAccountKeySource(path)
	_, err := src.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected error rejecting authorized_user JSON, got nil")
	}
	if !strings.Contains(err.Error(), "authorized_user") {
		t.Errorf("error should mention authorized_user, got: %v", err)
	}
}

func TestServiceAccountKeySource_RejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("write bad json: %v", err)
	}

	src := NewServiceAccountKeySource(path)
	_, err := src.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
	if !strings.Contains(err.Error(), "parse service account key") {
		t.Errorf("error should mention parse failure, got: %v", err)
	}
}

func TestServiceAccountKeySource_RejectsMissingType(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notype.json")
	if err := os.WriteFile(path, []byte(`{"project_id":"x"}`), 0o600); err != nil {
		t.Fatalf("write notype.json: %v", err)
	}

	src := NewServiceAccountKeySource(path)
	_, err := src.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected error for missing type field, got nil")
	}
}

func TestServiceAccountKeySource_RejectsUnknownType(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "weird.json")
	if err := os.WriteFile(path, []byte(`{"type":"external_account"}`), 0o600); err != nil {
		t.Fatalf("write weird.json: %v", err)
	}

	src := NewServiceAccountKeySource(path)
	_, err := src.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected error for unknown type, got nil")
	}
	if !strings.Contains(err.Error(), `"external_account"`) {
		t.Errorf("error should mention the rejected type, got: %v", err)
	}
}

// TestServiceAccountKeySource_ContextCancelDoesNotFailRefresh verifies
// the B1 fix: a Resolve(ctx) call must not bind ctx to subsequent token
// refreshes. Otherwise cancelling the factory/pre-run context (as
// happens on signal, sub-agent teardown, or factory failure) would
// poison every future Token() call even though the long-lived
// agentic-loop context is still valid.
//
// We do not verify that Token() succeeds — the fake credentials are
// not signed with a key Google IAM trusts — only that the error, if
// any, is not "context canceled".
func TestServiceAccountKeySource_ContextCancelDoesNotFailRefresh(t *testing.T) {
	dir := t.TempDir()
	path := writeServiceAccountJSON(t, dir, nil)

	ctx, cancel := context.WithCancel(context.Background())
	src := NewServiceAccountKeySource(path)
	cred, err := src.Resolve(ctx)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.GoogleTokenSource == nil {
		t.Fatal("expected GoogleTokenSource")
	}
	cancel()

	// Token() will hit the OAuth2 endpoint and likely fail because the
	// JWT was signed with a fake key. The point is that the failure
	// must not be due to the cancelled Resolve ctx.
	_, err = cred.GoogleTokenSource.Token()
	if err != nil && strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("Token() failed with cancelled Resolve context — refresh ctx is still bound to factory ctx: %v", err)
	}
}

func TestServiceAccountKeySource_RejectsDirectory(t *testing.T) {
	dir := t.TempDir()

	src := NewServiceAccountKeySource(dir)
	_, err := src.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected error when path points at a directory, got nil")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error should mention directory, got: %v", err)
	}
}

func TestGoogleWorkloadIdentitySource_Construct(t *testing.T) {
	// We cannot meaningfully test Resolve() without a metadata server;
	// the test just confirms the constructor returns a source whose
	// Resolve produces a non-nil GoogleTokenSource synchronously
	// (oauth2.ReuseTokenSource is lazy; no network call yet).
	src := NewGoogleWorkloadIdentitySource()
	cred, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.GoogleTokenSource == nil {
		t.Fatal("GoogleTokenSource should be non-nil after Resolve")
	}
}

func TestBuildSource_InfersGemini(t *testing.T) {
	cfg := types.ProviderConfig{Type: "gemini"}
	src, err := BuildSource(cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := src.(*GoogleADCSource); !ok {
		t.Errorf("expected *GoogleADCSource, got %T", src)
	}
}

func TestBuildSource_ExplicitGCPDefault(t *testing.T) {
	cfg := types.ProviderConfig{
		Type: "gemini",
		Credential: &types.CredentialConfig{
			Type: "gcp-default",
		},
	}
	src, err := BuildSource(cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := src.(*GoogleADCSource); !ok {
		t.Errorf("expected *GoogleADCSource, got %T", src)
	}
}

func TestBuildSource_ExplicitGCPServiceAccount(t *testing.T) {
	cfg := types.ProviderConfig{
		Type:               "gemini",
		GCPCredentialsFile: "/etc/secrets/key.json",
		Credential: &types.CredentialConfig{
			Type: "gcp-service-account",
		},
	}
	src, err := BuildSource(cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sa, ok := src.(*ServiceAccountKeySource)
	if !ok {
		t.Fatalf("expected *ServiceAccountKeySource, got %T", src)
	}
	if sa.path != "/etc/secrets/key.json" {
		t.Errorf("path = %q, want %q", sa.path, "/etc/secrets/key.json")
	}
}

// TestBuildSource_UnsupportedGCPCredentialTypeReturnsError verifies
// that an unrecognised credential.type on a gemini provider produces
// a clear error rather than silently falling through to the default
// ADC source. A typo like "gcp-unkown" should fail loudly so the
// operator knows the credential layer never honoured their intent.
func TestBuildSource_UnsupportedGCPCredentialTypeReturnsError(t *testing.T) {
	cfg := types.ProviderConfig{
		Type: "gemini",
		Credential: &types.CredentialConfig{
			Type: "gcp-unknown",
		},
	}
	_, err := BuildSource(cfg, nil)
	if err == nil {
		t.Fatal("expected error for unsupported gcp credential type, got nil")
	}
}

func TestBuildSource_ExplicitGCPWorkloadIdentity(t *testing.T) {
	cfg := types.ProviderConfig{
		Type: "gemini",
		Credential: &types.CredentialConfig{
			Type: "gcp-workload-identity",
		},
	}
	src, err := BuildSource(cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := src.(*GoogleWorkloadIdentitySource); !ok {
		t.Errorf("expected *GoogleWorkloadIdentitySource, got %T", src)
	}
}
