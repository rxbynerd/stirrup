package gcs

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// staticBearer returns a fixed token. The bearer closure is the GCS
// client's only path to authentication, so almost every test in this
// file routes through it.
func staticBearer(token string) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) { return token, nil }
}

// TestUploadObject_Success confirms a 200-class response is the happy
// path: no error returned, request carries the expected headers and
// query-string contract, response body is drained so the underlying
// HTTP transport can reuse the connection.
func TestUploadObject_Success(t *testing.T) {
	var (
		gotPath   string
		gotQuery  string
		gotAuth   string
		gotCT     string
		gotBody   []byte
		gotMethod string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"obj"}`))
	}))
	defer srv.Close()

	err := UploadObject(context.Background(), srv.Client(), UploadOptions{
		Bucket:          "stirrup-results",
		Object:          "traces/run-1.jsonl",
		Body:            []byte("hello\n"),
		ContentType:     "application/x-ndjson",
		Bearer:          staticBearer("test-token"),
		EndpointBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("UploadObject returned %v, want nil", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if want := "/upload/storage/v1/b/stirrup-results/o"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if !strings.Contains(gotQuery, "uploadType=media") {
		t.Errorf("query missing uploadType=media: %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "name=traces/run-1.jsonl") {
		t.Errorf("query missing name=traces/run-1.jsonl: %q", gotQuery)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want \"Bearer test-token\"", gotAuth)
	}
	if gotCT != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", gotCT)
	}
	if string(gotBody) != "hello\n" {
		t.Errorf("body = %q, want %q", gotBody, "hello\n")
	}
}

// TestUploadObject_ContentTypeDefault pins the documented default:
// empty ContentType resolves to application/octet-stream rather than
// passing an empty header that an intermediary might interpret
// differently from the GCS REST API.
func TestUploadObject_ContentTypeDefault(t *testing.T) {
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := UploadObject(context.Background(), srv.Client(), UploadOptions{
		Bucket:          "b",
		Object:          "o",
		Body:            []byte("x"),
		Bearer:          staticBearer("t"),
		EndpointBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("UploadObject returned %v, want nil", err)
	}
	if gotCT != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", gotCT)
	}
}

// TestUploadObject_Non2xxError covers the family of HTTP failures.
// Each non-2xx response should produce an error containing the status
// code so log-based diagnosis is unambiguous.
func TestUploadObject_Non2xxError(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
	}{
		{"forbidden", http.StatusForbidden, "permission denied"},
		{"not found", http.StatusNotFound, "no such bucket"},
		{"service unavailable", http.StatusServiceUnavailable, "try again"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			err := UploadObject(context.Background(), srv.Client(), UploadOptions{
				Bucket:          "b",
				Object:          "o",
				Body:            []byte("x"),
				Bearer:          staticBearer("t"),
				EndpointBaseURL: srv.URL,
			})
			if err == nil {
				t.Fatal("UploadObject returned nil, want error for non-2xx status")
			}
			if !strings.Contains(err.Error(), "HTTP") {
				t.Errorf("error %v does not mention HTTP", err)
			}
			if !strings.Contains(err.Error(), tc.body) {
				t.Errorf("error %v does not surface response body %q", err, tc.body)
			}
		})
	}
}

// TestUploadObject_BearerError pins propagation of the credential
// closure's failure: a metadata-server hiccup that produces no token
// must surface as an error, not as an unauthenticated request.
func TestUploadObject_BearerError(t *testing.T) {
	sentinel := errors.New("simulated metadata server failure")
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := UploadObject(context.Background(), srv.Client(), UploadOptions{
		Bucket: "b",
		Object: "o",
		Body:   []byte("x"),
		Bearer: func(_ context.Context) (string, error) {
			return "", sentinel
		},
		EndpointBaseURL: srv.URL,
	})
	if err == nil {
		t.Fatal("UploadObject returned nil, want error from bearer closure")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error chain does not wrap sentinel: %v", err)
	}
	if called {
		t.Error("HTTP server was called despite bearer-acquisition failure")
	}
}

// TestUploadObject_RequiredFieldsRejected pins the four cheap shape
// checks at the top of UploadObject. Each missing field produces a
// distinguishable error so an operator can tell which axis is wrong
// without reading the code path.
func TestUploadObject_RequiredFieldsRejected(t *testing.T) {
	cases := []struct {
		name string
		opts UploadOptions
		want string
	}{
		{"missing bucket", UploadOptions{Object: "o", Body: []byte("x"), Bearer: staticBearer("t")}, "bucket is required"},
		{"missing object", UploadOptions{Bucket: "b", Body: []byte("x"), Bearer: staticBearer("t")}, "object is required"},
		{"missing bearer", UploadOptions{Bucket: "b", Object: "o", Body: []byte("x")}, "bearer is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := UploadObject(context.Background(), &http.Client{}, tc.opts)
			if err == nil {
				t.Fatalf("UploadObject returned nil, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// TestUploadObject_NilClientRejected pins the explicit nil-client
// guard. Falling back to http.DefaultClient would silently violate
// the project's HTTP-discipline policy (CLAUDE.md), so the function
// must refuse to run without an explicit, timeout-bearing client.
func TestUploadObject_NilClientRejected(t *testing.T) {
	err := UploadObject(context.Background(), nil, UploadOptions{
		Bucket: "b",
		Object: "o",
		Body:   []byte("x"),
		Bearer: staticBearer("t"),
	})
	if err == nil || !strings.Contains(err.Error(), "http client is required") {
		t.Errorf("error = %v, want \"http client is required\"", err)
	}
}

// TestURLPathEscape is the table-driven pinning the brief calls out.
// The function is the defence-in-depth layer for object-name
// injection: alphanumerics, the four unreserved punctuation chars,
// and '/' pass through unchanged, while every other byte is
// percent-encoded with uppercase hex. A regression that started
// encoding '/' would silently break the gcsObjectName contract; one
// that stopped encoding bytes like ':' or '@' would let a malicious
// runID rewrite the URL.
func TestURLPathEscape(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"alphanumeric unchanged", "abcXYZ012", "abcXYZ012"},
		{"slash preserved", "a/b/c", "a/b/c"},
		{"unreserved punctuation", "a-b_c.d~e", "a-b_c.d~e"},
		{"space encoded", "a b", "a%20b"},
		{"at sign", "a@b", "a%40b"},
		{"colon", "a:b", "a%3Ab"},
		{"question mark", "a?b", "a%3Fb"},
		{"ampersand", "a&b", "a%26b"},
		{"percent literal", "100%", "100%25"},
		{"unicode two-byte", "café", "caf%C3%A9"},
		{"unicode three-byte", "日", "%E6%97%A5"},
		{"control byte CR", "a\rb", "a%0Db"},
		{"control byte LF", "a\nb", "a%0Ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := urlPathEscape(tc.in); got != tc.want {
				t.Errorf("urlPathEscape(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
