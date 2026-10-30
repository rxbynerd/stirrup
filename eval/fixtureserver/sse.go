package fixtureserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
)

const (
	// maxStreamFileBytes bounds a canned stream file.
	maxStreamFileBytes = 1 << 20

	// maxRequestBytes bounds the request body the server reads and discards.
	maxRequestBytes = 32 << 20

	// turnSeparator is the line that ends one turn's response in a stream
	// file. It cannot occur inside an SSE record, whose lines all begin with
	// a field name.
	turnSeparator = "---"
)

// LoadTurns reads a canned stream file and returns one response body per
// turn. Turns are separated by a line holding only "---". Each body is raw
// SSE text in the wire format of the provider under test, so the file
// decides how that provider's tool-call arguments are split across events.
func LoadTurns(path string) ([]string, error) {
	if path == "" {
		return nil, fmt.Errorf("fixture server: stream file is required")
	}
	// O_NONBLOCK keeps the open of a FIFO from blocking for a writer.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("fixture server: stream file: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("fixture server: stream file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("fixture server: stream file %q is not a regular file", path)
	}
	// Reading one byte past the cap keeps the bound if the file grows
	// after the stat.
	raw, err := io.ReadAll(io.LimitReader(f, maxStreamFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fixture server: stream file: %w", err)
	}
	if len(raw) > maxStreamFileBytes {
		return nil, fmt.Errorf("fixture server: stream file %q exceeds the %d byte cap", path, maxStreamFileBytes)
	}
	data := strings.ReplaceAll(string(raw), "\r\n", "\n")

	var turns []string
	var current []string
	flush := func() {
		if body := strings.TrimSpace(strings.Join(current, "\n")); body != "" {
			turns = append(turns, body+"\n\n")
		}
		current = nil
	}
	for line := range strings.SplitSeq(data, "\n") {
		if strings.TrimRight(line, " \t") == turnSeparator {
			flush()
			continue
		}
		current = append(current, line)
	}
	flush()

	if len(turns) == 0 {
		return nil, fmt.Errorf("fixture server: stream file %q holds no turn", path)
	}
	return turns, nil
}

// newSSEHandler answers the n-th request with the n-th turn, one flushed
// write per SSE record so the client sees the records as separate network
// reads. A request past the last turn is refused with a non-retryable
// error, so a run that loops longer than its recording fails visibly.
func newSSEHandler(turns []string) http.Handler {
	var served atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, maxRequestBytes))

		n := int(served.Add(1)) - 1
		if n >= len(turns) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{
					"message": fmt.Sprintf("fixture server: request %d but the stream file holds %d turn(s)", n+1, len(turns)),
					"type":    "invalid_request_error",
				},
			})
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for record := range strings.SplitAfterSeq(turns[n], "\n\n") {
			if record == "" {
				continue
			}
			if _, err := io.WriteString(w, record); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
	return mux
}
