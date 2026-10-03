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
	"sync"
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

// lateDataBody blocks every Read until Close, then returns data with
// tailErr, modelling bytes that land at the instant the idle timer fires
// and closes the body.
type lateDataBody struct {
	data    string
	tailErr error
	closed  chan struct{}
	once    sync.Once
}

func newLateDataBody(data string, tailErr error) *lateDataBody {
	return &lateDataBody{data: data, tailErr: tailErr, closed: make(chan struct{})}
}

func (b *lateDataBody) Read(p []byte) (int, error) {
	<-b.closed
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, b.tailErr
}

func (b *lateDataBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestIdleTimeoutBody_DataRacingExpiryIsDelivered(t *testing.T) {
	b := newIdleTimeoutBody(newLateDataBody("late", nil), 20*time.Millisecond)
	buf := make([]byte, 16)

	n, err := b.Read(buf)
	if err != nil || string(buf[:n]) != "late" {
		t.Fatalf("Read = (%q, %v), want the late bytes with a nil error", buf[:n], err)
	}
	if n, err := b.Read(buf); n != 0 || !errors.Is(err, errStreamIdle) {
		t.Fatalf("next Read = (%d, %v), want (0, errStreamIdle)", n, err)
	}
}

func TestIdleTimeoutBody_EOFRacingExpiryIsEOF(t *testing.T) {
	b := newIdleTimeoutBody(newLateDataBody("end", io.EOF), 20*time.Millisecond)
	buf := make([]byte, 16)

	n, err := b.Read(buf)
	if !errors.Is(err, io.EOF) || string(buf[:n]) != "end" {
		t.Fatalf("Read = (%q, %v), want (\"end\", io.EOF)", buf[:n], err)
	}
	if _, err := b.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("next Read = %v, want io.EOF", err)
	}
}

func TestIdleTimeoutBody_ReadAfterClose(t *testing.T) {
	const idle = 10 * time.Millisecond
	body, w := newTrackedPipe()
	defer func() { _ = w.Close() }()
	b := newIdleTimeoutBody(body, idle)
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err := b.Read(make([]byte, 8))
	if err == nil || errors.Is(err, errStreamIdle) {
		t.Errorf("Read after Close = %v, want the body's own close error", err)
	}
	if b.timer.Stop() {
		t.Error("Read after Close armed the idle timer")
	}
	time.Sleep(3 * idle)
	if n := body.closes.Load(); n != 1 {
		t.Errorf("underlying body closed %d times, want 1", n)
	}
}

func TestIdleTimeoutBody_ContextCancelIsNotIdle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeFlush(w, "x")
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	b := newIdleTimeoutBody(resp.Body, time.Hour)
	buf := make([]byte, 8)
	if _, err := b.Read(buf); err != nil {
		t.Fatalf("first Read: %v", err)
	}

	time.AfterFunc(20*time.Millisecond, cancel)
	_, err = b.Read(buf)
	if !errors.Is(err, context.Canceled) || errors.Is(err, errStreamIdle) {
		t.Errorf("Read after cancel = %v, want context.Canceled and not errStreamIdle", err)
	}
	if err := b.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if b.timer.Stop() {
		t.Error("idle timer still armed after Close")
	}
}

func TestIdleTimeoutBody_ConcurrentCloseAndExpiryRace(t *testing.T) {
	const idle = 2 * time.Millisecond
	for i := range 200 {
		body, w := newTrackedPipe()
		go func() {
			for {
				time.Sleep(idle)
				if _, err := w.Write([]byte("x")); err != nil {
					return
				}
			}
		}()

		b := newIdleTimeoutBody(body, idle)
		readerDone := make(chan struct{})
		go func() {
			defer close(readerDone)
			buf := make([]byte, 4)
			for {
				if _, err := b.Read(buf); err != nil {
					return
				}
			}
		}()

		time.Sleep(time.Duration(i%7) * time.Millisecond)
		_ = b.Close()
		select {
		case <-readerDone:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: reader did not stop after Close", i)
		}
		if n := body.closes.Load(); n != 1 {
			t.Fatalf("iteration %d: underlying body closed %d times, want 1", i, n)
		}
		_ = w.Close()
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
		if eb.timer.Stop() {
			t.Fatal("expired body left its timer armed after Close")
		}

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
		if cb.timer.Stop() {
			t.Fatal("completed body left its timer armed after Close")
		}
	}

	// Wait past the timeout so any expiry goroutine has finished.
	time.Sleep(50 * time.Millisecond)
	waitForGoroutines(t, baseline)
}

