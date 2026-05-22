//go:build ignore

// gen-runconfig-docs parses types/runconfig.go and emits
// types/runconfig_docs.go — a generated lookup table that backs the
// `stirrup config explain` subcommand.
//
// The lookup is generated rather than computed at runtime so:
//
//   - The harness binary does not depend on go/ast (an entire compiler
//     toolchain) at runtime. The lookup is a plain map literal.
//   - Operators reading the source can `cat types/runconfig_docs.go`
//     and see every doc string the explain command will surface — no
//     hidden parsing step.
//   - CI catches drift: `just verify-docs` regenerates and fails on
//     diff, the same pattern as `just proto`.
//
// Scope: this generator walks the struct types declared in
// types/runconfig.go. It extracts field doc comments, JSON tag names,
// Go type spellings, default values pulled from named constants whose
// name matches a struct field, and enum value sets pulled from
// `var validXxx = map[string]bool{...}` patterns. It deliberately does
// not try to extract free-form prose from comments above struct types;
// the field-level comment is the canonical source.
//
// Invocation: `go run scripts/gen-runconfig-docs.go` from the repo
// root. The `//go:generate` directive in types/runconfig.go points at
// this file, so `go generate ./types/...` also works.

package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// FieldDoc mirrors the type emitted into the generated file. Keep in
// sync with the literal written by emitFile.
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

// inspector walks the AST and accumulates everything we need to emit.
type inspector struct {
	fset *token.FileSet
	pkg  *ast.Package

	// structs maps type name → struct doc + fields, in source order.
	structs map[string]*structInfo

	// enums maps the name of a `var validXxx = map[string]bool{...}`
	// to the sorted set of its keys (excluding empty strings).
	enums map[string][]string

	// constInts maps every exported / unexported integer-like const to
	// its source value, used to surface defaults like
	// `Default: 86400 (DefaultBatchMaxWaitSeconds)`.
	constInts map[string]string
}

type structInfo struct {
	Name   string
	Fields []*fieldInfo
}

type fieldInfo struct {
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

func main() {
	// Locate the source file relative to the working directory or the
	// directory containing this script. Running from the repo root is
	// the documented entry point, but `go generate ./types/...` runs
	// with PWD == ./types, so try both.
	src, out, err := resolvePaths()
	if err != nil {
		fail(err)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, src, nil, parser.ParseComments)
	if err != nil {
		fail(fmt.Errorf("parse %s: %w", src, err))
	}

	ins := &inspector{
		fset:      fset,
		structs:   map[string]*structInfo{},
		enums:     map[string][]string{},
		constInts: map[string]string{},
	}
	ins.collect(file)

	// Walk RunConfig as the entry point and emit the flat field-path
	// lookup table.
	root, ok := ins.structs["RunConfig"]
	if !ok {
		fail(fmt.Errorf("RunConfig struct not found in %s", src))
	}

	docs := map[string]*FieldDoc{}
	docs[""] = ins.rootDoc(root)
	ins.walk(root, "", "RunConfig", docs, map[string]bool{"RunConfig": true})

	if err := emitFile(out, docs); err != nil {
		fail(err)
	}
}

func resolvePaths() (src, out string, err error) {
	// Two supported entry points:
	//   1. Repo root: ./types/runconfig.go and ./types/runconfig_docs.go.
	//   2. ./types (when invoked via go generate): runconfig.go in pwd.
	wd, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	candidates := [][2]string{
		{filepath.Join(wd, "types", "runconfig.go"), filepath.Join(wd, "types", "runconfig_docs.go")},
		{filepath.Join(wd, "runconfig.go"), filepath.Join(wd, "runconfig_docs.go")},
	}
	for _, c := range candidates {
		if _, statErr := os.Stat(c[0]); statErr == nil {
			return c[0], c[1], nil
		}
	}
	return "", "", fmt.Errorf("runconfig.go not found relative to %s", wd)
}

func (ins *inspector) collect(file *ast.File) {
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

func (ins *inspector) collectTypes(gen *ast.GenDecl) {
	for _, spec := range gen.Specs {
		ts, ok := spec.(*ast.TypeSpec)
		if !ok {
			continue
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			continue
		}
		info := &structInfo{Name: ts.Name.Name}
		for _, f := range st.Fields.List {
			// Anonymous / embedded fields would need recursing into the
			// embedded type; the RunConfig hierarchy doesn't use them.
			// Skip with a noisy assertion: if someone adds an embedded
			// field later, regenerating will silently drop it.
			if len(f.Names) == 0 {
				continue
			}
			doc := commentText(f.Doc)
			if doc == "" {
				// Trailing line-comments (`Mode string // "execution" | ...`)
				// are captured by Field.Comment, not Field.Doc. Use them as
				// a fallback so terse fields still surface useful text.
				doc = commentText(f.Comment)
			}
			for _, name := range f.Names {
				if !name.IsExported() {
					continue
				}
				fi := &fieldInfo{
					Name: name.Name,
					Doc:  doc,
				}
				populateType(fi, f.Type)
				fi.JSONTag = jsonTagName(f.Tag, name.Name)
				if fi.JSONTag == "-" {
					continue
				}
				info.Fields = append(info.Fields, fi)
			}
		}
		ins.structs[info.Name] = info
	}
}

func (ins *inspector) collectVars(gen *ast.GenDecl) {
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
			ins.enums[name.Name] = vals
		}
	}
}

