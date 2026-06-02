package builtins

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeSessionController records calls and returns canned results.
type fakeSessionController struct {
	startID     string
	startErr    error
	gotPrompt   string
	gotMode     string
	gotMaxTurns int

	statusJSON json.RawMessage
	statusErr  error

	waitJSON      json.RawMessage
	waitErr       error
	gotWaitID     string
	gotWaitTimout int

	terminateJSON json.RawMessage
	terminateErr  error
	gotTerminate  string
}

func (f *fakeSessionController) Start(_ context.Context, prompt, mode string, maxTurns int) (string, error) {
	f.gotPrompt, f.gotMode, f.gotMaxTurns = prompt, mode, maxTurns
	return f.startID, f.startErr
}

func (f *fakeSessionController) Status(string) (json.RawMessage, error) {
	return f.statusJSON, f.statusErr
}

func (f *fakeSessionController) Wait(_ context.Context, id string, timeoutSeconds int) (json.RawMessage, error) {
	f.gotWaitID, f.gotWaitTimout = id, timeoutSeconds
	return f.waitJSON, f.waitErr
}

func (f *fakeSessionController) Terminate(id string) (json.RawMessage, error) {
	f.gotTerminate = id
	return f.terminateJSON, f.terminateErr
}

func TestStartSessionTool_Dispatch(t *testing.T) {
	f := &fakeSessionController{startID: "session-7"}
	tl := StartSessionTool(f)

	out, err := tl.Handler(context.Background(), json.RawMessage(`{"prompt":"go","mode":"research","max_turns":5}`))
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if f.gotPrompt != "go" || f.gotMode != "research" || f.gotMaxTurns != 5 {
		t.Fatalf("controller got (%q,%q,%d), want (go,research,5)", f.gotPrompt, f.gotMode, f.gotMaxTurns)
	}
	var got struct {
		SessionID string `json:"sessionId"`
		State     string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output not JSON: %v (%s)", err, out)
	}
	if got.SessionID != "session-7" || got.State != "running" {
		t.Fatalf("output = %#v, want {session-7 running}", got)
	}
	if !tl.RequiresApproval {
		t.Error("start_session should require approval (it spawns)")
	}
	if tl.WorkspaceMutating {
		t.Error("start_session should not be workspace-mutating")
	}
}

func TestStartSessionTool_ControllerError(t *testing.T) {
	f := &fakeSessionController{startErr: errors.New("session limit reached")}
	tl := StartSessionTool(f)
	if _, err := tl.Handler(context.Background(), json.RawMessage(`{"prompt":"go"}`)); err == nil {
		t.Fatal("Handler err = nil, want controller error surfaced")
	}
}

func TestCheckSessionTool_Dispatch(t *testing.T) {
	f := &fakeSessionController{statusJSON: json.RawMessage(`{"sessionId":"session-1","state":"running"}`)}
	tl := CheckSessionTool(f)
	out, err := tl.Handler(context.Background(), json.RawMessage(`{"session_id":"session-1"}`))
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if out != `{"sessionId":"session-1","state":"running"}` {
		t.Fatalf("output = %s", out)
	}
	if tl.RequiresApproval {
		t.Error("check_session should not require approval (read-only poll)")
	}
}

func TestCheckSessionTool_MissingID(t *testing.T) {
	tl := CheckSessionTool(&fakeSessionController{})
	if _, err := tl.Handler(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("Handler err = nil, want missing session_id error")
	}
}

func TestWaitSessionTool_Dispatch(t *testing.T) {
	f := &fakeSessionController{waitJSON: json.RawMessage(`{"sessionId":"session-2","state":"done"}`)}
	tl := WaitSessionTool(f)
	out, err := tl.Handler(context.Background(), json.RawMessage(`{"session_id":"session-2","timeout_seconds":30}`))
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if f.gotWaitID != "session-2" || f.gotWaitTimout != 30 {
		t.Fatalf("controller got (%q,%d), want (session-2,30)", f.gotWaitID, f.gotWaitTimout)
	}
	if !strings.Contains(out, `"state":"done"`) {
		t.Fatalf("output = %s", out)
	}
}

func TestWaitSessionTool_MissingID(t *testing.T) {
	tl := WaitSessionTool(&fakeSessionController{})
	if _, err := tl.Handler(context.Background(), json.RawMessage(`{"timeout_seconds":5}`)); err == nil {
		t.Fatal("Handler err = nil, want missing session_id error")
	}
}

func TestTerminateSessionTool_Dispatch(t *testing.T) {
	f := &fakeSessionController{terminateJSON: json.RawMessage(`{"sessionId":"session-3","state":"terminated"}`)}
	tl := TerminateSessionTool(f)
	out, err := tl.Handler(context.Background(), json.RawMessage(`{"session_id":"session-3"}`))
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if f.gotTerminate != "session-3" {
		t.Fatalf("controller got %q, want session-3", f.gotTerminate)
	}
	if !strings.Contains(out, `"terminated"`) {
		t.Fatalf("output = %s", out)
	}
}

func TestTerminateSessionTool_ControllerError(t *testing.T) {
	f := &fakeSessionController{terminateErr: errors.New("unknown session")}
	tl := TerminateSessionTool(f)
	if _, err := tl.Handler(context.Background(), json.RawMessage(`{"session_id":"nope"}`)); err == nil {
		t.Fatal("Handler err = nil, want controller error surfaced")
	}
}

// TestSessionTools_EnrichedShape mirrors TestBuiltinDescriptions_EnrichedShape
// for the factory-registered session tools, which RegisterBuiltins does not
// cover.
func TestSessionTools_EnrichedShape(t *testing.T) {
	defs := []struct {
		name string
		desc string
	}{
		{"start_session", StartSessionTool(nil).Description},
		{"check_session", CheckSessionTool(nil).Description},
		{"wait_session", WaitSessionTool(nil).Description},
		{"terminate_session", TerminateSessionTool(nil).Description},
	}
	for _, d := range defs {
		t.Run(d.name, func(t *testing.T) {
			if len(d.desc) > maxToolDescriptionLen {
				t.Errorf("description length %d exceeds cap %d", len(d.desc), maxToolDescriptionLen)
			}
			if !hasPositiveUseThis(d.desc) {
				t.Errorf("description missing positive when-to-use guidance")
			}
			example, ok := extractJSONExample(d.desc)
			if !ok {
				t.Fatalf("description missing JSON example after Example marker")
			}
			var probe map[string]any
			if err := json.Unmarshal([]byte(example), &probe); err != nil {
				t.Errorf("example is not valid JSON: %v\n%s", err, example)
			}
		})
	}
}
