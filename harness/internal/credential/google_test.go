package credential

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

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
	must := require.New(t)
	fakeRSAKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		must.NoError(err, "generate RSA key")
		der, err := x509.MarshalPKCS8PrivateKey(key)
		must.NoError(err, "marshal PKCS8 key")
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
	must := require.New(t)
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
	must.NoError(err, "marshal service account JSON")
	path := filepath.Join(dir, "key.json")
	must.NoError(os.WriteFile(path, data, 0o600), "write key file")
	return path
}

// writeUserCredentialsJSON writes an authorized_user credential file, which
// both credential sources must reject.
func writeUserCredentialsJSON(t *testing.T, dir string) string {
	t.Helper()
	must := require.New(t)
	path := filepath.Join(dir, "user.json")
	body := []byte(`{"type":"authorized_user","client_id":"x","client_secret":"y","refresh_token":"z"}`)
	must.NoError(os.WriteFile(path, body, 0o600), "write user creds")
	return path
}

func TestGoogleADCSource_RejectsAuthorizedUser(t *testing.T) {
	must := require.New(t)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", writeUserCredentialsJSON(t, t.TempDir()))

	src := &GoogleADCSource{}
	_, err := src.Resolve(context.Background())
	must.Error(err)
	for _, want := range []string{"authorized_user", "GOOGLE_APPLICATION_CREDENTIALS", "gcp-service-account", "gcp-workload-identity"} {
		t.Run("guidance mentions "+want, func(t *testing.T) {
			must := require.New(t)
			must.ErrorContains(err, want)
		})
	}
}

func TestGoogleADCSource_AcceptsServiceAccount(t *testing.T) {
	must := require.New(t)
	path := writeServiceAccountJSON(t, t.TempDir(), nil)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)

	src := &GoogleADCSource{}
	cred, err := src.Resolve(context.Background())
	must.NoError(err)
	must.NotNil(cred.BearerToken)
	must.Nil(cred.AWSCredentials)
}

func TestServiceAccountKeySource_AcceptsValidKey(t *testing.T) {
	must := require.New(t)
	path := writeServiceAccountJSON(t, t.TempDir(), nil)

	src := NewServiceAccountKeySource(path)
	cred, err := src.Resolve(context.Background())
	must.NoError(err)
	must.NotNil(cred.BearerToken)
}

func TestServiceAccountKeySource_RejectsMissingFile(t *testing.T) {
	must := require.New(t)
	src := NewServiceAccountKeySource(filepath.Join(t.TempDir(), "does-not-exist.json"))
	_, err := src.Resolve(context.Background())
	must.ErrorContains(err, "open service account key")
}

func TestServiceAccountKeySource_RejectsEmptyPath(t *testing.T) {
	must := require.New(t)
	src := NewServiceAccountKeySource("")
	_, err := src.Resolve(context.Background())
	must.Error(err)
}

func TestServiceAccountKeySource_RejectsOversizedFile(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "huge.json")
	// Content does not need to be valid JSON; the size check trips first.
	big := bytes.Repeat([]byte{'a'}, maxServiceAccountKeyBytes+1)
	must.NoError(os.WriteFile(path, big, 0o600), "write huge file")

	src := NewServiceAccountKeySource(path)
	_, err := src.Resolve(context.Background())
	must.ErrorContains(err, "refusing to read files larger than")
}

func TestServiceAccountKeySource_RejectsAuthorizedUser(t *testing.T) {
	must := require.New(t)
	src := NewServiceAccountKeySource(writeUserCredentialsJSON(t, t.TempDir()))
	_, err := src.Resolve(context.Background())
	must.ErrorContains(err, "authorized_user")
}

func TestServiceAccountKeySource_RejectsMalformedJSON(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "bad.json")
	must.NoError(os.WriteFile(path, []byte("{not valid json"), 0o600), "write bad json")

	src := NewServiceAccountKeySource(path)
	_, err := src.Resolve(context.Background())
	must.ErrorContains(err, "parse service account key")
}

func TestServiceAccountKeySource_RejectsMissingType(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "notype.json")
	must.NoError(os.WriteFile(path, []byte(`{"project_id":"x"}`), 0o600), "write notype.json")

	src := NewServiceAccountKeySource(path)
	_, err := src.Resolve(context.Background())
	must.Error(err)
}

