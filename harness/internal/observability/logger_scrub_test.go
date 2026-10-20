package observability

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type secretStringer struct{ token string }

func (s secretStringer) String() string { return "stringer holds " + s.token }

type secretValuer struct{ token string }

func (s secretValuer) LogValue() slog.Value { return slog.StringValue("valuer holds " + s.token) }

type secretGroupValuer struct{ token string }

func (s secretGroupValuer) LogValue() slog.Value {
	return slog.GroupValue(slog.String("inner", "group valuer holds "+s.token))
}

type secretStruct struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

type namedString string

type namedInt int

type panickingStringer struct{}

func (panickingStringer) String() string { panic("boom") }

// renderRecord flattens a captured record, message and attributes (groups
// included), into one string for substring assertions on the export sink.
func renderRecord(r slog.Record) string {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" ")
		b.WriteString(a.String())
		return true
	})
	return b.String()
}

func TestScrubHandler_ScrubsEveryValueKindOnBothSinks(t *testing.T) {
	key := anthropicKeyFixture
	rootErr := fmt.Errorf("HTTP 401 from upstream: body echoed %s", key)

	cases := []struct {
		name string
		log  func(l *slog.Logger)
	}{
		{"message", func(l *slog.Logger) { l.Info("failed with " + key) }},
		{"string attribute", func(l *slog.Logger) { l.Info("m", "token", key) }},
		{"error", func(l *slog.Logger) { l.Info("m", "error", rootErr) }},
		{"wrapped error", func(l *slog.Logger) { l.Info("m", "error", fmt.Errorf("outer: %w", rootErr)) }},
		{"joined error", func(l *slog.Logger) { l.Info("m", "error", errors.Join(errors.New("a"), rootErr)) }},
		{"group containing error", func(l *slog.Logger) { l.Info("m", slog.Group("g", slog.Any("error", rootErr))) }},
		{"nested group containing error", func(l *slog.Logger) {
			l.Info("m", slog.Group("g", slog.Group("h", slog.Any("error", rootErr))))
		}},
		{"with-bound error", func(l *slog.Logger) { l.With("error", rootErr).Info("m") }},
		{"with-bound string", func(l *slog.Logger) { l.With("token", key).Info("m") }},
		{"log valuer string", func(l *slog.Logger) { l.Info("m", "v", secretValuer{key}) }},
		{"log valuer group", func(l *slog.Logger) { l.Info("m", "v", secretGroupValuer{key}) }},
		{"stringer", func(l *slog.Logger) { l.Info("m", "v", secretStringer{key}) }},
		{"map", func(l *slog.Logger) { l.Info("m", "v", map[string]string{"auth": key}) }},
		{"struct", func(l *slog.Logger) { l.Info("m", "v", secretStruct{Name: "n", Token: key}) }},
		{"struct pointer", func(l *slog.Logger) { l.Info("m", "v", &secretStruct{Name: "n", Token: key}) }},
		{"slice", func(l *slog.Logger) { l.Info("m", "v", []string{"a", key}) }},
		{"byte slice", func(l *slog.Logger) { l.Info("m", "v", []byte("body: "+key)) }},
		{"named string", func(l *slog.Logger) { l.Info("m", "v", namedString(key)) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var buf bytes.Buffer
			capture := newCaptureHandler()
			spy := &spyNotifier{}
			logger := NewScopedLoggerWithExport(NewRunScope("run-1"), slog.LevelInfo, &buf, spy, capture)

			tc.log(logger)

			must.NotContains(buf.String(), key, "key leaked into JSON sink")
			records := capture.snapshot()
			must.Len(records, 1, "export sink records")
			must.NotContains(renderRecord(records[0]), key, "key leaked into export sink")
			must.NotEmpty(spy.snapshot(), "SecretRedactedInOutput did not fire")
		})
	}
}

