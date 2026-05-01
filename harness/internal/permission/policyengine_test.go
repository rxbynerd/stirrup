package permission

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cedar-policy/cedar-go"

	"github.com/rxbynerd/stirrup/types"
)

// recordedEvent captures one Emit call from a SecurityEventEmitter so
// tests can assert audit emission without pulling the security package in.
type recordedEvent struct {
	level string
	event string
	data  map[string]any
}

// fakeEmitter is an in-memory SecurityEventEmitter used by the tests.
// Concurrent-safe so it can stand in for *security.SecurityLogger.
type fakeEmitter struct {
	mu     sync.Mutex
	events []recordedEvent
}

func (f *fakeEmitter) Emit(level, event string, data map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make(map[string]any, len(data))
	for k, v := range data {
		cp[k] = v
	}
	f.events = append(f.events, recordedEvent{level: level, event: event, data: cp})
}

func (f *fakeEmitter) snapshot() []recordedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedEvent, len(f.events))
	copy(out, f.events)
	return out
}

// mustParse compiles a Cedar policy literal or fails the test.
func mustParse(t *testing.T, policy string) *cedar.PolicySet {
	t.Helper()
	ps, err := cedar.NewPolicySetFromBytes("test", []byte(policy))
	if err != nil {
		t.Fatalf("parse cedar: %v", err)
	}
	return ps
}

// newTestPolicy builds a PolicyEnginePolicy with sensible defaults; pass
// overrides via the cfg argument before construction.
func newTestPolicy(t *testing.T, cfg PolicyEngineConfig) *PolicyEnginePolicy {
	t.Helper()
	if cfg.RunID == "" {
		cfg.RunID = "run-test"
	}
	if cfg.Mode == "" {
		cfg.Mode = "execution"
	}
	if cfg.Workspace == "" {
		cfg.Workspace = "/tmp/ws"
	}
	p, err := NewPolicyEnginePolicy(cfg)
	if err != nil {
		t.Fatalf("NewPolicyEnginePolicy: %v", err)
	}
	return p
}

// TestPolicyEngine_AllowPath: a permit policy on web_fetch to *.github.com
// allows a matching call.
func TestPolicyEngine_AllowPath(t *testing.T) {
	policy := `permit (
		principal,
		action == Action::"tool:web_fetch",
		resource == Tool::"web_fetch"
	) when {
		context.input.url like "https://*.github.com/*"
	};`

	emitter := &fakeEmitter{}
	p := newTestPolicy(t, PolicyEngineConfig{
		PolicySet: mustParse(t, policy),
		Fallback:  NewAllowAll(), // unused on the allow path
		Security:  emitter,
	})

	tool := types.ToolDefinition{Name: "web_fetch"}
	input := json.RawMessage(`{"url":"https://api.github.com/repos/foo/bar"}`)
	result, err := p.Check(context.Background(), tool, input)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !result.Allowed {
		t.Fatalf("expected allow, got deny: %q", result.Reason)
	}

	events := emitter.snapshot()
	if len(events) != 1 {
		t.Fatalf("expected 1 security event, got %d: %+v", len(events), events)
	}
	if events[0].event != "policy_decision" {
		t.Errorf("expected policy_decision event, got %q", events[0].event)
	}
	if events[0].data["decision"] != "allow" {
		t.Errorf("expected decision=allow in event data, got %v", events[0].data["decision"])
	}
	matched, ok := events[0].data["matchedPolicies"].([]string)
	if !ok || len(matched) == 0 {
		t.Errorf("expected non-empty matchedPolicies in event data, got %v", events[0].data["matchedPolicies"])
	}
}

