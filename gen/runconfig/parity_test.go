package runconfig

import (
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

// notOnWirePaths are the RunConfig JSON paths with no RunConfig wire
// field. ToProto rejects each when set; transport is also accepted as
// {type: "grpc"}, the value FromProto reports.
var notOnWirePaths = []string{
	"traceEmitter.archive.credential",
	"traceEmitter.credential",
	"transport",
}

type (
	fromProtoFunc func(*pb.RunConfig) *types.RunConfig
	toProtoFunc   func(*types.RunConfig) (*pb.RunConfig, error)
)

// filledProto returns a RunConfig message with every field set to a
// non-zero value, recursive messages filled one level deeper than
// types.ConfigFieldTypeNesting.
func filledProto(t *testing.T) *pb.RunConfig {
	t.Helper()
	pc := &pb.RunConfig{}
	fillProtoMessage(t, pc.ProtoReflect(), map[protoreflect.FullName]int{})
	return pc
}

// filledRunConfig returns a RunConfig with every field the wire carries
// set to a non-zero value, and transport set to the grpc value FromProto
// reports.
func filledRunConfig(t *testing.T) *types.RunConfig {
	t.Helper()
	rc, _ := filledRunConfigPaths(t)
	return rc
}

// filledRunConfigPaths is filledRunConfig, also returning every JSON path
// it set.
func filledRunConfigPaths(t *testing.T) (*types.RunConfig, map[string]bool) {
	t.Helper()
	rc := &types.RunConfig{}
	set := map[string]bool{}
	fillGoValue(t, reflect.ValueOf(rc).Elem(), "", notOnWirePaths, set, map[reflect.Type]int{})
	rc.Transport.Type = "grpc"
	return rc, set
}

// lostProtoFields sends a fully populated message through from and then
// to, and returns the proto JSON path of every field whose value did not
// survive: a wire field one direction does not map.
func lostProtoFields(t *testing.T, from fromProtoFunc, to toProtoFunc) []string {
	t.Helper()
	want := filledProto(t)
	got, err := to(from(want))
	require.New(t).NoError(err)
	var lost []string
	diffProtoMessages("", want.ProtoReflect(), got.ProtoReflect(), &lost)
	sort.Strings(lost)
	return lost
}

// lostGoFields sends a fully populated RunConfig through to and then
// from, and returns the JSON path of every field whose value did not
// survive: a Go field one direction does not map.
func lostGoFields(t *testing.T, from fromProtoFunc, to toProtoFunc) []string {
	t.Helper()
	want := filledRunConfig(t)
	pc, err := to(want)
	require.New(t).NoError(err)
	var lost []string
	diffGoValues("", reflect.ValueOf(want).Elem(), reflect.ValueOf(from(pc)).Elem(), &lost)
	sort.Strings(lost)
	return lost
}

func TestTranslator_EveryProtoFieldSurvivesFromProtoThenToProto(t *testing.T) {
	check := assert.New(t)
	check.Empty(lostProtoFields(t, FromProto, ToProto), "wire fields with no mapping in FromProto or ToProto")
}

func TestTranslator_EveryGoFieldSurvivesToProtoThenFromProto(t *testing.T) {
	check := assert.New(t)
	check.Empty(lostGoFields(t, FromProto, ToProto), "RunConfig fields with no mapping in ToProto or FromProto")
}

// TestFilledRunConfig_SetsEveryWireField pins that the Go-side parity
// check populates every field the wire carries, so a field the filler
// skipped cannot pass unnoticed.
func TestFilledRunConfig_SetsEveryWireField(t *testing.T) {
	check := assert.New(t)
	_, set := filledRunConfigPaths(t)
	for _, p := range types.ConfigFieldPaths() {
		if !underAny(p, notOnWirePaths) {
			check.True(set[p], "filledRunConfig leaves %q unset", p)
		}
	}
	for _, p := range notOnWirePaths {
		check.False(set[p], "filledRunConfig sets %q, which has no wire field", p)
	}
}

func underAny(path string, roots []string) bool {
	for _, r := range roots {
		if path == r || strings.HasPrefix(path, r+".") {
			return true
		}
	}
	return false
}

// TestTranslator_ParityChecksReportAnUnmappedField proves the two parity
// checks above fail when either direction of the translation drops a
// field.
func TestTranslator_ParityChecksReportAnUnmappedField(t *testing.T) {
	dropRuntimeFromProto := func(pc *pb.RunConfig) *types.RunConfig {
		rc := FromProto(pc)
		rc.Executor.Runtime = ""
		return rc
	}
	dropProfileToProto := func(rc *types.RunConfig) (*pb.RunConfig, error) {
		pc, err := ToProto(rc)
		if err == nil {
			pc.Tools.Profile = ""
		}
		return pc, err
	}
	t.Run("proto walk, FromProto drops a field", func(t *testing.T) {
		check := assert.New(t)
		check.Equal([]string{"executor.runtime"}, lostProtoFields(t, dropRuntimeFromProto, ToProto))
	})
	t.Run("proto walk, ToProto drops a field", func(t *testing.T) {
		check := assert.New(t)
		check.Equal([]string{"tools.profile"}, lostProtoFields(t, FromProto, dropProfileToProto))
	})
	t.Run("Go walk, FromProto drops a field", func(t *testing.T) {
		check := assert.New(t)
		check.Equal([]string{"executor.runtime"}, lostGoFields(t, dropRuntimeFromProto, ToProto))
	})
	t.Run("Go walk, ToProto drops a field", func(t *testing.T) {
		check := assert.New(t)
		check.Equal([]string{"tools.profile"}, lostGoFields(t, FromProto, dropProfileToProto))
	})
}

// fillProtoMessage sets every field of m to a non-zero value. A recursive
// message is filled one level deeper than types.ConfigFieldTypeNesting.
func fillProtoMessage(t *testing.T, m protoreflect.Message, active map[protoreflect.FullName]int) {
	t.Helper()
	md := m.Descriptor()
	if md.FullName() == "google.protobuf.Struct" {
		s, err := structpb.NewStruct(map[string]any{"type": "object"})
		require.New(t).NoError(err)
		s.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			m.Set(fd, v)
			return true
		})
		return
	}
	if active[md.FullName()] > types.ConfigFieldTypeNesting {
		return
	}
	active[md.FullName()]++
	defer func() { active[md.FullName()]-- }()
	fields := md.Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		switch {
		case fd.IsMap():
			entries := m.Mutable(fd).Map()
			v := entries.NewValue()
			if fd.MapValue().Kind() == protoreflect.MessageKind {
				fillProtoMessage(t, v.Message(), active)
			} else {
				v = nonZeroScalar(t, fd.MapValue())
			}
			entries.Set(protoreflect.ValueOfString("key").MapKey(), v)
		case fd.IsList():
			list := m.Mutable(fd).List()
			if fd.Kind() == protoreflect.MessageKind {
				v := list.NewElement()
				fillProtoMessage(t, v.Message(), active)
				list.Append(v)
			} else {
				list.Append(nonZeroScalar(t, fd))
			}
		case fd.Kind() == protoreflect.MessageKind:
			v := m.NewField(fd)
			fillProtoMessage(t, v.Message(), active)
			m.Set(fd, v)
		default:
			m.Set(fd, nonZeroScalar(t, fd))
		}
	}
}

