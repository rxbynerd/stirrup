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

// result is the terminal state of one run.
type result struct {
	// Outcome is the run outcome carried by the done event. The harness puts
	// it in stop_reason; trace.outcome is preferred when a trace is attached.
	Outcome string
}

// Succeeded reports whether the run ended successfully.
func (r result) Succeeded() bool {
	return r.Outcome == "success"
}

// server serves exactly one run: the first harness to connect receives the
// task and its outcome is delivered on results. A second concurrent stream is
// refused.
type server struct {
	harnessv1.UnimplementedHarnessServiceServer

	task       *harnessv1.RunConfig
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

// handle drives one run: wait for ready, assign the task, answer permission
// requests and print events until done. A stream that closes before done is
// reported as an error, since it means the harness exited abnormally.
func (s *server) handle(ctx context.Context, stream runStream) (result, error) {
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return result{}, errors.New("stream closed before the done event")
		}
		if err != nil {
			return result{}, fmt.Errorf("receive: %w", err)
		}

		switch ev.GetType() {
		case "ready":
			writef(s.errOut, "[ready] harness %s connected; assigning run %s\n", ev.GetHarnessVersion(), s.task.GetRunId())
			if err := stream.Send(&harnessv1.ControlEvent{Type: "task_assignment", Task: s.task}); err != nil {
				return result{}, fmt.Errorf("send task_assignment: %w", err)
			}
		case "text_delta":
			writef(s.out, "%s", ev.GetText())
		case "tool_call":
			writef(s.errOut, "\n[tool_call] %s %s\n", ev.GetName(), ev.GetInput())
		case "tool_result":
			writef(s.errOut, "[tool_result] %s\n", preview(ev.GetContent()))
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
				return result{}, fmt.Errorf("send permission_response: %w", err)
			}
		case "warning":
			writef(s.errOut, "[warning] %s\n", ev.GetMessage())
		case "error":
			writef(s.errOut, "[error] %s\n", ev.GetMessage())
		case "heartbeat":
		case "done":
			res := result{Outcome: ev.GetTrace().GetOutcome()}
			if res.Outcome == "" {
				res.Outcome = ev.GetStopReason()
			}
			writef(s.errOut, "\n[done] outcome=%s\n", res.Outcome)
			return res, nil
		default:
			writef(s.errOut, "[%s] not handled by this example; the run may stall\n", ev.GetType())
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
	writef(s.errOut, "\n[permission] allow %s %s? [y/N] ", ev.GetToolName(), ev.GetInput())
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

func preview(s string) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) > maxToolResultPreview {
		return s[:maxToolResultPreview] + "..."
	}
	return s
}
