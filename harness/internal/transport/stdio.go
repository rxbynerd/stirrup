package transport

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// StdioTransport implements Transport over newline-delimited JSON on
// stdin (control events) and stdout (harness events).
type StdioTransport struct {
	writer    io.Writer
	reader    io.Reader
	mu        sync.Mutex // serialises writes to writer
	handlerMu sync.Mutex // serialises handler registration
	handlers  []func(types.ControlEvent)
	done      chan struct{}
	startOnce sync.Once // ensures the read goroutine is started exactly once
}

// NewStdioTransport creates a StdioTransport that writes harness events to
// w and reads control events from r. Call OnControl before starting the
// read loop.
func NewStdioTransport(w io.Writer, r io.Reader) *StdioTransport {
	return &StdioTransport{
		writer: w,
		reader: r,
		done:   make(chan struct{}),
	}
}

// Emit scrubs secret patterns from the event's string fields, marshals it as
// a single JSON line, and writes it to the output stream.
func (s *StdioTransport) Emit(event types.HarnessEvent) error {
	// Scrub known secret patterns from all string fields.
	event.Text = security.Scrub(event.Text)
	event.Content = security.Scrub(event.Content)
	event.Message = security.Scrub(event.Message)

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal harness event: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data = append(data, '\n')
	_, err = s.writer.Write(data)
	return err
}

// OnControl registers a handler for incoming control events. Multiple calls
// accumulate handlers; all registered handlers are called for each event
// (fan-out). The underlying read goroutine is started on the first call and
// is not restarted on subsequent calls.
func (s *StdioTransport) OnControl(handler func(event types.ControlEvent)) {
	s.handlerMu.Lock()
	s.handlers = append(s.handlers, handler)
	s.handlerMu.Unlock()

	s.startOnce.Do(func() {
		go func() {
			defer close(s.done)
			scanner := bufio.NewScanner(s.reader)
			for scanner.Scan() {
				var ev types.ControlEvent
				if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
					// Skip malformed lines — non-JSON output may be interleaved on stdin.
					continue
				}
				s.handlerMu.Lock()
				hs := make([]func(types.ControlEvent), len(s.handlers))
				copy(hs, s.handlers)
				s.handlerMu.Unlock()
				for _, h := range hs {
					h(ev)
				}
			}
		}()
	})
}

// Close is a no-op for stdio transport. The caller owns the underlying
// reader and writer.
func (s *StdioTransport) Close() error {
	return nil
}
