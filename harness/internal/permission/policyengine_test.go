package permission

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/cedar-policy/cedar-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	must := require.New(t)
	ps, err := cedar.NewPolicySetFromBytes("test", []byte(policy))
	must.NoError(err)
	return ps
}

// newTestPolicy builds a PolicyEnginePolicy with sensible defaults; pass
// overrides via the cfg argument before construction.
func newTestPolicy(t *testing.T, cfg PolicyEngineConfig) *PolicyEnginePolicy {
	t.Helper()
	must := require.New(t)
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
	must.NoError(err)
	return p
}

// A permit policy on web_fetch scoped to *.github.com allows a matching call.
func TestPolicyEngine_PermitMatchAllowsCall(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
	must.NoError(err)
	must.True(result.Allowed, "expected allow, got deny: %q", result.Reason)

	events := emitter.snapshot()
	must.Len(events, 1, "security events: %+v", events)
	check.Equal("policy_decision", events[0].event)
	check.Equal("allow", events[0].data["decision"])
	matched, _ := events[0].data["matchedPolicies"].([]string)
	check.NotEmpty(matched, "matchedPolicies in event data")
}

// A forbid policy on run_command blocks a `rm -rf /` command and reports the
// matched policy ID.
func TestPolicyEngine_ForbidMatchDeniesCall(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	policy := `forbid (
		principal,
		action == Action::"tool:run_command",
		resource == Tool::"run_command"
	) when {
		context.input.command like "*rm -rf*"
	};`

	emitter := &fakeEmitter{}
	p := newTestPolicy(t, PolicyEngineConfig{
		PolicySet: mustParse(t, policy),
		Fallback:  NewAllowAll(), // would-be allow if no forbid matched
		Security:  emitter,
	})

	tool := types.ToolDefinition{Name: "run_command"}
	input := json.RawMessage(`{"command":"rm -rf /"}`)
	result, err := p.Check(context.Background(), tool, input)
	must.NoError(err)
	must.False(result.Allowed, "expected deny, got allow")
	check.Contains(result.Reason, "policy0")

	events := emitter.snapshot()
	must.Len(events, 1, "events: %+v", events)
	check.Equal("policy_denied", events[0].event)
	check.Equal("warn", events[0].level)
	matched, _ := events[0].data["matchedPolicies"].([]string)
	must.NotEmpty(matched, "matchedPolicies in event data")
	check.Equal("policy0", matched[0])
}

// A tool not covered by any policy falls through to the configured fallback.
// With deny-side-effects, a workspace-mutating tool is denied.
func TestPolicyEngine_NoDecision_FallbackDeny(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
	must.NoError(err)
	must.False(result.Allowed, "expected fallback deny, got allow")

	events := emitter.snapshot()
	must.Len(events, 1, "events: %+v", events)
	check.Equal("policy_decision", events[0].event)
	check.Equal("no_match", events[0].data["decision"])
	fallback, _ := events[0].data["fallback"].(string)
	check.True(strings.HasPrefix(fallback, "deny"), "fallback outcome %v should start with 'deny'", events[0].data["fallback"])
}

// A tool with no matching policy takes the allow-all fallback's decision.
func TestPolicyEngine_NoDecision_FallbackAllow(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)
	must.True(result.Allowed, "expected fallback allow, got deny: %q", result.Reason)

	events := emitter.snapshot()
	must.Len(events, 1, "events: %+v", events)
	check.Equal("policy_decision", events[0].event)
	check.Equal("no_match", events[0].data["decision"])
	check.Equal("allow", events[0].data["fallback"])
}

// TestPolicyEngine_ForbidWinsOverPermit confirms Cedar's documented
// precedence (any forbid wins over all permits).
func TestPolicyEngine_ForbidWinsOverPermit(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	must.False(result.Allowed, "expected forbid to win, got allow")
}

// A sub-agent (parentRunId set) is forbidden from run_command.
func TestPolicyEngine_SubAgentCap_DeniesRunCommand(t *testing.T) {
	must := require.New(t)
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
	result, err := p.Check(context.Background(), tool, json.RawMessage(`{"command":"echo hi"}`))
	must.NoError(err)
	must.False(result.Allowed, "expected sub-agent run_command to be denied")

	// Without parentRunId, no policy matches and the allow-all fallback applies.
	p2 := newTestPolicy(t, PolicyEngineConfig{
		PolicySet: mustParse(t, policy),
		Fallback:  NewAllowAll(),
	})
	result2, err := p2.Check(context.Background(), tool, json.RawMessage(`{"command":"echo hi"}`))
	must.NoError(err)
	must.True(result2.Allowed, "expected parent-run run_command to be allowed (no parentRunId), got deny: %q", result2.Reason)
}

