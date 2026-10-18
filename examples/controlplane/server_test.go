package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	harnessv1 "github.com/rxbynerd/stirrup/gen/harness/v1"
)

// fakeStream replays a scripted sequence of harness events and records what
// the handler sends back. Recv returns io.EOF once the script is exhausted.
type fakeStream struct {
	events []*harnessv1.HarnessEvent
	sent   []*harnessv1.ControlEvent
}

func (f *fakeStream) Recv() (*harnessv1.HarnessEvent, error) {
	if len(f.events) == 0 {
		return nil, io.EOF
	}
	ev := f.events[0]
	f.events = f.events[1:]
	return ev, nil
}

func (f *fakeStream) Send(ev *harnessv1.ControlEvent) error {
	f.sent = append(f.sent, ev)
	return nil
}

func testServer(t *testing.T, mode permissionMode, stdin string) (*server, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errOut bytes.Buffer
	task := &harnessv1.RunConfig{RunId: "run-1", Prompt: "do it"}
	return newServer(task, mode, strings.NewReader(stdin), &out, &errOut), &out, &errOut
}

func permissionRequest(id string) *harnessv1.HarnessEvent {
	return &harnessv1.HarnessEvent{Type: "permission_request", RequestId: id, ToolName: "run_command", Input: []byte(`{"command":"ls"}`)}
}

func TestHandleAssignsTaskAndReportsSuccess(t *testing.T) {
	srv, out, _ := testServer(t, permissionDeny, "")
	stream := &fakeStream{events: []*harnessv1.HarnessEvent{
		{Type: "ready", HarnessVersion: "v1"},
		{Type: "text_delta", Text: "hello "},
		{Type: "heartbeat"},
		{Type: "text_delta", Text: "world"},
		{Type: "done", StopReason: "success"},
	}}

	res, err := srv.handle(context.Background(), stream)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !res.Succeeded() {
		t.Errorf("Succeeded() = false for outcome %q", res.Outcome)
	}
	if len(stream.sent) != 1 || stream.sent[0].GetType() != "task_assignment" || stream.sent[0].GetTask().GetRunId() != "run-1" {
		t.Errorf("sent = %v, want a single task_assignment for run-1", stream.sent)
	}
	if got := out.String(); got != "hello world" {
		t.Errorf("stdout = %q, want the concatenated text deltas", got)
	}
}

func TestHandleRendersLiveTranscriptEvents(t *testing.T) {
	srv, out, errOut := testServer(t, permissionDeny, "")
	turn := int32(0)
	stream := &fakeStream{events: []*harnessv1.HarnessEvent{
		{Type: "ready", HarnessVersion: "v1"},
		{Type: "run_started", Id: "run-1"},
		{Type: "user_message", Source: "initial_prompt", Text: "do it"},
		{Type: "turn_started", Turn: &turn, Provider: "anthropic", Model: "claude-x"},
		{Type: "thinking_delta", Text: "plan"},
		{Type: "tool_call", Id: "tc1", Name: "read_file", Input: []byte(`{}`)},
		{Type: "tool_result", ToolUseId: "tc1", Name: "read_file", IsError: true, DurationMs: 12, Content: "no such file"},
		{Type: "turn_committed", Turn: &turn, StopReason: "tool_use", ToolResultCount: 1,
			Usage: &harnessv1.TokenUsage{InputTokens: 100, OutputTokens: 5}},
		{Type: "text_delta", Text: "answer"},
		{Type: "done", StopReason: "success"},
	}}

	if _, err := srv.handle(context.Background(), stream); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if got := out.String(); got != "answer" {
		t.Errorf("stdout = %q, want only the model's text", got)
	}
	for _, want := range []string{
		"[run_started] run-1",
		"[user_message] initial_prompt: do it",
		"[turn_started] 0 anthropic/claude-x",
		"plan",
		"[tool_result] read_file error 12ms no such file",
		"[turn_committed] 0 tool_use in=100 out=5",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut.String())
		}
	}
	if strings.Contains(errOut.String(), "not handled") {
		t.Errorf("a live transcript event was reported as unhandled:\n%s", errOut.String())
	}
}

