package transport

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/rubynerd/stirrup/types"
)

// StdioTransport implements Transport over newline-delimited JSON on
// stdin (control events) and stdout (harness events).
type StdioTransport struct {
	writer  io.Writer
	reader  io.Reader
	mu      sync.Mutex // serialises writes to writer
	handler func(event types.ControlEvent)
	done    chan struct{}
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

// Emit marshals the event as a single JSON line and writes it to the output
// stream.
func (s *StdioTransport) Emit(event types.HarnessEvent) error {
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

// OnControl registers a handler for incoming control events and starts a
// goroutine that reads JSON lines from the input stream. The handler is
// called synchronously for each parsed event.
func (s *StdioTransport) OnControl(handler func(event types.ControlEvent)) {
	s.handler = handler

	go func() {
		defer close(s.done)
		scanner := bufio.NewScanner(s.reader)
		for scanner.Scan() {
			var ev types.ControlEvent
			if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
				continue // skip malformed lines
			}
			if s.handler != nil {
				s.handler(ev)
			}
		}
	}()
}

// Close is a no-op for stdio transport. The caller owns the underlying
// reader and writer.
func (s *StdioTransport) Close() error {
	return nil
}
