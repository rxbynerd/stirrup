// Command stubprovider is a scripted OpenAI-compatible Chat Completions
// server for exercising the harness without an API key or network access.
// In "success" mode every request is answered with one streamed final
// assistant message; in "empty" mode with a streamed message that ends the
// turn without any content; in "error" mode every request is rejected with
// a 400.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

const finalMessage = "Hello from the scripted provider."

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("stubprovider", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:0", "address to serve on")
	mode := fs.String("mode", "success", "scripted behaviour: success, empty or error")
	addrFile := fs.String("addr-file", "", "write the bound listen address to this file once listening")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *mode != "success" && *mode != "empty" && *mode != "error" {
		writef(stderr, "stubprovider: invalid -mode %q: want success, empty or error\n", *mode)
		return 2
	}

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		writef(stderr, "stubprovider: %v\n", err)
		return 2
	}
	if *addrFile != "" {
		if err := os.WriteFile(*addrFile, []byte(lis.Addr().String()), 0o600); err != nil {
			writef(stderr, "stubprovider: %v\n", err)
			return 2
		}
	}

	srv := &http.Server{
		Handler:           newHandler(*mode),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
		writef(stderr, "stubprovider: %v\n", err)
		return 1
	}
	return 0
}

func newHandler(mode string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /chat/completions", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if mode == "error" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{"message": "scripted provider failure", "type": "invalid_request_error"},
			})
			return
		}
		content := finalMessage
		if mode == "empty" {
			content = ""
		}
		writeFinalMessage(w, content)
	})
	return mux
}

// writeFinalMessage streams one assistant message that ends the turn, in the
// chunk sequence real servers use: content delta, finish_reason, usage, then
// the [DONE] terminator.
func writeFinalMessage(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	chunks := []any{
		map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"role": "assistant", "content": content}, "finish_reason": nil,
		}}},
		map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
		}}},
		map[string]any{"choices": []any{}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 7}},
	}
	for _, c := range chunks {
		b, _ := json.Marshal(c)
		writef(w, "data: %s\n\n", b)
	}
	writef(w, "data: [DONE]\n\n")
}

func writef(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}
