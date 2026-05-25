package tool

import (
	"context"
	"encoding/json"
	"testing"
)

// fixedTool builds a no-op tool for presenter tests.
func fixedTool(name, desc string) *Tool {
	return &Tool{
		Name:        name,
		Description: desc,
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return name, nil
		},
	}
}

func newRegistryWith(tools ...*Tool) *Registry {
	r := NewRegistry()
	for _, t := range tools {
		r.Register(t)
	}
	return r
}

func TestProfileFor(t *testing.T) {
	for _, name := range []string{"", "default"} {
		p, ok := ProfileFor(name)
		if !ok {
			t.Fatalf("ProfileFor(%q) ok=false, want true", name)
		}
		if p != defaultProfile {
			t.Errorf("ProfileFor(%q) did not return the default profile", name)
		}
	}
	if p, ok := ProfileFor("coding-classic"); !ok || p.Name != "coding-classic" {
		t.Fatalf("ProfileFor(coding-classic) = %v, %v", p, ok)
	}
	if _, ok := ProfileFor("does-not-exist"); ok {
		t.Error("ProfileFor(unknown) ok=true, want false")
	}
}

// Default profile presents tools under their internal names and resolves
// them unchanged: a config that names tools by their internal IDs keeps
// working (the config-compatibility guarantee).
func TestPresenter_DefaultProfileIsIdentity(t *testing.T) {
	reg := newRegistryWith(
		fixedTool("grep_files", "regex search"),
		fixedTool("edit_file", "edit"),
	)
	p, err := NewPresenter(reg, defaultProfile)
	if err != nil {
		t.Fatalf("NewPresenter: %v", err)
	}
	defs := p.List()
	if len(defs) != 2 || defs[0].Name != "grep_files" || defs[1].Name != "edit_file" {
		t.Fatalf("default profile changed presented names: %+v", defs)
	}
	for _, name := range []string{"grep_files", "edit_file"} {
		if got := p.Resolve(name); got == nil || got.Name != name {
			t.Errorf("Resolve(%q) = %v, want tool with that internal name", name, got)
		}
	}
}

// A profile presents aliases; dispatch resolves the alias to the internal
// tool, and the internal name is still callable for config compatibility.
func TestPresenter_AliasPresentationAndResolution(t *testing.T) {
	reg := newRegistryWith(
		fixedTool("grep_files", "regex search"),
		fixedTool("find_files", "glob search"),
		fixedTool("run_command", "shell"),
		fixedTool("read_file", "read"),
	)
	p, err := NewPresenter(reg, codingClassicProfile)
	if err != nil {
		t.Fatalf("NewPresenter: %v", err)
	}

	wantPresented := map[string]string{
		"grep_files":  "grep",
		"find_files":  "find",
		"run_command": "bash",
		"read_file":   "read_file", // unaliased: presented unchanged
	}
	defs := p.List()
	gotPresented := map[string]bool{}
	for _, d := range defs {
		gotPresented[d.Name] = true
	}
	for internal, alias := range wantPresented {
		if !gotPresented[alias] {
			t.Errorf("List did not present %q as %q; defs=%+v", internal, alias, defs)
		}
	}

	// Resolve by alias returns the internal tool unchanged.
	for internal, alias := range wantPresented {
		got := p.Resolve(alias)
		if got == nil {
			t.Fatalf("Resolve(alias %q) = nil", alias)
		}
		if got.Name != internal {
			t.Errorf("Resolve(alias %q).Name = %q, want internal %q", alias, got.Name, internal)
		}
		// Internal name still resolves (config compatibility).
		if byInternal := p.Resolve(internal); byInternal == nil || byInternal.Name != internal {
			t.Errorf("Resolve(internal %q) = %v, want the tool", internal, byInternal)
		}
		if p.InternalName(alias) != internal {
			t.Errorf("InternalName(%q) = %q, want %q", alias, p.InternalName(alias), internal)
		}
	}
}

// A profile may re-describe a tool without renaming it.
func TestPresenter_DescriptionOverride(t *testing.T) {
	reg := newRegistryWith(fixedTool("read_file", "original"))
	profile := &Profile{
		Name:         "describe-test",
		descriptions: map[string]string{"read_file": "rewritten for this provider"},
	}
	p, err := NewPresenter(reg, profile)
	if err != nil {
		t.Fatalf("NewPresenter: %v", err)
	}
	defs := p.List()
	if defs[0].Name != "read_file" {
		t.Errorf("description override should not rename: got %q", defs[0].Name)
	}
	if defs[0].Description != "rewritten for this provider" {
		t.Errorf("description not overridden: got %q", defs[0].Description)
	}
}

