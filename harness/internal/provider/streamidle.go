package provider

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// defaultStreamIdleTimeout bounds how long a streamed response body may
// stay silent. It is an idle deadline, not a total one: a stream that
// keeps delivering bytes runs for as long as the model keeps generating.
const defaultStreamIdleTimeout = 120 * time.Second

// errStreamIdle matches the read error returned once a streamed body has
// delivered no bytes within its idle timeout.
var errStreamIdle = errors.New("stream idle")

// idleTimeoutBody fails any Read that waits longer than timeout for data,
// closing the wrapped body to unblock it. The timer runs only while a
// Read is in flight, so time the consumer spends between reads (e.g.
// blocked on a full event channel) never counts as provider silence.
//
// Bytes that arrive as the timer fires are still delivered: that Read
// returns them with a nil error and the idle error follows on the next
// Read. A body that reaches io.EOF as the timer fires ends with io.EOF.
type idleTimeoutBody struct {
	body    io.ReadCloser
	timeout time.Duration
	timer   *time.Timer

	closeOnce sync.Once
	closeErr  error

	mu sync.Mutex
	// terminal, once set, is returned by every later Read.
	terminal error
	closed   bool
}

// newIdleTimeoutBody wraps body with an idle-read deadline. A timeout of
// zero or less selects defaultStreamIdleTimeout.
func newIdleTimeoutBody(body io.ReadCloser, timeout time.Duration) *idleTimeoutBody {
	if timeout <= 0 {
		timeout = defaultStreamIdleTimeout
	}
	b := &idleTimeoutBody{body: body, timeout: timeout}
	b.timer = time.AfterFunc(timeout, b.expire)
	b.timer.Stop()
	return b
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.terminal != nil {
		err := b.terminal
		b.mu.Unlock()
		return 0, err
	}
	if b.closed {
		b.mu.Unlock()
		return b.body.Read(p)
	}
	b.timer.Reset(b.timeout)
	b.mu.Unlock()

	n, err := b.body.Read(p)

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timer.Stop() || b.closed {
		return n, err
	}
	// The timer fired while this Read was in flight.
	if errors.Is(err, io.EOF) {
		b.terminal = io.EOF
		return n, io.EOF
	}
	b.terminal = b.idleErr()
	if n > 0 {
		return n, nil
	}
	return 0, b.terminal
}

// Close stops the idle timer and closes the wrapped body. It is safe to
// call concurrently with Read and more than once.
func (b *idleTimeoutBody) Close() error {
	b.mu.Lock()
	b.closed = true
	b.timer.Stop()
	b.mu.Unlock()
	return b.closeBody()
}

func (b *idleTimeoutBody) expire() {
	b.mu.Lock()
	if b.terminal == nil {
		b.terminal = b.idleErr()
	}
	b.mu.Unlock()
	_ = b.closeBody()
}

func (b *idleTimeoutBody) closeBody() error {
	b.closeOnce.Do(func() { b.closeErr = b.body.Close() })
	return b.closeErr
}

func (b *idleTimeoutBody) idleErr() error {
	return fmt.Errorf("%w for %vs", errStreamIdle, b.timeout.Seconds())
}