func nonZeroScalar(t *testing.T, fd protoreflect.FieldDescriptor) protoreflect.Value {
	t.Helper()
	switch fd.Kind() {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString("x")
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(true)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return protoreflect.ValueOfInt32(1)
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return protoreflect.ValueOfInt64(1)
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return protoreflect.ValueOfUint32(1)
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return protoreflect.ValueOfUint64(1)
	case protoreflect.FloatKind:
		return protoreflect.ValueOfFloat32(0.5)
	case protoreflect.DoubleKind:
		return protoreflect.ValueOfFloat64(0.5)
	case protoreflect.BytesKind:
		return protoreflect.ValueOfBytes([]byte("x"))
	case protoreflect.EnumKind:
		return protoreflect.ValueOfEnum(1)
	}
	require.New(t).FailNow("unhandled proto kind", "%s", fd.Kind())
	return protoreflect.Value{}
}

// diffProtoMessages records the proto JSON path of every field whose
// value differs between want and got, descending into messages, list
// elements and map values so the deepest differing field is named.
func diffProtoMessages(prefix string, want, got protoreflect.Message, out *[]string) {
	fields := want.Descriptor().Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		path := joinPath(prefix, fd.JSONName())
		wv, gv := want.Get(fd), got.Get(fd)
		if wv.Equal(gv) {
			continue
		}
		if want.Has(fd) != got.Has(fd) {
			*out = append(*out, path)
			continue
		}
		switch {
		case fd.IsMap():
			wm, gm := wv.Map(), gv.Map()
			if wm.Len() != gm.Len() || fd.MapValue().Kind() != protoreflect.MessageKind {
				*out = append(*out, path)
				continue
			}
			wm.Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
				if !gm.Has(k) {
					*out = append(*out, path)
					return false
				}
				diffProtoMessages(path+"."+types.ConfigFieldWildcard, v.Message(), gm.Get(k).Message(), out)
				return true
			})
		case fd.IsList():
			wl, gl := wv.List(), gv.List()
			if wl.Len() != gl.Len() || fd.Kind() != protoreflect.MessageKind {
				*out = append(*out, path)
				continue
			}
			for j := range wl.Len() {
				diffProtoMessages(path, wl.Get(j).Message(), gl.Get(j).Message(), out)
			}
		case fd.Kind() == protoreflect.MessageKind:
			diffProtoMessages(path, wv.Message(), gv.Message(), out)
		default:
			*out = append(*out, path)
		}
	}
}

