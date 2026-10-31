package invariants

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

// configOnlyFields are RunConfig JSON paths deliberately kept off the gRPC
// wire, each with the reason. A path listed here covers its sub-fields.
var configOnlyFields = map[string]string{
	"transport":                       "a task_assignment arrives over gRPC by construction, so the translator sets transport itself",
	"traceEmitter.credential":         "the gcs credential override has no proto field; a gcs emitter on the wire uses workload identity",
	"traceEmitter.archive.credential": "the gcs credential override has no proto field; a gcs archive on the wire uses workload identity",
}

// mirrorKey normalises a field path so a Go JSON tag and a proto field
// name for the same field compare equal ("baseURL" and "base_url").
func mirrorKey(path string) string {
	return strings.ToLower(strings.ReplaceAll(path, "_", ""))
}

// runConfigJSONPaths returns the path of every field encoding/json
// serialises from RunConfig, intermediate structs included, as dotted JSON
// names. An untagged field is named by its Go name and an untagged
// embedded struct is flattened into its parent, as encoding/json does.
func runConfigJSONPaths() []string {
	var paths []string
	var walk func(t reflect.Type, prefix string, stack map[reflect.Type]bool)
	walk = func(t reflect.Type, prefix string, stack map[reflect.Type]bool) {
		stack[t] = true
		defer delete(stack, t)
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			if name == "" {
				if elem := structElem(f.Type); f.Anonymous && elem != nil {
					walk(elem, prefix, stack)
					continue
				}
				name = f.Name
			}
			path := prefix + name
			paths = append(paths, path)
			if elem := structElem(f.Type); elem != nil && !stack[elem] {
				walk(elem, path+".", stack)
			}
		}
	}
	walk(reflect.TypeFor[types.RunConfig](), "", map[reflect.Type]bool{})
	return paths
}

// structElem unwraps pointers, slices and map values and returns the
// struct type underneath, or nil for a leaf.
func structElem(t reflect.Type) reflect.Type {
	for {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			t = t.Elem()
			continue
		case reflect.Struct:
			if t == reflect.TypeFor[time.Time]() || t == reflect.TypeFor[json.RawMessage]() {
				return nil
			}
			return t
		default:
			return nil
		}
	}
}

// protoField is one field reachable from the proto RunConfig.
type protoField struct {
	path  string
	field protoreflect.FieldDescriptor
}

// protoRunConfigFields returns every field reachable from pb.RunConfig,
// keyed by mirrorKey of its dotted proto path.
func protoRunConfigFields() map[string]protoField {
	out := map[string]protoField{}
	var walk func(md protoreflect.MessageDescriptor, prefix string, stack map[protoreflect.FullName]bool)
	walk = func(md protoreflect.MessageDescriptor, prefix string, stack map[protoreflect.FullName]bool) {
		stack[md.FullName()] = true
		defer delete(stack, md.FullName())
		fields := md.Fields()
		for i := range fields.Len() {
			fd := fields.Get(i)
			path := prefix + string(fd.Name())
			out[mirrorKey(path)] = protoField{path: path, field: fd}
			sub := elemMessage(fd)
			if sub == nil || sub.ParentFile().Package() != md.ParentFile().Package() || stack[sub.FullName()] {
				continue
			}
			walk(sub, path+".", stack)
		}
	}
	walk((&pb.RunConfig{}).ProtoReflect().Descriptor(), "", map[protoreflect.FullName]bool{})
	return out
}

// elemMessage returns the message a field holds, or the element message of
// a repeated or map field; nil for a scalar.
func elemMessage(fd protoreflect.FieldDescriptor) protoreflect.MessageDescriptor {
	if fd == nil {
		return nil
	}
	if fd.IsMap() {
		return fd.MapValue().Message()
	}
	return fd.Message()
}

