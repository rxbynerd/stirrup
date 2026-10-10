package core

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/rxbynerd/stirrup/harness/internal/git"
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
func (g *spanProbeGit) Finalise(context.Context) (*git.GitResult, error) {
	return &git.GitResult{}, g.finaliseErr
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
			name: "git finalise", span: "git.finalise",
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