func TestHandleDoneOutcome(t *testing.T) {
	tests := []struct {
		name string
		done *harnessv1.HarnessEvent
		want bool
	}{
		{"stop reason success", &harnessv1.HarnessEvent{Type: "done", StopReason: "success"}, true},
		{"stop reason max_turns", &harnessv1.HarnessEvent{Type: "done", StopReason: "max_turns"}, false},
		{"stop reason error", &harnessv1.HarnessEvent{Type: "done", StopReason: "error"}, false},
		{"trace outcome wins", &harnessv1.HarnessEvent{Type: "done", StopReason: "error", Trace: &harnessv1.RunTrace{Outcome: "success"}}, true},
		{"trace failure wins", &harnessv1.HarnessEvent{Type: "done", StopReason: "success", Trace: &harnessv1.RunTrace{Outcome: "verification_failed"}}, false},
		{"empty", &harnessv1.HarnessEvent{Type: "done"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := testServer(t, permissionDeny, "")
			res, err := srv.handle(context.Background(), &fakeStream{events: []*harnessv1.HarnessEvent{tc.done}})
			if err != nil {
				t.Fatalf("handle: %v", err)
			}
			if res.Succeeded() != tc.want {
				t.Errorf("Succeeded() = %v, want %v (outcome %q)", res.Succeeded(), tc.want, res.Outcome)
			}
		})
	}
}

func TestHandleStreamClosedBeforeDone(t *testing.T) {
	srv, _, _ := testServer(t, permissionDeny, "")
	_, err := srv.handle(context.Background(), &fakeStream{events: []*harnessv1.HarnessEvent{{Type: "ready"}}})
	if err == nil || !strings.Contains(err.Error(), "before the done event") {
		t.Fatalf("err = %v, want a closed-before-done error", err)
	}
}

func TestHandlePermissionModes(t *testing.T) {
	tests := []struct {
		name  string
		mode  permissionMode
		stdin string
		want  bool
	}{
		{"allow", permissionAllow, "", true},
		{"deny", permissionDeny, "", false},
		{"ask yes", permissionAsk, "y\n", true},
		{"ask full yes", permissionAsk, "YES\n", true},
		{"ask no", permissionAsk, "n\n", false},
		{"ask other input", permissionAsk, "maybe\n", false},
		{"ask end of input", permissionAsk, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := testServer(t, tc.mode, tc.stdin)
			stream := &fakeStream{events: []*harnessv1.HarnessEvent{
				permissionRequest("req-7"),
				{Type: "done", StopReason: "success"},
			}}
			if _, err := srv.handle(context.Background(), stream); err != nil {
				t.Fatalf("handle: %v", err)
			}
			if len(stream.sent) != 1 {
				t.Fatalf("sent %d events, want 1 permission_response", len(stream.sent))
			}
			resp := stream.sent[0]
			if resp.GetType() != "permission_response" || resp.GetRequestId() != "req-7" {
				t.Errorf("response = %v, want permission_response echoing req-7", resp)
			}
			if resp.GetAllowed() == nil || resp.GetAllowed().GetValue() != tc.want {
				t.Errorf("allowed = %v, want %v", resp.GetAllowed(), tc.want)
			}
			if !tc.want && resp.GetReason() == "" {
				t.Error("a denial carries no reason for the model")
			}
		})
	}
}

func TestAskTerminalDeniesWhenContextCancelled(t *testing.T) {
	srv, _, _ := testServer(t, permissionAsk, "")
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	srv.stdin.Reset(pr)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if srv.askTerminal(ctx, permissionRequest("r")) {
		t.Error("askTerminal allowed a request after its context was cancelled")
	}
}