// TestGoValueToCedar_DeepNestingReturnsError: a tool input nested past
// maxCedarDepth must produce a clean error rather than recursing far
// enough to exhaust the goroutine stack and panic.
func TestGoValueToCedar_DeepNestingReturnsError(t *testing.T) {
	must := require.New(t)
	// Build a 100-deep nest. 100 > maxCedarDepth (64) so the limit
	// should fire well before any stack growth becomes a concern.
	var nested any = "leaf"
	for i := 0; i < 100; i++ {
		nested = map[string]any{"k": nested}
	}

	_, err := goValueToCedar(nested)
	must.ErrorContains(err, "too deeply nested")

	// Input at exactly the limit still converts, so legitimate nested objects keep working.
	var atLimit any = "leaf"
	for i := 0; i < maxCedarDepth-1; i++ {
		atLimit = map[string]any{"k": atLimit}
	}
	_, err = goValueToCedar(atLimit)
	must.NoError(err, "depth %d", maxCedarDepth-1)
}

// TestPolicyEngine_ForChildRun_PopulatesParentRunID: a sub-agent's
// permission policy must be a clone with parentRunId set to the
// parent's runID. The same Cedar policy that permits run_command from
// the parent must deny it from the child.
func TestPolicyEngine_ForChildRun_PopulatesParentRunID(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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

	parentResult, err := parent.Check(context.Background(), tool, json.RawMessage(`{"command":"echo hi"}`))
	must.NoError(err, "parent Check")
	must.True(parentResult.Allowed, "parent run_command should fall through to allow-all fallback (no parentRunId), got deny: %q", parentResult.Reason)

	child := parent.ForChildRun("run-child-1", "")
	must.NotNil(child, "ForChildRun returned nil")
	must.NotSame(parent, child, "ForChildRun should return a clone, not the receiver")
	check.Equal("run-parent-1", child.parentRunID)
	check.Equal("run-child-1", child.runID)

	childResult, err := child.Check(context.Background(), tool, json.RawMessage(`{"command":"echo hi"}`))
	must.NoError(err, "child Check")
	must.False(childResult.Allowed, "child run_command should be denied by subagent-capability-cap, got allow")

	// Empty IDs keep the existing values, and ForChildRun never mutates the receiver.
	child2 := parent.ForChildRun("", "")
	check.Equal(parent.runID, child2.runID, `ForChildRun("", "") should preserve runID`)
	check.Empty(parent.parentRunID, "parent parentRunID was mutated")
}

// TestPolicyEngine_ForChildRun_UsesChildMode pins that a child clone
// presents its own mode as principal.mode, so a Cedar rule keyed on a
// read-only mode fires for a child in that mode but not for its parent.
func TestPolicyEngine_ForChildRun_UsesChildMode(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	policy := `forbid (
		principal,
		action == Action::"tool:write_file",
		resource == Tool::"write_file"
	) when {
		principal.mode == "research"
	};`

	parent := newTestPolicy(t, PolicyEngineConfig{
		PolicySet: mustParse(t, policy),
		Fallback:  NewAllowAll(),
		RunID:     "run-parent-1",
		Mode:      "execution",
	})
	tool := types.ToolDefinition{Name: "write_file"}
	input := json.RawMessage(`{"path":"a.txt","content":"x"}`)

	allowsWriteFile := func(t *testing.T, p *PolicyEnginePolicy) bool {
		t.Helper()
		must := require.New(t)
		result, err := p.Check(context.Background(), tool, input)
		must.NoError(err, "Check")
		return result.Allowed
	}

	must.True(allowsWriteFile(t, parent), "execution parent: write_file should fall through to the allow-all fallback")

	research := parent.ForChildRun("run-child-1", "research")
	check.Equal("research", research.mode, "child mode")
	check.False(allowsWriteFile(t, research), "research child: write_file should be denied by the principal.mode forbid")
	check.Equal("execution", parent.mode, "parent mode")

	inherited := parent.ForChildRun("run-child-2", "")
	check.Equal("execution", inherited.mode, "empty childMode should keep the parent's mode")
	check.True(allowsWriteFile(t, inherited), "child inheriting execution mode: write_file should be allowed")
}