var rawMessageType = reflect.TypeFor[json.RawMessage]()

// fillGoValue sets v, and every field beneath it, to a non-zero value,
// skipping the JSON paths in skip. A struct type is filled at most
// types.ConfigFieldTypeNesting+1 levels inside itself.
func fillGoValue(t *testing.T, v reflect.Value, path string, skip []string, set map[string]bool, active map[reflect.Type]int) {
	t.Helper()
	if slices.Contains(skip, path) {
		return
	}
	if path != "" {
		set[path] = true
	}
	switch v.Kind() {
	case reflect.Pointer:
		if tooDeep(v.Type(), active) {
			return
		}
		v.Set(reflect.New(v.Type().Elem()))
		fillGoValue(t, v.Elem(), path, skip, set, active)
	case reflect.Struct:
		if tooDeep(v.Type(), active) {
			return
		}
		active[v.Type()]++
		defer func() { active[v.Type()]-- }()
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			fillGoValue(t, v.Field(i), joinPath(path, jsonName(f)), skip, set, active)
		}
	case reflect.Slice:
		if v.Type() == rawMessageType {
			v.SetBytes([]byte(`{"type":"object"}`))
			return
		}
		if tooDeep(v.Type(), active) {
			return
		}
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fillGoValue(t, v.Index(0), path, skip, set, active)
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		elem := reflect.New(v.Type().Elem()).Elem()
		fillGoValue(t, elem, path+"."+types.ConfigFieldWildcard, skip, set, active)
		v.SetMapIndex(reflect.ValueOf("key").Convert(v.Type().Key()), elem)
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Float64:
		v.SetFloat(0.5)
	default:
		require.New(t).FailNow("unhandled Go kind", "%s at %s", v.Kind(), path)
	}
}

// tooDeep reports whether the struct beneath t has already been entered
// more than types.ConfigFieldTypeNesting times on this descent.
func tooDeep(t reflect.Type, active map[reflect.Type]int) bool {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct && active[t] > types.ConfigFieldTypeNesting
}

// diffGoValues records the JSON path of every field whose value differs
// between want and got. A struct, pointer, slice or map that got lost
// entirely is recorded at its own path rather than at each field below.
func diffGoValues(path string, want, got reflect.Value, out *[]string) {
	if want.Type() == rawMessageType {
		if !jsonEqual(want.Bytes(), got.Bytes()) {
			*out = append(*out, path)
		}
		return
	}
	if assert.ObjectsAreEqual(want.Interface(), got.Interface()) {
		return
	}
	if !want.IsZero() && got.IsZero() {
		*out = append(*out, path)
		return
	}
	switch want.Kind() {
	case reflect.Pointer:
		if want.IsNil() || got.IsNil() {
			*out = append(*out, path)
			return
		}
		diffGoValues(path, want.Elem(), got.Elem(), out)
	case reflect.Struct:
		for i := range want.NumField() {
			f := want.Type().Field(i)
			if f.IsExported() {
				diffGoValues(joinPath(path, jsonName(f)), want.Field(i), got.Field(i), out)
			}
		}
	case reflect.Slice:
		if want.Len() != got.Len() {
			*out = append(*out, path)
			return
		}
		for i := range want.Len() {
			diffGoValues(path, want.Index(i), got.Index(i), out)
		}
	case reflect.Map:
		if want.Len() != got.Len() {
			*out = append(*out, path)
			return
		}
		iter := want.MapRange()
		for iter.Next() {
			gv := got.MapIndex(iter.Key())
			if !gv.IsValid() {
				*out = append(*out, path)
				continue
			}
			diffGoValues(path+"."+types.ConfigFieldWildcard, iter.Value(), gv, out)
		}
	default:
		*out = append(*out, path)
	}
}

func jsonEqual(a, b []byte) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	return assert.ObjectsAreEqual(av, bv)
}

func jsonName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if name == "" {
		return f.Name
	}
	return name
}

func joinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}