// idleTestAdapter is one streaming adapter pointed at a test server, with
// its idle timeout shortened and pre-stream retries enabled.
type idleTestAdapter struct {
	client *http.Client
	stream func(ctx context.Context) (<-chan types.StreamEvent, error)
}

// idleTestRetryPolicy allows pre-stream retries, so a test can prove a
// mid-stream failure is never replayed.
var idleTestRetryPolicy = RetryPolicy{MaxAttempts: 3, InitialDelay: time.Millisecond, MaxDelay: time.Millisecond}

// idleStreamCase drives one streaming adapter against an httptest server.
type idleStreamCase struct {
	name string
	// complete is a full, well-formed stream body in the adapter's dialect.
	complete string
	// partial is an event cut off mid-line, as a stall inside a record
	// leaves it.
	partial string
	build   func(url string, idle time.Duration) idleTestAdapter
}

func idleStreamCases() []idleStreamCase {
	return []idleStreamCase{
		{
			name:    "anthropic",
			partial: "event: content_block_delta\n" + `data: {"index":0,"del`,
			complete: joinLines(
				makeSSE("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`),
				makeSSE("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"Hello"}}`),
				makeSSE("content_block_stop", `{"index":0}`),
				makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
				makeSSE("message_stop", `{}`),
			),
			build: func(url string, idle time.Duration) idleTestAdapter {
				a := NewAnthropicAdapter(staticBearer("k"), AuthModeAPIKey)
				a.baseURL = url
				a.streamIdleTimeout = idle
				a.RetryPolicy = idleTestRetryPolicy
				return idleTestAdapter{client: a.httpClient, stream: func(ctx context.Context) (<-chan types.StreamEvent, error) {
					return a.Stream(ctx, types.StreamParams{Model: "claude-sonnet-4-6", MaxTokens: 1024})
				}}
			},
		},
		{
			name:    "openai-compatible",
			partial: `data: {"id":"c","choices":[{"ind`,
			complete: makeOpenAIChunk(`{"id":"c","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`) +
				makeOpenAIChunk(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
				"data: [DONE]\n\n",
			build: func(url string, idle time.Duration) idleTestAdapter {
				a := NewOpenAICompatibleAdapter(staticBearer("k"), url, OpenAIAuthConfig{}, idleTestRetryPolicy)
				a.streamIdleTimeout = idle
				return idleTestAdapter{client: a.httpClient, stream: func(ctx context.Context) (<-chan types.StreamEvent, error) {
					return a.Stream(ctx, types.StreamParams{Model: "gpt-4o", MaxTokens: 1024})
				}}
			},
		},
		{
			name:    "openai-responses",
			partial: "event: response.output_text.delta\n" + `data: {"item_id":"msg_1","del`,
			complete: makeResponsesEvent("response.output_item.added", `{"output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant"}}`) +
				makeResponsesEvent("response.output_text.delta", `{"item_id":"msg_1","output_index":0,"delta":"Hello"}`) +
				makeResponsesEvent("response.completed", `{"response":{"id":"r","status":"completed","output":[{"type":"message","id":"msg_1"}]}}`),
			build: func(url string, idle time.Duration) idleTestAdapter {
				a := NewOpenAIResponsesAdapter(staticBearer("k"), url, OpenAIAuthConfig{})
				a.streamIdleTimeout = idle
				a.RetryPolicy = idleTestRetryPolicy
				return idleTestAdapter{client: a.httpClient, stream: func(ctx context.Context) (<-chan types.StreamEvent, error) {
					return a.Stream(ctx, types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
				}}
			},
		},
		{
			name:    "gemini",
			partial: `data: {"candidates":[{"cont`,
			complete: makeGeminiData(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]}}]}`) +
				makeGeminiData(`{"candidates":[{"finishReason":"STOP"}]}`),
			build: func(url string, idle time.Duration) idleTestAdapter {
				a := newGeminiTestAdapter(url, &stubTokenSource{token: "t"})
				a.streamIdleTimeout = idle
				a.RetryPolicy = idleTestRetryPolicy
				return idleTestAdapter{client: a.httpClient, stream: func(ctx context.Context) (<-chan types.StreamEvent, error) {
					return a.Stream(ctx, types.StreamParams{Model: "gemini-2.5-pro", MaxTokens: 1024})
				}}
			},
		},
	}
}

// writeFlush writes s and flushes it to the client immediately.
func writeFlush(w http.ResponseWriter, s string) {
	_, _ = fmt.Fprint(w, s)
	w.(http.Flusher).Flush()
}

// stallHandler sends preamble, then holds the stream open without writing
// until the client goes away (closing disconnected) or five seconds pass.
// hits counts requests so tests can assert nothing was retried.
func stallHandler(preamble string, hits *atomic.Int32, disconnected chan<- struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeFlush(w, preamble)
		select {
		case <-r.Context().Done():
			if disconnected != nil {
				close(disconnected)
			}
		case <-time.After(5 * time.Second):
		}
	}
}

// assertIdleFailure checks that a stalled stream produced exactly one
// error event, naming the idle timeout, after at least idle has elapsed.
func assertIdleFailure(t *testing.T, events []types.StreamEvent, elapsed, idle time.Duration) {
	t.Helper()
	if len(events) != 1 || events[0].Type != "error" {
		t.Fatalf("events = %+v, want exactly one error event", events)
	}
	if !errors.Is(events[0].Error, errStreamIdle) {
		t.Errorf("error = %v, want errStreamIdle", events[0].Error)
	}
	if want := fmt.Sprintf("stream idle for %vs", idle.Seconds()); !strings.Contains(events[0].Error.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", events[0].Error, want)
	}
	if elapsed < idle || elapsed > 3*time.Second {
		t.Errorf("idle error after %v, want between %v and 3s", elapsed, idle)
	}
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

			ad := tc.build(srv.URL, idle)
			defer ad.client.CloseIdleConnections()
			start := time.Now()
			ch, err := ad.stream(context.Background())
			if err != nil {
				t.Fatalf("Stream() error: %v", err)
			}
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
		for _, stall := range []struct{ name, preamble string }{
			{"between-events", ": keepalive\n\n"},
			{"mid-line", tc.partial},
		} {
			t.Run(tc.name+"/"+stall.name, func(t *testing.T) {
				var hits atomic.Int32
				disconnected := make(chan struct{})
				srv := httptest.NewServer(stallHandler(stall.preamble, &hits, disconnected))
				defer srv.Close()

				ad := tc.build(srv.URL, idle)
				defer ad.client.CloseIdleConnections()
				start := time.Now()
				ch, err := ad.stream(context.Background())
				if err != nil {
					t.Fatalf("Stream() error: %v", err)
				}
				events := collectEvents(t, ch)
				assertIdleFailure(t, events, time.Since(start), idle)

				select {
				case <-disconnected:
				case <-time.After(2 * time.Second):
					t.Error("server never observed the client closing the stalled stream")
				}
				if n := hits.Load(); n != 1 {
					t.Errorf("server saw %d requests, want 1: a mid-stream idle failure must not be retried", n)
				}
			})
		}
	}
}

// A Responses record may span several data lines. A stall after a complete
// line but before the record's terminating blank line leaves buffered data
// that is not a whole record; it must not be dispatched ahead of the idle
// error.
func TestOpenAIResponsesAdapter_StallMidRecordFailsWithIdleError(t *testing.T) {
	const idle = 150 * time.Millisecond
	var hits atomic.Int32
	srv := httptest.NewServer(stallHandler(
		"event: response.output_text.delta\n"+`data: {"item_id":"msg_1",`+"\n",
		&hits, nil,
	))
	defer srv.Close()

	a := NewOpenAIResponsesAdapter(staticBearer("k"), srv.URL, OpenAIAuthConfig{})
	a.streamIdleTimeout = idle
	defer a.httpClient.CloseIdleConnections()
	start := time.Now()
	ch, err := a.Stream(context.Background(), types.StreamParams{Model: "gpt-4.1", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	assertIdleFailure(t, collectEvents(t, ch), time.Since(start), idle)
}

func TestStreamingAdapters_CancelInsideIdleWindowIsNotIdle(t *testing.T) {
	const idle = 2 * time.Second
	for _, tc := range idleStreamCases() {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(stallHandler(": keepalive\n\n", &hits, nil))
			defer srv.Close()

			ad := tc.build(srv.URL, idle)
			defer ad.client.CloseIdleConnections()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			start := time.Now()
			ch, err := ad.stream(ctx)
			if err != nil {
				t.Fatalf("Stream() error: %v", err)
			}
			time.AfterFunc(50*time.Millisecond, cancel)
			events := collectEvents(t, ch)

			if elapsed := time.Since(start); elapsed >= idle {
				t.Fatalf("stream ended after %v, not inside the %v idle window", elapsed, idle)
			}
			for _, ev := range events {
				if errors.Is(ev.Error, errStreamIdle) {
					t.Errorf("cancelled stream reported an idle timeout: %v", ev.Error)
				}
			}
		})
	}
}

func TestStreamingAdapters_IdleTimeoutOverHTTP2(t *testing.T) {
	const idle = 150 * time.Millisecond
	for _, tc := range idleStreamCases() {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			disconnected := make(chan struct{})
			stall := stallHandler(": keepalive\n\n", &hits, disconnected)
			srv := newHTTP2TLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor != 2 {
					t.Errorf("request arrived over %s, want HTTP/2", r.Proto)
				}
				stall(w, r)
			}))

			ad := tc.build(srv.URL, idle)
			trustServer(t, ad.client, srv)
			defer ad.client.CloseIdleConnections()
			start := time.Now()
			ch, err := ad.stream(context.Background())
			if err != nil {
				t.Fatalf("Stream() error: %v", err)
			}
			assertIdleFailure(t, collectEvents(t, ch), time.Since(start), idle)

			select {
			case <-disconnected:
			case <-time.After(2 * time.Second):
				t.Error("server never observed the client resetting the stalled HTTP/2 stream")
			}
		})
	}
}

func TestStreamingAdapters_IdleTimeoutLeavesNoGoroutines(t *testing.T) {
	const idle = 100 * time.Millisecond
	baseline := runtime.NumGoroutine()

	for _, tc := range idleStreamCases() {
		func() {
			var stall atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				if !stall.Load() {
					writeFlush(w, tc.complete)
					return
				}
				writeFlush(w, ": keepalive\n\n")
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
			}))
			defer srv.Close()

			for _, stalled := range []bool{true, false, true, false} {
				stall.Store(stalled)
				ad := tc.build(srv.URL, idle)
				ch, err := ad.stream(context.Background())
				if err != nil {
					t.Fatalf("%s: Stream() error: %v", tc.name, err)
				}
				events := collectEvents(t, ch)
				ad.client.CloseIdleConnections()
				if stalled && (len(events) != 1 || !errors.Is(events[0].Error, errStreamIdle)) {
					t.Fatalf("%s: stalled stream events = %+v, want one idle error", tc.name, events)
				}
			}
		}()
	}

	waitForGoroutines(t, baseline)
}