func TestParsePermissionMode(t *testing.T) {
	for _, ok := range []string{"allow", "deny", "ask"} {
		if _, err := parsePermissionMode(ok); err != nil {
			t.Errorf("parsePermissionMode(%q): %v", ok, err)
		}
	}
	if _, err := parsePermissionMode("always"); err == nil {
		t.Error("parsePermissionMode accepted an unknown mode")
	}
}

func TestLoadRunConfig(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "runconfig.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("drops the transport key", func(t *testing.T) {
		cfg, err := loadRunConfig(write(t, `{"runId":"r","mode":"planning","prompt":"p","transport":{"type":"stdio"},"maxTurns":3}`))
		if err != nil {
			t.Fatalf("loadRunConfig: %v", err)
		}
		if cfg.GetRunId() != "r" || cfg.GetMaxTurns() != 3 {
			t.Errorf("cfg = %v, want runId r and maxTurns 3", cfg)
		}
	})
	t.Run("rejects other unknown fields", func(t *testing.T) {
		if _, err := loadRunConfig(write(t, `{"runId":"r","promt":"typo"}`)); err == nil {
			t.Error("loadRunConfig accepted a misspelt field")
		}
	})
	t.Run("rejects malformed JSON", func(t *testing.T) {
		if _, err := loadRunConfig(write(t, `{`)); err == nil {
			t.Error("loadRunConfig accepted malformed JSON")
		}
	})
	t.Run("rejects a missing file", func(t *testing.T) {
		if _, err := loadRunConfig(filepath.Join(t.TempDir(), "absent.json")); err == nil {
			t.Error("loadRunConfig accepted a missing file")
		}
	})
}

// Every shipped example RunConfig must be assignable, so that any of them can
// be passed to -config.
func TestLoadRunConfigAcceptsShippedExamples(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "runconfig", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob examples: %v (%d files)", err, len(paths))
	}
	for _, path := range paths {
		if _, err := loadRunConfig(path); err != nil {
			t.Errorf("%s: %v", filepath.Base(path), err)
		}
	}
}

func TestSmokeRunConfigIsAssignable(t *testing.T) {
	if _, err := loadRunConfig("smoke-runconfig.json"); err != nil {
		t.Fatal(err)
	}
}

// startServer serves srv on a loopback listener and returns a client.
func startServer(t *testing.T, srv *server) harnessv1.HarnessServiceClient {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	harnessv1.RegisterHarnessServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return harnessv1.NewHarnessServiceClient(conn)
}

func TestRunTaskOverGRPC(t *testing.T) {
	srv, _, _ := testServer(t, permissionAllow, "")
	client := startServer(t, srv)

	stream, err := client.RunTask(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&harnessv1.HarnessEvent{Type: "ready"}); err != nil {
		t.Fatal(err)
	}
	assign, err := stream.Recv()
	if err != nil || assign.GetType() != "task_assignment" || assign.GetTask().GetPrompt() != "do it" {
		t.Fatalf("assignment = %v, %v; want task_assignment carrying the prompt", assign, err)
	}
	if err := stream.Send(permissionRequest("req-1")); err != nil {
		t.Fatal(err)
	}
	resp, err := stream.Recv()
	if err != nil || resp.GetType() != "permission_response" || !resp.GetAllowed().GetValue() {
		t.Fatalf("response = %v, %v; want an allow", resp, err)
	}
	if err := stream.Send(&harnessv1.HarnessEvent{Type: "done", StopReason: "success"}); err != nil {
		t.Fatal(err)
	}

	out := <-srv.results
	if out.err != nil || !out.result.Succeeded() {
		t.Errorf("outcome = %+v, want success", out)
	}
}

