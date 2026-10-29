package gcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// staticBearer returns a fixed token. The bearer closure is the GCS
// client's only path to authentication, so almost every test in this
// file routes through it.
func staticBearer(token string) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) { return token, nil }
}

// TestUploadObject_SendsMediaUploadRequest confirms a 200-class response is
// the happy path: no error returned, request carries the expected method,
// path, headers and query-string contract, and the body is sent intact.
func TestUploadObject_SendsMediaUploadRequest(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)

	// The request checks are independent, so all mismatches are reported together.
	check.Equal(http.MethodPost, gotMethod)
	check.Equal("/upload/storage/v1/b/stirrup-results/o", gotPath)
	check.Contains(gotQuery, "uploadType=media")
	check.Contains(gotQuery, "name=traces/run-1.jsonl")
	check.Equal("Bearer test-token", gotAuth)
	check.Equal("application/x-ndjson", gotCT)
	check.Equal("hello\n", string(gotBody))
}

// TestUploadObject_CreateOnlySetsGenerationPrecondition pins that a
// create-only upload asks GCS to refuse to replace an existing object,
// and that the default upload does not.
func TestUploadObject_CreateOnlySetsGenerationPrecondition(t *testing.T) {
	for _, createOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("createOnly=%t", createOnly), func(t *testing.T) {
			must := require.New(t)
			var gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.RawQuery
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			err := UploadObject(context.Background(), srv.Client(), UploadOptions{
				Bucket:          "b",
				Object:          "o",
				Body:            []byte("x"),
				Bearer:          staticBearer("t"),
				EndpointBaseURL: srv.URL,
				CreateOnly:      createOnly,
			})
			must.NoError(err)
			must.Equal(createOnly, strings.Contains(gotQuery, "ifGenerationMatch=0"), "query=%q", gotQuery)
		})
	}
}

// TestUploadObject_DefaultsContentTypeToOctetStream pins the documented
// default: empty ContentType resolves to application/octet-stream rather
// than passing an empty header that an intermediary might interpret
// differently from the GCS REST API.
func TestUploadObject_DefaultsContentTypeToOctetStream(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	must.Equal("application/octet-stream", gotCT)
}

// TestUploadObject_Non2xxStatusReturnsError covers the family of HTTP
// failures. Each non-2xx response should produce an error that names HTTP
// and surfaces the response body, so log-based diagnosis is unambiguous.
func TestUploadObject_Non2xxStatusReturnsError(t *testing.T) {
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
			must := require.New(t)
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
			must.Error(err)
			must.ErrorContains(err, "HTTP")
			must.ErrorContains(err, tc.body)
			var statusErr *UploadStatusError
			must.ErrorAs(err, &statusErr)
			must.Equal(tc.code, statusErr.StatusCode)
		})
	}
}

// TestUploadObject_BearerFailureSkipsRequest pins propagation of the
// credential closure's failure: a metadata-server hiccup that produces no
// token must surface as an error, not as an unauthenticated request.
func TestUploadObject_BearerFailureSkipsRequest(t *testing.T) {
	must := require.New(t)
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
	must.Error(err)
	must.ErrorIs(err, sentinel)
	must.False(called, "server must not receive a request without a token")
}

// TestUploadObject_RequiredFieldsRejected pins the four cheap shape checks
// at the top of UploadObject. Each missing field produces a distinguishable
// error so an operator can tell which axis is wrong without reading the
// code path.
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
			must := require.New(t)
			err := UploadObject(context.Background(), &http.Client{}, tc.opts)
			must.Error(err)
			must.ErrorContains(err, tc.want)
		})
	}
}

// TestUploadObject_NilClientRejected pins the explicit nil-client guard.
// Falling back to http.DefaultClient would silently violate the project's
// HTTP-discipline policy (CLAUDE.md), so the function must refuse to run
// without an explicit, timeout-bearing client.
func TestUploadObject_NilClientRejected(t *testing.T) {
	must := require.New(t)
	err := UploadObject(context.Background(), nil, UploadOptions{
		Bucket: "b",
		Object: "o",
		Body:   []byte("x"),
		Bearer: staticBearer("t"),
	})
	must.ErrorContains(err, "http client is required")
}

// TestURLPathEscape_EncodesAllButUnreservedAndSlash covers the defence-in-depth
// layer against object-name injection. Alphanumerics, the unreserved
// punctuation characters and '/' pass through unchanged; every other byte is
// percent-encoded with uppercase hex. Keeping '/' unencoded preserves the
// gcsObjectName contract, and encoding ':' and '@' stops a crafted run ID
// from rewriting the URL.
func TestURLPathEscape_EncodesAllButUnreservedAndSlash(t *testing.T) {
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
			must := require.New(t)
			must.Equal(tc.want, urlPathEscape(tc.in))
		})
	}
}
