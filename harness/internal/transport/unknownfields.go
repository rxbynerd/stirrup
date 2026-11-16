package transport

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

const (
	// maxUnknownFieldEntries bounds how many unknown fields are described;
	// the walk stops once one more is found.
	maxUnknownFieldEntries = 16
	// maxUnknownFieldPathBytes bounds a described path, which a deeply
	// nested Struct value could otherwise make arbitrarily long.
	maxUnknownFieldPathBytes = 256
	// moreUnknownFields is the final entry when the walk stopped early.
	moreUnknownFields = "and further unknown fields"
)

// unknownField is unknown-field data found in the message at path.
// Number 0 marks data that does not parse as protobuf wire format.
type unknownField struct {
	path      string
	number    protowire.Number
	wrongType bool // number is a known field sent with another wire type
}

// unknownRunConfigFields describes the fields of rc, at any depth, that
// this harness's compiled-in schema does not carry, sorted and without
// duplicates. Paths are in types.ConfigFieldPaths form, so the result never
// echoes a map key or an index; it is nil when there are none.
func unknownRunConfigFields(rc *pb.RunConfig) []string {
	if rc == nil {
		return nil
	}
	w := &unknownFieldWalk{seen: map[unknownField]bool{}}
	w.message(rc.ProtoReflect())
	if len(w.seen) == 0 {
		return nil
	}
	found := make([]unknownField, 0, len(w.seen))
	for f := range w.seen {
		found = append(found, f)
	}
	slices.SortFunc(found, func(a, b unknownField) int {
		return cmp.Or(cmp.Compare(a.path, b.path), cmp.Compare(a.number, b.number))
	})
	out := make([]string, 0, len(found)+1)
	for _, f := range found {
		out = append(out, f.String())
	}
	if w.more {
		out = append(out, moreUnknownFields)
	}
	return out
}

func (f unknownField) String() string {
	field := fmt.Sprintf("field %d", f.number)
	switch {
	case f.number == 0:
		field = "unparseable field data"
	case f.wrongType:
		field += " with an unexpected wire type"
	}
	if f.path == "" {
		return field + " at the top level"
	}
	return field + " under " + f.path
}

// unknownFieldWalk visits a message tree depth-first. The current path is
// kept as a stack of segments and rendered only where unknown data is
// found, so a deep tree costs memory linear in its depth.
type unknownFieldWalk struct {
	segments []string
	seen     map[unknownField]bool
	more     bool
}

func (w *unknownFieldWalk) done() bool { return w.more }

func (w *unknownFieldWalk) record(f unknownField) {
	if w.seen[f] {
		return
	}
	if len(w.seen) == maxUnknownFieldEntries {
		w.more = true
		return
	}
	w.seen[f] = true
}

func (w *unknownFieldWalk) path() string {
	var b strings.Builder
	for i, s := range w.segments {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(s)
		if b.Len() > maxUnknownFieldPathBytes {
			return b.String()[:maxUnknownFieldPathBytes] + "..."
		}
	}
	return b.String()
}

func (w *unknownFieldWalk) message(m protoreflect.Message) {
	if b := m.GetUnknown(); len(b) > 0 {
		fields := m.Descriptor().Fields()
		path := w.path()
		for len(b) > 0 && !w.done() {
			num, typ, n := protowire.ConsumeTag(b)
			if n < 0 {
				w.record(unknownField{path: path})
				break
			}
			v := protowire.ConsumeFieldValue(num, typ, b[n:])
			if v < 0 {
				w.record(unknownField{path: path})
				break
			}
			w.record(unknownField{path: path, number: num, wrongType: fields.ByNumber(num) != nil})
			b = b[n+v:]
		}
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		w.segments = append(w.segments, fd.JSONName())
		defer func() { w.segments = w.segments[:len(w.segments)-1] }()
		switch {
		case fd.IsMap():
			if fd.MapValue().Message() == nil {
				break
			}
			w.segments = append(w.segments, types.ConfigFieldWildcard)
			v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
				w.message(mv.Message())
				return !w.done()
			})
			w.segments = w.segments[:len(w.segments)-1]
		case fd.IsList():
			if fd.Message() == nil {
				break
			}
			list := v.List()
			for i := 0; i < list.Len() && !w.done(); i++ {
				w.message(list.Get(i).Message())
			}
		case fd.Message() != nil:
			w.message(v.Message())
		}
		return !w.done()
	})
}
