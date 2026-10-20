package provider

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWireTapTransport_DumpsRequestAndResponse drives WireTapTransport
// against a real httptest.Server so the RoundTrip path (request dump,
// response header dump, streamed body tee) is exercised end to end, and
// asserts the request and response bodies both land in the injected
// output writer unredacted — including a chunked/streamed response body,
// which is the SSE shape --trace-wire exists to capture live rather than
// only after the stream closes.
func TestWireTapTransport_DumpsRequestAndResponse(t *testing.T) {
	must := require.New(t)
	const reqBody = `{"secret":"sk-ant-super-secret-token"}`
	const respChunk1 = `data: {"chunk":1}` + "\n\n"
	const respChunk2 = `data: {"chunk":2,"secret":"sk-ant-response-secret"}` + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test-Header", "response-header-value")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("httptest ResponseWriter does not support flushing")
			return
		}
		_, _ = io.WriteString(w, respChunk1)
		flusher.Flush()
		_, _ = io.WriteString(w, respChunk2)
		flusher.Flush()
	}))
	defer srv.Close()

	var out bytes.Buffer
	client := &http.Client{Transport: WireTapTransport(http.DefaultTransport, &out)}

	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(reqBody))
	must.NoError(err, "build request")
	req.Header.Set("Authorization", "Bearer sk-ant-request-secret")

	resp, err := client.Do(req)
	must.NoError(err)
	defer func() { _ = resp.Body.Close() }()

	// The real caller must still be able to read the full, correct body —
	// tapping must not consume or corrupt the stream it observes.
	gotBody, err := io.ReadAll(resp.Body)
	must.NoError(err, "read response body")
	must.Equal(respChunk1+respChunk2, string(gotBody))

	dumped := out.String()

	// Request: header, auth, and body all reach the tap unredacted.
	for _, tc := range []struct{ name, want string }{
		{"request marker", "---- REQUEST ----"},
		{"request authorization header", "Authorization: Bearer sk-ant-request-secret"},
		{"request body", reqBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Contains(dumped, tc.want)
		})
	}

	// Response: headers dumped up front, and the full streamed body
	// (across both chunks) reaches the tap unredacted.
	for _, tc := range []struct{ name, want string }{
		{"response headers marker", "---- RESPONSE HEADERS ----"},
		{"response header", "X-Test-Header: response-header-value"},
		{"response body marker", "---- RESPONSE BODY ----"},
		{"first response chunk", respChunk1},
		{"second response chunk", respChunk2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Contains(dumped, tc.want)
		})
	}
}

// TestWireTapTransport_DefaultsWhenNil asserts the nil-base/nil-out
// defaulting documented on WireTapTransport: a nil out falls back to
// something non-panicking (os.Stderr in production; here we only assert
// construction and a round trip do not panic), and a nil base falls back
// to http.DefaultTransport.
func TestWireTapTransport_DefaultsWhenNil(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	rt := WireTapTransport(nil, nil)
	client := &http.Client{Transport: rt}

	resp, err := client.Get(srv.URL)
	must.NoError(err)
	defer func() { _ = resp.Body.Close() }()
	must.Equal(http.StatusNoContent, resp.StatusCode)
}

// TestWireTapTransport_PropagatesRoundTripError asserts a base transport
// failure (e.g. connection refused) is returned unchanged rather than
// swallowed by the tap.
func TestWireTapTransport_PropagatesRoundTripError(t *testing.T) {
	must := require.New(t)
	var out bytes.Buffer
	client := &http.Client{Transport: WireTapTransport(http.DefaultTransport, &out)}

	// No listener on this port; the connection must fail.
	_, err := client.Get("http://127.0.0.1:1/does-not-exist")
	must.Error(err)
	must.Contains(out.String(), "---- REQUEST ----",
		"the request is dumped before the failed round trip")
}
