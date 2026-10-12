package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

const spanProbeKey = "AKIAIOSFODNN7EXAMPLE"

var errSpanProbe = errors.New("upstream echoed key " + spanProbeKey + " in response")

// spanProbeGit fails the selected git operations with errSpanProbe.
type spanProbeGit struct {
	setupErr, checkpointErr, finaliseErr error
}

func (g *spanProbeGit) Setup(context.Context, string, string) error { return g.setupErr }
func (g *spanProbeGit) Checkpoint(context.Context, string) error    { return g.checkpointErr }
func (g *spanProbeGit) Finalise(context.Context, string) (*types.GitHandoff, error) {
	return &types.GitHandoff{}, g.finaliseErr
}

type spanProbeVerifier struct{}

func (spanProbeVerifier) Verify(context.Context, verifier.VerifyContext) (*types.VerificationResult, error) {
	return nil, errSpanProbe
}

type spanProbeStrategy struct{ contextpkg.ContextStrategy }

func (spanProbeStrategy) Prepare(context.Context, []types.Message, contextpkg.TokenBudget) ([]types.Message, error) {
	return nil, errSpanProbe
}

type spanProbePermission struct{}

func (spanProbePermission) Check(context.Context, types.ToolDefinition, json.RawMessage) (*permission.PermissionResult, error) {
	return nil, errSpanProbe
}

func newSpanProbeLoop(prov *mockProvider) (*AgenticLoop, *tracetest.InMemoryExporter) {
	loop := buildTestLoop(prov)
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	loop.Tracer = tp.Tracer("test")
	return loop, exporter
}

func spanByName(t *testing.T, exporter *tracetest.InMemoryExporter, name string) tracetest.SpanStub {
	t.Helper()
	for _, s := range exporter.GetSpans() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no %q span exported", name)
	return tracetest.SpanStub{}
}

// assertSpanErrorScrubbed checks that the span carries a readable error
// recorded without the probe credential in its events or status.
func assertSpanErrorScrubbed(t *testing.T, span tracetest.SpanStub, wantStatus bool) {
	t.Helper()
	if wantStatus {
		if span.Status.Code != codes.Error {
			t.Errorf("status code = %v, want Error", span.Status.Code)
		}
		if strings.Contains(span.Status.Description, spanProbeKey) {
			t.Errorf("status description leaks the probe: %q", span.Status.Description)
		}
		if !strings.Contains(span.Status.Description, "upstream echoed key") {
			t.Errorf("status description = %q, want the readable error text", span.Status.Description)
		}
	}
	var message string
	for _, ev := range span.Events {
		for _, attr := range ev.Attributes {
			if strings.Contains(attr.Value.String(), spanProbeKey) {
				t.Errorf("event %q attribute %s leaks the probe: %q", ev.Name, attr.Key, attr.Value.String())
			}
			if ev.Name == "exception" && attr.Key == "exception.message" {
				message = attr.Value.AsString()
			}
		}
	}
	if !strings.Contains(message, "upstream echoed key") || !strings.Contains(message, "[REDACTED]") {
		t.Errorf("exception.message = %q, want readable scrubbed text", message)
	}
}

func TestLoop_SpanErrorsAreScrubbed(t *testing.T) {
	toolCallProvider := func() *mockProvider {
		return &mockProvider{events: []types.StreamEvent{
			{Type: "tool_call", ID: "tc1", Name: "test_tool", Input: map[string]any{}},
			{Type: "message_complete", StopReason: "tool_use"},
		}}
	}

	cases := []struct {
		name       string
		span       string
		wantStatus bool
		prov       *mockProvider
		setup      func(l *AgenticLoop, c *types.RunConfig)
	}{
		{
			name: "pre-run hook", span: "hooks.preRun", wantStatus: true,
			setup: func(l *AgenticLoop, _ *types.RunConfig) {
				l.Hooks = &fakeHookRunner{preErr: errSpanProbe}
			},
		},
		{
			name: "post-run hook", span: "hooks.postRun", wantStatus: true,
			setup: func(l *AgenticLoop, _ *types.RunConfig) {
				l.Hooks = &fakeHookRunner{postErr: errSpanProbe}
			},
		},
		{
			name: "git setup", span: "git.setup", wantStatus: true,
			setup: func(l *AgenticLoop, _ *types.RunConfig) {
				l.Git = &spanProbeGit{setupErr: errSpanProbe}
			},
		},
		{
			name: "git finalise", span: "git.finalise", wantStatus: true,
			setup: func(l *AgenticLoop, _ *types.RunConfig) {
				l.Git = &spanProbeGit{finaliseErr: errSpanProbe}
			},
		},
		{
			name: "git checkpoint", span: "git.checkpoint", prov: toolCallProvider(),
			setup: func(l *AgenticLoop, c *types.RunConfig) {
				l.Git = &spanProbeGit{checkpointErr: errSpanProbe}
				c.MaxTurns = 1
			},
		},
		{
			name: "verifier", span: "verifier.verify", wantStatus: true,
			setup: func(l *AgenticLoop, _ *types.RunConfig) {
				l.Verifier = spanProbeVerifier{}
			},
		},
		{
			name: "context strategy", span: "context.prepare", wantStatus: true,
			setup: func(l *AgenticLoop, _ *types.RunConfig) {
				l.Context = spanProbeStrategy{l.Context}
			},
		},
		{
			name: "guard", span: "guard.pre_turn", wantStatus: true,
			setup: func(l *AgenticLoop, c *types.RunConfig) {
				l.GuardRail = &errorGuard{err: errSpanProbe}
				c.GuardRail = &types.GuardRailConfig{Type: "granite-guardian", FailOpen: true}
			},
		},
		{
			name: "provider request", span: "provider.stream", wantStatus: true,
			setup: func(l *AgenticLoop, _ *types.RunConfig) {
				l.Provider = &errorProvider{err: errSpanProbe}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := tc.prov
			if prov == nil {
				prov = simpleSuccessProvider()
			}
			loop, exporter := newSpanProbeLoop(prov)
			config := buildTestConfig()
			tc.setup(loop, config)

			// Setup failures also return the error; only the span is under test.
			_, _ = loop.Run(context.Background(), config)
			assertSpanErrorScrubbed(t, spanByName(t, exporter, tc.span), tc.wantStatus)
		})
	}
}

