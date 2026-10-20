package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// presentedNames returns the names the presenter advertises, in List order.
func presentedNames(p *Presenter) []string {
	defs := p.List()
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	return names
}

func TestProfileFor_ResolvesDefaultAndNamedProfiles(t *testing.T) {
	must := require.New(t)
	cases := []struct {
		name  string
		input string
	}{
		{name: "empty name", input: ""},
		{name: "default name", input: "default"},
	}
	for _, tc := range cases {
		t.Run(tc.name+" resolves to the default profile", func(t *testing.T) {
			must := require.New(t)
			p, ok := ProfileFor(tc.input)
			must.True(ok)
			must.Same(defaultProfile, p)
		})
	}

	p, ok := ProfileFor("coding-classic")
	must.True(ok)
	must.Equal("coding-classic", p.Name)

	_, ok = ProfileFor("does-not-exist")
	must.False(ok)
}

// Default profile presents tools under their internal names and resolves
// them unchanged: a config that names tools by their internal IDs keeps
// working (the config-compatibility guarantee).
func TestPresenter_DefaultProfileIsIdentity(t *testing.T) {
	must := require.New(t)
	reg := newRegistryWith(
		fixedTool("grep_files", "regex search"),
		fixedTool("edit_file", "edit"),
	)
	p, err := NewPresenter(reg, defaultProfile)
	must.NoError(err)

	must.Equal([]string{"grep_files", "edit_file"}, presentedNames(p),
		"default profile changed presented names")
	for _, name := range []string{"grep_files", "edit_file"} {
		t.Run(name+" resolves unchanged", func(t *testing.T) {
			must := require.New(t)
			got := p.Resolve(name)
			must.NotNil(got)
			must.Equal(name, got.Name)
		})
	}
}

// A profile presents aliases; dispatch resolves the alias to the internal
// tool, and the internal name is still callable for config compatibility.
func TestPresenter_AliasPresentationAndResolution(t *testing.T) {
	must := require.New(t)
	reg := newRegistryWith(
		fixedTool("grep_files", "regex search"),
		fixedTool("find_files", "glob search"),
		fixedTool("run_command", "shell"),
		fixedTool("read_file", "read"),
	)
	p, err := NewPresenter(reg, codingClassicProfile)
	must.NoError(err)

	wantPresented := map[string]string{
		"grep_files":  "grep",
		"find_files":  "find",
		"run_command": "bash",
		"read_file":   "read_file", // unaliased: presented unchanged
	}
	presented := presentedNames(p)
	for internal, alias := range wantPresented {
		t.Run(internal+" presented as "+alias, func(t *testing.T) {
			must := require.New(t)
			must.Contains(presented, alias)

			got := p.Resolve(alias)
			must.NotNil(got)
			must.Equal(internal, got.Name)

			// The internal name keeps resolving for config compatibility.
			byInternal := p.Resolve(internal)
			must.NotNil(byInternal)
			must.Equal(internal, byInternal.Name)

			must.Equal(internal, p.InternalName(alias))
		})
	}
}

// A profile may re-describe a tool without renaming it.
func TestPresenter_DescriptionOverrideKeepsName(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	reg := newRegistryWith(fixedTool("read_file", "original"))
	profile := &Profile{
		Name:         "describe-test",
		descriptions: map[string]string{"read_file": "rewritten for this provider"},
	}
	p, err := NewPresenter(reg, profile)
	must.NoError(err)

	defs := p.List()
	must.Len(defs, 1)
	check.Equal("read_file", defs[0].Name, "description override should not rename")
	check.Equal("rewritten for this provider", defs[0].Description, "description not overridden")
}

// A deliberate alias collision (two internal tools aliasing to the same
// target) must be resolved by the shared toolname collision code: both
// presented names are distinct and both resolve back to their own
// internal tool.
func TestPresenter_AliasCollisionResolvedByToolname(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)

	presented := presentedNames(p)
	must.Len(presented, 2)
	must.NotEqual(presented[0], presented[1], "collision not disambiguated")

	// Exactly one presented name keeps the bare "search" target; the other
	// carries toolname's deterministic "_<6 hex>" disambiguation suffix.
	must.Contains(presented, "search")
	var suffixed string
	for _, name := range presented {
		if name != "search" {
			suffixed = name
		}
	}
	must.Len(suffixed, len("search")+7, "suffixed name is not toolname's search_<6hex> form")
	must.True(strings.HasPrefix(suffixed, "search_"),
		"suffixed name %q is not toolname's search_<6hex> form", suffixed)

	resolved := make([]string, 0, len(presented))
	for _, name := range presented {
		got := p.Resolve(name)
		must.NotNil(got, "Resolve(presented %q)", name)
		resolved = append(resolved, got.Name)
	}
	must.ElementsMatch([]string{"grep_files", "find_files"}, resolved,
		"each presented name must resolve to its own internal tool")
}

// Collision resolution must be independent of registration order: the same
// tool set always pins the same alias to the same tool regardless of the
// order the registry listed them in.
func TestPresenter_CollisionOrderIndependent(t *testing.T) {
	must := require.New(t)
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
		must.NoError(err, "NewPresenter(%s,%s)", first, second)
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
		t.Run(internal+" keeps its presented name", func(t *testing.T) {
			must := require.New(t)
			must.Equal(forward[internal], reverse[internal],
				"presented name for %q depends on registration order", internal)
		})
	}
}

func TestPresenter_ResolveUnknownReturnsNil(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	reg := newRegistryWith(fixedTool("read_file", "read"))
	p, err := NewPresenter(reg, codingClassicProfile)
	must.NoError(err)

	check.Nil(p.Resolve("not_a_tool"))
	check.Equal("not_a_tool", p.InternalName("not_a_tool"), "unknown names should pass through")
}

// The presenter only aliases tools that are actually registered. A
// profile aliasing a tool a read-only mode declined to register (here
// run_command -> bash, with run_command absent from the registry)
// produces no "bash" alias and no way to reach the excluded tool.
func TestPresenter_AliasCannotSurfaceUnregisteredTool(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	// A read-only-style registry: no run_command / edit_file registered.
	reg := newRegistryWith(
		fixedTool("read_file", "read"),
		fixedTool("grep_files", "regex"),
		fixedTool("find_files", "glob"),
	)
	p, err := NewPresenter(reg, codingClassicProfile)
	must.NoError(err)

	// "bash" is run_command's alias, but run_command was never registered,
	// so no presented name should be "bash" and Resolve("bash") is nil.
	must.NotContains(presentedNames(p), "bash", "alias must not surface an unregistered tool")
	check.Nil(p.Resolve("bash"), "alias must not surface an unregistered tool")
	check.Nil(p.Resolve("run_command"), "tool was never registered")
}

func TestPresenter_Unwrap_ReturnsWrappedRegistry(t *testing.T) {
	must := require.New(t)
	reg := newRegistryWith(fixedTool("read_file", "read"))
	p, err := NewPresenter(reg, defaultProfile)
	must.NoError(err)

	must.Same(reg, p.Unwrap())
}
