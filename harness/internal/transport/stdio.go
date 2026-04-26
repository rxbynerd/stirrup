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
	startOnce sync.Once                // ensures the read goroutine is started exactly once
	Security  *security.SecurityLogger // optional; emits SecretRedactedInOutput when scrubbing fires
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
// a single JSON line, and writes it to the output stream. When the optional
// Security logger is wired and any redaction occurs, emits a
// SecretRedactedInOutput event with the matched pattern name and a stable
// location string identifying the call site.
func (s *StdioTransport) Emit(event types.HarnessEvent) error {
	// Scrub known secret patterns from all string fields and report stats.
	event.Text = s.scrubAndReport(event.Text, "transport.stdio.event.text")
	event.Content = s.scrubAndReport(event.Content, "transport.stdio.event.content")
	event.Message = s.scrubAndReport(event.Message, "transport.stdio.event.message")

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

// scrubAndReport scrubs s and, if any redaction happened, fires a
// SecretRedactedInOutput event for each distinct pattern that matched.
// Reporting failures (no Security logger) silently skip the event but do not
// affect scrubbing.
func (s *StdioTransport) scrubAndReport(value, location string) string {
	scrubbed, stats := security.ScrubWithStats(value)
	if stats.Count > 0 && s.Security != nil {
		for _, p := range stats.Patterns {
			s.Security.SecretRedactedInOutput(p, location)
		}
	}
	return scrubbed
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