// A deliberate alias collision (two internal tools aliasing to the same
// target) must be resolved by the shared toolname collision code: both
// presented names are distinct and both resolve back to their own
// internal tool. Proves the presenter routes through toolname rather than
// hand-rolling a second algorithm.
func TestPresenter_AliasCollisionResolvedByToolname(t *testing.T) {
	reg := newRegistryWith(
		fixedTool("grep_files", "regex"),
		fixedTool("find_files", "glob"),
	)
	// Both alias to "search" — a collision toolname.Build must disambiguate.
	collide := &Profile{
		Name: "collide",
		aliases: map[string]string{
			"grep_files": "search",
			"find_files": "search",
		},
	}
	p, err := NewPresenter(reg, collide)
	if err != nil {
		t.Fatalf("NewPresenter on collision: %v", err)
	}

	defs := p.List()
	presented := make([]string, len(defs))
	for i, d := range defs {
		presented[i] = d.Name
	}
	if presented[0] == presented[1] {
		t.Fatalf("collision not disambiguated: both presented as %q", presented[0])
	}

	// Exactly one presented name keeps the bare "search" target; the other
	// carries toolname's deterministic "_<6 hex>" disambiguation suffix.
	// Asserting the suffix shape (rather than just "they differ") proves
	// the presenter reused the toolname collision algorithm rather than
	// inventing its own scheme.
	var bare, suffixed string
	for _, name := range presented {
		if name == "search" {
			bare = name
		} else {
			suffixed = name
		}
	}
	if bare == "" || suffixed == "" {
		t.Fatalf("expected one bare and one suffixed name, got %v", presented)
	}
	if len(suffixed) != len("search")+7 || suffixed[:len("search")+1] != "search_" {
		t.Errorf("suffixed name %q is not toolname's search_<6hex> form", suffixed)
	}

	// Each presented name resolves back to its own internal tool.
	for _, d := range defs {
		got := p.Resolve(d.Name)
		if got == nil {
			t.Fatalf("Resolve(presented %q) = nil", d.Name)
		}
		if got.Name != "grep_files" && got.Name != "find_files" {
			t.Errorf("Resolve(presented %q) = %q, not an internal tool", d.Name, got.Name)
		}
	}
	// The two presented names map to two distinct internal tools.
	a := p.Resolve(presented[0]).Name
	b := p.Resolve(presented[1]).Name
	if a == b {
		t.Errorf("both presented names resolved to the same internal tool %q", a)
	}
}

// Collision resolution must be independent of registration order:
// NewPresenter sorts by internal ID before calling BuildFromCandidates,
// so the same tool set always pins the same alias to the same tool no
// matter which order the registry listed them in. Registering the two
// colliding tools in both orders must yield identical presented-name
// mappings (same bare-alias winner, same disambiguated suffix).
func TestPresenter_CollisionOrderIndependent(t *testing.T) {
	collide := &Profile{
		Name: "collide",
		aliases: map[string]string{
			"grep_files": "search",
			"find_files": "search",
		},
	}

	presentedFor := func(first, second string) map[string]string {
		reg := newRegistryWith(
			fixedTool(first, "f"),
			fixedTool(second, "s"),
		)
		p, err := NewPresenter(reg, collide)
		if err != nil {
			t.Fatalf("NewPresenter(%s,%s): %v", first, second, err)
		}
		// Map internal ID → presented name, independent of List order.
		m := map[string]string{}
		for _, d := range p.List() {
			m[p.InternalName(d.Name)] = d.Name
		}
		return m
	}

	forward := presentedFor("grep_files", "find_files")
	reverse := presentedFor("find_files", "grep_files")

	for _, internal := range []string{"grep_files", "find_files"} {
		if forward[internal] != reverse[internal] {
			t.Errorf("presented name for %q depends on registration order: %q vs %q",
				internal, forward[internal], reverse[internal])
		}
	}
}

func TestPresenter_ResolveUnknownReturnsNil(t *testing.T) {
	reg := newRegistryWith(fixedTool("read_file", "read"))
	p, err := NewPresenter(reg, codingClassicProfile)
	if err != nil {
		t.Fatalf("NewPresenter: %v", err)
	}
	if got := p.Resolve("not_a_tool"); got != nil {
		t.Errorf("Resolve(unknown) = %v, want nil", got)
	}
	// InternalName falls through for an unknown name.
	if p.InternalName("not_a_tool") != "not_a_tool" {
		t.Errorf("InternalName(unknown) should round-trip")
	}
}

// The presenter only aliases tools that are actually registered. A
// profile aliasing a tool a read-only mode declined to register (here
// run_command -> bash, with run_command absent from the registry)
// produces no "bash" alias and no way to reach the excluded tool — the
// alias cannot smuggle an unregistered tool past the registry. This is
// the registry-level half of the issue #234 read-only-mode invariant
// (the config-level half is enforced by ValidateRunConfig).
func TestPresenter_AliasCannotSurfaceUnregisteredTool(t *testing.T) {
	// A read-only-style registry: no run_command / edit_file registered.
	reg := newRegistryWith(
		fixedTool("read_file", "read"),
		fixedTool("grep_files", "regex"),
		fixedTool("find_files", "glob"),
	)
	p, err := NewPresenter(reg, codingClassicProfile)
	if err != nil {
		t.Fatalf("NewPresenter: %v", err)
	}

	// "bash" is run_command's alias, but run_command was never registered,
	// so no presented name should be "bash" and Resolve("bash") is nil.
	for _, d := range p.List() {
		if d.Name == "bash" {
			t.Fatalf("alias 'bash' present for an unregistered tool: %+v", d)
		}
	}
	if got := p.Resolve("bash"); got != nil {
		t.Errorf("Resolve('bash') = %v, want nil — alias must not surface an unregistered tool", got)
	}
	if got := p.Resolve("run_command"); got != nil {
		t.Errorf("Resolve('run_command') = %v, want nil — tool was never registered", got)
	}
}

func TestPresenter_Unwrap(t *testing.T) {
	reg := newRegistryWith(fixedTool("read_file", "read"))
	p, err := NewPresenter(reg, defaultProfile)
	if err != nil {
		t.Fatalf("NewPresenter: %v", err)
	}
	if p.Unwrap() != reg {
		t.Error("Unwrap did not return the wrapped registry")
	}
}
