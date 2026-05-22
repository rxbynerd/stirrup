// Package docsgen extracts the testable core of the RunConfig
// documentation generator. The driver at scripts/gen-runconfig-docs.go
// wires this package up to types/runconfig.go and emits the lookup
// table that backs the `stirrup config explain` subcommand.
//
// The split exists so the parsing heuristics — pointer/slice/map
// kind classification, cycle protection in walk, DefaultForField's
// leading-word resolution, EnumForField's suffix chain — are
// reachable by `go test`. Without this split the 700-line generator
// is a //go:build ignore file with no test coverage; `just verify-docs`
// catches output drift but not internal regressions.
//
// This package is generator-side machinery, not a public API: nothing
// in harness/ or eval/ depends on it at runtime. It lives under
// types/ (rather than internal/) so the //go:build ignore driver in
// scripts/ can import it through the workspace.
package docsgen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
)

// FieldDoc mirrors the type emitted into the generated file. Callers
// transcribe these into a Go literal; this package keeps the shape
// minimal so a downstream rendering layer can format it.
type FieldDoc struct {
	Path        string
	Name        string
	Type        string
	JSONTag     string
	Doc         string
	Default     string
	Enum        []string
	Children    []string
	Kind        string // "struct" | "leaf" | "map" | "slice"
	Optional    bool   // pointer field
	ParentPath  string
	OwnerStruct string
}

// StructInfo carries the inspected fields of a Go struct declaration.
type StructInfo struct {
	Name   string
	Fields []*FieldInfo
}

// FieldInfo describes one exported field on a struct: its source
// shape (pointer/slice/map), its JSON tag, and the doc comment we
// surface for it.
type FieldInfo struct {
	Name     string
	JSONTag  string
	Doc      string
	GoType   string
	IsPtr    bool
	IsSlice  bool
	IsMap    bool
	ElemType string // map value type or slice elem; "" otherwise
	MapKey   string // map key type; "" otherwise
}

// Inspector accumulates the parsed shape of a Go source file:
// every struct declaration, every `valid*` map literal that names an
// enum set, every integer/float constant whose name follows the
// Default* / default* convention.
type Inspector struct {
	// Structs maps type name -> struct info, in source order.
	Structs map[string]*StructInfo

	// Enums maps the name of a `var validXxx = map[string]bool{...}`
	// to the sorted set of its keys (excluding empty strings).
	Enums map[string][]string

	// ConstInts maps every exported / unexported integer-like const
	// to its source value, used to surface defaults like
	// "86400 (DefaultBatchMaxWaitSeconds)".
	ConstInts map[string]string
}

// NewInspector returns an empty Inspector ready for Collect.
func NewInspector() *Inspector {
	return &Inspector{
		Structs:   map[string]*StructInfo{},
		Enums:     map[string][]string{},
		ConstInts: map[string]string{},
	}
}

// ParseFile parses a Go source file and returns its AST. Callers
// pass the result to Collect.
func ParseFile(path string) (*ast.File, *token.FileSet, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return file, fset, nil
}

// Collect walks every top-level declaration in file and populates the
// Inspector's Structs, Enums, and ConstInts maps. Call once per source
// file; calling it again accumulates.
func (ins *Inspector) Collect(file *ast.File) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		switch gen.Tok {
		case token.TYPE:
			ins.collectTypes(gen)
		case token.VAR:
			ins.collectVars(gen)
		case token.CONST:
			ins.collectConsts(gen)
		}
	}
}

func (ins *Inspector) collectTypes(gen *ast.GenDecl) {
	for _, spec := range gen.Specs {
		ts, ok := spec.(*ast.TypeSpec)
		if !ok {
			continue
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			continue
		}
		info := &StructInfo{Name: ts.Name.Name}
		for _, f := range st.Fields.List {
			// Anonymous / embedded fields would need recursing into the
			// embedded type; the RunConfig hierarchy doesn't use them.
			// Skip — if someone adds an embedded field later,
			// regenerating will silently drop it.
			if len(f.Names) == 0 {
				continue
			}
			doc := commentText(f.Doc)
			if doc == "" {
				// Trailing line-comments are captured by Field.Comment,
				// not Field.Doc. Use them as a fallback so terse fields
				// still surface useful text.
				doc = commentText(f.Comment)
			}
			for _, name := range f.Names {
				if !name.IsExported() {
					continue
				}
				fi := &FieldInfo{
					Name: name.Name,
					Doc:  doc,
				}
				PopulateType(fi, f.Type)
				fi.JSONTag = jsonTagName(f.Tag, name.Name)
				if fi.JSONTag == "-" {
					continue
				}
				info.Fields = append(info.Fields, fi)
			}
		}
		ins.Structs[info.Name] = info
	}
}

