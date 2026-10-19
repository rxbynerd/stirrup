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
	srv, out, _ := testServer(t, permissionDeny, "")
	stream := &fakeStream{events: []*harnessv1.HarnessEvent{
		{Type: "ready", HarnessVersion: "v1"},
		{Type: "text_delta", Text: "hello "},
		{Type: "heartbeat"},
		{Type: "text_delta", Text: "world"},
		{Type: "done", StopReason: "success"},
	}}

	res, err := srv.handle(context.Background(), stream)
	require.NoError(t, err)
	assert.True(t, res.Succeeded(), "outcome %q", res.Outcome)
	require.Len(t, stream.sent, 1)
	assert.Equal(t, "task_assignment", stream.sent[0].GetType())
	assert.Equal(t, "run-1", stream.sent[0].GetTask().GetRunId())
	assert.Equal(t, "hello world", out.String(), "stdout should be the concatenated text deltas")
}

func TestHandle_RendersLiveTranscriptEvents(t *testing.T) {
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
	require.NoError(t, err)
	assert.Equal(t, "answer", out.String(), "stdout should hold only the model's text")
	for _, want := range []string{
		"[run_started] run-1",
		"[user_message] initial_prompt: do it",
		"[turn_started] 0 anthropic/claude-x",
		"plan",
		"[tool_result] read_file error 12ms no such file",
		"[turn_committed] 0 tool_use in=100 out=5",
	} {
		assert.Contains(t, errOut.String(), want)
	}
	assert.NotContains(t, errOut.String(), "not handled", "a live transcript event was reported as unhandled")
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
			srv, _, _ := testServer(t, permissionDeny, "")
			res, err := srv.handle(context.Background(), &fakeStream{events: []*harnessv1.HarnessEvent{tc.done}})
			require.NoError(t, err)
			require.Equal(t, tc.want, res.Succeeded(), "outcome %q", res.Outcome)
		})
	}
}

func TestHandle_StreamClosedBeforeDoneIsError(t *testing.T) {
	srv, _, _ := testServer(t, permissionDeny, "")
	_, err := srv.handle(context.Background(), &fakeStream{events: []*harnessv1.HarnessEvent{{Type: "ready"}}})
	require.ErrorContains(t, err, "before the done event")
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
			srv, _, _ := testServer(t, tc.mode, tc.stdin)
			stream := &fakeStream{events: []*harnessv1.HarnessEvent{
				permissionRequest("req-7"),
				{Type: "done", StopReason: "success"},
			}}
			_, err := srv.handle(context.Background(), stream)
			require.NoError(t, err)
			require.Len(t, stream.sent, 1, "want one permission_response")
			resp := stream.sent[0]
			require.Equal(t, "permission_response", resp.GetType())
			require.Equal(t, "req-7", resp.GetRequestId())
			require.NotNil(t, resp.GetAllowed())
			require.Equal(t, tc.want, resp.GetAllowed().GetValue())
			if !tc.want {
				require.NotEmpty(t, resp.GetReason(), "a denial carries no reason for the model")
			}
		})
	}
}

func TestAskTerminal_DeniesWhenContextCancelled(t *testing.T) {
	srv, _, _ := testServer(t, permissionAsk, "")
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	srv.stdin.Reset(pr)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, srv.askTerminal(ctx, permissionRequest("r")),
		"askTerminal allowed a request after its context was cancelled")
}

func TestParsePermissionMode_AcceptsKnownAndRejectsUnknown(t *testing.T) {
	for _, name := range []string{"allow", "deny", "ask"} {
		t.Run(name, func(t *testing.T) {
			_, err := parsePermissionMode(name)
			require.NoError(t, err)
		})
	}
	t.Run("unknown mode", func(t *testing.T) {
		_, err := parsePermissionMode("always")
		require.Error(t, err)
	})
}

