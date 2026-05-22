package docsgen_test

import (
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"testing"

	"github.com/rxbynerd/stirrup/types/docsgen"
)

// parseFixture parses an inline Go source string and returns a primed
// Inspector. Keeps each test's input self-contained instead of relying
// on disk fixtures.
func parseFixture(t *testing.T, src string) *docsgen.Inspector {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	ins := docsgen.NewInspector()
	ins.Collect(file)
	return ins
}

// TestGenerator_SyntheticStruct exercises the happy path: a small
// struct with a doc-commented leaf field, an integer default const,
// and an enum-style `valid*` map should produce a FieldDoc with the
// expected Default and Enum hints.
func TestGenerator_SyntheticStruct(t *testing.T) {
	src := `package fixture

const DefaultThingMaxRetries = 3

var validThingModes = map[string]bool{
	"alpha": true,
	"beta":  true,
}

// ThingConfig configures the thing subsystem.
type ThingConfig struct {
	// MaxRetries is the per-call retry budget.
	MaxRetries int ` + "`json:\"maxRetries\"`" + `

	// Mode selects the thing mode.
	Mode string ` + "`json:\"mode\"`" + `
}
`
	ins := parseFixture(t, src)

	root, ok := ins.Structs["ThingConfig"]
	if !ok {
		t.Fatal("ThingConfig not collected")
	}
	if len(root.Fields) != 2 {
		t.Fatalf("got %d fields, want 2", len(root.Fields))
	}

	docs := map[string]*docsgen.FieldDoc{}
	ins.Walk(root, "", "ThingConfig", docs, map[string]bool{"ThingConfig": true})

	maxRetries, ok := docs["maxRetries"]
	if !ok {
		t.Fatal("maxRetries not in docs")
	}
	if maxRetries.Kind != "leaf" {
		t.Errorf("maxRetries.Kind = %q, want leaf", maxRetries.Kind)
	}
	if maxRetries.Default != "3 (DefaultThingMaxRetries)" {
		t.Errorf("maxRetries.Default = %q, want 3 (DefaultThingMaxRetries)", maxRetries.Default)
	}
	if maxRetries.Doc != "MaxRetries is the per-call retry budget." {
		t.Errorf("maxRetries.Doc = %q", maxRetries.Doc)
	}

	mode, ok := docs["mode"]
	if !ok {
		t.Fatal("mode not in docs")
	}
	if !reflect.DeepEqual(mode.Enum, []string{"alpha", "beta"}) {
		t.Errorf("mode.Enum = %v, want [alpha beta]", mode.Enum)
	}
}

// TestGenerator_CycleProtection guards against unbounded recursion on
// a self-referential struct (mirrors GuardRailConfig.Stages
// []GuardRailConfig). The FieldDoc for the cycling field is emitted,
// but its nested children are pruned: the walk on the inner copy
// halts immediately and no `node.children.children.*` keys appear.
func TestGenerator_CycleProtection(t *testing.T) {
	src := `package fixture

type Node struct {
	// Name is the node label.
	Name string ` + "`json:\"name\"`" + `
	// Children are nested nodes.
	Children []Node ` + "`json:\"children\"`" + `
}
`
	ins := parseFixture(t, src)
	root := ins.Structs["Node"]
	if root == nil {
		t.Fatal("Node not collected")
	}

	docs := map[string]*docsgen.FieldDoc{}
	ins.Walk(root, "", "Node", docs, map[string]bool{"Node": true})

	// `children` is a slice of Node — the FieldDoc must exist.
	fd, ok := docs["children"]
	if !ok {
		t.Fatal("children field missing")
	}
	if fd.Kind != "slice" {
		t.Errorf("children.Kind = %q, want slice", fd.Kind)
	}

	// Cycle protection: the recursive walk into Node from inside
	// children must not descend further. The `[]` placeholder is
	// emitted only when the element struct is not on the stack —
	// here Node is, so no `children.[]` path should appear.
	if _, exists := docs["children.[]"]; exists {
		t.Errorf("cycle protection failed: children.[] should not have been emitted")
	}
	for k := range docs {
		if k == "" {
			continue
		}
		// No path should have descended into a second `children` layer.
		if k != "children" && k != "name" && k != "children.name" && k != "children.children" {
			// Anything else would indicate runaway recursion.
			if len(k) > 30 {
				t.Errorf("unexpected deep path %q — cycle protection likely broken", k)
			}
		}
	}
}

// TestGenerator_DefaultForFieldLeadingWord pins the BatchProvider ->
// Batch leading-word heuristic: a const named
// `DefaultBatchMaxWaitSeconds` should resolve from a field
// `MaxWaitSeconds` on owner `BatchProviderConfig`, even though the
// stripped owner is `BatchProvider` (not `Batch`).
func TestGenerator_DefaultForFieldLeadingWord(t *testing.T) {
	src := `package fixture

const DefaultBatchMaxWaitSeconds = 86400

type BatchProviderConfig struct {
	MaxWaitSeconds int ` + "`json:\"maxWaitSeconds\"`" + `
}
`
	ins := parseFixture(t, src)

	got := ins.DefaultForField("BatchProviderConfig", "MaxWaitSeconds")
	want := "86400 (DefaultBatchMaxWaitSeconds)"
	if got != want {
		t.Errorf("DefaultForField = %q, want %q", got, want)
	}

	// Negative control: a field with no matching const returns empty.
	if got := ins.DefaultForField("BatchProviderConfig", "Unrelated"); got != "" {
		t.Errorf("DefaultForField(Unrelated) = %q, want empty", got)
	}
}