func (ins *Inspector) collectVars(gen *ast.GenDecl) {
	for _, spec := range gen.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, name := range vs.Names {
			if i >= len(vs.Values) {
				continue
			}
			// Only collect `valid*` map literals of map[string]bool.
			if !strings.HasPrefix(name.Name, "valid") {
				continue
			}
			lit, ok := vs.Values[i].(*ast.CompositeLit)
			if !ok {
				continue
			}
			mt, ok := lit.Type.(*ast.MapType)
			if !ok {
				continue
			}
			kt, ok := mt.Key.(*ast.Ident)
			if !ok || kt.Name != "string" {
				continue
			}
			vt, ok := mt.Value.(*ast.Ident)
			if !ok || vt.Name != "bool" {
				continue
			}
			vals := make([]string, 0, len(lit.Elts))
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.BasicLit)
				if !ok || key.Kind != token.STRING {
					continue
				}
				s, err := strconv.Unquote(key.Value)
				if err != nil || s == "" {
					continue
				}
				vals = append(vals, s)
			}
			sort.Strings(vals)
			ins.Enums[name.Name] = vals
		}
	}
}

func (ins *Inspector) collectConsts(gen *ast.GenDecl) {
	for _, spec := range gen.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, name := range vs.Names {
			if i >= len(vs.Values) {
				continue
			}
			lit, ok := vs.Values[i].(*ast.BasicLit)
			if !ok {
				continue
			}
			if lit.Kind != token.INT && lit.Kind != token.FLOAT {
				continue
			}
			ins.ConstInts[name.Name] = lit.Value
		}
	}
}

// PopulateType decodes a Go type expression onto fi. Exposed so tests
// can build synthetic FieldInfos without a parsed source file.
func PopulateType(fi *FieldInfo, expr ast.Expr) {
	switch t := expr.(type) {
	case *ast.StarExpr:
		fi.IsPtr = true
		PopulateType(fi, t.X)
		// PopulateType overwrote GoType with the underlying. Re-prefix.
		fi.GoType = "*" + fi.GoType
	case *ast.Ident:
		fi.GoType = t.Name
	case *ast.SelectorExpr:
		// e.g. time.Duration
		if x, ok := t.X.(*ast.Ident); ok {
			fi.GoType = x.Name + "." + t.Sel.Name
		} else {
			fi.GoType = t.Sel.Name
		}
	case *ast.ArrayType:
		fi.IsSlice = true
		elem := &FieldInfo{}
		PopulateType(elem, t.Elt)
		fi.ElemType = elem.GoType
		fi.GoType = "[]" + elem.GoType
	case *ast.MapType:
		fi.IsMap = true
		k := &FieldInfo{}
		v := &FieldInfo{}
		PopulateType(k, t.Key)
		PopulateType(v, t.Value)
		fi.MapKey = k.GoType
		fi.ElemType = v.GoType
		fi.GoType = "map[" + k.GoType + "]" + v.GoType
	default:
		fi.GoType = "unknown"
	}
}

func jsonTagName(tag *ast.BasicLit, fallback string) string {
	if tag == nil {
		return LowerFirst(fallback)
	}
	raw, err := strconv.Unquote(tag.Value)
	if err != nil {
		return LowerFirst(fallback)
	}
	// crude tag parse — the canonical "json" key is the only one we need.
	for _, part := range splitTag(raw) {
		if !strings.HasPrefix(part, "json:") {
			continue
		}
		inner, err := strconv.Unquote(strings.TrimPrefix(part, "json:"))
		if err != nil {
			return LowerFirst(fallback)
		}
		comma := strings.IndexByte(inner, ',')
		if comma >= 0 {
			inner = inner[:comma]
		}
		if inner == "" {
			return LowerFirst(fallback)
		}
		return inner
	}
	return LowerFirst(fallback)
}

