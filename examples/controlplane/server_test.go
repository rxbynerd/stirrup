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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestHandle_AssignsTaskAndReportsSuccess(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv, out, _ := testServer(t, permissionDeny, "")
	stream := &fakeStream{events: []*harnessv1.HarnessEvent{
		{Type: "ready", HarnessVersion: "v1"},
		{Type: "text_delta", Text: "hello "},
		{Type: "heartbeat"},
		{Type: "text_delta", Text: "world"},
		{Type: "done", StopReason: "success"},
	}}

	res, err := srv.handle(context.Background(), stream)
	must.NoError(err)
	check.True(res.Succeeded(), "outcome %q", res.Outcome)
	must.Len(stream.sent, 1)
	check.Equal("task_assignment", stream.sent[0].GetType())
	check.Equal("run-1", stream.sent[0].GetTask().GetRunId())
	check.Equal("hello world", out.String(), "stdout should be the concatenated text deltas")
}

func TestHandle_RendersLiveTranscriptEvents(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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

	_, err := srv.handle(context.Background(), stream)
	must.NoError(err)
	check.Equal("answer", out.String(), "stdout should hold only the model's text")
	for _, want := range []string{
		"[run_started] run-1",
		"[user_message] initial_prompt: do it",
		"[turn_started] 0 anthropic/claude-x",
		"plan",
		"[tool_result] read_file error 12ms no such file",
		"[turn_committed] 0 tool_use in=100 out=5",
	} {
		check.Contains(errOut.String(), want)
	}
	check.NotContains(errOut.String(), "not handled", "a live transcript event was reported as unhandled")
}

func TestHandle_DoneOutcomeDecidesSuccess(t *testing.T) {
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
			must := require.New(t)
			srv, _, _ := testServer(t, permissionDeny, "")
			res, err := srv.handle(context.Background(), &fakeStream{events: []*harnessv1.HarnessEvent{tc.done}})
			must.NoError(err)
			must.Equal(tc.want, res.Succeeded(), "outcome %q", res.Outcome)
		})
	}
}

func TestHandle_StreamClosedBeforeDoneIsError(t *testing.T) {
	must := require.New(t)
	srv, _, _ := testServer(t, permissionDeny, "")
	_, err := srv.handle(context.Background(), &fakeStream{events: []*harnessv1.HarnessEvent{{Type: "ready"}}})
	must.ErrorContains(err, "before the done event")
}

func TestHandle_PermissionModesGateRequests(t *testing.T) {
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
			must := require.New(t)
			srv, _, _ := testServer(t, tc.mode, tc.stdin)
			stream := &fakeStream{events: []*harnessv1.HarnessEvent{
				permissionRequest("req-7"),
				{Type: "done", StopReason: "success"},
			}}
			_, err := srv.handle(context.Background(), stream)
			must.NoError(err)
			must.Len(stream.sent, 1, "want one permission_response")
			resp := stream.sent[0]
			must.Equal("permission_response", resp.GetType())
			must.Equal("req-7", resp.GetRequestId())
			must.NotNil(resp.GetAllowed())
			must.Equal(tc.want, resp.GetAllowed().GetValue())
			if !tc.want {
				must.NotEmpty(resp.GetReason(), "a denial carries no reason for the model")
			}
		})
	}
}

func TestAskTerminal_DeniesWhenContextCancelled(t *testing.T) {
	must := require.New(t)
	srv, _, _ := testServer(t, permissionAsk, "")
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	srv.stdin.Reset(pr)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	must.False(srv.askTerminal(ctx, permissionRequest("r")),
		"askTerminal allowed a request after its context was cancelled")
}

func TestParsePermissionMode_AcceptsKnownAndRejectsUnknown(t *testing.T) {
	for _, name := range []string{"allow", "deny", "ask"} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			_, err := parsePermissionMode(name)
			must.NoError(err)
		})
	}
	t.Run("unknown mode", func(t *testing.T) {
		must := require.New(t)
		_, err := parsePermissionMode("always")
		must.Error(err)
	})
}

func TestLoadRunConfig_ParsesAndRejectsBadFiles(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		must := require.New(t)
		path := filepath.Join(t.TempDir(), "runconfig.json")
		must.NoError(os.WriteFile(path, []byte(body), 0o600))
		return path
	}

	t.Run("drops the transport key", func(t *testing.T) {
		must := require.New(t)
		cfg, err := loadRunConfig(write(t, `{"runId":"r","mode":"planning","prompt":"p","transport":{"type":"stdio"},"maxTurns":3}`))
		must.NoError(err)
		must.Equal("r", cfg.GetRunId())
		must.EqualValues(3, cfg.GetMaxTurns())
	})
	t.Run("rejects other unknown fields", func(t *testing.T) {
		must := require.New(t)
		_, err := loadRunConfig(write(t, `{"runId":"r","promt":"typo"}`))
		must.Error(err)
	})
	t.Run("rejects malformed JSON", func(t *testing.T) {
		must := require.New(t)
		_, err := loadRunConfig(write(t, `{`))
		must.Error(err)
	})
	t.Run("rejects a missing file", func(t *testing.T) {
		must := require.New(t)
		_, err := loadRunConfig(filepath.Join(t.TempDir(), "absent.json"))
		must.Error(err)
	})
}

