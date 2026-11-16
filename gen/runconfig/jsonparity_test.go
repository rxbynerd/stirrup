package runconfig

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

// The JSON differences between file-config JSON and protobuf JSON that
// the "JSON round trip" section of docs/integration-guide.md lists; a
// change here belongs there too.
var (
	// alwaysWrittenUnmirroredKeys are the keys encoding/json writes for
	// every RunConfig that protobuf JSON has no field for.
	alwaysWrittenUnmirroredKeys = []string{"transport"}

	// protojsonStringIntegerPaths are the 64-bit integer fields, which
	// protobuf JSON writes as JSON strings and encoding/json does not
	// decode into an integer.
	protojsonStringIntegerPaths = []string{
		"tools.commandOutput.inlineMaxBytes",
		"tools.commandOutput.maxBytesPerRun",
		"tools.commandOutput.maxBytesPerStream",
		"tools.commandOutput.previewBytesPerStream",
	}
)

// TestRunConfigJSON_ParsesAsProtobufJSON pins that encoding/json output
// for a RunConfig with every mirrored field set parses strictly as
// protobuf JSON, and carries every field, once the keys with no proto
// field are removed.
func TestRunConfigJSON_ParsesAsProtobufJSON(t *testing.T) {
	want := filledRunConfig(t)
	want.Transport = types.TransportConfig{}
	raw, err := json.Marshal(want)
	require.New(t).NoError(err)

	t.Run("strict parse rejects the unmirrored keys", func(t *testing.T) {
		must := require.New(t)
		err := protojson.Unmarshal(raw, &pb.RunConfig{})
		must.ErrorContains(err, `unknown field "transport"`)
	})

	t.Run("every mirrored field survives", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		var doc map[string]json.RawMessage
		must.NoError(json.Unmarshal(raw, &doc))
		for _, k := range alwaysWrittenUnmirroredKeys {
			must.Contains(doc, k)
			delete(doc, k)
		}
		stripped, err := json.Marshal(doc)
		must.NoError(err)
		pc := &pb.RunConfig{}
		must.NoError(protojson.Unmarshal(stripped, pc))

		got := FromProto(pc)
		got.Transport = types.TransportConfig{}
		var lost []string
		diffGoValues("", reflect.ValueOf(want).Elem(), reflect.ValueOf(got).Elem(), &lost)
		check.Empty(lost, "fields whose file-config JSON does not parse as protobuf JSON")
	})
}

// TestProtobufJSON_DecodesAsRunConfigJSON pins that protobuf JSON for a
// message with every field set decodes as file-config JSON, apart from
// the 64-bit integers protobuf JSON writes as strings.
func TestProtobufJSON_DecodesAsRunConfigJSON(t *testing.T) {
	want := filledRunConfig(t)
	pc, err := ToProto(want)
	require.New(t).NoError(err)
	raw, err := protojson.Marshal(pc)
	require.New(t).NoError(err)
	want.Transport = types.TransportConfig{}

	t.Run("string integers are the only fields lost", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		var got types.RunConfig
		err := strictDecode(raw, &got)
		must.ErrorContains(err, "tools.commandOutput.")

		var lenient types.RunConfig
		var typeErr *json.UnmarshalTypeError
		must.ErrorAs(json.Unmarshal(raw, &lenient), &typeErr)
		var lost []string
		diffGoValues("", reflect.ValueOf(want).Elem(), reflect.ValueOf(&lenient).Elem(), &lost)
		sort.Strings(lost)
		check.Equal(protojsonStringIntegerPaths, lost)
	})

	t.Run("every field survives once string integers are numbers", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		var doc map[string]any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		must.NoError(dec.Decode(&doc))
		for _, path := range protojsonStringIntegerPaths {
			unquoteInteger(t, doc, strings.Split(path, "."))
		}
		fixed, err := json.Marshal(doc)
		must.NoError(err)

		var got types.RunConfig
		must.NoError(strictDecode(fixed, &got))
		var lost []string
		diffGoValues("", reflect.ValueOf(want).Elem(), reflect.ValueOf(&got).Elem(), &lost)
		check.Empty(lost)
	})
}

// TestProtojsonStringIntegerPaths_ListsEvery64BitField pins the list
// against the schema, so a new 64-bit field cannot reach the wire
// without its JSON difference being documented.
func TestProtojsonStringIntegerPaths_ListsEvery64BitField(t *testing.T) {
	check := assert.New(t)
	var found []string
	collect64BitFields("", (&pb.RunConfig{}).ProtoReflect().Descriptor(), map[protoreflect.FullName]bool{}, &found)
	sort.Strings(found)
	check.Equal(protojsonStringIntegerPaths, found)
}

func collect64BitFields(prefix string, md protoreflect.MessageDescriptor, stack map[protoreflect.FullName]bool, out *[]string) {
	if stack[md.FullName()] {
		return
	}
	stack[md.FullName()] = true
	defer delete(stack, md.FullName())
	fields := md.Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		path := joinPath(prefix, fd.JSONName())
		if fd.IsMap() {
			fd = fd.MapValue()
			path += "." + types.ConfigFieldWildcard
		}
		switch fd.Kind() {
		case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
			protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
			*out = append(*out, path)
		case protoreflect.MessageKind:
			if fd.Message().ParentFile().Package() == md.ParentFile().Package() {
				collect64BitFields(path, fd.Message(), stack, out)
			}
		}
	}
}

func strictDecode(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// unquoteInteger replaces the JSON string at path in doc with the number
// it holds.
func unquoteInteger(t *testing.T, doc map[string]any, path []string) {
	t.Helper()
	must := require.New(t)
	for _, k := range path[:len(path)-1] {
		next, ok := doc[k].(map[string]any)
		must.True(ok, "no object at %q", k)
		doc = next
	}
	last := path[len(path)-1]
	s, ok := doc[last].(string)
	must.True(ok, "%q is not a JSON string", last)
	_, err := strconv.ParseInt(s, 10, 64)
	must.NoError(err)
	doc[last] = json.Number(s)
}
