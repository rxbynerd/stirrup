package runconfig

import (
	"encoding/json"
	"fmt"
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
// field; configOnlyFields in harness/internal/invariants lists the same
// paths. ToProto rejects each when set, except that transport must be
// exactly {type: "grpc"}, the value FromProto reports.
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
// value no other field holds, every list and map holding two entries,
// and recursive messages filled one level deeper than
// types.ConfigFieldTypeNesting. Distinct values make a field translated
// into a sibling of the same type fail the round trip.
func filledProto(t *testing.T) *pb.RunConfig {
	t.Helper()
	pc := &pb.RunConfig{}
	(&protoFiller{t: t, active: map[protoreflect.FullName]int{}}).fill(pc.ProtoReflect())
	return pc
}

// filledRunConfig returns a RunConfig with every field the wire carries
// set as filledProto sets them, and transport set to the grpc value
// FromProto reports.
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
	f := &goFiller{t: t, skip: notOnWirePaths, set: map[string]bool{}, active: map[reflect.Type]int{}}
	f.fill(reflect.ValueOf(rc).Elem(), "")
	rc.Transport.Type = "grpc"
	return rc, f.set
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
	return lostGoFieldsOf(t, filledRunConfig(t), from, to)
}

func lostGoFieldsOf(t *testing.T, want *types.RunConfig, from fromProtoFunc, to toProtoFunc) []string {
	t.Helper()
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

// TestTranslator_ExplicitZeroOptionalValuesSurvive pins that an optional
// field set to its zero value (temperature 0, ruleOfTwo.enforce false,
// timeout 0) stays set in both directions, since the harness reads an
// unset optional field as "apply the default".
func TestTranslator_ExplicitZeroOptionalValuesSurvive(t *testing.T) {
	t.Run("Go walk", func(t *testing.T) {
		check := assert.New(t)
		rc := filledRunConfig(t)
		zeroed := zeroScalarPointees(reflect.ValueOf(rc).Elem())
		check.Contains(zeroed, "temperature")
		check.Contains(zeroed, "ruleOfTwo.enforce")
		check.Empty(lostGoFieldsOf(t, rc, FromProto, ToProto))
	})
	t.Run("proto walk", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		want := filledProto(t)
		zeroed := zeroOptionalScalars(want.ProtoReflect(), "")
		check.Contains(zeroed, "temperature")
		check.Contains(zeroed, "ruleOfTwo.enforce")
		got, err := ToProto(FromProto(want))
		must.NoError(err)
		var lost []string
		diffProtoMessages("", want.ProtoReflect(), got.ProtoReflect(), &lost)
		check.Empty(lost)
	})
}

// zeroScalarPointees sets every pointer-to-scalar reachable from v to
// point at its type's zero value, and returns the paths it changed.
func zeroScalarPointees(v reflect.Value) []string {
	var out []string
	var walk func(v reflect.Value, path string)
	walk = func(v reflect.Value, path string) {
		switch v.Kind() {
		case reflect.Pointer:
			if v.IsNil() {
				return
			}
			switch v.Elem().Kind() {
			case reflect.Bool, reflect.Int, reflect.Float64, reflect.String:
				v.Elem().SetZero()
				out = append(out, path)
			default:
				walk(v.Elem(), path)
			}
		case reflect.Struct:
			for i := range v.NumField() {
				if f := v.Type().Field(i); f.IsExported() {
					walk(v.Field(i), joinPath(path, jsonName(f)))
				}
			}
		case reflect.Slice:
			for i := range v.Len() {
				walk(v.Index(i), path)
			}
		}
	}
	walk(v, "")
	return out
}

// zeroOptionalScalars sets every populated proto3 optional scalar
// reachable from m to its zero value, keeping it populated, and returns
// the paths it changed.
func zeroOptionalScalars(m protoreflect.Message, prefix string) []string {
	var out []string
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		path := joinPath(prefix, fd.JSONName())
		switch {
		case fd.IsList() && fd.Kind() == protoreflect.MessageKind && isHarnessMessage(fd):
			for i := range v.List().Len() {
				out = append(out, zeroOptionalScalars(v.List().Get(i).Message(), path)...)
			}
		case fd.IsList() || fd.IsMap():
		case fd.Kind() == protoreflect.MessageKind:
			if isHarnessMessage(fd) {
				out = append(out, zeroOptionalScalars(v.Message(), path)...)
			}
		case fd.HasPresence():
			m.Set(fd, fd.Default())
			out = append(out, path)
		}
		return true
	})
	return out
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

// protoFiller fills a message for filledProto, drawing each scalar from
// a counter so no two fields hold the same value.
type protoFiller struct {
	t      *testing.T
	n      int
	active map[protoreflect.FullName]int
}