// TestGenerator_LeadingWords pins the helper that drives the
// leading-word heuristic. Confirms the case the comment cites
// (BatchProvider -> [Batch, BatchProvider]) and a single-word input.
func TestGenerator_LeadingWords(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"BatchProvider", []string{"Batch", "BatchProvider"}},
		{"ToolDispatch", []string{"Tool", "ToolDispatch"}},
		{"Mode", []string{"Mode"}},
		// Single-word and empty inputs round-trip to a one-element
		// slice — the helper guarantees the input itself is the last
		// element so DefaultForField always has at least one candidate.
		{"", []string{""}},
	}
	for _, tc := range cases {
		got := docsgen.LeadingWords(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("LeadingWords(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestGenerator_EnumSuffixFallback exercises the enum suffix chain:
// `validXxx` (no suffix), `validXxxs`, `validXxxValues`, `validXxxTypes`
// must all resolve.
func TestGenerator_EnumSuffixFallback(t *testing.T) {
	src := `package fixture

var validFooTypes = map[string]bool{"a": true, "b": true}
var validBars = map[string]bool{"x": true, "y": true}

type ThingConfig struct {
	Foo string ` + "`json:\"foo\"`" + `
	Bar string ` + "`json:\"bar\"`" + `
}
`
	ins := parseFixture(t, src)

	if got := ins.EnumForField("ThingConfig", "Foo"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("EnumForField(ThingConfig, Foo) = %v, want [a b]", got)
	}
	if got := ins.EnumForField("ThingConfig", "Bar"); !reflect.DeepEqual(got, []string{"x", "y"}) {
		t.Errorf("EnumForField(ThingConfig, Bar) = %v, want [x y]", got)
	}
	if got := ins.EnumForField("ThingConfig", "Nope"); got != nil {
		t.Errorf("EnumForField(ThingConfig, Nope) = %v, want nil", got)
	}
}

// TestGenerator_MapAndSliceSegments pins the synthetic path segments
// emitted for map-valued and slice-valued struct fields. Operators
// rely on `providers.<name>.foo` and `tools.mcpServers.[].uri` as
// stable explain paths.
func TestGenerator_MapAndSliceSegments(t *testing.T) {
	src := `package fixture

type ItemConfig struct {
	// URI is the item address.
	URI string ` + "`json:\"uri\"`" + `
}

type ContainerConfig struct {
	// Providers is a map keyed by name.
	Providers map[string]ItemConfig ` + "`json:\"providers\"`" + `
	// Items is an ordered list.
	Items []ItemConfig ` + "`json:\"items\"`" + `
}
`
	ins := parseFixture(t, src)
	root := ins.Structs["ContainerConfig"]
	docs := map[string]*docsgen.FieldDoc{}
	ins.Walk(root, "", "ContainerConfig", docs, map[string]bool{"ContainerConfig": true})

	// Map field: emits `<key>` segment with the lowercased key type.
	mapKeyPath := "providers.<string>"
	if _, ok := docs[mapKeyPath]; !ok {
		t.Errorf("map placeholder %q not emitted; have keys:", mapKeyPath)
		var keys []string
		for k := range docs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Log(keys)
	}
	if _, ok := docs["providers.<string>.uri"]; !ok {
		t.Errorf("map child path providers.<string>.uri not emitted")
	}

	// Slice field: emits `[]` segment.
	if _, ok := docs["items.[]"]; !ok {
		t.Errorf("slice placeholder items.[] not emitted")
	}
	if _, ok := docs["items.[].uri"]; !ok {
		t.Errorf("slice child path items.[].uri not emitted")
	}
}

// TestGenerator_PointerFieldOptional confirms a pointer-typed field
// is marked Optional and its emitted Type retains the `*` prefix.
// This is the generator-side guarantee that backs the
// renderText pointer-prefix branch in config_explain.go.
func TestGenerator_PointerFieldOptional(t *testing.T) {
	src := `package fixture

type Sub struct {
	// Name is the sub label.
	Name string ` + "`json:\"name\"`" + `
}

type Outer struct {
	// Inner is an optional sub-config.
	Inner *Sub ` + "`json:\"inner,omitempty\"`" + `
}
`
	ins := parseFixture(t, src)
	root := ins.Structs["Outer"]
	docs := map[string]*docsgen.FieldDoc{}
	ins.Walk(root, "", "Outer", docs, map[string]bool{"Outer": true})

	fd, ok := docs["inner"]
	if !ok {
		t.Fatal("inner not in docs")
	}
	if !fd.Optional {
		t.Errorf("inner.Optional = false, want true")
	}
	if fd.Type != "*Sub" {
		t.Errorf("inner.Type = %q, want *Sub", fd.Type)
	}
	// The struct walk should still descend into the underlying Sub
	// type even though the field is a pointer.
	if _, ok := docs["inner.name"]; !ok {
		t.Errorf("expected descent into pointer target: inner.name missing")
	}
}
