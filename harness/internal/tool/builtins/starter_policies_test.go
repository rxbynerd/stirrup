package builtins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
)

var (
	cedarInputAttrRe = regexp.MustCompile(`context\.input(?:\.(\w+)|\s+has\s+(\w+))`)
	cedarToolRe      = regexp.MustCompile(`Action::"tool:(\w+)"`)
)

// TestStarterPolicies_InputAttrsMatchToolSchemas cross-checks every
// `context.input` attribute referenced by the shipped Cedar starter
// policies against the input schemas the built-in tools actually
// declare. Tool schemas set additionalProperties:false, so an attribute
// no schema declares can never reach Cedar — a policy keyed on one
// parses, loads, and silently never fires. Issue #524 shipped exactly
// that: destructive-shell.cedar keyed on `cmd` while run_command
// declares `command`, so a plain `rm -rf /workspace` was allowed by the
// policy that claimed to forbid it.
//
// Scoping rule: when a policy file names tools via Action::"tool:X",
// each referenced attribute must be declared by at least one named
// tool's schema; a file with no Action constraint (e.g.
// no-secret-in-input.cedar applies to every tool) checks against the
// union of all registered schemas.
//
// This covers the shipped starters only. Operator-authored policy files
// get no equivalent load-time check yet — that follow-up is tracked on
// issue #524.
func TestStarterPolicies_InputAttrsMatchToolSchemas(t *testing.T) {
	registry := tool.NewRegistry()
	registerAllForTest(registry, &mockExecutor{})

	schemaProps := map[string]map[string]bool{}
	union := map[string]bool{}
	for _, def := range registry.List() {
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
			t.Fatalf("parse %s schema: %v", def.Name, err)
		}
		props := map[string]bool{}
		for name := range schema.Properties {
			props[name] = true
			union[name] = true
		}
		schemaProps[def.Name] = props
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	policiesDir := filepath.Join(filepath.Clean(filepath.Join(wd, "..", "..", "..", "..")), "examples", "policies")
	entries, err := os.ReadDir(policiesDir)
	if err != nil {
		t.Fatalf("read %s: %v", policiesDir, err)
	}

	checked := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".cedar") {
			continue
		}
		checked++
		t.Run(entry.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(policiesDir, entry.Name()))
			if err != nil {
				t.Fatalf("read policy: %v", err)
			}
			// Strip comment lines so prose mentioning field names or
			// retired patterns is not parsed as a reference.
			var code []string
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "//") {
					continue
				}
				code = append(code, line)
			}
			source := strings.Join(code, "\n")

			attrs := map[string]bool{}
			for _, m := range cedarInputAttrRe.FindAllStringSubmatch(source, -1) {
				if m[1] != "" {
					attrs[m[1]] = true
				}
				if m[2] != "" {
					attrs[m[2]] = true
				}
			}
			if len(attrs) == 0 {
				return // policy keys on principal/resource, not input
			}

			scoped := map[string]bool{}
			for _, m := range cedarToolRe.FindAllStringSubmatch(source, -1) {
				for name := range schemaProps[m[1]] {
					scoped[name] = true
				}
			}
			declared := scoped
			if len(cedarToolRe.FindAllString(source, -1)) == 0 {
				declared = union
			}

			for attr := range attrs {
				if !declared[attr] {
					t.Errorf("policy references context.input.%s, which no targeted tool schema declares — the clause can never fire (see issue #524)", attr)
				}
			}
		})
	}
	if checked == 0 {
		t.Fatalf("no .cedar files found in %s — directory moved?", policiesDir)
	}
}