func (f *protoFiller) fill(m protoreflect.Message) {
	f.t.Helper()
	md := m.Descriptor()
	if md.FullName() == "google.protobuf.Struct" {
		f.n++
		s, err := structpb.NewStruct(map[string]any{"type": "object", "title": fmt.Sprintf("v%d", f.n)})
		require.New(f.t).NoError(err)
		s.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			m.Set(fd, v)
			return true
		})
		return
	}
	if f.active[md.FullName()] > types.ConfigFieldTypeNesting {
		return
	}
	f.active[md.FullName()]++
	defer func() { f.active[md.FullName()]-- }()
	fields := md.Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		switch {
		case fd.IsMap():
			entries := m.Mutable(fd).Map()
			for _, key := range []string{"key1", "key2"} {
				v := entries.NewValue()
				if fd.MapValue().Kind() == protoreflect.MessageKind {
					f.fill(v.Message())
				} else {
					v = f.scalar(fd.MapValue())
				}
				entries.Set(protoreflect.ValueOfString(key).MapKey(), v)
			}
		case fd.IsList():
			list := m.Mutable(fd).List()
			for range 2 {
				if fd.Kind() == protoreflect.MessageKind {
					v := list.NewElement()
					f.fill(v.Message())
					list.Append(v)
				} else {
					list.Append(f.scalar(fd))
				}
			}
		case fd.Kind() == protoreflect.MessageKind:
			v := m.NewField(fd)
			f.fill(v.Message())
			m.Set(fd, v)
		default:
			m.Set(fd, f.scalar(fd))
		}
	}
}

// scalar returns the next counter value in fd's kind. Booleans can only
// be true, so a boolean swapped with another goes undetected.
func (f *protoFiller) scalar(fd protoreflect.FieldDescriptor) protoreflect.Value {
	f.t.Helper()
	f.n++
	switch fd.Kind() {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(fmt.Sprintf("v%d", f.n))
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(true)
	case protoreflect.Int32Kind:
		return protoreflect.ValueOfInt32(int32(f.n))
	case protoreflect.Int64Kind:
		return protoreflect.ValueOfInt64(int64(f.n))
	case protoreflect.DoubleKind:
		return protoreflect.ValueOfFloat64(float64(f.n) + 0.5)
	}
	require.New(f.t).FailNow("unhandled proto kind", "%s", fd.Kind())
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

// goFiller fills a RunConfig for filledRunConfig the way protoFiller
// fills a message, skipping the JSON paths in skip and recording every
// path it sets. A struct type is filled at most
// types.ConfigFieldTypeNesting+1 levels inside itself.
type goFiller struct {
	t      *testing.T
	n      int
	skip   []string
	set    map[string]bool
	active map[reflect.Type]int
}

func (f *goFiller) fill(v reflect.Value, path string) {
	f.t.Helper()
	if slices.Contains(f.skip, path) {
		return
	}
	if path != "" {
		f.set[path] = true
	}
	switch v.Kind() {
	case reflect.Pointer:
		if tooDeep(v.Type(), f.active) {
			return
		}
		v.Set(reflect.New(v.Type().Elem()))
		f.fill(v.Elem(), path)
	case reflect.Struct:
		if tooDeep(v.Type(), f.active) {
			return
		}
		f.active[v.Type()]++
		defer func() { f.active[v.Type()]-- }()
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if field.IsExported() {
				f.fill(v.Field(i), joinPath(path, jsonName(field)))
			}
		}
	case reflect.Slice:
		if v.Type() == rawMessageType {
			f.n++
			v.SetBytes(fmt.Appendf(nil, `{"type":"object","title":"v%d"}`, f.n))
			return
		}
		if tooDeep(v.Type(), f.active) {
			return
		}
		v.Set(reflect.MakeSlice(v.Type(), 2, 2))
		for i := range 2 {
			f.fill(v.Index(i), path)
		}
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		for _, key := range []string{"key1", "key2"} {
			elem := reflect.New(v.Type().Elem()).Elem()
			f.fill(elem, path+"."+types.ConfigFieldWildcard)
			v.SetMapIndex(reflect.ValueOf(key).Convert(v.Type().Key()), elem)
		}
	case reflect.String:
		f.n++
		v.SetString(fmt.Sprintf("v%d", f.n))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int32, reflect.Int64:
		f.n++
		v.SetInt(int64(f.n))
	case reflect.Float64:
		f.n++
		v.SetFloat(float64(f.n) + 0.5)
	default:
		require.New(f.t).FailNow("unhandled Go kind", "%s at %s", v.Kind(), path)
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