func splitTag(tag string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range tag {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ' ' && !inQuote:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// LowerFirst lowercases the first rune of s. Exported for the driver's
// map-key segment generation.
func LowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func commentText(cg *ast.CommentGroup) string {
	if cg == nil {
		return ""
	}
	// ast.CommentGroup.Text() drops the leading "//" and trailing
	// spaces, merging consecutive comments into one paragraph-style
	// string.
	return strings.TrimRight(cg.Text(), "\n")
}

// RootDoc builds the entry for the empty path (== `--root`). Children
// are the top-level fields of the root struct in stable order.
func (ins *Inspector) RootDoc(root *StructInfo) *FieldDoc {
	children := make([]string, 0, len(root.Fields))
	for _, f := range root.Fields {
		children = append(children, f.JSONTag)
	}
	sort.Strings(children)
	return &FieldDoc{
		Path:        "",
		Name:        "RunConfig",
		Type:        "RunConfig",
		JSONTag:     "",
		Doc:         "RunConfig fully describes a single harness run. Use `stirrup config explain <field>` to drill into individual fields.",
		Kind:        "struct",
		Children:    children,
		OwnerStruct: "RunConfig",
	}
}

// Walk descends through StructInfo, emitting one FieldDoc per
// reachable field path. Pointer fields are dereferenced for display;
// their underlying struct's children are still walked.
//
// Map fields contribute a single placeholder path
// (`<prefix>.<jsonTag>.<key>`) so an operator can ask
// `stirrup config explain providers.<name>` to see the per-entry
// shape. Slice elements use `[]`.
//
// Cycle protection: `stack` tracks struct names currently on the
// descent path; revisiting one halts the recursive walk for that
// field. The FieldDoc for the cycling field is still emitted; only
// its nested children are pruned. This guards self-referential types
// like `GuardRailConfig.Stages []GuardRailConfig`.
func (ins *Inspector) Walk(s *StructInfo, prefix, ownerName string, docs map[string]*FieldDoc, stack map[string]bool) {
	for _, f := range s.Fields {
		path := f.JSONTag
		if prefix != "" {
			path = prefix + "." + f.JSONTag
		}
		fd := &FieldDoc{
			Path:        path,
			Name:        f.Name,
			Type:        f.GoType,
			JSONTag:     f.JSONTag,
			Doc:         f.Doc,
			Optional:    f.IsPtr,
			ParentPath:  prefix,
			OwnerStruct: ownerName,
		}

		// Default / enum hints surfaced via name-matching.
		if dv := ins.DefaultForField(ownerName, f.Name); dv != "" {
			fd.Default = dv
		}
		if enum := ins.EnumForField(ownerName, f.Name); enum != nil {
			fd.Enum = enum
		}

		switch {
		case f.IsMap:
			fd.Kind = "map"
			if child, ok := ins.Structs[f.ElemType]; ok && !stack[child.Name] {
				keyPath := path + ".<" + LowerFirst(f.MapKey) + ">"
				childPaths := make([]string, 0, len(child.Fields))
				for _, cf := range child.Fields {
					childPaths = append(childPaths, cf.JSONTag)
				}
				sort.Strings(childPaths)
				docs[keyPath] = &FieldDoc{
					Path:        keyPath,
					Name:        child.Name,
					Type:        child.Name,
					Doc:         fmt.Sprintf("Per-entry shape of %s.", path),
					Kind:        "struct",
					Children:    childPaths,
					ParentPath:  path,
					OwnerStruct: child.Name,
				}
				stack[child.Name] = true
				ins.Walk(child, keyPath, child.Name, docs, stack)
				delete(stack, child.Name)
			}
		case f.IsSlice:
			fd.Kind = "slice"
			if child, ok := ins.Structs[f.ElemType]; ok && !stack[child.Name] {
				idxPath := path + ".[]"
				childPaths := make([]string, 0, len(child.Fields))
				for _, cf := range child.Fields {
					childPaths = append(childPaths, cf.JSONTag)
				}
				sort.Strings(childPaths)
				docs[idxPath] = &FieldDoc{
					Path:        idxPath,
					Name:        child.Name,
					Type:        child.Name,
					Doc:         fmt.Sprintf("Element shape of %s.", path),
					Kind:        "struct",
					Children:    childPaths,
					ParentPath:  path,
					OwnerStruct: child.Name,
				}
				stack[child.Name] = true
				ins.Walk(child, idxPath, child.Name, docs, stack)
				delete(stack, child.Name)
			}
		default:
			elem := f.GoType
			elem = strings.TrimPrefix(elem, "*")
			if child, ok := ins.Structs[elem]; ok && !stack[child.Name] {
				fd.Kind = "struct"
				childPaths := make([]string, 0, len(child.Fields))
				for _, cf := range child.Fields {
					childPaths = append(childPaths, cf.JSONTag)
				}
				sort.Strings(childPaths)
				fd.Children = childPaths
				stack[child.Name] = true
				ins.Walk(child, path, child.Name, docs, stack)
				delete(stack, child.Name)
			} else {
				fd.Kind = "leaf"
			}
		}
		docs[path] = fd
	}
}

// DefaultForField returns a human-readable hint when a `Default<X>` /
// `default<X>` constant exists, where `<X>` is built from variations
// of the owner-struct name and the field name. The naming convention
// in runconfig.go is inconsistent: `MaxParallel` lives under
// `ToolDispatchConfig` as `DefaultToolDispatchMaxParallel`, while
// `MaxWaitSeconds` lives under `BatchProviderConfig` as
// `DefaultBatchMaxWaitSeconds` (only the first word of the owner is
// re-used). The matcher tries every leading word of the owner name
// in addition to the verbatim and stripped forms so both conventions
// are reachable without hand-curation.
//
// First match wins. The hint is rendered as "<value> (<const name>)".
func (ins *Inspector) DefaultForField(ownerStruct, field string) string {
	stripped := strings.TrimSuffix(ownerStruct, "Config")
	candidateBases := []string{field, stripped + field, ownerStruct + field}
	for _, w := range LeadingWords(stripped) {
		candidateBases = append(candidateBases, w+field)
	}
	for _, base := range candidateBases {
		for _, prefix := range []string{"Default", "default"} {
			name := prefix + base
			if v, ok := ins.ConstInts[name]; ok {
				return fmt.Sprintf("%s (%s)", v, name)
			}
		}
	}
	return ""
}

// LeadingWords yields successive leading CamelCase words of s.
// e.g. "BatchProvider" -> ["Batch", "BatchProvider"];
// "ToolDispatch" -> ["Tool", "ToolDispatch"]. Used to bridge
// runconfig.go's inconsistent `Default<First-word-only><Field>`
// naming convention.
func LeadingWords(s string) []string {
	var out []string
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			out = append(out, s[:i])
		}
	}
	if len(out) == 0 || out[len(out)-1] != s {
		out = append(out, s)
	}
	return out
}

// EnumForField returns the closed-set values when a `valid<Name>Types` /
// `valid<Name>Values` / `valid<Name>s` var exists. The owner-struct
// prefix is consulted before the bare field name so a `Type` field on
// `ProviderConfig` resolves to `validProviderTypes` rather than the
// first matching `validXxxTypes` map. Leading words of the stripped
// owner are also tried (e.g. `BatchProvider` -> `Batch`) so the
// inconsistent naming used for the batch-related enums is reachable.
func (ins *Inspector) EnumForField(ownerStruct, field string) []string {
	stripped := strings.TrimSuffix(ownerStruct, "Config")
	bases := []string{stripped + field, ownerStruct + field, field}
	for _, w := range LeadingWords(stripped) {
		bases = append(bases, w+field)
	}
	for _, base := range bases {
		for _, suffix := range []string{"Types", "Values", "s", ""} {
			if v, ok := ins.Enums["valid"+base+suffix]; ok {
				return v
			}
		}
	}
	return nil
}
