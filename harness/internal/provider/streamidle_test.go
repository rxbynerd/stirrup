package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// trackedBody is an io.ReadCloser over an io.Pipe that counts Close calls,
// so tests can assert the idle wrapper releases the underlying body.
type trackedBody struct {
	*io.PipeReader
	closes atomic.Int32
}

func (b *trackedBody) Close() error {
	b.closes.Add(1)
	return b.PipeReader.Close()
}

func newTrackedPipe() (*trackedBody, *io.PipeWriter) {
	r, w := io.Pipe()
	return &trackedBody{PipeReader: r}, w
}

// waitForGoroutines fails the test unless the goroutine count settles at or
// below baseline within a bounded window.
func waitForGoroutines(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		n := runtime.NumGoroutine()
		if n <= baseline {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			buf = buf[:runtime.Stack(buf, true)]
			t.Fatalf("goroutines did not settle: have %d, baseline %d\n%s", n, baseline, buf)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestIdleTimeoutBody_SteadyDripOutlivesTimeout(t *testing.T) {
	const idle = 150 * time.Millisecond
	body, w := newTrackedPipe()
	go func() {
		for range 12 { // 12 x 50 ms = 4x the idle timeout
			time.Sleep(50 * time.Millisecond)
			if _, err := w.Write([]byte("chunk\n")); err != nil {
				return
			}
		}
		_ = w.Close()
	}()

	b := newIdleTimeoutBody(body, idle)
	start := time.Now()
	data, err := io.ReadAll(b)
	if err != nil {
		t.Fatalf("ReadAll error after %v: %v", time.Since(start), err)
	}
	if got := strings.Count(string(data), "chunk\n"); got != 12 {
		t.Errorf("read %d chunks, want 12", got)
	}
	if elapsed := time.Since(start); elapsed <= idle {
		t.Errorf("drip finished in %v, not longer than the %v idle timeout; test proves nothing", elapsed, idle)
	}
	_ = b.Close()
}

func TestIdleTimeoutBody_StallFailsAndClosesBody(t *testing.T) {
	const idle = 100 * time.Millisecond
	body, w := newTrackedPipe()
	defer func() { _ = w.Close() }()
	go func() { _, _ = w.Write([]byte("first\n")) }()

	b := newIdleTimeoutBody(body, idle)
	start := time.Now()
	data, err := io.ReadAll(b)
	elapsed := time.Since(start)

	if string(data) != "first\n" {
		t.Errorf("data = %q, want the bytes delivered before the stall", data)
	}
	if !errors.Is(err, errStreamIdle) {
		t.Fatalf("err = %v, want errStreamIdle", err)
	}
	if got, want := err.Error(), "stream idle for 0.1s"; got != want {
		t.Errorf("err message = %q, want %q", got, want)
	}
	if elapsed < idle {
		t.Errorf("idle error after %v, before the %v timeout", elapsed, idle)
	}
	if n := body.closes.Load(); n != 1 {
		t.Errorf("underlying body closed %d times on expiry, want 1", n)
	}

	if _, err := b.Read(make([]byte, 8)); !errors.Is(err, errStreamIdle) {
		t.Errorf("Read after expiry = %v, want sticky errStreamIdle", err)
	}
	if err := b.Close(); err != nil {
		t.Errorf("Close after expiry: %v", err)
	}
	if n := body.closes.Load(); n != 1 {
		t.Errorf("underlying body closed %d times after caller Close, want 1", n)
	}
}

func TestIdleTimeoutBody_ConsumerPauseIsNotIdleness(t *testing.T) {
	const idle = 80 * time.Millisecond
	body, w := newTrackedPipe()
	go func() {
		_, _ = w.Write([]byte("a"))
		_, _ = w.Write([]byte("b"))
		_ = w.Close()
	}()

	b := newIdleTimeoutBody(body, idle)
	defer func() { _ = b.Close() }()
	buf := make([]byte, 1)
	if _, err := b.Read(buf); err != nil {
		t.Fatalf("first Read: %v", err)
	}
	time.Sleep(3 * idle)
	if _, err := b.Read(buf); err != nil {
		t.Fatalf("Read after a consumer-side pause: %v, want data", err)
	}
	if string(buf) != "b" {
		t.Errorf("second Read = %q, want %q", buf, "b")
	}
}

func TestIdleTimeoutBody_CallerCloseDuringReadIsNotIdle(t *testing.T) {
	body, w := newTrackedPipe()
	defer func() { _ = w.Close() }()
	b := newIdleTimeoutBody(body, time.Hour)

	errc := make(chan error, 1)
	go func() {
		_, err := b.Read(make([]byte, 8))
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-errc:
		if err == nil || errors.Is(err, errStreamIdle) {
			t.Errorf("Read unblocked by caller Close returned %v, want the body's own close error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not unblock after Close")
	}
}

func TestIdleTimeoutBody_ZeroTimeoutSelectsDefault(t *testing.T) {
	body, w := newTrackedPipe()
	defer func() { _ = w.Close() }()
	b := newIdleTimeoutBody(body, 0)
	defer func() { _ = b.Close() }()
	if b.timeout != defaultStreamIdleTimeout {
		t.Errorf("timeout = %v, want %v", b.timeout, defaultStreamIdleTimeout)
	}
	if got, want := b.idleErr().Error(), "stream idle for 120s"; got != want {
		t.Errorf("idle message = %q, want %q", got, want)
	}
}

func TestIdleTimeoutBody_NoGoroutineOrTimerLeak(t *testing.T) {
	baseline := runtime.NumGoroutine()

	for range 20 {
		expiring, ew := newTrackedPipe()
		eb := newIdleTimeoutBody(expiring, 10*time.Millisecond)
		if _, err := io.ReadAll(eb); !errors.Is(err, errStreamIdle) {
			t.Fatalf("expiring body: err = %v, want errStreamIdle", err)
		}
		_ = eb.Close()
		_ = ew.Close()

		completing, cw := newTrackedPipe()
		go func() {
			_, _ = cw.Write([]byte("done"))
			_ = cw.Close()
		}()
		cb := newIdleTimeoutBody(completing, 10*time.Millisecond)
		if _, err := io.ReadAll(cb); err != nil {
			t.Fatalf("completing body: %v", err)
		}
		_ = cb.Close()
	}

	// Any timer left armed after Close would fire here and bump a close
	// count; waiting past the timeout also lets expiry goroutines finish.
	time.Sleep(50 * time.Millisecond)
	waitForGoroutines(t, baseline)
}

// idleStreamCase drives one streaming adapter against an httptest server.
type idleStreamCase struct {
	name string
	// complete is a full, well-formed stream body in the adapter's dialect.
	complete string
	stream   func(url string, idle time.Duration) (<-chan types.StreamEvent, func(), error)
}

func idleStreamCases() []idleStreamCase {
	return []idleStreamCase{
		{
			name: "anthropic",
			complete: joinLines(
				makeSSE("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`),
				makeSSE("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"Hello"}}`),
				makeSSE("content_block_stop", `{"index":0}`),
				makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
				makeSSE("message_stop", `{}`),
			),
			stream: func(url string, idle time.Duration) (<-chan types.StreamEvent, func(), error) {
				a := NewAnthropicAdapter(staticBearer("k"), AuthModeAPIKey)
				a.baseURL = url
				a.streamIdleTimeout = idle
				ch, err := a.Stream(context.Background(), types.StreamParams{Model: "claude-sonnet-4-6", MaxTokens: 1024})
				return ch, a.httpClient.CloseIdleConnections, err
			},
		},
		{
			name: "openai-compatible",
			complete: makeOpenAIChunk(`{"id":"c","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`) +
				makeOpenAIChunk(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
				"data: [DONE]\n\n",
			stream: func(url string, idle time.Duration) (<-chan types.StreamEvent, func(), error) {
				a := NewOpenAICompatibleAdapter(staticBearer("k"), url, OpenAIAuthConfig{}, RetryPolicy{})
				a.streamIdleTimeout = idle
				ch, err := a.Stream(context.Background(), types.StreamParams{Model: "gpt-4o", MaxTokens: 1024})
				return ch, a.httpClient.CloseIdleConnections, err
			},
		},
		{
			name: "openai-responses",
			complete: makeResponsesEvent("response.output_item.added", `{"output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant"}}`) +
				makeResponsesEvent("response.output_text.delta", `{"item_id":"msg_1","output_index":0,"delta":"Hello"}`) +
				makeResponsesEvent("response.completed", `{"response":{"id":"r","status":"completed","output":[{"type":"message","id":"msg_1"}]}}`),
			stream: func(url string, idle time.Duration) (<-chan types.StreamEvent, func(), error) {
				a := NewOpenAIResponsesAdapter(staticBearer("k"), url, OpenAIAuthConfig{})
				a.streamIdleTimeout = idle
				ch, err := a.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
				return ch, a.httpClient.CloseIdleConnections, err
			},
		},
		{
			name: "gemini",
			complete: makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]}}]}`) +
				makeGeminiData(`{"candidates":[{"finishReason":"STOP"}]}`),
			stream: func(url string, idle time.Duration) (<-chan types.StreamEvent, func(), error) {
				a := newGeminiTestAdapter(url, &stubTokenSource{token: "t"})
				a.streamIdleTimeout = idle
				ch, err := a.Stream(context.Background(), types.StreamParams{Model: "gemini-2.5-pro", MaxTokens: 1024})
				return ch, a.httpClient.CloseIdleConnections, err
			},
		},
	}
}

// writeFlush writes s and flushes it to the client immediately.
func writeFlush(w http.ResponseWriter, s string) {
	_, _ = fmt.Fprint(w, s)
	w.(http.Flusher).Flush()
}

func TestStreamingAdapters_SlowSteadyStreamOutlivesIdleTimeout(t *testing.T) {
	const idle = 150 * time.Millisecond
	for _, tc := range idleStreamCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				for range 12 { // 12 x 50 ms = 4x the idle timeout
					writeFlush(w, ": keepalive\n\n")
					time.Sleep(50 * time.Millisecond)
				}
				writeFlush(w, tc.complete)
			}))
			defer srv.Close()

			start := time.Now()
			ch, closeIdle, err := tc.stream(srv.URL, idle)
			if err != nil {
				t.Fatalf("Stream() error: %v", err)
			}
			defer closeIdle()
			events := collectEvents(t, ch)
			elapsed := time.Since(start)

			if elapsed <= idle {
				t.Fatalf("stream finished in %v, not longer than the %v idle timeout; test proves nothing", elapsed, idle)
			}
			for _, ev := range events {
				if ev.Type == "error" {
					t.Fatalf("unexpected error event after %v: %v", elapsed, ev.Error)
				}
			}
			if len(events) == 0 || events[len(events)-1].Type != "message_complete" {
				t.Fatalf("events = %+v, want a trailing message_complete", events)
			}
		})
	}
}

func TestStreamingAdapters_StalledStreamFailsWithIdleError(t *testing.T) {
	const idle = 150 * time.Millisecond
	for _, tc := range idleStreamCases() {
		t.Run(tc.name, func(t *testing.T) {
			disconnected := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				writeFlush(w, ": keepalive\n\n")
				select {
				case <-r.Context().Done():
					close(disconnected)
				case <-time.After(5 * time.Second):
				}
			}))
			defer srv.Close()

			start := time.Now()
			ch, closeIdle, err := tc.stream(srv.URL, idle)
			if err != nil {
				t.Fatalf("Stream() error: %v", err)
			}
			defer closeIdle()
			events := collectEvents(t, ch)
			elapsed := time.Since(start)

			if len(events) != 1 || events[0].Type != "error" {
				t.Fatalf("events = %+v, want exactly one error event", events)
			}
			if !errors.Is(events[0].Error, errStreamIdle) {
				t.Errorf("error = %v, want errStreamIdle", events[0].Error)
			}
			if !strings.Contains(events[0].Error.Error(), "stream idle for 0.15s") {
				t.Errorf("error = %q, want it to name the idle timeout", events[0].Error)
			}
			if elapsed < idle || elapsed > 3*time.Second {
				t.Errorf("idle error after %v, want between %v and 3s", elapsed, idle)
			}
			select {
			case <-disconnected:
			case <-time.After(2 * time.Second):
				t.Error("server never observed the client closing the stalled stream")
			}
		})
	}
}

func TestStreamingAdapters_IdleTimeoutLeavesNoGoroutines(t *testing.T) {
	const idle = 100 * time.Millisecond
	baseline := runtime.NumGoroutine()

	func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if !strings.HasSuffix(r.URL.Path, "/stall") {
				writeFlush(w, makeSSE("message_stop", `{}`))
				return
			}
			writeFlush(w, ": keepalive\n\n")
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}))
		defer srv.Close()

		for _, path := range []string{"/stall", "/ok", "/stall", "/ok"} {
			a := NewAnthropicAdapter(staticBearer("k"), AuthModeAPIKey)
			a.baseURL = srv.URL + path
			a.streamIdleTimeout = idle
			ch, err := a.Stream(context.Background(), types.StreamParams{Model: "claude-sonnet-4-6", MaxTokens: 1024})
			if err != nil {
				t.Fatalf("Stream(%s) error: %v", path, err)
			}
			events := collectEvents(t, ch)
			a.httpClient.CloseIdleConnections()
			if path == "/stall" && (len(events) != 1 || !errors.Is(events[0].Error, errStreamIdle)) {
				t.Fatalf("stalled stream events = %+v, want one idle error", events)
			}
		}
	}()

	waitForGoroutines(t, baseline)
}