func TestScrubHandler_NonScalarValuesRenderAsOneJSONString(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	capture := newCaptureHandler()
	logger := NewLoggerWithExport("run-1", slog.LevelInfo, &buf, nil, capture)

	logger.Info("m",
		"map", map[string]string{"b": "2", "a": "1"},
		"struct", secretStruct{Name: "n", Token: "t"},
		"slice", []string{"x", "y"},
		"html", map[string]string{"k": "<&>"},
	)

	var rec map[string]any
	must.NoError(json.Unmarshal(buf.Bytes(), &rec), "record: %s", buf.String())
	records := capture.snapshot()
	must.Len(records, 1, "export sink records")

	want := map[string]string{
		"map":    `{"a":"1","b":"2"}`,
		"struct": `{"name":"n","token":"t"}`,
		"slice":  `["x","y"]`,
		"html":   `{"k":"<&>"}`,
	}
	for k, w := range want {
		t.Run(k, func(t *testing.T) {
			must := require.New(t)
			must.Equal(w, rec[k], "JSON sink")
			got, _ := attrValue(records[0], k)
			must.Equal(w, got, "export sink")
		})
	}
}

func TestScrubHandler_TextualKindsRenderAsText(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	logger := NewLogger("run-1", slog.LevelInfo, &buf)

	logger.Info("m",
		"bytes", []byte("plain body"),
		"stringer", secretStringer{"t"},
		"err", errors.New("boom"),
		"named", namedString("n"),
	)

	var rec map[string]any
	must.NoError(json.Unmarshal(buf.Bytes(), &rec), "record: %s", buf.String())
	want := map[string]string{
		"bytes":    "plain body",
		"stringer": "stringer holds t",
		"err":      "boom",
		"named":    "n",
	}
	for k, w := range want {
		t.Run(k, func(t *testing.T) {
			must := require.New(t)
			must.Equal(w, rec[k])
		})
	}
}

func TestScrubHandler_ScalarKindsKeepTheirShape(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	logger := NewLogger("run-1", slog.LevelInfo, &buf)

	logger.Info("m", "i", 3, "named", namedInt(7), "b", true, "f", 1.5, "d", slog.DurationValue(2e9))

	var rec map[string]any
	must.NoError(json.Unmarshal(buf.Bytes(), &rec), "record: %s", buf.String())
	want := map[string]any{"i": float64(3), "named": float64(7), "b": true, "f": 1.5, "d": float64(2e9)}
	for k, w := range want {
		t.Run(k, func(t *testing.T) {
			must := require.New(t)
			must.Equal(w, rec[k])
		})
	}
}

func TestScrubHandler_UnrenderableValuesDoNotPanic(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	logger := NewLogger("run-1", slog.LevelInfo, &buf)
	var nilErr *wrappedErr

	logger.Info("m",
		"panics", panickingStringer{},
		"nilptr", nilErr,
		"chan", make(chan int),
	)

	var rec map[string]any
	must.NoError(json.Unmarshal(buf.Bytes(), &rec), "record: %s", buf.String())
	for _, k := range []string{"panics", "nilptr"} {
		t.Run(k, func(t *testing.T) {
			must := require.New(t)
			must.Equal("[unrenderable log value]", rec[k], "want placeholder")
		})
	}
	t.Run("chan", func(t *testing.T) {
		must := require.New(t)
		got, _ := rec["chan"].(string)
		must.True(strings.HasPrefix(got, "0x"), "chan = %v, want %%+v fallback", rec["chan"])
	})
}

type wrappedErr struct{ inner error }

func (e *wrappedErr) Error() string { return e.inner.Error() }

func TestScrubHandler_ZeroAttrAndNilValueAreUnchanged(t *testing.T) {
	h := &ScrubHandler{}

	t.Run("zero attr is unchanged", func(t *testing.T) {
		must := require.New(t)
		got := h.scrubAttrAndReport(slog.Attr{})
		must.True(got.Equal(slog.Attr{}), "zero attr = %v, want it unchanged", got)
	})

	t.Run("nil value stays a nil any value", func(t *testing.T) {
		must := require.New(t)
		got := h.scrubAttrAndReport(slog.Any("k", nil))
		must.Equal("k", got.Key)
		must.Equal(slog.KindAny, got.Value.Kind())
		must.Nil(got.Value.Any())
	})

	t.Run("record renders nil as JSON null and drops empty keys", func(t *testing.T) {
		must := require.New(t)
		var buf bytes.Buffer
		logger := NewLogger("run-1", slog.LevelInfo, &buf)
		logger.Info("m", slog.Attr{}, "nilval", nil)

		var rec map[string]any
		must.NoError(json.Unmarshal(buf.Bytes(), &rec), "record: %s", buf.String())
		v, ok := rec["nilval"]
		must.True(ok, "nilval is missing from the record")
		must.Nil(v, "nilval should be JSON null")
		must.NotContains(rec, "", "record has an empty-key attribute")
	})
}