func TestLoadRunConfig_ParsesAndRejectsBadFiles(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "runconfig.json")
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		return path
	}

	t.Run("drops the transport key", func(t *testing.T) {
		cfg, err := loadRunConfig(write(t, `{"runId":"r","mode":"planning","prompt":"p","transport":{"type":"stdio"},"maxTurns":3}`))
		require.NoError(t, err)
		require.Equal(t, "r", cfg.GetRunId())
		require.EqualValues(t, 3, cfg.GetMaxTurns())
	})
	t.Run("rejects other unknown fields", func(t *testing.T) {
		_, err := loadRunConfig(write(t, `{"runId":"r","promt":"typo"}`))
		require.Error(t, err)
	})
	t.Run("rejects malformed JSON", func(t *testing.T) {
		_, err := loadRunConfig(write(t, `{`))
		require.Error(t, err)
	})
	t.Run("rejects a missing file", func(t *testing.T) {
		_, err := loadRunConfig(filepath.Join(t.TempDir(), "absent.json"))
		require.Error(t, err)
	})
}

// Every shipped example RunConfig must be assignable, so that any of them can
// be passed to -config.
func TestLoadRunConfig_AcceptsShippedExamples(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "runconfig", "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, paths, "no example RunConfigs matched ../runconfig/*.json")
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			_, err := loadRunConfig(path)
			require.NoError(t, err)
		})
	}
}

func TestLoadRunConfig_AcceptsSmokeRunConfig(t *testing.T) {
	_, err := loadRunConfig("smoke-runconfig.json")
	require.NoError(t, err)
}

// startServer serves srv on a loopback listener and returns a client.
func startServer(t *testing.T, srv *server) harnessv1.HarnessServiceClient {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer()
	harnessv1.RegisterHarnessServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return harnessv1.NewHarnessServiceClient(conn)
}

func TestRunTask_OverGRPCCompletesRun(t *testing.T) {
	srv, _, _ := testServer(t, permissionAllow, "")
	client := startServer(t, srv)

	stream, err := client.RunTask(context.Background())
	require.NoError(t, err)
	require.NoError(t, stream.Send(&harnessv1.HarnessEvent{Type: "ready"}))

	assign, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, "task_assignment", assign.GetType())
	require.Equal(t, "do it", assign.GetTask().GetPrompt())

	require.NoError(t, stream.Send(permissionRequest("req-1")))
	resp, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, "permission_response", resp.GetType())
	require.True(t, resp.GetAllowed().GetValue(), "want an allow")

	require.NoError(t, stream.Send(&harnessv1.HarnessEvent{Type: "done", StopReason: "success"}))

	out := <-srv.results
	require.NoError(t, out.err)
	require.True(t, out.result.Succeeded(), "outcome = %+v", out)
}

func TestRunTask_RefusesSecondStream(t *testing.T) {
	srv, _, _ := testServer(t, permissionDeny, "")
	client := startServer(t, srv)

	first, err := client.RunTask(context.Background())
	require.NoError(t, err)
	require.NoError(t, first.Send(&harnessv1.HarnessEvent{Type: "ready"}))
	_, err = first.Recv()
	require.NoError(t, err)

	second, err := client.RunTask(context.Background())
	require.NoError(t, err)
	_, err = second.Recv()
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "second stream error = %v", err)
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
			srv, _, _ := testServer(t, permissionDeny, "")
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			require.NoError(t, err)
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

			require.Equal(t, tc.want, serve(lis, srv, io.Discard))
		})
	}
}

func TestRun_UsageErrorsExitWithUsageCode(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "runconfig.json")
	require.NoError(t, os.WriteFile(cfg, []byte(`{"runId":"r"}`), 0o600))
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
			got := run(tc.args, strings.NewReader(""), io.Discard, &stderr)
			require.Equal(t, exitUsage, got, "stderr: %s", stderr.String())
		})
	}
}

func TestHandle_SurfacesReceiveError(t *testing.T) {
	srv, _, _ := testServer(t, permissionDeny, "")
	_, err := srv.handle(context.Background(), errStream{})
	require.ErrorContains(t, err, "boom")
}

type errStream struct{}

func (errStream) Recv() (*harnessv1.HarnessEvent, error) { return nil, errors.New("boom") }
func (errStream) Send(*harnessv1.ControlEvent) error     { return nil }

func TestHandle_RendersTurnAbandoned(t *testing.T) {
	srv, _, errOut := testServer(t, permissionDeny, "")
	turn := int32(2)
	stream := &fakeStream{events: []*harnessv1.HarnessEvent{
		{Type: "ready", HarnessVersion: "v1"},
		{Type: "turn_started", Turn: &turn},
		{Type: "turn_abandoned", Turn: &turn, AbandonReason: "empty_response"},
		{Type: "done", StopReason: "success"},
	}}

	_, err := srv.handle(context.Background(), stream)
	require.NoError(t, err)
	require.Contains(t, errOut.String(), "[turn_abandoned] 2 empty_response")
}

func TestHandle_SendsFollowUpsAfterEachDone(t *testing.T) {
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
	require.NoError(t, err)
	assert.False(t, res.Succeeded(), "a run ending in max_turns is not a success")
	assert.Equal(t, "max_turns", res.Outcome)
	assert.Equal(t, []string{"success", "success", "max_turns"}, res.Outcomes)
	assert.Len(t, stream.events, 1, "handle read past the last run's done")

	var got []string
	for _, ev := range stream.sent {
		got = append(got, ev.GetType()+":"+ev.GetUserResponse()+":"+ev.GetRequestId())
	}
	assert.Equal(t, []string{
		"task_assignment::",
		"user_response:second:followup-1",
		"user_response:third:followup-2",
	}, got)
	assert.Contains(t, errOut.String(), "[followup] followup-1: second")
}

func TestHandle_FailsWhenStreamClosesBeforeFollowUpRun(t *testing.T) {
	srv, _, _ := testServer(t, permissionDeny, "")
	srv.followUps = []string{"second"}
	stream := &fakeStream{events: []*harnessv1.HarnessEvent{
		{Type: "ready", HarnessVersion: "v1"},
		{Type: "done", StopReason: "success"},
	}}

	_, err := srv.handle(context.Background(), stream)
	require.Error(t, err, "the stream closed before the follow-up run's done")
}

func TestHandle_EscapesTerminalControls(t *testing.T) {
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
	require.NoError(t, err)

	wantStdout := `red \x1b[31mtext\x1b[0m` + "\nnext\tline" + `\r`
	assert.Equal(t, wantStdout, out.String())
	for _, want := range []string{
		`v1\x1b]0;pwned\x07`,
		`\u202eevil\u200bhidden\ue0041`,
		`[tool_call] read\x1b[2Jfile`,
		`line1\nline2\x1b[1A\xff`,
		`careful\x9b31m`,
	} {
		assert.Contains(t, errOut.String(), want)
	}
	for _, b := range []byte{0x1b, 0x07, 0x0d} {
		assert.NotContains(t, out.String(), string(b), "raw control byte %#x reached stdout", b)
		assert.NotContains(t, errOut.String(), string(b), "raw control byte %#x reached stderr", b)
	}
	assert.NotContains(t, errOut.String(), "\u202e", "a raw bidi override reached stderr")
	assert.NotContains(t, errOut.String(), "\u009b", "a raw C1 control reached stderr")
}

func TestPreview_CutsOnRuneBoundary(t *testing.T) {
	got := preview(strings.Repeat("é", maxToolResultPreview))
	assert.True(t, strings.HasSuffix(got, "..."), "preview = %q", got)
	assert.LessOrEqual(t, len(got), maxToolResultPreview+3, "preview = %q", got)
	assert.True(t, utf8.ValidString(got), "preview = %q", got)
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
			require.Equal(t, tc.want, displayInput([]byte(tc.in)))
		})
	}
}