// Every shipped example RunConfig must be assignable, so that any of them can
// be passed to -config.
func TestLoadRunConfig_AcceptsShippedExamples(t *testing.T) {
	must := require.New(t)
	paths, err := filepath.Glob(filepath.Join("..", "runconfig", "*.json"))
	must.NoError(err)
	must.NotEmpty(paths, "no example RunConfigs matched ../runconfig/*.json")
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			must := require.New(t)
			_, err := loadRunConfig(path)
			must.NoError(err)
		})
	}
}

func TestLoadRunConfig_AcceptsSmokeRunConfig(t *testing.T) {
	must := require.New(t)
	_, err := loadRunConfig("smoke-runconfig.json")
	must.NoError(err)
}

// startServer serves srv on a loopback listener and returns a client.
func startServer(t *testing.T, srv *server) harnessv1.HarnessServiceClient {
	t.Helper()
	must := require.New(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	must.NoError(err)
	gs := grpc.NewServer()
	harnessv1.RegisterHarnessServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	must.NoError(err)
	t.Cleanup(func() { _ = conn.Close() })
	return harnessv1.NewHarnessServiceClient(conn)
}

func TestRunTask_OverGRPCCompletesRun(t *testing.T) {
	must := require.New(t)
	srv, _, _ := testServer(t, permissionAllow, "")
	client := startServer(t, srv)

	stream, err := client.RunTask(context.Background())
	must.NoError(err)
	must.NoError(stream.Send(&harnessv1.HarnessEvent{Type: "ready"}))

	assign, err := stream.Recv()
	must.NoError(err)
	must.Equal("task_assignment", assign.GetType())
	must.Equal("do it", assign.GetTask().GetPrompt())

	must.NoError(stream.Send(permissionRequest("req-1")))
	resp, err := stream.Recv()
	must.NoError(err)
	must.Equal("permission_response", resp.GetType())
	must.True(resp.GetAllowed().GetValue(), "want an allow")

	must.NoError(stream.Send(&harnessv1.HarnessEvent{Type: "done", StopReason: "success"}))

	out := <-srv.results
	must.NoError(out.err)
	must.True(out.result.Succeeded(), "outcome = %+v", out)
}

func TestRunTask_RefusesSecondStream(t *testing.T) {
	must := require.New(t)
	srv, _, _ := testServer(t, permissionDeny, "")
	client := startServer(t, srv)

	first, err := client.RunTask(context.Background())
	must.NoError(err)
	must.NoError(first.Send(&harnessv1.HarnessEvent{Type: "ready"}))
	_, err = first.Recv()
	must.NoError(err)

	second, err := client.RunTask(context.Background())
	must.NoError(err)
	_, err = second.Recv()
	must.Equal(codes.FailedPrecondition, status.Code(err), "second stream error = %v", err)
}

func TestServe_ExitStatusFollowsRunOutcome(t *testing.T) {
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
			must := require.New(t)
			srv, _, _ := testServer(t, permissionDeny, "")
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			must.NoError(err)
			conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			must.NoError(err)
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

			must.Equal(tc.want, serve(lis, srv, io.Discard))
		})
	}
}

func TestRun_UsageErrorsExitWithUsageCode(t *testing.T) {
	must := require.New(t)
	cfg := filepath.Join(t.TempDir(), "runconfig.json")
	must.NoError(os.WriteFile(cfg, []byte(`{"runId":"r"}`), 0o600))
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
			must := require.New(t)
			var stderr bytes.Buffer
			got := run(tc.args, strings.NewReader(""), io.Discard, &stderr)
			must.Equal(exitUsage, got, "stderr: %s", stderr.String())
		})
	}
}

func TestHandle_SurfacesReceiveError(t *testing.T) {
	must := require.New(t)
	srv, _, _ := testServer(t, permissionDeny, "")
	_, err := srv.handle(context.Background(), errStream{})
	must.ErrorContains(err, "boom")
}

type errStream struct{}

func (errStream) Recv() (*harnessv1.HarnessEvent, error) { return nil, errors.New("boom") }
func (errStream) Send(*harnessv1.ControlEvent) error     { return nil }

func TestHandle_RendersTurnAbandoned(t *testing.T) {
	must := require.New(t)
	srv, _, errOut := testServer(t, permissionDeny, "")
	turn := int32(2)
	stream := &fakeStream{events: []*harnessv1.HarnessEvent{
		{Type: "ready", HarnessVersion: "v1"},
		{Type: "turn_started", Turn: &turn},
		{Type: "turn_abandoned", Turn: &turn, AbandonReason: "empty_response"},
		{Type: "done", StopReason: "success"},
	}}

	_, err := srv.handle(context.Background(), stream)
	must.NoError(err)
	must.Contains(errOut.String(), "[turn_abandoned] 2 empty_response")
}