// The starter policy files are evaluated with the field names the tools'
// JSON Schemas declare (`command` for run_command, `content` for write_file,
// `url` for web_fetch). The schema validator rejects other names before Cedar
// runs, so a policy keyed on an undeclared name never fires in production.
func TestPolicyEngine_StarterPolicyDecisions(t *testing.T) {
	must := require.New(t)
	// Go runs tests from the package directory, so the repo root is three levels up.
	wd, err := os.Getwd()
	must.NoError(err)
	root := filepath.Clean(filepath.Join(wd, "..", "..", ".."))
	policiesDir := filepath.Join(root, "examples", "policies")

	cases := []struct {
		name    string
		file    string
		tool    string
		input   string
		allowed bool
		// fallback overrides the default allow-all fallback.
		fallback PermissionPolicy
		// extra is an optional config override.
		extra func(*PolicyEngineConfig)
	}{
		{
			name:    "destructive-shell denies rm -rf",
			file:    "destructive-shell.cedar",
			tool:    "run_command",
			input:   `{"command":"rm -rf /"}`,
			allowed: false,
		},
		{
			name:    "destructive-shell passes benign command to fallback",
			file:    "destructive-shell.cedar",
			tool:    "run_command",
			input:   `{"command":"go test ./..."}`,
			allowed: true,
		},
		{
			name:    "github-only-fetch permits github api",
			file:    "github-only-fetch.cedar",
			tool:    "web_fetch",
			input:   `{"url":"https://api.github.com/repos/foo/bar"}`,
			allowed: true,
		},
		{
			// Cedar `like` wildcards match `/`, so `https://*.github.com/*`
			// would match this URL. The anchored per-host patterns must not.
			name:     "github-only-fetch does not permit embedded-host bypass",
			file:     "github-only-fetch.cedar",
			tool:     "web_fetch",
			input:    `{"url":"https://evil.example/x.github.com/y"}`,
			allowed:  false,
			fallback: NewDenyAll(),
		},
		{
			name:     "github-only-fetch with deny-all denies other hosts",
			file:     "github-only-fetch.cedar",
			tool:     "web_fetch",
			input:    `{"url":"https://pypi.org/simple/requests/"}`,
			allowed:  false,
			fallback: NewDenyAll(),
		},
		{
			name:    "no-secret-in-input denies secret in write_file content",
			file:    "no-secret-in-input.cedar",
			tool:    "write_file",
			input:   `{"content":"key=sk-abc123"}`,
			allowed: false,
		},
		{
			name:    "no-secret-in-input denies secret in run_command command",
			file:    "no-secret-in-input.cedar",
			tool:    "run_command",
			input:   `{"command":"curl -H 'Authorization: Bearer sk-abc123' https://x"}`,
			allowed: false,
		},
		{
			name:    "subagent-capability-cap denies sub-agent shell",
			file:    "subagent-capability-cap.cedar",
			tool:    "run_command",
			input:   `{"command":"echo hi"}`,
			allowed: false,
			extra: func(cfg *PolicyEngineConfig) {
				cfg.ParentRunID = "parent-1"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			path := filepath.Join(policiesDir, tc.file)
			ps, err := LoadPolicySetFromFile(path)
			must.NoError(err, "load %s", tc.file)
			cfg := PolicyEngineConfig{
				PolicySet: ps,
				// allow-all isolates each file's own permit/forbid
				// effect; deny-the-rest cases override with deny-all,
				// the fallback the docs pair permit files with.
				Fallback: NewAllowAll(),
			}
			if tc.fallback != nil {
				cfg.Fallback = tc.fallback
			}
			if tc.extra != nil {
				tc.extra(&cfg)
			}
			p := newTestPolicy(t, cfg)
			tool := types.ToolDefinition{Name: tc.tool}
			result, err := p.Check(context.Background(), tool, json.RawMessage(tc.input))
			must.NoError(err)
			must.Equal(tc.allowed, result.Allowed, "reason: %q", result.Reason)
		})
	}
}

// PolicySet and Fallback are required.
func TestNewPolicyEnginePolicy_RequiresPolicySetAndFallback(t *testing.T) {
	t.Run("missing policy set", func(t *testing.T) {
		must := require.New(t)
		_, err := NewPolicyEnginePolicy(PolicyEngineConfig{})
		must.Error(err)
	})

	t.Run("missing fallback", func(t *testing.T) {
		must := require.New(t)
		_, err := NewPolicyEnginePolicy(PolicyEngineConfig{
			PolicySet: cedar.NewPolicySet(),
		})
		must.Error(err)
	})
}

// LoadPolicySetFromFile rejects a missing file, an empty path and invalid Cedar syntax.
func TestLoadPolicySetFromFile_Errors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		must := require.New(t)
		_, err := LoadPolicySetFromFile("/nonexistent/path.cedar")
		must.ErrorContains(err, "read policy file")
	})

	t.Run("empty path", func(t *testing.T) {
		must := require.New(t)
		_, err := LoadPolicySetFromFile("")
		must.Error(err)
	})

	t.Run("bad cedar syntax", func(t *testing.T) {
		must := require.New(t)
		dir := t.TempDir()
		bad := filepath.Join(dir, "bad.cedar")
		must.NoError(os.WriteFile(bad, []byte("this is not a policy"), 0o600))
		_, err := LoadPolicySetFromFile(bad)
		must.ErrorContains(err, "parse policy file")
	})
}

