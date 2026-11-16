package runconfig

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// TestFromProto_DoesNotAliasTheMessage pins that every slice, map and
// pointer FromProto returns is its own copy: rewriting every value in the
// result leaves the message unchanged.
func TestFromProto_DoesNotAliasTheMessage(t *testing.T) {
	check := assert.New(t)
	pc := filledProto(t)
	want := proto.Clone(pc)
	rc := FromProto(pc)
	mutateGoValue(reflect.ValueOf(rc).Elem())
	check.True(proto.Equal(want, pc), "mutating the RunConfig must not reach the message it came from")
}

// TestToProto_DoesNotAliasTheRunConfig pins the same for ToProto:
// rewriting every value in the message leaves the RunConfig unchanged.
func TestToProto_DoesNotAliasTheRunConfig(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	rc := filledRunConfig(t)
	want := filledRunConfig(t)
	pc, err := ToProto(rc)
	must.NoError(err)
	mutateProtoMessage(pc.ProtoReflect())
	check.Equal(want, rc, "mutating the message must not reach the RunConfig it came from")
}

// mutateGoValue changes every scalar reachable from v in place,
// including through pointers and inside slices and maps.
func mutateGoValue(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			mutateGoValue(v.Elem())
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				mutateGoValue(v.Field(i))
			}
		}
	case reflect.Slice:
		for i := range v.Len() {
			mutateGoValue(v.Index(i))
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			elem := reflect.New(v.Type().Elem()).Elem()
			elem.Set(iter.Value())
			mutateGoValue(elem)
			v.SetMapIndex(iter.Key(), elem)
		}
	case reflect.String:
		v.SetString(v.String() + "-mutated")
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Int, reflect.Int32, reflect.Int64:
		v.SetInt(v.Int() + 1)
	case reflect.Uint8:
		v.SetUint(v.Uint() + 1)
	case reflect.Float64:
		v.SetFloat(v.Float() + 1)
	}
}

// mutateProtoMessage changes every scalar reachable from m in place,
// including inside lists, maps and sub-messages.
func mutateProtoMessage(m protoreflect.Message) {
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
			mp := v.Map()
			mp.Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				if fd.MapValue().Kind() == protoreflect.MessageKind {
					mutateProtoMessage(mv.Message())
				} else {
					mp.Set(k, mutatedScalar(fd.MapValue(), mv))
				}
				return true
			})
		case fd.IsList():
			list := v.List()
			for i := range list.Len() {
				if fd.Kind() == protoreflect.MessageKind {
					mutateProtoMessage(list.Get(i).Message())
				} else {
					list.Set(i, mutatedScalar(fd, list.Get(i)))
				}
			}
		case fd.Kind() == protoreflect.MessageKind:
			mutateProtoMessage(v.Message())
		default:
			m.Set(fd, mutatedScalar(fd, v))
		}
		return true
	})
}

func mutatedScalar(fd protoreflect.FieldDescriptor, v protoreflect.Value) protoreflect.Value {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(v.String() + "-mutated")
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(!v.Bool())
	case protoreflect.Int32Kind:
		return protoreflect.ValueOfInt32(int32(v.Int()) + 1)
	case protoreflect.Int64Kind:
		return protoreflect.ValueOfInt64(v.Int() + 1)
	case protoreflect.DoubleKind:
		return protoreflect.ValueOfFloat64(v.Float() + 1)
	}
	return v
}
