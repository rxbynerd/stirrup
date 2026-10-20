package builtins

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

// TestStarterPolicies_PassTheLoadTimeLinter runs the shipped Cedar starters
// through the exact linter a run applies to an operator's policy file, with
// the schemas the built-in tools actually declare.
//
// The linter protects operator-authored files at load time, and this test
// protects the files the project ships as the template for those. A policy
// keyed on an attribute the tool does not declare matches nothing, so a
// starter must reference only declared attributes.
//
// The starters are held to a stricter bar than the linter enforces at
// runtime: warnings fail this test too. A shipped starter that warns is a
// starter an operator would copy and then have to reason about.
func TestStarterPolicies_PassTheLoadTimeLinter(t *testing.T) {
	must := require.New(t)
	registry := tool.NewRegistry()
	registerAllForTest(registry, &mockExecutor{})

	schemas := map[string]permission.ToolSchema{}
	for _, def := range registry.List() {
		schemas[def.Name] = permission.NewToolSchema(def.InputSchema)
	}

	policiesDir := filepath.Join("..", "..", "..", "..", "examples", "policies")
	entries, err := os.ReadDir(policiesDir)
	must.NoError(err)

	checked := 0
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".cedar" {
			continue
		}
		checked++
		t.Run(entry.Name(), func(t *testing.T) {
			must := require.New(t)
			ps, err := permission.LoadPolicySetFromFile(filepath.Join(policiesDir, entry.Name()))
			must.NoError(err, "starter fails the structural lint")

			findings := append(
				permission.LintPolicySetStructure(ps),
				permission.LintPolicySetTools(ps, schemas)...,
			)
			must.Empty(findings)
		})
	}
	must.NotZero(checked, "no .cedar files found in %s; directory moved?", policiesDir)
}

// TestBuiltinSchemas_AreClosed pins the property the linter's "can never
// fire" claim rests on: tool dispatch validates input against the JSON Schema
// before consulting Cedar, so an undeclared property is rejected before the
// policy engine sees it, but only when the schema closes the object. A
// built-in that stops setting additionalProperties:false would silently
// downgrade every undeclared-input-attribute error against it to a warning.
func TestBuiltinSchemas_AreClosed(t *testing.T) {
	registry := tool.NewRegistry()
	registerAllForTest(registry, &mockExecutor{})

	for _, def := range registry.List() {
		t.Run(def.Name, func(t *testing.T) {
			must := require.New(t)
			schema := permission.NewToolSchema(def.InputSchema)
			must.True(schema.Closed,
				"tool does not set additionalProperties:false; the policy linter cannot prove a clause keyed on an undeclared attribute is dead")
		})
	}
}

// TestAutonomousAllowListStarter_Decisions pins what the autonomous
// allow-list starter grants, forbids, and leaves to the deny-all fallback.
func TestAutonomousAllowListStarter_Decisions(t *testing.T) {
	must := require.New(t)
	path := filepath.Join("..", "..", "..", "..", "examples", "policies", "autonomous-allow-list.cedar")
	ps, err := permission.LoadPolicySetFromFile(path)
	must.NoError(err)

	policy, err := permission.NewPolicyEnginePolicy(permission.PolicyEngineConfig{
		PolicySet: ps,
		Fallback:  permission.NewDenyAll(),
		RunID:     "run-test",
		Mode:      "execution",
		Workspace: "/workspace",
	})
	must.NoError(err)

	cases := []struct {
		name  string
		tool  string
		input string
		allow bool
	}{
		{"read_file", "read_file", `{"path":"main.go"}`, true},
		{"list_directory", "list_directory", `{"path":"."}`, true},
		{"grep_files", "grep_files", `{"pattern":"TODO"}`, true},
		{"find_files", "find_files", `{"pattern":"*.go"}`, true},
		{"read_command_output", "read_command_output", `{"ref":"cmd-1"}`, true},
		{"edit_file", "edit_file", `{"path":"main.go"}`, true},
		{"benign command", "run_command", `{"command":"go test ./..."}`, true},
		{"recursive remove", "run_command", `{"command":"rm -rf /workspace"}`, false},
		{"force push", "run_command", `{"command":"git push --force origin main"}`, false},
		{"web_fetch falls to deny-all", "web_fetch", `{"url":"https://example.com/"}`, false},
		{"spawn_agent falls to deny-all", "spawn_agent", `{"prompt":"x"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			result, err := policy.Check(context.Background(), types.ToolDefinition{Name: tc.tool}, json.RawMessage(tc.input))
			must.NoError(err)
			must.Equal(tc.allow, result.Allowed, "reason: %q", result.Reason)
		})
	}
}