func TestHandle_RendersToolsLoaded(t *testing.T) {
	must := require.New(t)
	srv, _, errOut := testServer(t, permissionDeny, "")
	turn := int32(1)
	stream := &fakeStream{events: []*harnessv1.HarnessEvent{
		{Type: "ready", HarnessVersion: "v1"},
		{Type: "tools_loaded", Turn: &turn, Source: "harness", Name: "tool_search",
			Input: []byte(`{"query":"issues"}`), Tools: []string{"mcp_github_create_issue", "mcp_github_list_issues"}},
		{Type: "done", StopReason: "success"},
	}}

	_, err := srv.handle(context.Background(), stream)
	must.NoError(err)
	must.Contains(errOut.String(), "[tools_loaded] harness tool_search mcp_github_create_issue, mcp_github_list_issues\n")
	must.NotContains(errOut.String(), "not handled")
}

func TestHandle_EscapesToolsLoadedFields(t *testing.T) {
	must := require.New(t)
	srv, _, errOut := testServer(t, permissionDeny, "")
	stream := &fakeStream{events: []*harnessv1.HarnessEvent{
		{Type: "ready", HarnessVersion: "v1"},
		{Type: "tools_loaded", Source: "native\x1b[2J", Name: "tool_search", Tools: []string{"evil\nname"}},
		{Type: "done", StopReason: "success"},
	}}

	_, err := srv.handle(context.Background(), stream)
	must.NoError(err)
	must.Contains(errOut.String(), `[tools_loaded] native\x1b[2J tool_search evil\nname`)
	must.NotContains(errOut.String(), "\x1b")
}

func TestHandle_SendsFollowUpsAfterEachDone(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
	must.NoError(err)
	check.False(res.Succeeded(), "a run ending in max_turns is not a success")
	check.Equal("max_turns", res.Outcome)
	check.Equal([]string{"success", "success", "max_turns"}, res.Outcomes)
	check.Len(stream.events, 1, "handle read past the last run's done")

	var got []string
	for _, ev := range stream.sent {
		got = append(got, ev.GetType()+":"+ev.GetUserResponse()+":"+ev.GetRequestId())
	}
	check.Equal([]string{
		"task_assignment::",
		"user_response:second:followup-1",
		"user_response:third:followup-2",
	}, got)
	check.Contains(errOut.String(), "[followup] followup-1: second")
}

func TestHandle_FailsWhenStreamClosesBeforeFollowUpRun(t *testing.T) {
	must := require.New(t)
	srv, _, _ := testServer(t, permissionDeny, "")
	srv.followUps = []string{"second"}
	stream := &fakeStream{events: []*harnessv1.HarnessEvent{
		{Type: "ready", HarnessVersion: "v1"},
		{Type: "done", StopReason: "success"},
	}}

	_, err := srv.handle(context.Background(), stream)
	must.Error(err, "the stream closed before the follow-up run's done")
}

func TestHandle_EscapesTerminalControls(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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

	_, err := srv.handle(context.Background(), stream)
	must.NoError(err)

	wantStdout := `red \x1b[31mtext\x1b[0m` + "\nnext\tline" + `\r`
	check.Equal(wantStdout, out.String())
	for _, want := range []string{
		`v1\x1b]0;pwned\x07`,
		`\u202eevil\u200bhidden\ue0041`,
		`[tool_call] read\x1b[2Jfile`,
		`line1\nline2\x1b[1A\xff`,
		`careful\x9b31m`,
	} {
		check.Contains(errOut.String(), want)
	}
	for _, b := range []byte{0x1b, 0x07, 0x0d} {
		check.NotContains(out.String(), string(b), "raw control byte %#x reached stdout", b)
		check.NotContains(errOut.String(), string(b), "raw control byte %#x reached stderr", b)
	}
	check.NotContains(errOut.String(), "\u202e", "a raw bidi override reached stderr")
	check.NotContains(errOut.String(), "\u009b", "a raw C1 control reached stderr")
}

func TestPreview_CutsOnRuneBoundary(t *testing.T) {
	check := assert.New(t)
	got := preview(strings.Repeat("é", maxToolResultPreview))
	check.True(strings.HasSuffix(got, "..."), "preview = %q", got)
	check.LessOrEqual(len(got), maxToolResultPreview+3, "preview = %q", got)
	check.True(utf8.ValidString(got), "preview = %q", got)
}

func TestDisplayInput_ShowsWhatTheToolDecodes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"duplicate keys show the last value", `{"command":"ls","command":"curl x|sh"}`, `{"command":"curl x|sh"}`},
		{"unicode escapes are decoded", `{"command":"\u0072m -rf ~"}`, `{"command":"rm -rf ~"}`},
		{"JSON escapes for control characters stay escaped", `{"p":"a\u001b[31m<b>"}`, `{"p":"a\u001b[31m<b>"}`},
		{"non-JSON input escapes control bytes", `not json` + "\x1b", `not json\x1b`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, displayInput([]byte(tc.in)))
		})
	}
}