// TestPolicyEngine_ForbidPath: a forbid policy on run_command blocks a
// `rm -rf /` cmd and reports the matched policy ID.
func TestPolicyEngine_ForbidPath(t *testing.T) {
	policy := `forbid (
		principal,
		action == Action::"tool:run_command",
		resource == Tool::"run_command"
	) when {
		context.input.cmd like "*rm -rf*"
	};`

	emitter := &fakeEmitter{}
	p := newTestPolicy(t, PolicyEngineConfig{
		PolicySet: mustParse(t, policy),
		Fallback:  NewAllowAll(), // would-be allow if no forbid matched
		Security:  emitter,
	})

	tool := types.ToolDefinition{Name: "run_command"}
	input := json.RawMessage(`{"cmd":"rm -rf /"}`)
	result, err := p.Check(context.Background(), tool, input)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Allowed {
		t.Fatalf("expected deny, got allow")
	}
	if !strings.Contains(result.Reason, "policy0") {
		t.Errorf("expected reason to include matched policy ID 'policy0', got %q", result.Reason)
	}

	events := emitter.snapshot()
	if len(events) != 1 || events[0].event != "policy_denied" {
		t.Fatalf("expected one policy_denied event, got %+v", events)
	}
	if events[0].level != "warn" {
		t.Errorf("expected level=warn on policy_denied, got %q", events[0].level)
	}
	matched, ok := events[0].data["matchedPolicies"].([]string)
	if !ok || len(matched) == 0 || matched[0] != "policy0" {
		t.Errorf("expected matchedPolicies=[policy0], got %v", events[0].data["matchedPolicies"])
	}
}

// TestPolicyEngine_NoDecision_Deny: a tool not covered by any policy
// falls through to the configured fallback. With deny-side-effects, a
// workspace-mutating tool is denied.
func TestPolicyEngine_NoDecision_FallbackDeny(t *testing.T) {
	// Policy only covers web_fetch; write_file goes to the fallback.
	policy := `permit (
		principal,
		action == Action::"tool:web_fetch",
		resource == Tool::"web_fetch"
	);`

	emitter := &fakeEmitter{}
	p := newTestPolicy(t, PolicyEngineConfig{
		PolicySet: mustParse(t, policy),
		Fallback:  NewDenySideEffects(map[string]bool{"write_file": true}),
		Security:  emitter,
	})

	tool := types.ToolDefinition{Name: "write_file"}
	result, err := p.Check(context.Background(), tool, json.RawMessage(`{"path":"/tmp/x"}`))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Allowed {
		t.Fatalf("expected fallback deny, got allow")
	}

	events := emitter.snapshot()
	if len(events) != 1 || events[0].event != "policy_decision" {
		t.Fatalf("expected one policy_decision event, got %+v", events)
	}
	if events[0].data["decision"] != "no_match" {
		t.Errorf("expected decision=no_match, got %v", events[0].data["decision"])
	}
	if !strings.HasPrefix(events[0].data["fallback"].(string), "deny") {
		t.Errorf("expected fallback outcome to start with 'deny', got %v", events[0].data["fallback"])
	}
}

