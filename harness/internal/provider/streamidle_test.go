package provider

import (
	"context"
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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
			require.Failf(t, "goroutines did not settle", "have %d, baseline %d\n%s", n, baseline, buf)
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
	require.NoError(t, err, "ReadAll after %v", time.Since(start))
	chunks := strings.Count(string(data), "chunk\n")
	assert.Equal(t, 12, chunks)
	elapsed := time.Since(start)
	assert.Greater(t, elapsed, idle, "drip must outlast the idle timeout, or the test proves nothing")
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

	assert.Equal(t, "first\n", string(data), "data delivered before the stall")
	require.ErrorIs(t, err, errStreamIdle)
	assert.Equal(t, "stream idle for 0.1s", err.Error())
	assert.GreaterOrEqual(t, elapsed, idle, "idle error must not fire before the timeout")
	assert.EqualValues(t, 1, body.closes.Load(), "underlying body closed once on expiry")

	_, err = b.Read(make([]byte, 8))
	require.ErrorIs(t, err, errStreamIdle, "Read after expiry returns the idle error")
	require.NoError(t, b.Close(), "Close after expiry")
	assert.EqualValues(t, 1, body.closes.Load(), "underlying body closed once after caller Close")
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
	_, err := b.Read(buf)
	require.NoError(t, err, "first Read")
	time.Sleep(3 * idle)
	_, err = b.Read(buf)
	require.NoError(t, err, "Read after a consumer-side pause should return data")
	require.Equal(t, "b", string(buf))
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
	require.NoError(t, b.Close())

	select {
	case err := <-errc:
		require.Error(t, err, "Read unblocked by caller Close should return the body's close error")
		require.NotErrorIs(t, err, errStreamIdle)
	case <-time.After(2 * time.Second):
		require.Fail(t, "Read did not unblock after Close")
	}
}

func TestIdleTimeoutBody_ZeroTimeoutSelectsDefault(t *testing.T) {
	body, w := newTrackedPipe()
	defer func() { _ = w.Close() }()
	b := newIdleTimeoutBody(body, 0)
	defer func() { _ = b.Close() }()
	assert.Equal(t, defaultStreamIdleTimeout, b.timeout)
	assert.EqualError(t, b.idleErr(), "stream idle for 120s")
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
	require.NoError(t, err)
	require.Equal(t, "late", string(buf[:n]))
	n, err = b.Read(buf)
	require.Zero(t, n)
	require.ErrorIs(t, err, errStreamIdle)
}

func TestIdleTimeoutBody_EOFRacingExpiryIsEOF(t *testing.T) {
	b := newIdleTimeoutBody(newLateDataBody("end", io.EOF), 20*time.Millisecond)
	buf := make([]byte, 16)

	n, err := b.Read(buf)
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, "end", string(buf[:n]))
	_, err = b.Read(buf)
	require.ErrorIs(t, err, io.EOF)
}

func TestIdleTimeoutBody_ReadAfterCloseIsNotIdle(t *testing.T) {
	const idle = 10 * time.Millisecond
	body, w := newTrackedPipe()
	defer func() { _ = w.Close() }()
	b := newIdleTimeoutBody(body, idle)
	require.NoError(t, b.Close())

	_, err := b.Read(make([]byte, 8))
	require.Error(t, err)
	require.NotErrorIs(t, err, errStreamIdle, "Read after Close returns the body's own close error")
	assert.False(t, b.timer.Stop(), "Read after Close must not arm the idle timer")
	time.Sleep(3 * idle)
	assert.EqualValues(t, 1, body.closes.Load(), "underlying body closed once")
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
	require.NoError(t, err)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	b := newIdleTimeoutBody(resp.Body, time.Hour)
	buf := make([]byte, 8)
	_, err = b.Read(buf)
	require.NoError(t, err, "first Read")

	time.AfterFunc(20*time.Millisecond, cancel)
	_, err = b.Read(buf)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, errStreamIdle)
	require.NoError(t, b.Close())
	assert.False(t, b.timer.Stop(), "idle timer must be disarmed after Close")
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
			require.Failf(t, "reader did not stop after Close", "iteration %d", i)
		}
		require.EqualValues(t, 1, body.closes.Load(), "iteration %d: underlying body closes", i)
		_ = w.Close()
	}
}