// fieldByGoName returns the field of md whose generated Go struct field is
// goName.
func fieldByGoName(md protoreflect.MessageDescriptor, goName string) protoreflect.FieldDescriptor {
	mt, err := protoregistry.GlobalTypes.FindMessageByName(md.FullName())
	if err != nil {
		return nil
	}
	f, ok := reflect.TypeOf(mt.Zero().Interface()).Elem().FieldByName(goName)
	if !ok {
		return nil
	}
	for _, part := range strings.Split(f.Tag.Get("protobuf"), ",") {
		if name, ok := strings.CutPrefix(part, "name="); ok {
			return md.Fields().ByName(protoreflect.Name(name))
		}
	}
	return nil
}

// protoReads returns the proto fields the functions in f read, as field
// full names. A read is X.Field or X.GetField() where X is tied to a proto
// message: a parameter of type *pb.M, a local assigned from such a read,
// or a loop variable ranging over a repeated or map message field. Field
// names on the Go side being written never count, even when they match.
func protoReads(f *ast.File, pbName string) map[protoreflect.FullName]bool {
	pkg := (&pb.RunConfig{}).ProtoReflect().Descriptor().ParentFile().Package()
	reads := map[protoreflect.FullName]bool{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		env := map[string]protoreflect.MessageDescriptor{}
		for _, param := range fn.Type.Params.List {
			msg := pbMessageOf(param.Type, pbName)
			if msg == "" {
				continue
			}
			md, err := protoregistry.GlobalFiles.FindDescriptorByName(pkg.Append(protoreflect.Name(msg)))
			if err != nil {
				continue
			}
			for _, name := range param.Names {
				env[name.Name] = md.(protoreflect.MessageDescriptor)
			}
		}
		var denote func(ast.Expr) protoreflect.MessageDescriptor
		fieldOf := func(sel *ast.SelectorExpr) protoreflect.FieldDescriptor {
			md := denote(sel.X)
			if md == nil {
				return nil
			}
			if fd := fieldByGoName(md, sel.Sel.Name); fd != nil {
				return fd
			}
			if name, ok := strings.CutPrefix(sel.Sel.Name, "Get"); ok {
				return fieldByGoName(md, name)
			}
			return nil
		}
		denote = func(e ast.Expr) protoreflect.MessageDescriptor {
			switch x := e.(type) {
			case *ast.Ident:
				return env[x.Name]
			case *ast.ParenExpr:
				return denote(x.X)
			case *ast.StarExpr:
				return denote(x.X)
			case *ast.IndexExpr:
				return denote(x.X)
			case *ast.SelectorExpr:
				return elemMessage(fieldOf(x))
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && len(x.Args) == 0 {
					return elemMessage(fieldOf(sel))
				}
			}
			return nil
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				if len(x.Lhs) == len(x.Rhs) {
					for i, lhs := range x.Lhs {
						if id, ok := lhs.(*ast.Ident); ok {
							if md := denote(x.Rhs[i]); md != nil {
								env[id.Name] = md
							}
						}
					}
				}
			case *ast.RangeStmt:
				if id, ok := x.Value.(*ast.Ident); ok {
					if md := denote(x.X); md != nil {
						env[id.Name] = md
					}
				}
			case *ast.SelectorExpr:
				if fd := fieldOf(x); fd != nil {
					reads[fd.FullName()] = true
				}
			}
			return true
		})
	}
	return reads
}

// translateReads returns the proto fields grpc_translate.go reads.
func translateReads(t *testing.T) map[protoreflect.FullName]bool {
	t.Helper()
	fset := token.NewFileSet()
	reads := map[protoreflect.FullName]bool{}
	for _, sf := range parseProductionFiles(t, fset, "harness/internal/transport") {
		if strings.HasSuffix(sf.path, "/grpc_translate.go") {
			reads = protoReads(sf.file, importName(sf.file, "github.com/rxbynerd/stirrup/gen/harness/v1"))
		}
	}
	require.New(t).NotEmpty(reads, "no proto field reads found in grpc_translate.go")
	return reads
}

// pbMessageOf returns the message name of a *pb.X, []*pb.X or
// map[K]*pb.X parameter type, or "".
func pbMessageOf(expr ast.Expr, pbName string) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		if sel, ok := e.X.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == pbName {
				return sel.Sel.Name
			}
		}
	case *ast.ArrayType:
		return pbMessageOf(e.Elt, pbName)
	case *ast.MapType:
		return pbMessageOf(e.Value, pbName)
	}
	return ""
}