func TestRunTaskRefusesSecondStream(t *testing.T) {
	srv, _, _ := testServer(t, permissionDeny, "")
	client := startServer(t, srv)

	first, err := client.RunTask(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Send(&harnessv1.HarnessEvent{Type: "ready"}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Recv(); err != nil {
		t.Fatal(err)
	}

	second, err := client.RunTask(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = second.Recv()
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("second stream error = %v, want FailedPrecondition", err)
	}
}

func TestServeExitStatus(t *testing.T) {
	tests := []struct {
		name string
		send []*harnessv1.HarnessEvent
		want int
	}{
		{"success", []*harnessv1.HarnessEvent{{Type: "ready"}, {Type: "done", StopReason: "success"}}, exitSuccess},
		{"failed run", []*harnessv1.HarnessEvent{{Type: "ready"}, {Type: "done", StopReason: "error"}}, exitFailed},
		{"no done event", []*harnessv1.HarnessEvent{{Type: "ready"}}, exitFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := testServer(t, permissionDeny, "")
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			go func() {
				stream, err := harnessv1.NewHarnessServiceClient(conn).RunTask(context.Background())
				if err != nil {
					return
				}
				for _, ev := range tc.send {
					if stream.Send(ev) != nil {
						return
					}
				}
				_ = stream.CloseSend()
				for {
					if _, err := stream.Recv(); err != nil {
						return
					}
				}
			}()

			if got := serve(lis, srv, io.Discard); got != tc.want {
				t.Errorf("serve = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRunUsageErrors(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "runconfig.json")
	if err := os.WriteFile(cfg, []byte(`{"runId":"r"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
	}{
		{"missing config flag", nil},
		{"unknown permission mode", []string{"-config", cfg, "-permission", "sometimes"}},
		{"unreadable config", []string{"-config", filepath.Join(t.TempDir(), "absent.json")}},
		{"unknown flag", []string{"-bogus"}},
		{"unusable listen address", []string{"-config", cfg, "-listen", "not-an-address"}},
		{"follow-up without followUpGrace", []string{"-config", cfg, "-followup", "again"}},
		{"empty follow-up", []string{"-config", cfg, "-followup", " "}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := run(tc.args, strings.NewReader(""), io.Discard, &stderr); got != exitUsage {
				t.Errorf("run = %d, want %d (stderr: %s)", got, exitUsage, stderr.String())
			}
		})
	}
}

func TestHandleSurfacesReceiveError(t *testing.T) {
	srv, _, _ := testServer(t, permissionDeny, "")
	_, err := srv.handle(context.Background(), errStream{})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want the receive error", err)
	}
}

type errStream struct{}

func (errStream) Recv() (*harnessv1.HarnessEvent, error) { return nil, errors.New("boom") }
func (errStream) Send(*harnessv1.ControlEvent) error     { return nil }

func TestHandleRendersTurnAbandoned(t *testing.T) {
	srv, _, errOut := testServer(t, permissionDeny, "")
	turn := int32(2)
	stream := &fakeStream{events: []*harnessv1.HarnessEvent{
		{Type: "ready", HarnessVersion: "v1"},
		{Type: "turn_started", Turn: &turn},
		{Type: "turn_abandoned", Turn: &turn, AbandonReason: "empty_response"},
		{Type: "done", StopReason: "success"},
	}}

	if _, err := srv.handle(context.Background(), stream); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !strings.Contains(errOut.String(), "[turn_abandoned] 2 empty_response") {
		t.Errorf("stderr lacks the abandoned turn:\n%s", errOut.String())
	}
}

func TestHandleSendsFollowUpsAfterEachDone(t *testing.T) {
	srv, _, errOut := testServer(t, permissionDeny, "")
	srv.followUps = []string{"second", "third"}
	stream := &fakeStream{events: []*harnessv1.HarnessEvent{
		{Type: "ready", HarnessVersion: "v1"},
		{Type: "done", StopReason: "success"},
		{Type: "run_started", Id: "run-1-2"},
		{Type: "done", StopReason: "success"},
		{Type: "run_started", Id: "run-1-3"},
		{Type: "done", StopReason: "max_turns"},
		{Type: "text_delta", Text: "never read"},
	}}

	res, err := srv.handle(context.Background(), stream)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if res.Succeeded() || res.Outcome != "max_turns" || strings.Join(res.Outcomes, ",") != "success,success,max_turns" {
		t.Errorf("result = %+v, want three runs ending max_turns and not succeeded", res)
	}
	if len(stream.events) != 1 {
		t.Errorf("handle read past the last run's done: %d events left", len(stream.events))
	}
	var got []string
	for _, ev := range stream.sent {
		got = append(got, ev.GetType()+":"+ev.GetUserResponse()+":"+ev.GetRequestId())
	}
	if want := "task_assignment::,user_response:second:followup-1,user_response:third:followup-2"; strings.Join(got, ",") != want {
		t.Errorf("sent = %v, want %s", got, want)
	}
	if !strings.Contains(errOut.String(), "[followup] followup-1: second") {
		t.Errorf("stderr lacks the follow-up:\n%s", errOut.String())
	}
}

func TestHandleFailsWhenStreamClosesBeforeFollowUpRun(t *testing.T) {
	srv, _, _ := testServer(t, permissionDeny, "")
	srv.followUps = []string{"second"}
	stream := &fakeStream{events: []*harnessv1.HarnessEvent{
		{Type: "ready", HarnessVersion: "v1"},
		{Type: "done", StopReason: "success"},
	}}

	if _, err := srv.handle(context.Background(), stream); err == nil {
		t.Fatal("handle: want an error when the stream closes before the follow-up run's done")
	}
}

func TestHandleEscapesTerminalControls(t *testing.T) {
	srv, out, errOut := testServer(t, permissionDeny, "")
	stream := &fakeStream{events: []*harnessv1.HarnessEvent{
		{Type: "ready", HarnessVersion: "v1\x1b]0;pwned\a"},
		{Type: "text_delta", Text: "red \x1b[31mtext\x1b[0m\nnext\tline\r"},
		{Type: "thinking_delta", Text: "\u202eevil\u200bhidden\U000E0041"},
		{Type: "tool_call", Id: "tc1", Name: "read\x1b[2Jfile", Input: []byte(`{"p":"a\u0007"}`)},
		{Type: "tool_result", ToolUseId: "tc1", Name: "read_file", Content: "line1\nline2\x1b[1A\xff"},
		{Type: "warning", Message: "careful\u009b31m"},
		{Type: "done", StopReason: "success"},
	}}

	if _, err := srv.handle(context.Background(), stream); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if want := `red \x1b[31mtext\x1b[0m` + "\nnext\tline" + `\r`; out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
	for _, want := range []string{
		`v1\x1b]0;pwned\x07`,
		`\u202eevil\u200bhidden\ue0041`,
		`[tool_call] read\x1b[2Jfile`,
		`line1\nline2\x1b[1A\xff`,
		`careful\x9b31m`,
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut.String())
		}
	}
	for _, b := range []byte{0x1b, 0x07, 0x0d} {
		if bytes.IndexByte(out.Bytes(), b) >= 0 || bytes.IndexByte(errOut.Bytes(), b) >= 0 {
			t.Errorf("raw control byte %#x reached the terminal", b)
		}
	}
	if strings.ContainsRune(errOut.String(), 0x202e) || strings.ContainsRune(errOut.String(), 0x9b) {
		t.Error("a raw bidi override or C1 control reached the terminal")
	}
}

func TestPreviewCutsOnRuneBoundary(t *testing.T) {
	got := preview(strings.Repeat("é", maxToolResultPreview))
	if !strings.HasSuffix(got, "...") || len(got) > maxToolResultPreview+3 || !utf8.ValidString(got) {
		t.Errorf("preview = %q", got)
	}
}

func TestDisplayInputShowsWhatTheToolDecodes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"command":"ls","command":"curl x|sh"}`, `{"command":"curl x|sh"}`},
		{`{"command":"\u0072m -rf ~"}`, `{"command":"rm -rf ~"}`},
		{`{"p":"a\u001b[31m<b>"}`, `{"p":"a\u001b[31m<b>"}`},
		{`not json` + "\x1b", `not json\x1b`},
	} {
		if got := displayInput([]byte(tc.in)); got != tc.want {
			t.Errorf("displayInput(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