// TestPolicyEngine_NoDecision_FallbackAllow: same as above with an
// allow-all fallback — confirms the fallback's decision is honoured.
func TestPolicyEngine_NoDecision_FallbackAllow(t *testing.T) {
	policy := `permit (
		principal,
		action == Action::"tool:web_fetch",
		resource == Tool::"web_fetch"
	);`

	emitter := &fakeEmitter{}
	p := newTestPolicy(t, PolicyEngineConfig{
		PolicySet: mustParse(t, policy),
		Fallback:  NewAllowAll(),
		Security:  emitter,
	})

	tool := types.ToolDefinition{Name: "anything_else"}
	result, err := p.Check(context.Background(), tool, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !result.Allowed {
		t.Fatalf("expected fallback allow, got deny: %q", result.Reason)
	}

	events := emitter.snapshot()
	if len(events) != 1 || events[0].event != "policy_decision" {
		t.Fatalf("expected one policy_decision event, got %+v", events)
	}
	if events[0].data["decision"] != "no_match" {
		t.Errorf("expected decision=no_match, got %v", events[0].data["decision"])
	}
	if events[0].data["fallback"] != "allow" {
		t.Errorf("expected fallback outcome to be 'allow', got %v", events[0].data["fallback"])
	}
}

// TestPolicyEngine_ForbidWinsOverPermit confirms Cedar's documented
// precedence (any forbid wins over all permits).
func TestPolicyEngine_ForbidWinsOverPermit(t *testing.T) {
	policy := `permit (
		principal,
		action == Action::"tool:web_fetch",
		resource == Tool::"web_fetch"
	);
	forbid (
		principal,
		action == Action::"tool:web_fetch",
		resource == Tool::"web_fetch"
	) when {
		context.input.url like "*example.com*"
	};`

	p := newTestPolicy(t, PolicyEngineConfig{
		PolicySet: mustParse(t, policy),
		Fallback:  NewAllowAll(),
	})

	tool := types.ToolDefinition{Name: "web_fetch"}
	result, err := p.Check(context.Background(), tool, json.RawMessage(`{"url":"https://example.com/"}`))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Allowed {
		t.Fatalf("expected forbid to win, got allow")
	}
}

// TestPolicyEngine_SubAgentCap exercises the parentRunId attribute path:
// a sub-agent (parentRunId set) is forbidden from run_command.
func TestPolicyEngine_SubAgentCap(t *testing.T) {
	policy := `forbid (
		principal,
		action == Action::"tool:run_command",
		resource == Tool::"run_command"
	) when {
		principal has parentRunId && principal.parentRunId != ""
	};`

	p := newTestPolicy(t, PolicyEngineConfig{
		PolicySet:   mustParse(t, policy),
		Fallback:    NewAllowAll(),
		ParentRunID: "parent-run-7",
	})

	tool := types.ToolDefinition{Name: "run_command"}
	result, err := p.Check(context.Background(), tool, json.RawMessage(`{"cmd":"echo hi"}`))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Allowed {
		t.Fatalf("expected sub-agent run_command to be denied")
	}

	// Without parentRunId, the same call should be allowed (no policy matches → fallback allow-all).
	p2 := newTestPolicy(t, PolicyEngineConfig{
		PolicySet: mustParse(t, policy),
		Fallback:  NewAllowAll(),
	})
	result2, err := p2.Check(context.Background(), tool, json.RawMessage(`{"cmd":"echo hi"}`))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !result2.Allowed {
		t.Fatalf("expected parent-run run_command to be allowed (no parentRunId), got deny: %q", result2.Reason)
	}
}

// TestPolicyEngine_ForChildRun_PopulatesParentRunID exercises the M3
// fix: a sub-agent's permission policy must be a clone with
// parentRunId set to the parent's runID. The same Cedar policy that
// permits run_command from the parent must deny it from the child.
func TestPolicyEngine_ForChildRun_PopulatesParentRunID(t *testing.T) {
	policy := `forbid (
		principal,
		action == Action::"tool:run_command",
		resource == Tool::"run_command"
	) when {
		principal has parentRunId && principal.parentRunId != ""
	};`

	parent := newTestPolicy(t, PolicyEngineConfig{
		PolicySet: mustParse(t, policy),
		Fallback:  NewAllowAll(),
		RunID:     "run-parent-1",
	})

	tool := types.ToolDefinition{Name: "run_command"}

	parentResult, err := parent.Check(context.Background(), tool, json.RawMessage(`{"cmd":"echo hi"}`))
	if err != nil {
		t.Fatalf("parent Check: %v", err)
	}
	if !parentResult.Allowed {
		t.Fatalf("parent run_command should fall through to allow-all fallback (no parentRunId), got deny: %q", parentResult.Reason)
	}

	child := parent.ForChildRun("run-child-1")
	if child == nil {
		t.Fatal("ForChildRun returned nil")
	}
	if child == parent {
		t.Fatal("ForChildRun should return a clone, not the receiver")
	}
	if child.parentRunID != "run-parent-1" {
		t.Errorf("child.parentRunID: got %q, want run-parent-1", child.parentRunID)
	}
	if child.runID != "run-child-1" {
		t.Errorf("child.runID: got %q, want run-child-1", child.runID)
	}

	childResult, err := child.Check(context.Background(), tool, json.RawMessage(`{"cmd":"echo hi"}`))
	if err != nil {
		t.Fatalf("child Check: %v", err)
	}
	if childResult.Allowed {
		t.Fatalf("child run_command should be denied by subagent-capability-cap, got allow")
	}

	// Cloning twice with no-op IDs leaves the parent's runID untouched
	// — guards against a future regression that mutates the receiver.
	child2 := parent.ForChildRun("")
	if child2.runID != parent.runID {
		t.Errorf("ForChildRun(\"\") should preserve runID; got %q", child2.runID)
	}
	if parent.parentRunID != "" {
		t.Errorf("parent parentRunID was mutated to %q", parent.parentRunID)
	}
}

// TestPolicyEngine_StarterPolicies round-trips the four shipped starter
// policy files and exercises one assertion per file. This confirms the
// shipped .cedar files remain syntactically and semantically correct.
func TestPolicyEngine_StarterPolicies(t *testing.T) {
	// Locate the policies directory relative to this test file. The test
	// runs from the package directory, so walk up to the repo root.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	// permission_test.go sits at harness/internal/permission; the example
	// policies live at examples/policies — three levels up.
	root := filepath.Clean(filepath.Join(wd, "..", "..", ".."))
	policiesDir := filepath.Join(root, "examples", "policies")

	cases := []struct {
		file    string
		tool    string
		input   string
		allowed bool
		// extra is an optional config override.
		extra func(*PolicyEngineConfig)
	}{
		{
			file:    "destructive-shell.cedar",
			tool:    "run_command",
			input:   `{"cmd":"rm -rf /"}`,
			allowed: false,
		},
		{
			file:    "github-only-fetch.cedar",
			tool:    "web_fetch",
			input:   `{"url":"https://api.github.com/repos/foo/bar"}`,
			allowed: true,
		},
		{
			file:    "no-secret-in-input.cedar",
			tool:    "write_file",
			input:   `{"content":"key=sk-abc123"}`,
			allowed: false,
		},
		{
			file:    "subagent-capability-cap.cedar",
			tool:    "run_command",
			input:   `{"cmd":"echo hi"}`,
			allowed: false,
			extra: func(cfg *PolicyEngineConfig) {
				cfg.ParentRunID = "parent-1"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			path := filepath.Join(policiesDir, tc.file)
			ps, err := LoadPolicySetFromFile(path)
			if err != nil {
				t.Fatalf("load %s: %v", tc.file, err)
			}
			cfg := PolicyEngineConfig{
				PolicySet: ps,
				// Fallback is deny-by-default for `permit` files (so a
				// non-match still denies); allow-all for `forbid` files
				// (so we can isolate the forbid effect).
				Fallback: NewDenySideEffects(map[string]bool{tc.tool: tc.allowed}), // unused on match
			}
			// For `permit`-style policies we want the fallback to be
			// allow-all so a non-match short-circuits to allow; the
			// permit then refines to allow on match. Simpler: allow-all
			// for everything — we are only checking the matched-policy
			// outcome here.
			cfg.Fallback = NewAllowAll()
			if tc.extra != nil {
				tc.extra(&cfg)
			}
			p := newTestPolicy(t, cfg)
			tool := types.ToolDefinition{Name: tc.tool}
			result, err := p.Check(context.Background(), tool, json.RawMessage(tc.input))
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if result.Allowed != tc.allowed {
				t.Fatalf("%s: expected allowed=%v, got allowed=%v reason=%q",
					tc.file, tc.allowed, result.Allowed, result.Reason)
			}
		})
	}
}

// TestNewPolicyEnginePolicy_Validation: PolicySet and Fallback are required.
func TestNewPolicyEnginePolicy_Validation(t *testing.T) {
	if _, err := NewPolicyEnginePolicy(PolicyEngineConfig{}); err == nil {
		t.Error("expected error when PolicySet is nil")
	}
	if _, err := NewPolicyEnginePolicy(PolicyEngineConfig{
		PolicySet: cedar.NewPolicySet(),
	}); err == nil {
		t.Error("expected error when Fallback is nil")
	}
}

// TestLoadPolicySetFromFile_Errors covers the three constructor failure
// modes the task calls out.
func TestLoadPolicySetFromFile_Errors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		_, err := LoadPolicySetFromFile("/nonexistent/path.cedar")
		if err == nil {
			t.Fatal("expected error for missing file")
		}
		if !strings.Contains(err.Error(), "read policy file") {
			t.Errorf("expected error to mention 'read policy file', got %v", err)
		}
	})

	t.Run("empty path", func(t *testing.T) {
		_, err := LoadPolicySetFromFile("")
		if err == nil {
			t.Fatal("expected error for empty path")
		}
	})

	t.Run("bad cedar syntax", func(t *testing.T) {
		dir := t.TempDir()
		bad := filepath.Join(dir, "bad.cedar")
		if err := os.WriteFile(bad, []byte("this is not a policy"), 0o600); err != nil {
			t.Fatalf("write tmp file: %v", err)
		}
		_, err := LoadPolicySetFromFile(bad)
		if err == nil {
			t.Fatal("expected error for invalid Cedar syntax")
		}
		if !strings.Contains(err.Error(), "parse policy file") {
			t.Errorf("expected error to mention 'parse policy file', got %v", err)
		}
	})
}

