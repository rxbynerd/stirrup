package builtins

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
)

// TestStarterPolicies_PassTheLoadTimeLinter runs the shipped Cedar
// starters through the exact linter a run applies to an operator's policy
// file (issue #538), with the schemas the built-in tools actually declare.
//
// This is the repo-side half of the guarantee: the linter protects
// operator-authored files at load time, and this test protects the files
// the project ships as the template for those. Issue #524 shipped
// destructive-shell.cedar keyed on `cmd` while run_command declares
// `command`, so a plain `rm -rf /workspace` was allowed by the policy that
// claimed to forbid it.
//
// The starters are held to a stricter bar than the linter enforces at
// runtime: warnings fail this test too. A shipped starter that warns is a
// starter an operator would copy and then have to reason about.
func TestStarterPolicies_PassTheLoadTimeLinter(t *testing.T) {
	registry := tool.NewRegistry()
	registerAllForTest(registry, &mockExecutor{})

	schemas := map[string]permission.ToolSchema{}
	for _, def := range registry.List() {
		schemas[def.Name] = permission.NewToolSchema(def.InputSchema)
	}

	policiesDir := filepath.Join("..", "..", "..", "..", "examples", "policies")
	entries, err := os.ReadDir(policiesDir)
	if err != nil {
		t.Fatalf("read %s: %v", policiesDir, err)
	}

	checked := 0
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".cedar" {
			continue
		}
		checked++
		t.Run(entry.Name(), func(t *testing.T) {
			ps, err := permission.LoadPolicySetFromFile(filepath.Join(policiesDir, entry.Name()))
			if err != nil {
				t.Fatalf("starter fails the structural lint: %v", err)
			}
			findings := append(
				permission.LintPolicySetStructure(ps),
				permission.LintPolicySetTools(ps, schemas)...,
			)
			for _, f := range findings {
				t.Errorf("%s", f)
			}
		})
	}
	if checked == 0 {
		t.Fatalf("no .cedar files found in %s — directory moved?", policiesDir)
	}
}

// TestBuiltinSchemasAreClosed pins the property the linter's
// "can never fire" claim rests on: tool dispatch validates input against
// the JSON Schema before consulting Cedar, so an undeclared property is
// rejected before the policy engine sees it — but only when the schema
// closes the object. A built-in that stops setting
// additionalProperties:false would silently downgrade every
// undeclared-input-attribute error against it to a warning.
func TestBuiltinSchemasAreClosed(t *testing.T) {
	registry := tool.NewRegistry()
	registerAllForTest(registry, &mockExecutor{})

	for _, def := range registry.List() {
		schema := permission.NewToolSchema(def.InputSchema)
		if !schema.Closed {
			t.Errorf("tool %q does not set additionalProperties:false; the policy linter cannot prove a clause keyed on an undeclared attribute is dead", def.Name)
		}
	}
}
