package observability

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
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
		{"With-bound error", func(l *slog.Logger) { l.With("error", rootErr).Info("m") }},
		{"With-bound string", func(l *slog.Logger) { l.With("token", key).Info("m") }},
		{"LogValuer string", func(l *slog.Logger) { l.Info("m", "v", secretValuer{key}) }},
		{"LogValuer group", func(l *slog.Logger) { l.Info("m", "v", secretGroupValuer{key}) }},
		{"Stringer", func(l *slog.Logger) { l.Info("m", "v", secretStringer{key}) }},
		{"map", func(l *slog.Logger) { l.Info("m", "v", map[string]string{"auth": key}) }},
		{"struct", func(l *slog.Logger) { l.Info("m", "v", secretStruct{Name: "n", Token: key}) }},
		{"struct pointer", func(l *slog.Logger) { l.Info("m", "v", &secretStruct{Name: "n", Token: key}) }},
		{"slice", func(l *slog.Logger) { l.Info("m", "v", []string{"a", key}) }},
		{"byte slice", func(l *slog.Logger) { l.Info("m", "v", []byte("body: "+key)) }},
		{"named string", func(l *slog.Logger) { l.Info("m", "v", namedString(key)) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			capture := newCaptureHandler()
			spy := &spyNotifier{}
			logger := NewScopedLoggerWithExport(NewRunScope("run-1"), slog.LevelInfo, &buf, spy, capture)

			tc.log(logger)

			if strings.Contains(buf.String(), key) {
				t.Errorf("key leaked into JSON sink: %s", buf.String())
			}
			records := capture.snapshot()
			if len(records) != 1 {
				t.Fatalf("export sink received %d records, want 1", len(records))
			}
			if got := renderRecord(records[0]); strings.Contains(got, key) {
				t.Errorf("key leaked into export sink: %s", got)
			}
			if len(spy.snapshot()) == 0 {
				t.Error("SecretRedactedInOutput did not fire")
			}
		})
	}
}

func TestScrubHandler_NonScalarValuesRenderAsOneJSONString(t *testing.T) {
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
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	want := map[string]string{
		"map":    `{"a":"1","b":"2"}`,
		"struct": `{"name":"n","token":"t"}`,
		"slice":  `["x","y"]`,
		"html":   `{"k":"<&>"}`,
	}
	for k, w := range want {
		got, ok := rec[k].(string)
		if !ok {
			t.Errorf("%s: JSON sink value is %T, want string", k, rec[k])
			continue
		}
		if got != w {
			t.Errorf("%s: JSON sink = %q, want %q", k, got, w)
		}
	}

	records := capture.snapshot()
	if len(records) != 1 {
		t.Fatalf("export sink received %d records, want 1", len(records))
	}
	for k, w := range want {
		if got, _ := attrValue(records[0], k); got != w {
			t.Errorf("%s: export sink = %q, want %q", k, got, w)
		}
	}
}

func TestScrubHandler_TextualKindsRenderAsText(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("run-1", slog.LevelInfo, &buf)

	logger.Info("m",
		"bytes", []byte("plain body"),
		"stringer", secretStringer{"t"},
		"err", errors.New("boom"),
		"named", namedString("n"),
	)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	want := map[string]string{
		"bytes":    "plain body",
		"stringer": "stringer holds t",
		"err":      "boom",
		"named":    "n",
	}
	for k, w := range want {
		if got, _ := rec[k].(string); got != w {
			t.Errorf("%s = %v, want %q", k, rec[k], w)
		}
	}
}

func TestScrubHandler_ScalarKindsKeepTheirShape(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("run-1", slog.LevelInfo, &buf)

	logger.Info("m", "i", 3, "named", namedInt(7), "b", true, "f", 1.5, "d", slog.DurationValue(2e9))

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	for k, w := range map[string]any{"i": float64(3), "named": float64(7), "b": true, "f": 1.5, "d": float64(2e9)} {
		if rec[k] != w {
			t.Errorf("%s = %#v, want %#v", k, rec[k], w)
		}
	}
}

func TestScrubHandler_UnrenderableValuesDoNotPanic(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("run-1", slog.LevelInfo, &buf)
	var nilErr *wrappedErr

	logger.Info("m",
		"panics", panickingStringer{},
		"nilptr", nilErr,
		"chan", make(chan int),
	)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	for _, k := range []string{"panics", "nilptr"} {
		if rec[k] != "[unrenderable log value]" {
			t.Errorf("%s = %v, want placeholder", k, rec[k])
		}
	}
	if got, _ := rec["chan"].(string); !strings.HasPrefix(got, "0x") {
		t.Errorf("chan = %v, want %%+v fallback", rec["chan"])
	}
}

type wrappedErr struct{ inner error }

func (e *wrappedErr) Error() string { return e.inner.Error() }

func TestScrubHandler_ZeroAttrAndNilValueAreUnchanged(t *testing.T) {
	h := &ScrubHandler{}

	if got := h.scrubAttrAndReport(slog.Attr{}); !got.Equal(slog.Attr{}) {
		t.Errorf("zero attr = %v, want it unchanged", got)
	}
	got := h.scrubAttrAndReport(slog.Any("k", nil))
	if got.Key != "k" || got.Value.Kind() != slog.KindAny || got.Value.Any() != nil {
		t.Errorf("nil value = %v (kind %v), want a nil KindAny value", got, got.Value.Kind())
	}

	var buf bytes.Buffer
	logger := NewLogger("run-1", slog.LevelInfo, &buf)
	logger.Info("m", slog.Attr{}, "nilval", nil)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if v, ok := rec["nilval"]; !ok || v != nil {
		t.Errorf("nilval = %#v (present=%v), want JSON null", v, ok)
	}
	if _, ok := rec[""]; ok {
		t.Errorf("record has an empty-key attribute: %s", buf.String())
	}
}