func TestLoop_OTelChatSpanCarriesScrubbedProviderFailure(t *testing.T) {
	loop, exporter := otelLoop(t, nil)
	loop.Provider = &errorProvider{err: errSpanProbe}
	_, _ = loop.Run(context.Background(), buildTestConfig())

	spans := exporter.GetSpans()
	chats := spansNamed(spans, "chat claude-sonnet-4-6")
	if len(chats) != 1 {
		t.Fatalf("found %d chat spans, want 1", len(chats))
	}
	chat := chats[0]
	if got, _ := spanAttr(chat, "error.type"); got != "provider_request_failed" {
		t.Errorf("chat error.type = %q, want provider_request_failed", got)
	}
	if chat.Status.Code != codes.Error || !strings.Contains(chat.Status.Description, "upstream echoed key") {
		t.Errorf("chat status = %v %q, want Error with the readable error text", chat.Status.Code, chat.Status.Description)
	}
	assertSpanErrorScrubbed(t, spanByName(t, exporter, "provider.stream"), true)
	for _, s := range spans {
		if strings.Contains(s.Status.Description, spanProbeKey) {
			t.Errorf("%s status description leaks the probe: %q", s.Name, s.Status.Description)
		}
		for _, attr := range s.Attributes {
			if strings.Contains(attr.Value.String(), spanProbeKey) {
				t.Errorf("%s attribute %s leaks the probe", s.Name, attr.Key)
			}
		}
	}
}

func TestDispatchToolCall_PermissionErrorSpanIsScrubbed(t *testing.T) {
	loop, exporter := newSpanProbeLoop(&mockProvider{})
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:              "mutating_tool",
		Description:       "mutates the workspace",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{}}`),
		WorkspaceMutating: true,
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return "unreachable", nil
		},
	})
	loop.Tools = registry
	loop.Permissions = spanProbePermission{}

	loop.dispatchToolCall(context.Background(), types.ToolCall{
		ID: "tc1", Name: "mutating_tool", Input: json.RawMessage(`{}`),
	})
	assertSpanErrorScrubbed(t, spanByName(t, exporter, "permission.check"), true)
}

type spanProbeTypedError struct{ msg string }

func (e *spanProbeTypedError) Error() string { return e.msg }

func TestRecordSpanError_PreservesExceptionType(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"plain", errSpanProbe, "*errors.errorString"},
		{"typed", &spanProbeTypedError{msg: errSpanProbe.Error()}, "*core.spanProbeTypedError"},
		{"wrapped typed", fmt.Errorf("stream: %w", &spanProbeTypedError{msg: errSpanProbe.Error()}), "*core.spanProbeTypedError"},
		{"wrapped plain", fmt.Errorf("stream: %w", errSpanProbe), "*fmt.wrapError"},
		{"multi-wrapped typed", fmt.Errorf("%w: %w", errSpanProbe, &spanProbeTypedError{msg: "x"}), "*core.spanProbeTypedError"},
		{"joined typed", errors.Join(errSpanProbe, fmt.Errorf("inner: %w", &spanProbeTypedError{msg: "x"})), "*core.spanProbeTypedError"},
		{"joined plain", errors.Join(errSpanProbe, errSpanProbe), "*errors.joinError"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exporter := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			_, span := tp.Tracer("test").Start(context.Background(), "probe")
			recordSpanError(span, tc.err)
			span.End()

			stub := spanByName(t, exporter, "probe")
			assertSpanErrorScrubbed(t, stub, true)
			var got string
			for _, ev := range stub.Events {
				for _, attr := range ev.Attributes {
					if ev.Name == "exception" && attr.Key == "exception.type" {
						got = attr.Value.AsString()
					}
				}
			}
			if got != tc.want {
				t.Errorf("exception.type = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCoreSpanErrorsGoThroughScrubbingHelpers fails when core code records
// an error on a span without the scrubbing helpers in spanerror.go: any
// RecordError call, or a SetStatus(codes.Error, ...) whose description is
// not a string literal.
func TestCoreSpanErrorsGoThroughScrubbingHelpers(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	scanned := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "spanerror.go" {
			continue
		}
		scanned[name] = true
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "RecordError":
				t.Errorf("%s: RecordError must go through recordSpanError or recordSpanException", fset.Position(call.Pos()))
			case "SetStatus":
				if len(call.Args) != 2 {
					return true
				}
				code, ok := call.Args[0].(*ast.SelectorExpr)
				if !ok || code.Sel.Name != "Error" {
					return true
				}
				if lit, ok := call.Args[1].(*ast.BasicLit); !ok || lit.Kind != token.STRING {
					t.Errorf("%s: SetStatus(codes.Error, ...) needs a literal description or recordSpanError", fset.Position(call.Pos()))
				}
			}
			return true
		})
	}
	for _, name := range []string{"loop.go", "types.go"} {
		if !scanned[name] {
			t.Errorf("scan did not cover %s; the glob must run from the core package directory", name)
		}
	}
}
