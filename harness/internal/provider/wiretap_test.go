package provider

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWireTapTransportDumpsRequestAndResponse drives WireTapTransport
// against a real httptest.Server so the RoundTrip path (request dump,
// response header dump, streamed body tee) is exercised end to end, and
// asserts the request and response bodies both land in the injected
// output writer unredacted — including a chunked/streamed response body,
// which is the SSE shape --trace-wire exists to capture live rather than
// only after the stream closes.
func TestWireTapTransportDumpsRequestAndResponse(t *testing.T) {
	const reqBody = `{"secret":"sk-ant-super-secret-token"}`
	const respChunk1 = `data: {"chunk":1}` + "\n\n"
	const respChunk2 = `data: {"chunk":2,"secret":"sk-ant-response-secret"}` + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test-Header", "response-header-value")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("httptest ResponseWriter does not support flushing")
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
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer sk-ant-request-secret")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The real caller must still be able to read the full, correct body —
	// tapping must not consume or corrupt the stream it observes.
	gotBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	wantBody := respChunk1 + respChunk2
	if string(gotBody) != wantBody {
		t.Fatalf("response body = %q, want %q", gotBody, wantBody)
	}

	dumped := out.String()

	// Request: header, auth, and body all reach the tap unredacted.
	for _, want := range []string{
		"---- REQUEST ----",
		"Authorization: Bearer sk-ant-request-secret",
		reqBody,
	} {
		if !strings.Contains(dumped, want) {
			t.Errorf("dumped output missing request content %q\nfull dump:\n%s", want, dumped)
		}
	}

	// Response: headers dumped up front, and the full streamed body
	// (across both chunks) reaches the tap unredacted.
	for _, want := range []string{
		"---- RESPONSE HEADERS ----",
		"X-Test-Header: response-header-value",
		"---- RESPONSE BODY ----",
		respChunk1,
		respChunk2,
	} {
		if !strings.Contains(dumped, want) {
			t.Errorf("dumped output missing response content %q\nfull dump:\n%s", want, dumped)
		}
	}
}

// TestWireTapTransportDefaultsWhenNil asserts the nil-base/nil-out
// defaulting documented on WireTapTransport: a nil out falls back to
// something non-panicking (os.Stderr in production; here we only assert
// construction and a round trip do not panic), and a nil base falls back
// to http.DefaultTransport.
func TestWireTapTransportDefaultsWhenNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	rt := WireTapTransport(nil, nil)
	client := &http.Client{Transport: rt}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}

// TestWireTapTransportPropagatesRoundTripError asserts a base transport
// failure (e.g. connection refused) is returned unchanged rather than
// swallowed by the tap.
func TestWireTapTransportPropagatesRoundTripError(t *testing.T) {
	var out bytes.Buffer
	client := &http.Client{Transport: WireTapTransport(http.DefaultTransport, &out)}

	// No listener on this port; the connection must fail.
	_, err := client.Get("http://127.0.0.1:1/does-not-exist")
	if err == nil {
		t.Fatal("expected a round-trip error, got nil")
	}
	if !strings.Contains(out.String(), "---- REQUEST ----") {
		t.Error("expected the request to still be dumped before the failed round trip")
	}
}