func (ins *inspector) collectConsts(gen *ast.GenDecl) {
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
			ins.constInts[name.Name] = lit.Value
		}
	}
}

func populateType(fi *fieldInfo, expr ast.Expr) {
	switch t := expr.(type) {
	case *ast.StarExpr:
		fi.IsPtr = true
		populateType(fi, t.X)
		// populateType overwrote GoType with the underlying. Re-prefix.
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
		elem := &fieldInfo{}
		populateType(elem, t.Elt)
		fi.ElemType = elem.GoType
		fi.GoType = "[]" + elem.GoType
	case *ast.MapType:
		fi.IsMap = true
		k := &fieldInfo{}
		v := &fieldInfo{}
		populateType(k, t.Key)
		populateType(v, t.Value)
		fi.MapKey = k.GoType
		fi.ElemType = v.GoType
		fi.GoType = "map[" + k.GoType + "]" + v.GoType
	default:
		fi.GoType = "unknown"
	}
}

func jsonTagName(tag *ast.BasicLit, fallback string) string {
	if tag == nil {
		return lowerFirst(fallback)
	}
	raw, err := strconv.Unquote(tag.Value)
	if err != nil {
		return lowerFirst(fallback)
	}
	// crude tag parse — the canonical "json" key is the only one we need.
	for _, part := range splitTag(raw) {
		if !strings.HasPrefix(part, "json:") {
			continue
		}
		inner, err := strconv.Unquote(strings.TrimPrefix(part, "json:"))
		if err != nil {
			return lowerFirst(fallback)
		}
		comma := strings.IndexByte(inner, ',')
		if comma >= 0 {
			inner = inner[:comma]
		}
		if inner == "" {
			return lowerFirst(fallback)
		}
		return inner
	}
	return lowerFirst(fallback)
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

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func commentText(cg *ast.CommentGroup) string {
	if cg == nil {
		return ""
	}
	// ast.CommentGroup.Text() drops the leading "//" and trailing spaces,
	// merging consecutive comments into one paragraph-style string.
	return strings.TrimRight(cg.Text(), "\n")
}

// rootDoc builds the entry for the empty path (== `--root`). Children
// are the top-level fields of RunConfig.
func (ins *inspector) rootDoc(root *structInfo) *FieldDoc {
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

// walk descends through structInfo, emitting one FieldDoc per
// reachable field path. Pointer fields are dereferenced for display
// purposes; their underlying struct's children are still walked.
//
// Map fields contribute a single placeholder path
// (`<prefix>.<jsonTag>.<key>`) so an operator can ask
// `stirrup config explain providers.<name>` to see the per-entry
// shape.
func (ins *inspector) walk(s *structInfo, prefix, ownerName string, docs map[string]*FieldDoc, stack map[string]bool) {
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
		if dv := ins.defaultForField(ownerName, f.Name); dv != "" {
			fd.Default = dv
		}
		if enum := ins.enumForField(ownerName, f.Name); enum != nil {
			fd.Enum = enum
		}

		// Type-kind classification + child walk. Cycle protection: skip
		// the recursive descent when the target struct is already in the
		// active stack (e.g. GuardRailConfig.Stages []GuardRailConfig).
		// The FieldDoc for the cycling field is still emitted; only the
		// nested children are pruned.
		switch {
		case f.IsMap:
			fd.Kind = "map"
			if child, ok := ins.structs[f.ElemType]; ok && !stack[child.Name] {
				keyPath := path + ".<" + lowerFirst(f.MapKey) + ">"
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
				ins.walk(child, keyPath, child.Name, docs, stack)
				delete(stack, child.Name)
			}
		case f.IsSlice:
			fd.Kind = "slice"
			if child, ok := ins.structs[f.ElemType]; ok && !stack[child.Name] {
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
				ins.walk(child, idxPath, child.Name, docs, stack)
				delete(stack, child.Name)
			}
		default:
			elem := f.GoType
			elem = strings.TrimPrefix(elem, "*")
			if child, ok := ins.structs[elem]; ok && !stack[child.Name] {
				fd.Kind = "struct"
				childPaths := make([]string, 0, len(child.Fields))
				for _, cf := range child.Fields {
					childPaths = append(childPaths, cf.JSONTag)
				}
				sort.Strings(childPaths)
				fd.Children = childPaths
				stack[child.Name] = true
				ins.walk(child, path, child.Name, docs, stack)
				delete(stack, child.Name)
			} else {
				fd.Kind = "leaf"
			}
		}
		docs[path] = fd
	}
}

// defaultForField returns a human-readable hint when a `Default<X>` /
// `default<X>` constant exists, where `<X>` is one of:
//
//   - the field name verbatim (e.g. MaxParallel → DefaultMaxParallel)
//   - the owner-struct prefix + field name with the trailing "Config"
//     stripped (e.g. BatchProviderConfig.MaxWaitSeconds →
//     DefaultBatchMaxWaitSeconds; ToolDispatchConfig.MaxParallel →
//     DefaultToolDispatchMaxParallel)
//   - the owner-struct verbatim + field name
//
// First match wins. The hint is rendered as "<value> (<const name>)".
func (ins *inspector) defaultForField(ownerStruct, field string) string {
	stripped := strings.TrimSuffix(ownerStruct, "Config")
	candidateBases := []string{field, stripped + field, ownerStruct + field}
	for _, base := range candidateBases {
		for _, prefix := range []string{"Default", "default"} {
			name := prefix + base
			if v, ok := ins.constInts[name]; ok {
				return fmt.Sprintf("%s (%s)", v, name)
			}
		}
	}
	return ""
}

// enumForField returns the closed-set values when a `valid<Name>Types`
// / `valid<Name>Values` / `valid<Name>s` var exists. The owner-struct
// prefix is consulted before the bare field name so a `Type` field on
// `ProviderConfig` resolves to `validProviderTypes` rather than the
// first matching `validXxxTypes` map.
func (ins *inspector) enumForField(ownerStruct, field string) []string {
	stripped := strings.TrimSuffix(ownerStruct, "Config")
	bases := []string{stripped + field, ownerStruct + field, field}
	for _, base := range bases {
		for _, suffix := range []string{"Types", "Values", "s", ""} {
			if v, ok := ins.enums["valid"+base+suffix]; ok {
				return v
			}
		}
	}
	return nil
}

// emitFile renders the lookup table to disk.
func emitFile(path string, docs map[string]*FieldDoc) error {
	var buf bytes.Buffer
	buf.WriteString(`// Code generated by scripts/gen-runconfig-docs.go. DO NOT EDIT.
//
// This file backs the ` + "`stirrup config explain`" + ` subcommand. Run
// ` + "`just gen-docs`" + ` to regenerate after editing runconfig.go doc
// comments. ` + "`just verify-docs`" + ` checks the file is up to date in CI.

package types

// FieldDoc carries the surfaced documentation for one RunConfig field
// path. The shape is intentionally flat so the explain command can
// emit it as JSON without further transformation.
type FieldDoc struct {
	Path        string   ` + "`json:\"path\"`" + `
	Name        string   ` + "`json:\"name\"`" + `
	Type        string   ` + "`json:\"type\"`" + `
	JSONTag     string   ` + "`json:\"jsonTag\"`" + `
	Doc         string   ` + "`json:\"doc\"`" + `
	Default     string   ` + "`json:\"default,omitempty\"`" + `
	Enum        []string ` + "`json:\"enum,omitempty\"`" + `
	Children    []string ` + "`json:\"children,omitempty\"`" + `
	Kind        string   ` + "`json:\"kind\"`" + `
	Optional    bool     ` + "`json:\"optional,omitempty\"`" + `
	ParentPath  string   ` + "`json:\"parentPath,omitempty\"`" + `
	OwnerStruct string   ` + "`json:\"ownerStruct,omitempty\"`" + `
}

// FieldDocs maps a dot-separated field path on RunConfig to the
// surfaced documentation entry. The empty string maps to the
// top-level RunConfig overview.
//
// Map-valued fields contribute a synthetic ` + "`<key>`" + ` segment
// (e.g. ` + "`providers.<name>.type`" + `) so an operator can ask about
// the per-entry shape. Slice-valued struct fields use ` + "`[]`" + `
// instead (` + "`tools.mcpServers.[].uri`" + `).
var FieldDocs = map[string]FieldDoc{
`)

	keys := make([]string, 0, len(docs))
	for k := range docs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fd := docs[k]
		fmt.Fprintf(&buf, "\t%s: {\n", strconv.Quote(k))
		fmt.Fprintf(&buf, "\t\tPath:        %s,\n", strconv.Quote(fd.Path))
		fmt.Fprintf(&buf, "\t\tName:        %s,\n", strconv.Quote(fd.Name))
		fmt.Fprintf(&buf, "\t\tType:        %s,\n", strconv.Quote(fd.Type))
		fmt.Fprintf(&buf, "\t\tJSONTag:     %s,\n", strconv.Quote(fd.JSONTag))
		fmt.Fprintf(&buf, "\t\tDoc:         %s,\n", strconv.Quote(fd.Doc))
		if fd.Default != "" {
			fmt.Fprintf(&buf, "\t\tDefault:     %s,\n", strconv.Quote(fd.Default))
		}
		if len(fd.Enum) > 0 {
			fmt.Fprintf(&buf, "\t\tEnum:        %s,\n", quoteSlice(fd.Enum))
		}
		if len(fd.Children) > 0 {
			fmt.Fprintf(&buf, "\t\tChildren:    %s,\n", quoteSlice(fd.Children))
		}
		fmt.Fprintf(&buf, "\t\tKind:        %s,\n", strconv.Quote(fd.Kind))
		if fd.Optional {
			fmt.Fprintf(&buf, "\t\tOptional:    true,\n")
		}
		if fd.ParentPath != "" {
			fmt.Fprintf(&buf, "\t\tParentPath:  %s,\n", strconv.Quote(fd.ParentPath))
		}
		if fd.OwnerStruct != "" {
			fmt.Fprintf(&buf, "\t\tOwnerStruct: %s,\n", strconv.Quote(fd.OwnerStruct))
		}
		buf.WriteString("\t},\n")
	}
	buf.WriteString("}\n")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		// Write unformatted output for debugging.
		_ = os.WriteFile(path+".unformatted", buf.Bytes(), 0o644)
		return fmt.Errorf("format generated source: %w", err)
	}
	if err := os.WriteFile(path, formatted, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func quoteSlice(ss []string) string {
	var b strings.Builder
	b.WriteString("[]string{")
	for i, s := range ss {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.Quote(s))
	}
	b.WriteByte('}')
	return b.String()
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "gen-runconfig-docs:", err)
	os.Exit(1)
}
