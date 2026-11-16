package transport

import (
	"cmp"
	"fmt"
	"slices"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

// unknownField is a field number the compiled-in schema does not carry,
// found in the message at path. Number 0 marks unknown-field data that
// does not parse as protobuf wire format.
type unknownField struct {
	path   string
	number protowire.Number
}

// unknownRunConfigFields describes every field of rc, at any depth, that
// this harness's compiled-in schema does not carry, as "field N at the top
// level" or "field N under P". P is a message path in
// types.ConfigFieldPaths form: a repeated field's elements share the
// field's path and a map's values sit under a "*" segment, so the result
// never echoes a map key or an index. Entries are sorted by path then
// number, without duplicates; the result is nil when there are none.
func unknownRunConfigFields(rc *pb.RunConfig) []string {
	if rc == nil {
		return nil
	}
	var found []unknownField
	collectUnknownFields("", rc.ProtoReflect(), &found)
	if len(found) == 0 {
		return nil
	}
	slices.SortFunc(found, func(a, b unknownField) int {
		return cmp.Or(cmp.Compare(a.path, b.path), cmp.Compare(a.number, b.number))
	})
	found = slices.Compact(found)
	out := make([]string, len(found))
	for i, f := range found {
		out[i] = f.String()
	}
	return out
}

func (f unknownField) String() string {
	field := fmt.Sprintf("field %d", f.number)
	if f.number == 0 {
		field = "unparseable field data"
	}
	if f.path == "" {
		return field + " at the top level"
	}
	return field + " under " + f.path
}

func collectUnknownFields(path string, m protoreflect.Message, out *[]unknownField) {
	for b := m.GetUnknown(); len(b) > 0; {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			*out = append(*out, unknownField{path: path})
			break
		}
		v := protowire.ConsumeFieldValue(num, typ, b[n:])
		if v < 0 {
			*out = append(*out, unknownField{path: path})
			break
		}
		*out = append(*out, unknownField{path: path, number: num})
		b = b[n+v:]
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		child := fd.JSONName()
		if path != "" {
			child = path + "." + child
		}
		switch {
		case fd.IsMap():
			if fd.MapValue().Message() == nil {
				return true
			}
			v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
				collectUnknownFields(child+"."+types.ConfigFieldWildcard, mv.Message(), out)
				return true
			})
		case fd.IsList():
			if fd.Message() == nil {
				return true
			}
			list := v.List()
			for i := range list.Len() {
				collectUnknownFields(child, list.Get(i).Message(), out)
			}
		case fd.Message() != nil:
			collectUnknownFields(child, v.Message(), out)
		}
		return true
	})
}