func TestServiceAccountKeySource_RejectsUnknownType(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "weird.json")
	must.NoError(os.WriteFile(path, []byte(`{"type":"external_account"}`), 0o600), "write weird.json")

	src := NewServiceAccountKeySource(path)
	_, err := src.Resolve(context.Background())
	must.ErrorContains(err, `"external_account"`)
}

// TestServiceAccountKeySource_ContextCancelDoesNotFailRefresh verifies
// that a Resolve(ctx) call does not bind ctx to subsequent token
// refreshes — otherwise cancelling the factory/pre-run context would
// poison every future Token() call even with a valid agentic-loop
// context. We only check the error, if any, is not "context canceled";
// the fake credentials aren't signed with a key Google IAM trusts.
func TestServiceAccountKeySource_ContextCancelDoesNotFailRefresh(t *testing.T) {
	must := require.New(t)
	path := writeServiceAccountJSON(t, t.TempDir(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	src := NewServiceAccountKeySource(path)
	cred, err := src.Resolve(ctx)
	must.NoError(err)
	must.NotNil(cred.BearerToken)
	cancel()

	// Refresh runs under context.Background(), so the cancelled Resolve
	// context must not appear in its error. The fake key makes the refresh
	// itself fail, which is expected.
	_, err = cred.BearerToken(context.Background())
	if err != nil {
		must.NotContains(err.Error(), "context canceled", "refresh ctx is still bound to the Resolve context")
	}
}

func TestServiceAccountKeySource_RejectsDirectory(t *testing.T) {
	must := require.New(t)
	src := NewServiceAccountKeySource(t.TempDir())
	_, err := src.Resolve(context.Background())
	must.ErrorContains(err, "directory")
}

func TestGoogleWorkloadIdentitySource_ResolveReturnsBearerToken(t *testing.T) {
	must := require.New(t)
	// We cannot meaningfully test Resolve() without a metadata server;
	// the test just confirms the constructor returns a source whose
	// Resolve produces a non-nil BearerToken closure synchronously
	// (oauth2.ReuseTokenSource is lazy; no network call yet).
	src := NewGoogleWorkloadIdentitySource()
	cred, err := src.Resolve(context.Background())
	must.NoError(err)
	must.NotNil(cred.BearerToken)
}

func TestBuildSource_GeminiWithoutCredentialUsesADC(t *testing.T) {
	must := require.New(t)
	cfg := types.ProviderConfig{Type: "gemini"}
	src, err := BuildSource(cfg, nil)
	must.NoError(err)
	must.IsType(&GoogleADCSource{}, src)
}

func TestBuildSource_ExplicitGCPDefault(t *testing.T) {
	must := require.New(t)
	cfg := types.ProviderConfig{
		Type: "gemini",
		Credential: &types.CredentialConfig{
			Type: "gcp-default",
		},
	}
	src, err := BuildSource(cfg, nil)
	must.NoError(err)
	must.IsType(&GoogleADCSource{}, src)
}

func TestBuildSource_ExplicitGCPServiceAccount(t *testing.T) {
	must := require.New(t)
	cfg := types.ProviderConfig{
		Type:               "gemini",
		GCPCredentialsFile: "/etc/secrets/key.json",
		Credential: &types.CredentialConfig{
			Type: "gcp-service-account",
		},
	}
	src, err := BuildSource(cfg, nil)
	must.NoError(err)
	sa, ok := src.(*ServiceAccountKeySource)
	must.True(ok, "expected *ServiceAccountKeySource, got %T", src)
	must.Equal("/etc/secrets/key.json", sa.path)
}

// TestBuildSource_UnsupportedGCPCredentialTypeReturnsError verifies an
// unrecognised credential.type fails loudly rather than silently
// falling through to the default ADC source.
func TestBuildSource_UnsupportedGCPCredentialTypeReturnsError(t *testing.T) {
	must := require.New(t)
	cfg := types.ProviderConfig{
		Type: "gemini",
		Credential: &types.CredentialConfig{
			Type: "gcp-unknown",
		},
	}
	_, err := BuildSource(cfg, nil)
	must.Error(err)
}

func TestBuildSource_ExplicitGCPWorkloadIdentity(t *testing.T) {
	must := require.New(t)
	cfg := types.ProviderConfig{
		Type: "gemini",
		Credential: &types.CredentialConfig{
			Type: "gcp-workload-identity",
		},
	}
	src, err := BuildSource(cfg, nil)
	must.NoError(err)
	must.IsType(&GoogleWorkloadIdentitySource{}, src)
}
