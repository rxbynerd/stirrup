package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	harnessv1 "github.com/rxbynerd/stirrup/gen/harness/v1"
)

// maxToolResultPreview bounds how much of a tool result is echoed to the
// terminal; the full content stays on the wire.
const maxToolResultPreview = 200

// permissionMode selects how permission_request events are answered.
type permissionMode string

const (
	permissionAllow permissionMode = "allow"
	permissionDeny  permissionMode = "deny"
	permissionAsk   permissionMode = "ask"
)

func parsePermissionMode(s string) (permissionMode, error) {
	switch m := permissionMode(s); m {
	case permissionAllow, permissionDeny, permissionAsk:
		return m, nil
	}
	return "", fmt.Errorf("invalid -permission %q: want allow, deny or ask", s)
}

// loadRunConfig reads a RunConfig JSON file, the same format accepted by
// `stirrup harness --config`, into its wire representation. The "transport"
// key is dropped because the wire RunConfig has no such field: a job always
// reports over the stream it is connected to. Any other unknown field is
// rejected so that a typo fails here rather than silently reaching the
// harness.
func loadRunConfig(path string) (*harnessv1.RunConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read run config: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("parse run config %s: %w", path, err)
	}
	delete(fields, "transport")
	if data, err = json.Marshal(fields); err != nil {
		return nil, fmt.Errorf("parse run config %s: %w", path, err)
	}
	cfg := &harnessv1.RunConfig{}
	if err := protojson.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse run config %s: %w", path, err)
	}
	return cfg, nil
}

// runStream is the subset of the generated server stream the handler uses,
// which lets tests drive it without a network.
type runStream interface {
	Send(*harnessv1.ControlEvent) error
	Recv() (*harnessv1.HarnessEvent, error)
}

// result is the terminal state of the session's runs.
type result struct {
	// Outcome is the last run's outcome, carried by its done event. The
	// harness puts it in stop_reason; trace.outcome is preferred when a
	// trace is attached.
	Outcome string
	// Outcomes holds every run's outcome in order, follow-ups included.
	Outcomes []string
}

// Succeeded reports whether every run ended successfully.
func (r result) Succeeded() bool {
	for _, o := range r.Outcomes {
		if o != "success" {
			return false
		}
	}
	return r.Outcome == "success"
}

// server serves exactly one session: the first harness to connect receives
// the task, each follow-up prompt starts another run after a done, and the
// session's result is delivered on results. A second concurrent stream is
// refused.
type server struct {
	harnessv1.UnimplementedHarnessServiceServer

	task       *harnessv1.RunConfig
	followUps  []string
	permission permissionMode
	stdin      *bufio.Reader
	out        io.Writer
	errOut     io.Writer

	mu      sync.Mutex
	claimed bool
	results chan runOutcome
}

// runOutcome is what a finished RunTask hands back to main: either a result
// from a done event or the error that ended the stream without one.
type runOutcome struct {
	result result
	err    error
}

func newServer(task *harnessv1.RunConfig, mode permissionMode, stdin io.Reader, out, errOut io.Writer) *server {
	return &server{
		task:       task,
		permission: mode,
		stdin:      bufio.NewReader(stdin),
		out:        out,
		errOut:     errOut,
		results:    make(chan runOutcome, 1),
	}
}

// RunTask implements the HarnessService stream for a single run.
func (s *server) RunTask(stream harnessv1.HarnessService_RunTaskServer) error {
	s.mu.Lock()
	if s.claimed {
		s.mu.Unlock()
		return status.Error(codes.FailedPrecondition, "this control plane serves a single run")
	}
	s.claimed = true
	s.mu.Unlock()

	res, err := s.handle(stream.Context(), stream)
	s.results <- runOutcome{result: res, err: err}
	return nil
}