func TestIdleTimeoutBody_NoGoroutineOrTimerLeak(t *testing.T) {
	baseline := runtime.NumGoroutine()

	for range 20 {
		expiring, ew := newTrackedPipe()
		eb := newIdleTimeoutBody(expiring, 10*time.Millisecond)
		_, err := io.ReadAll(eb)
		require.ErrorIs(t, err, errStreamIdle, "expiring body")
		_ = eb.Close()
		_ = ew.Close()
		require.False(t, eb.timer.Stop(), "expired body left its timer armed after Close")

		completing, cw := newTrackedPipe()
		go func() {
			_, _ = cw.Write([]byte("done"))
			_ = cw.Close()
		}()
		cb := newIdleTimeoutBody(completing, 10*time.Millisecond)
		_, err = io.ReadAll(cb)
		require.NoError(t, err, "completing body")
		_ = cb.Close()
		require.False(t, cb.timer.Stop(), "completed body left its timer armed after Close")
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
	require.Len(t, events, 1, "events = %+v, want exactly one error event", events)
	require.Equal(t, "error", events[0].Type, "events = %+v", events)
	require.ErrorIs(t, events[0].Error, errStreamIdle)
	assert.True(t, types.IsRetryable(events[0].Error), "error %v should be retryable for the loop's turn retry", events[0].Error)
	require.ErrorContains(t, events[0].Error, fmt.Sprintf("stream idle for %vs", idle.Seconds()))
	assert.GreaterOrEqual(t, elapsed, idle, "idle error must not fire before the timeout")
	assert.LessOrEqual(t, elapsed, 3*time.Second, "idle error must fire within 3s")
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
			require.NoError(t, err, "Stream()")
			events := collectEvents(t, ch)
			elapsed := time.Since(start)

			require.Greater(t, elapsed, idle, "stream must outlast the idle timeout, or the test proves nothing")
			for _, ev := range events {
				require.NotEqual(t, "error", ev.Type, "unexpected error event after %v: %v", elapsed, ev.Error)
			}
			require.NotEmpty(t, events, "want a trailing message_complete")
			require.Equal(t, "message_complete", events[len(events)-1].Type, "events = %+v", events)
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
				require.NoError(t, err, "Stream()")
				events := collectEvents(t, ch)
				assertIdleFailure(t, events, time.Since(start), idle)

				select {
				case <-disconnected:
				case <-time.After(2 * time.Second):
					require.Fail(t, "server never observed the client closing the stalled stream")
				}
				require.EqualValues(t, 1, hits.Load(), "server must see one request: the adapter must not replay a mid-stream idle failure")
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
	require.NoError(t, err, "Stream()")
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
			require.NoError(t, err, "Stream()")
			time.AfterFunc(50*time.Millisecond, cancel)
			events := collectEvents(t, ch)

			elapsed := time.Since(start)
			require.Less(t, elapsed, idle, "stream must end inside the idle window")
			for _, ev := range events {
				require.NotErrorIs(t, ev.Error, errStreamIdle, "cancelled stream must not report an idle timeout")
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
				assert.Equal(t, 2, r.ProtoMajor, "request arrived over %s, want HTTP/2", r.Proto)
				stall(w, r)
			}))

			ad := tc.build(srv.URL, idle)
			trustServer(t, ad.client, srv)
			defer ad.client.CloseIdleConnections()
			start := time.Now()
			ch, err := ad.stream(context.Background())
			require.NoError(t, err, "Stream()")
			assertIdleFailure(t, collectEvents(t, ch), time.Since(start), idle)

			select {
			case <-disconnected:
			case <-time.After(2 * time.Second):
				require.Fail(t, "server never observed the client resetting the stalled HTTP/2 stream")
			}
		})
	}
}

func TestStreamingAdapters_IdleTimeoutLeavesNoGoroutines(t *testing.T) {
	const idle = 100 * time.Millisecond
	baseline := runtime.NumGoroutine()

	for _, tc := range idleStreamCases() {
		t.Run(tc.name, func(t *testing.T) {
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
				require.NoError(t, err)
				events := collectEvents(t, ch)
				ad.client.CloseIdleConnections()
				if stalled {
					require.Len(t, events, 1, "stalled stream events = %+v", events)
					require.ErrorIs(t, events[0].Error, errStreamIdle)
				}
			}
		})
	}

	waitForGoroutines(t, baseline)
}
