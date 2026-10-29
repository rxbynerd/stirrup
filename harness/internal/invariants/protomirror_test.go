package invariants

import (
	"encoding/json"
	"go/ast"
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
	"traceEmitter.credential":         "gcs trace credential override is not yet on the wire; gcs uses workload identity",
	"traceEmitter.archive.credential": "gcs archive credential override is not yet on the wire; gcs uses workload identity",
}

// mirrorKey normalises a field path so a Go JSON tag and a proto field
// name for the same field compare equal ("baseURL" and "base_url").
func mirrorKey(path string) string {
	return strings.ToLower(strings.ReplaceAll(path, "_", ""))
}

// runConfigJSONPaths returns every JSON-tagged field path reachable from
// RunConfig, intermediate structs included, as dotted JSON names.
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
			if name == "-" || name == "" {
				continue
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
	path   string
	goName string
}

// protoRunConfigFields returns every field reachable from pb.RunConfig,
// keyed by mirrorKey of its dotted proto path.
func protoRunConfigFields(t *testing.T) map[string]protoField {
	t.Helper()
	out := map[string]protoField{}
	var walk func(md protoreflect.MessageDescriptor, prefix string, stack map[protoreflect.FullName]bool)
	walk = func(md protoreflect.MessageDescriptor, prefix string, stack map[protoreflect.FullName]bool) {
		stack[md.FullName()] = true
		defer delete(stack, md.FullName())
		goNames := generatedGoNames(t, md)
		fields := md.Fields()
		for i := range fields.Len() {
			fd := fields.Get(i)
			path := prefix + string(fd.Name())
			out[mirrorKey(path)] = protoField{path: path, goName: goNames[string(fd.Name())]}
			sub := fd.Message()
			if fd.IsMap() {
				sub = fd.MapValue().Message()
			}
			if sub == nil || sub.FullName().Parent() != md.ParentFile().Package() || stack[sub.FullName()] {
				continue
			}
			walk(sub, path+".", stack)
		}
	}
	walk((&pb.RunConfig{}).ProtoReflect().Descriptor(), "", map[protoreflect.FullName]bool{})
	return out
}

// generatedGoNames maps each proto field name of md to the Go struct field
// protoc-gen-go generated for it.
func generatedGoNames(t *testing.T, md protoreflect.MessageDescriptor) map[string]string {
	t.Helper()
	must := require.New(t)
	mt, err := protoregistry.GlobalTypes.FindMessageByName(md.FullName())
	must.NoError(err, "generated type for %s", md.FullName())
	goType := reflect.TypeOf(mt.Zero().Interface()).Elem()
	names := map[string]string{}
	for i := range goType.NumField() {
		f := goType.Field(i)
		for _, part := range strings.Split(f.Tag.Get("protobuf"), ",") {
			if name, ok := strings.CutPrefix(part, "name="); ok {
				names[name] = f.Name
			}
		}
	}
	return names
}

// translateSelectors returns, per proto message name, the selector names
// used by the grpc_translate.go functions that take that message.
func translateSelectors(t *testing.T) (byMessage map[string]map[string]bool) {
	t.Helper()
	fset := token.NewFileSet()
	files := parseProductionFiles(t, fset, "harness/internal/transport")
	byMessage = map[string]map[string]bool{}
	for _, sf := range files {
		if !strings.HasSuffix(sf.path, "/grpc_translate.go") {
			continue
		}
		pbName := importName(sf.file, "github.com/rxbynerd/stirrup/gen/harness/v1")
		for _, decl := range sf.file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			var messages []string
			for _, param := range fd.Type.Params.List {
				if msg := pbMessageOf(param.Type, pbName); msg != "" {
					messages = append(messages, msg)
				}
			}
			if len(messages) == 0 {
				continue
			}
			sels := map[string]bool{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					sels[sel.Sel.Name] = true
				}
				return true
			})
			for _, msg := range messages {
				if byMessage[msg] == nil {
					byMessage[msg] = map[string]bool{}
				}
				for s := range sels {
					byMessage[msg][s] = true
				}
			}
		}
	}
	require.New(t).NotEmpty(byMessage, "grpc_translate.go declares no proto translation functions")
	return byMessage
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
// runs started over `stirrup job`.
func TestRunConfigProtoMirror_EveryFieldIsMirroredAndTranslated(t *testing.T) {
	protoFields := protoRunConfigFields(t)
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
		selectors := translateSelectors(t)
		keys := make([]string, 0, len(protoFields))
		for k := range protoFields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			f := protoFields[k]
			msg := messageOwning(t, f.path)
			sels := selectorsFor(selectors, f.path)
			read := sels[f.goName] || sels["Get"+f.goName]
			check.True(read, "proto field %s (%s.%s) is never read in grpc_translate.go; a control plane setting it is silently ignored", f.path, msg, f.goName)
		}
	})
}

func configOnly(path string) bool {
	for p := range configOnlyFields {
		if path == p || strings.HasPrefix(path, p+".") {
			return true
		}
	}
	return false
}

// messageOwning returns the Go name of the proto message that declares the
// last segment of a dotted proto path from RunConfig.
func messageOwning(t *testing.T, path string) string {
	t.Helper()
	md := (&pb.RunConfig{}).ProtoReflect().Descriptor()
	segments := strings.Split(path, ".")
	for _, seg := range segments[:len(segments)-1] {
		fd := md.Fields().ByName(protoreflect.Name(seg))
		require.New(t).NotNil(fd, "path %s", path)
		if fd.IsMap() {
			md = fd.MapValue().Message()
		} else {
			md = fd.Message()
		}
	}
	return string(md.Name())
}

// selectorsFor returns the selectors of the translate functions taking the
// message that owns path's last field. A message translated inline in its
// parent's function has no function of its own, so its nearest ancestor's
// selectors are used.
func selectorsFor(byMessage map[string]map[string]bool, path string) map[string]bool {
	md := (&pb.RunConfig{}).ProtoReflect().Descriptor()
	chain := []string{string(md.Name())}
	segments := strings.Split(path, ".")
	for _, seg := range segments[:len(segments)-1] {
		fd := md.Fields().ByName(protoreflect.Name(seg))
		if fd.IsMap() {
			md = fd.MapValue().Message()
		} else {
			md = fd.Message()
		}
		chain = append(chain, string(md.Name()))
	}
	for i := len(chain) - 1; i >= 0; i-- {
		if sels, ok := byMessage[chain[i]]; ok {
			return sels
		}
	}
	return nil
}