// handle drives one session: wait for ready, assign the task, answer
// permission requests and print events until done, then send each follow-up
// prompt as a user_response and repeat until the last run's done. A stream
// that closes before that done is reported as an error, since it means the
// harness exited abnormally or closed the follow-up window.
func (s *server) handle(ctx context.Context, stream runStream) (result, error) {
	var res result
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			if len(res.Outcomes) > 0 {
				return res, fmt.Errorf("stream closed before follow-up run %d sent its done event", len(res.Outcomes)+1)
			}
			return res, errors.New("stream closed before the done event")
		}
		if err != nil {
			return res, fmt.Errorf("receive: %w", err)
		}

		switch ev.GetType() {
		case "ready":
			writef(s.errOut, "[ready] harness %s connected (capabilities: %s; config fields: %d); assigning run %s\n",
				escapeLine(ev.GetHarnessVersion()), escapeLine(strings.Join(ev.GetCapabilities(), ", ")), len(ev.GetConfigFields()), escapeLine(s.task.GetRunId()))
			if err := stream.Send(&harnessv1.ControlEvent{Type: "task_assignment", Task: s.task}); err != nil {
				return res, fmt.Errorf("send task_assignment: %w", err)
			}
		case "run_started":
			writef(s.errOut, "[run_started] %s\n", escapeLine(ev.GetId()))
		case "user_message":
			writef(s.errOut, "[user_message] %s: %s\n", escapeLine(ev.GetSource()), preview(ev.GetText()))
		case "turn_started":
			writef(s.errOut, "[turn_started] %d %s/%s\n", ev.GetTurn(), escapeLine(ev.GetProvider()), escapeLine(ev.GetModel()))
		case "thinking_delta":
			writef(s.errOut, "%s", escapeText(ev.GetText()))
		case "text_delta":
			writef(s.out, "%s", escapeText(ev.GetText()))
		case "tool_call":
			writef(s.errOut, "\n[tool_call] %s %s\n", escapeLine(ev.GetName()), displayInput(ev.GetInput()))
		case "tool_result":
			status := "ok"
			if ev.GetIsError() {
				status = "error"
			}
			writef(s.errOut, "[tool_result] %s %s %dms %s\n", escapeLine(ev.GetName()), status, ev.GetDurationMs(), preview(ev.GetContent()))
		case "tools_loaded":
			writef(s.errOut, "[tools_loaded] %s %s %s\n", escapeLine(ev.GetSource()), escapeLine(ev.GetName()), preview(strings.Join(ev.GetTools(), ", ")))
		case "turn_committed":
			usage := ev.GetUsage()
			writef(s.errOut, "\n[turn_committed] %d %s in=%d out=%d\n", ev.GetTurn(), escapeLine(ev.GetStopReason()), usage.GetInputTokens(), usage.GetOutputTokens())
		case "turn_abandoned":
			writef(s.errOut, "\n[turn_abandoned] %d %s\n", ev.GetTurn(), escapeLine(ev.GetAbandonReason()))
		case "permission_request":
			allowed := s.decide(ctx, ev)
			resp := &harnessv1.ControlEvent{
				Type:      "permission_response",
				RequestId: ev.GetRequestId(),
				Allowed:   &harnessv1.OptionalBool{Value: allowed},
			}
			if !allowed {
				resp.Reason = "denied by the example control plane"
			}
			if err := stream.Send(resp); err != nil {
				return res, fmt.Errorf("send permission_response: %w", err)
			}
		case "warning":
			writef(s.errOut, "[warning] %s\n", escapeLine(ev.GetMessage()))
		case "error":
			writef(s.errOut, "[error] %s\n", escapeLine(ev.GetMessage()))
		case "heartbeat":
		case "done":
			outcome := ev.GetTrace().GetOutcome()
			if outcome == "" {
				outcome = ev.GetStopReason()
			}
			res.Outcome = outcome
			res.Outcomes = append(res.Outcomes, outcome)
			writef(s.errOut, "\n[done] outcome=%s\n", escapeLine(outcome))
			n := len(res.Outcomes)
			if n > len(s.followUps) {
				return res, nil
			}
			requestID := fmt.Sprintf("followup-%d", n)
			writef(s.errOut, "[followup] %s: %s\n", requestID, preview(s.followUps[n-1]))
			if err := stream.Send(&harnessv1.ControlEvent{
				Type:         "user_response",
				UserResponse: s.followUps[n-1],
				RequestId:    requestID,
			}); err != nil {
				return res, fmt.Errorf("send user_response: %w", err)
			}
		default:
			writef(s.errOut, "[%s] not handled by this example; the run may stall\n", escapeLine(ev.GetType()))
		}
	}
}

// decide answers one permission request according to the configured mode.
func (s *server) decide(ctx context.Context, ev *harnessv1.HarnessEvent) bool {
	switch s.permission {
	case permissionAllow:
		return true
	case permissionAsk:
		return s.askTerminal(ctx, ev)
	default:
		return false
	}
}

// askTerminal prompts on the terminal and treats anything but an explicit yes
// as a denial, including end of input.
func (s *server) askTerminal(ctx context.Context, ev *harnessv1.HarnessEvent) bool {
	writef(s.errOut, "\n[permission] allow %s %s? [y/N] ", escapeLine(ev.GetToolName()), displayInput(ev.GetInput()))
	answer := make(chan string, 1)
	go func() {
		line, _ := s.stdin.ReadString('\n')
		answer <- line
	}()
	select {
	case line := <-answer:
		a := strings.ToLower(strings.TrimSpace(line))
		return a == "y" || a == "yes"
	case <-ctx.Done():
		return false
	}
}

// preview escapes s onto one line and shortens it to maxToolResultPreview
// bytes, cut on a UTF-8 boundary.
func preview(s string) string {
	s = escapeLine(s)
	if len(s) <= maxToolResultPreview {
		return s
	}
	n := maxToolResultPreview
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

// displayInput renders a JSON tool input as the tool will decode it: escaped
// characters are shown decoded and a duplicated key keeps only its last
// value, so the operator approves what will run rather than raw model bytes.
// Input that is not valid JSON is shown escaped as received.
func displayInput(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return escapeLine(string(raw))
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return escapeLine(string(raw))
	}
	return escapeLine(strings.TrimSuffix(b.String(), "\n"))
}

// escapeText makes streamed model text safe to write to a terminal: it
// keeps newlines and tabs, and escapes every other control character, so
// ANSI sequences and cursor movement in model or tool output are shown
// rather than interpreted.
func escapeText(s string) string {
	return escapeTerminal(s, true)
}

// escapeLine is escapeText for a single-line field: newlines and tabs are
// escaped too.
func escapeLine(s string) string {
	return escapeTerminal(s, false)
}

func escapeTerminal(s string, multiline bool) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case multiline && (r == '\n' || r == '\t'):
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case unicode.IsControl(r) || isInvisible(r):
			if r <= 0xff {
				fmt.Fprintf(&b, `\x%02x`, r)
			} else {
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// isInvisible reports the format characters (bidirectional controls,
// zero-width characters, tags), line and paragraph separators and blank
// fillers, which can make displayed text differ from its bytes.
func isInvisible(r rune) bool {
	switch r {
	case 0x115f, 0x1160, 0x3164, 0xffa0:
		return true
	}
	return unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)
}
