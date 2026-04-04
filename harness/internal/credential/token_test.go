package credential

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGKEMetadataTokenSource_Success(t *testing.T) {
	fakeToken := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJhdWQiOiJzdHMuYW1hem9uYXdzLmNvbSJ9.sig"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			t.Error("missing Metadata-Flavor: Google header")
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if !strings.Contains(r.URL.RawQuery, "audience=sts.amazonaws.com") {
			t.Errorf("unexpected query: %s", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fakeToken))
	}))
	defer srv.Close()

	ts := NewGKEMetadataTokenSource("sts.amazonaws.com", srv.URL)
	token, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(token) != fakeToken {
		t.Errorf("token = %q, want %q", string(token), fakeToken)
	}
}

func TestGKEMetadataTokenSource_TrimsWhitespace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("  token-value  \n"))
	}))
	defer srv.Close()

	ts := NewGKEMetadataTokenSource("test-audience", srv.URL)
	token, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(token) != "token-value" {
		t.Errorf("token = %q, want %q", string(token), "token-value")
	}
}

func TestGKEMetadataTokenSource_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	ts := NewGKEMetadataTokenSource("sts.amazonaws.com", srv.URL)
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention status code 404: %v", err)
	}
}

func TestGKEMetadataTokenSource_EmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ts := NewGKEMetadataTokenSource("sts.amazonaws.com", srv.URL)
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for empty token body")
	}
}

func TestGKEMetadataTokenSource_ServerDown(t *testing.T) {
	ts := NewGKEMetadataTokenSource("sts.amazonaws.com", "http://127.0.0.1:1")
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error when metadata server is unreachable")
	}
}

func TestFileTokenSource_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("file-token-value\n"), 0600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	ts := &FileTokenSource{path: path}
	token, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(token) != "file-token-value" {
		t.Errorf("token = %q, want %q", string(token), "file-token-value")
	}
}

func TestFileTokenSource_Missing(t *testing.T) {
	ts := &FileTokenSource{path: "/nonexistent/path/token"}
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestFileTokenSource_Empty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty")
	if err := os.WriteFile(path, []byte("  \n"), 0600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	ts := &FileTokenSource{path: path}
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for empty token file")
	}
}

func TestEnvTokenSource_Success(t *testing.T) {
	t.Setenv("TEST_OIDC_TOKEN", "env-token-value")
	ts := &EnvTokenSource{envVar: "TEST_OIDC_TOKEN"}
	token, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(token) != "env-token-value" {
		t.Errorf("token = %q, want %q", string(token), "env-token-value")
	}
}

func TestEnvTokenSource_Empty(t *testing.T) {
	t.Setenv("TEST_EMPTY_TOKEN", "")
	ts := &EnvTokenSource{envVar: "TEST_EMPTY_TOKEN"}
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for empty env var")
	}
}

func TestEnvTokenSource_Unset(t *testing.T) {
	ts := &EnvTokenSource{envVar: "DEFINITELY_NOT_SET_XYZ_123"}
	_, err := ts.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for unset env var")
	}
}