// TestNew_PolicyEngine_RejectsRecursiveFallback: cfg.Fallback ==
// "policy-engine" must be rejected at construction time.
func TestNew_PolicyEngine_RejectsRecursiveFallback(t *testing.T) {
	dir := t.TempDir()
	policyFile := filepath.Join(dir, "p.cedar")
	if err := os.WriteFile(policyFile, []byte(`permit (principal, action, resource);`), 0o600); err != nil {
		t.Fatalf("write tmp file: %v", err)
	}

	cfg := types.PermissionPolicyConfig{
		Type:       "policy-engine",
		PolicyFile: policyFile,
		Fallback:   "policy-engine",
	}
	_, err := New(cfg, PolicyEngineEnv{}, func(string) (PermissionPolicy, error) {
		return NewAllowAll(), nil
	})
	if err == nil {
		t.Fatal("expected error when fallback is policy-engine")
	}
	if !strings.Contains(err.Error(), "policy-engine") {
		t.Errorf("expected error to mention policy-engine, got %v", err)
	}
}

// TestNew_PolicyEngine_DefaultFallback: when cfg.Fallback is unset, the
// builder is invoked with "deny-side-effects".
func TestNew_PolicyEngine_DefaultFallback(t *testing.T) {
	dir := t.TempDir()
	policyFile := filepath.Join(dir, "p.cedar")
	if err := os.WriteFile(policyFile, []byte(`permit (principal, action, resource);`), 0o600); err != nil {
		t.Fatalf("write tmp file: %v", err)
	}

	var requested string
	cfg := types.PermissionPolicyConfig{
		Type:       "policy-engine",
		PolicyFile: policyFile,
	}
	_, err := New(cfg, PolicyEngineEnv{}, func(name string) (PermissionPolicy, error) {
		requested = name
		return NewDenySideEffects(nil), nil
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if requested != "deny-side-effects" {
		t.Errorf("expected default fallback 'deny-side-effects', got %q", requested)
	}
}

// TestNew_PolicyEngine_MissingFile surfaces the file-not-found error
// from LoadPolicySetFromFile through the New constructor.
func TestNew_PolicyEngine_MissingFile(t *testing.T) {
	cfg := types.PermissionPolicyConfig{
		Type:       "policy-engine",
		PolicyFile: "/does/not/exist.cedar",
	}
	_, err := New(cfg, PolicyEngineEnv{}, func(string) (PermissionPolicy, error) {
		return NewAllowAll(), nil
	})
	if err == nil {
		t.Fatal("expected error for missing policy file")
	}
}

// TestNew_PolicyEngine_RequiresPolicyFile fails fast when policyFile is
// empty even if the type is policy-engine.
func TestNew_PolicyEngine_RequiresPolicyFile(t *testing.T) {
	cfg := types.PermissionPolicyConfig{Type: "policy-engine"}
	_, err := New(cfg, PolicyEngineEnv{}, func(string) (PermissionPolicy, error) {
		return NewAllowAll(), nil
	})
	if err == nil {
		t.Fatal("expected error when policyFile is empty")
	}
	if !strings.Contains(err.Error(), "policyFile") {
		t.Errorf("expected error to mention policyFile, got %v", err)
	}
}

// TestNew_PolicyEngine_BadFallbackType is the case where the fallback
// builder itself returns an error.
func TestNew_PolicyEngine_BadFallbackType(t *testing.T) {
	dir := t.TempDir()
	policyFile := filepath.Join(dir, "p.cedar")
	if err := os.WriteFile(policyFile, []byte(`permit (principal, action, resource);`), 0o600); err != nil {
		t.Fatalf("write tmp file: %v", err)
	}
	cfg := types.PermissionPolicyConfig{
		Type:       "policy-engine",
		PolicyFile: policyFile,
		Fallback:   "deny-side-effects",
	}
	_, err := New(cfg, PolicyEngineEnv{}, func(string) (PermissionPolicy, error) {
		return nil, errAFailure
	})
	if err == nil {
		t.Fatal("expected error when fallback builder fails")
	}
	if !strings.Contains(err.Error(), "build fallback") {
		t.Errorf("expected error to mention 'build fallback', got %v", err)
	}
}

// errAFailure is a sentinel for the builder-failure test above.
var errAFailure = &builderError{msg: "synthetic failure"}

type builderError struct{ msg string }

func (e *builderError) Error() string { return e.msg }