// TestRunConfigProtoMirror_EveryFieldIsMirroredAndTranslated pins that
// every JSON-tagged RunConfig field has a proto field at the same path and
// that grpc_translate.go reads every proto field reachable from
// pb.RunConfig. A field missing from either side is silently dropped for
// runs started over `stirrup job`. The check proves a field is read, not
// that it lands in the right Go field; the round-trip tests in the
// transport package cover that.
func TestRunConfigProtoMirror_EveryFieldIsMirroredAndTranslated(t *testing.T) {
	protoFields := protoRunConfigFields()
	t.Run("every RunConfig field has a proto field", func(t *testing.T) {
		check := assert.New(t)
		for _, path := range runConfigJSONPaths() {
			if configOnly(path) {
				continue
			}
			_, ok := protoFields[mirrorKey(path)]
			check.True(ok, "RunConfig field %q has no proto counterpart in proto/harness/v1/harness.proto; add one (and its grpc_translate.go line) or list it in configOnlyFields with the reason", path)
		}
	})

	t.Run("every allow-listed field exists and is still unmirrored", func(t *testing.T) {
		check := assert.New(t)
		paths := runConfigJSONPaths()
		for path := range configOnlyFields {
			check.Contains(paths, path, "configOnlyFields lists %q, which is not a RunConfig field", path)
			_, mirrored := protoFields[mirrorKey(path)]
			check.False(mirrored, "configOnlyFields lists %q, which now has a proto field; remove the entry", path)
		}
	})

	t.Run("every proto field is read by grpc_translate.go", func(t *testing.T) {
		check := assert.New(t)
		reads := translateReads(t)
		keys := make([]string, 0, len(protoFields))
		for k := range protoFields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			f := protoFields[k]
			check.True(reads[f.field.FullName()], "proto field %s (%s) is never read in grpc_translate.go; a control plane setting it is silently ignored", f.path, f.field.FullName())
		}
	})
}

// TestProtoReads_CountsOnlyReadsThroughProtoReceivers pins the read scan
// itself: a field name that appears only on the Go side, or on a different
// proto message, does not count as reading the field.
func TestProtoReads_CountsOnlyReadsThroughProtoReceivers(t *testing.T) {
	const src = `package transport

func translate(pc *pb.RunConfig) types.RunConfig {
	rc := types.RunConfig{}
	rc.TraceEmitter.ObjectPrefix = pc.TraceEmitter.ObjectPrefix
	if archive := pc.TraceEmitter.GetArchive(); archive != nil {
		rc.TraceEmitter.Archive = &types.TraceArchiveConfig{Bucket: archive.GetBucket()}
	}
	for _, srv := range pc.GetTools().McpServers {
		_ = srv.Name
	}
	return rc
}
`
	must := require.New(t)
	f, err := parser.ParseFile(token.NewFileSet(), "snippet.go", src, parser.SkipObjectResolution)
	must.NoError(err)
	reads := protoReads(f, "pb")
	cases := []struct {
		field protoreflect.FullName
		want  bool
	}{
		{"stirrup.harness.v1.TraceEmitterConfig.object_prefix", true},
		{"stirrup.harness.v1.TraceArchiveConfig.bucket", true},
		{"stirrup.harness.v1.TraceArchiveConfig.object_prefix", false},
		{"stirrup.harness.v1.TraceArchiveConfig.type", false},
		{"stirrup.harness.v1.ToolsConfig.mcp_servers", true},
		{"stirrup.harness.v1.MCPServerConfig.name", true},
		{"stirrup.harness.v1.MCPServerConfig.uri", false},
	}
	for _, tc := range cases {
		t.Run(string(tc.field), func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, reads[tc.field])
		})
	}
}

func configOnly(path string) bool {
	for p := range configOnlyFields {
		if path == p || strings.HasPrefix(path, p+".") {
			return true
		}
	}
	return false
}