// cfg.Fallback == "policy-engine" must be rejected at construction time.
func TestNew_PolicyEngine_RejectsRecursiveFallback(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	policyFile := filepath.Join(dir, "p.cedar")
	must.NoError(os.WriteFile(policyFile, []byte(`permit (principal, action, resource);`), 0o600))

	cfg := types.PermissionPolicyConfig{
		Type:       "policy-engine",
		PolicyFile: policyFile,
		Fallback:   "policy-engine",
	}
	_, err := New(cfg, PolicyEngineEnv{}, func(string) (PermissionPolicy, error) {
		return NewAllowAll(), nil
	})
	must.ErrorContains(err, "policy-engine")
}

// When cfg.Fallback is unset, the builder is invoked with "deny-side-effects".
func TestNew_PolicyEngine_DefaultFallback(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	policyFile := filepath.Join(dir, "p.cedar")
	must.NoError(os.WriteFile(policyFile, []byte(`permit (principal, action, resource);`), 0o600))

	var requested string
	cfg := types.PermissionPolicyConfig{
		Type:       "policy-engine",
		PolicyFile: policyFile,
	}
	_, err := New(cfg, PolicyEngineEnv{}, func(name string) (PermissionPolicy, error) {
		requested = name
		return NewDenySideEffects(nil), nil
	})
	must.NoError(err, "New")
	must.Equal("deny-side-effects", requested, "default fallback")
}

// TestNew_PolicyEngine_MissingFileErrors surfaces the file-not-found error
// from LoadPolicySetFromFile through the New constructor.
func TestNew_PolicyEngine_MissingFileErrors(t *testing.T) {
	must := require.New(t)
	cfg := types.PermissionPolicyConfig{
		Type:       "policy-engine",
		PolicyFile: "/does/not/exist.cedar",
	}
	_, err := New(cfg, PolicyEngineEnv{}, func(string) (PermissionPolicy, error) {
		return NewAllowAll(), nil
	})
	must.Error(err)
}

// TestPermissionNew_EmptyType_ReturnsError: a zero-value
// PermissionPolicyConfig must error rather than silently coercing to
// allow-all.
func TestPermissionNew_EmptyType_ReturnsError(t *testing.T) {
	must := require.New(t)
	_, err := New(types.PermissionPolicyConfig{}, PolicyEngineEnv{}, nil)
	must.ErrorContains(err, "type is required")
}

// TestNew_PolicyEngine_RequiresPolicyFile fails fast when policyFile is
// empty even if the type is policy-engine.
func TestNew_PolicyEngine_RequiresPolicyFile(t *testing.T) {
	must := require.New(t)
	cfg := types.PermissionPolicyConfig{Type: "policy-engine"}
	_, err := New(cfg, PolicyEngineEnv{}, func(string) (PermissionPolicy, error) {
		return NewAllowAll(), nil
	})
	must.ErrorContains(err, "policyFile")
}

// TestNew_PolicyEngine_FallbackBuildFailureErrors is the case where the
// fallback builder itself returns an error.
func TestNew_PolicyEngine_FallbackBuildFailureErrors(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	policyFile := filepath.Join(dir, "p.cedar")
	must.NoError(os.WriteFile(policyFile, []byte(`permit (principal, action, resource);`), 0o600))
	cfg := types.PermissionPolicyConfig{
		Type:       "policy-engine",
		PolicyFile: policyFile,
		Fallback:   "deny-side-effects",
	}
	_, err := New(cfg, PolicyEngineEnv{}, func(string) (PermissionPolicy, error) {
		return nil, errAFailure
	})
	must.ErrorContains(err, "build fallback")
}

// errAFailure is a sentinel for the builder-failure test above.
var errAFailure = &builderError{msg: "synthetic failure"}

type builderError struct{ msg string }

func (e *builderError) Error() string { return e.msg }

// TestLoadPolicySetFromFile_RejectsUnboundedOrBlockingFiles pins that a
// policy path naming a FIFO is refused at once rather than waiting for a
// writer, and that a file over the size cap is refused before parsing.
func TestLoadPolicySetFromFile_RejectsUnboundedOrBlockingFiles(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "policy.cedar")
	require.New(t).NoError(syscall.Mkfifo(fifo, 0o600))
	big := filepath.Join(dir, "big.cedar")
	require.New(t).NoError(os.WriteFile(big, bytes.Repeat([]byte(" "), maxPolicyFileBytes+1), 0o600))
	cases := []struct {
		name, path, want string
	}{
		{name: "fifo", path: fifo, want: "not a regular file"},
		{name: "directory", path: dir, want: "not a regular file"},
		{name: "oversized", path: big, want: "exceeds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, err := LoadPolicySetFromFile(tc.path)
			var fileErr *PolicyFileError
			must.ErrorAs(err, &fileErr)
			must.ErrorContains(err, tc.want)
		})
	}
}
