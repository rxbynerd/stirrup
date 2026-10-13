package platform

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/attribute"

	"github.com/rxbynerd/stirrup/harness/internal/observability/otlp"
	"github.com/rxbynerd/stirrup/types"
)

// registerTestPlatform adds a platform to the registry for one test.
func registerTestPlatform(t *testing.T, name string, def definition) {
	t.Helper()
	if _, exists := registry[name]; exists {
		t.Fatalf("test platform %q collides with a registered platform", name)
	}
	registry[name] = def
	t.Cleanup(func() { delete(registry, name) })
}

func TestResolve_NoPlatform(t *testing.T) {
	p, err := Resolve(context.Background(), "", nil, Deps{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.name != "" || p.RequiredProtocol != "" || p.Signals != nil || p.ResourceAttrs != nil || p.WrapTransport != nil || p.WrapSpanExporter != nil {
		t.Errorf("empty platform must resolve to the zero Profile, got %+v", p)
	}

	if _, err := Resolve(context.Background(), "", map[string]string{"k": "v"}, Deps{}); err == nil {
		t.Error("options without a platform must be rejected")
	}
}

func TestResolve_ValidatesOptions(t *testing.T) {
	var gotOpts map[string]string
	registerTestPlatform(t, "test-platform", definition{
		options: map[string]bool{"needed": true, "extra": false},
		build: func(_ context.Context, opts map[string]string, _ Deps) (Profile, error) {
			gotOpts = opts
			return Profile{RequiredProtocol: otlp.ProtocolHTTPProtobuf}, nil
		},
	})
	ctx := context.Background()

	p, err := Resolve(ctx, "test-platform", map[string]string{"needed": "x", "extra": "y"}, Deps{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.name != "test-platform" {
		t.Errorf("profile name = %q, want test-platform", p.name)
	}
	if !maps.Equal(gotOpts, map[string]string{"needed": "x", "extra": "y"}) {
		t.Errorf("builder received %v", gotOpts)
	}

	cases := map[string]struct {
		name string
		opts map[string]string
		want string
	}{
		"unknown platform": {"no-such-platform", nil, `unknown traceEmitter.platform "no-such-platform"`},
		"unknown option":   {"test-platform", map[string]string{"needed": "x", "typo": "y"}, `does not accept option "typo" (accepted: extra, needed)`},
		"missing required": {"test-platform", map[string]string{"extra": "y"}, `requires option "needed"`},
		"empty required":   {"test-platform", map[string]string{"needed": ""}, `requires option "needed"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Resolve(ctx, tc.name, tc.opts, Deps{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestResolve_WrapsBuilderError(t *testing.T) {
	registerTestPlatform(t, "failing-platform", definition{
		build: func(context.Context, map[string]string, Deps) (Profile, error) {
			return Profile{}, errors.New("no region")
		},
	})
	_, err := Resolve(context.Background(), "failing-platform", nil, Deps{})
	if err == nil || err.Error() != `platform "failing-platform": no region` {
		t.Errorf("got %v", err)
	}
}

func noEnv(string) string { return "" }

func envOf(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// identityWrap marks a profile as signing without changing requests.
func identityWrap(_ otlp.Signal, rt http.RoundTripper) http.RoundTripper { return rt }

func TestApply_NoProfileKeepsExistingCascade(t *testing.T) {
	headers := map[string]string{"Authorization": "Bearer t"}
	got, err := Apply(Input{
		TraceEmitter: types.TraceEmitterConfig{
			Type:               "otel",
			Protocol:           otlp.ProtocolHTTPProtobuf,
			Endpoint:           "https://gw.example/otlp",
			MetricsTemporality: "delta",
		},
		Headers: headers,
		Getenv:  noEnv,
	}, Profile{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for sig, s := range map[otlp.Signal]otlp.Settings{otlp.SignalTraces: got.Traces, otlp.SignalMetrics: got.Metrics, otlp.SignalLogs: got.Logs} {
		if s.Endpoint != "https://gw.example/otlp" || s.Protocol != otlp.ProtocolHTTPProtobuf || !maps.Equal(s.Headers, headers) || s.HTTPClient != nil {
			t.Errorf("%s settings = %+v, want the trace endpoint, protocol and headers", sig, s)
		}
	}
	if got.Metrics.Temporality != "delta" || got.Traces.Temporality != "" {
		t.Errorf("temporality must reach the metric exporter only, got metrics %q traces %q", got.Metrics.Temporality, got.Traces.Temporality)
	}
	if got.Platform != "" || got.ResourceAttrs != nil || got.WrapSpanExporter != nil {
		t.Errorf("no profile must add no decoration, got %+v", got)
	}
}

func TestApply_DefaultsWithoutAnyEndpoint(t *testing.T) {
	got, err := Apply(Input{TraceEmitter: types.TraceEmitterConfig{Type: "otel"}, Getenv: noEnv}, Profile{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got.Traces.Endpoint != defaultTraceEndpoint {
		t.Errorf("trace endpoint = %q, want %q", got.Traces.Endpoint, defaultTraceEndpoint)
	}
	if got.Metrics.Endpoint != "" || got.Logs.Endpoint != "" {
		t.Errorf("metrics and logs must stay unset, got %q and %q", got.Metrics.Endpoint, got.Logs.Endpoint)
	}
}

func signalProfile() Profile {
	return Profile{
		name:             "test-platform",
		RequiredProtocol: otlp.ProtocolHTTPProtobuf,
		Signals: map[otlp.Signal]SignalDefaults{
			otlp.SignalTraces:  {Endpoint: "https://traces.example"},
			otlp.SignalMetrics: {Endpoint: "https://metrics.example"},
			otlp.SignalLogs:    {Endpoint: "https://logs.example", Headers: map[string]string{"x-log-group": "g", "x-log-stream": "s"}},
		},
		ResourceAttrs: []attribute.KeyValue{attribute.String("platform.attr", "v")},
	}
}

// TestApply_Precedence pins the one precedence rule: explicit field, then
// the OTel environment where the exporter honours it, then the profile,
// then the SDK default, with per-signal profile endpoints not inheriting
// traceEmitter.endpoint.
func TestApply_Precedence(t *testing.T) {
	cases := map[string]struct {
		cfg                    types.TraceEmitterConfig
		logsEndpoint           string
		env                    map[string]string
		traces, metrics, logs  string
		protocol               string
		logHeaders, metricHdrs map[string]string
		explicitHeaders        map[string]string
	}{
		"profile fills every blank": {
			cfg:        types.TraceEmitterConfig{Type: "otel"},
			traces:     "https://traces.example",
			metrics:    "https://metrics.example",
			logs:       "https://logs.example",
			protocol:   otlp.ProtocolHTTPProtobuf,
			logHeaders: map[string]string{"x-log-group": "g", "x-log-stream": "s"},
		},
		"explicit endpoint wins for traces only": {
			cfg:        types.TraceEmitterConfig{Type: "otel", Endpoint: "https://collector.example"},
			traces:     "https://collector.example",
			metrics:    "https://metrics.example",
			logs:       "https://logs.example",
			protocol:   otlp.ProtocolHTTPProtobuf,
			logHeaders: map[string]string{"x-log-group": "g", "x-log-stream": "s"},
		},
		"explicit per-signal endpoints win without the profile's headers": {
			cfg:          types.TraceEmitterConfig{Type: "otel", Protocol: otlp.ProtocolHTTPProtobuf, MetricsEndpoint: "https://m.example"},
			logsEndpoint: "https://l.example",
			traces:       "https://traces.example",
			metrics:      "https://m.example",
			logs:         "https://l.example",
			protocol:     otlp.ProtocolHTTPProtobuf,
		},
		"an explicit endpoint equal to the profile's keeps its headers": {
			cfg:          types.TraceEmitterConfig{Type: "otel"},
			logsEndpoint: "https://logs.example/",
			traces:       "https://traces.example",
			metrics:      "https://metrics.example",
			logs:         "https://logs.example/",
			protocol:     otlp.ProtocolHTTPProtobuf,
			logHeaders:   map[string]string{"x-log-group": "g", "x-log-stream": "s"},
		},
		"logs endpoint env beats the profile and its headers": {
			cfg:      types.TraceEmitterConfig{Type: "otel"},
			env:      map[string]string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "https://env-logs.example"},
			traces:   "https://traces.example",
			metrics:  "https://metrics.example",
			logs:     "",
			protocol: otlp.ProtocolHTTPProtobuf,
		},
		"generic endpoint env beats the profile and its headers for logs": {
			cfg:      types.TraceEmitterConfig{Type: "otel"},
			env:      map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://env.example"},
			traces:   "https://traces.example",
			metrics:  "https://metrics.example",
			logs:     "",
			protocol: otlp.ProtocolHTTPProtobuf,
		},
		"explicit headers override profile headers per key": {
			cfg:             types.TraceEmitterConfig{Type: "otel"},
			explicitHeaders: map[string]string{"X-Log-Group": "explicit", "X-Tenant": "a"},
			traces:          "https://traces.example",
			metrics:         "https://metrics.example",
			logs:            "https://logs.example",
			protocol:        otlp.ProtocolHTTPProtobuf,
			logHeaders:      map[string]string{"X-Log-Group": "explicit", "x-log-stream": "s", "X-Tenant": "a"},
			metricHdrs:      map[string]string{"X-Log-Group": "explicit", "X-Tenant": "a"},
		},
		"env headers override profile headers when none are explicit": {
			cfg: types.TraceEmitterConfig{Type: "otel"},
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_HEADERS":      "x-ignored=1",
				"OTEL_EXPORTER_OTLP_LOGS_HEADERS": "x-log-stream=env%20stream, x-extra = e ,malformed,=novalue",
			},
			traces:     "https://traces.example",
			metrics:    "https://metrics.example",
			logs:       "https://logs.example",
			protocol:   otlp.ProtocolHTTPProtobuf,
			logHeaders: map[string]string{"x-log-group": "g", "x-log-stream": "env stream", "x-extra": "e"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Apply(Input{
				TraceEmitter: tc.cfg,
				LogsEndpoint: tc.logsEndpoint,
				Headers:      tc.explicitHeaders,
				Getenv:       envOf(tc.env),
			}, signalProfile())
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if got.Traces.Endpoint != tc.traces || got.Metrics.Endpoint != tc.metrics || got.Logs.Endpoint != tc.logs {
				t.Errorf("endpoints = traces %q metrics %q logs %q, want %q %q %q",
					got.Traces.Endpoint, got.Metrics.Endpoint, got.Logs.Endpoint, tc.traces, tc.metrics, tc.logs)
			}
			for _, s := range []otlp.Settings{got.Traces, got.Metrics, got.Logs} {
				if s.Protocol != tc.protocol {
					t.Errorf("protocol = %q, want %q", s.Protocol, tc.protocol)
				}
			}
			if !maps.Equal(got.Logs.Headers, tc.logHeaders) {
				t.Errorf("log headers = %v, want %v", got.Logs.Headers, tc.logHeaders)
			}
			if !maps.Equal(got.Metrics.Headers, tc.metricHdrs) {
				t.Errorf("metric headers = %v, want %v (profile log headers must not leak)", got.Metrics.Headers, tc.metricHdrs)
			}
			if got.Platform != "test-platform" || len(got.ResourceAttrs) != 1 {
				t.Errorf("platform name and resource attributes must pass through, got %q %v", got.Platform, got.ResourceAttrs)
			}
		})
	}
}

func TestApply_NeverWritesBack(t *testing.T) {
	cfg := types.TraceEmitterConfig{Type: "otel", Headers: map[string]string{"X-Tenant": "a"}}
	headers := map[string]string{"X-Tenant": "a"}
	if _, err := Apply(Input{TraceEmitter: cfg, Headers: headers, Getenv: noEnv}, signalProfile()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if cfg.Endpoint != "" || cfg.Protocol != "" || cfg.MetricsEndpoint != "" {
		t.Errorf("config must not be modified, got %+v", cfg)
	}
	if !maps.Equal(headers, map[string]string{"X-Tenant": "a"}) {
		t.Errorf("explicit headers must not be modified, got %v", headers)
	}
}

func TestApply_WrapTransportBuildsBoundedClients(t *testing.T) {
	var wrapped []otlp.Signal
	p := Profile{
		name:             "signing-platform",
		RequiredProtocol: otlp.ProtocolHTTPProtobuf,
		WrapTransport: func(sig otlp.Signal, rt http.RoundTripper) http.RoundTripper {
			wrapped = append(wrapped, sig)
			return rt
		},
	}
	got, err := Apply(Input{TraceEmitter: types.TraceEmitterConfig{Type: "otel"}, Getenv: noEnv}, p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, s := range []otlp.Settings{got.Traces, got.Metrics, got.Logs} {
		if s.HTTPClient == nil || s.HTTPClient.Timeout != exportRequestTimeout {
			t.Fatalf("signed signals need a client with an explicit timeout, got %+v", s.HTTPClient)
		}
		if s.HTTPClient == http.DefaultClient {
			t.Fatal("http.DefaultClient must not be used")
		}
	}
	if len(wrapped) != 3 {
		t.Errorf("WrapTransport called for %v, want each of the three signals", wrapped)
	}
}

func TestApply_WrapTransportOnlyForTheProfilesEndpoints(t *testing.T) {
	p := signalProfile()
	p.WrapTransport = identityWrap
	got, err := Apply(Input{
		TraceEmitter: types.TraceEmitterConfig{Type: "otel", Endpoint: "https://collector.example"},
		Headers:      map[string]string{"X-Tenant": "a"},
		Getenv:       envOf(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://local:4318"}),
	}, p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got.Traces.HTTPClient != nil || got.Logs.HTTPClient != nil {
		t.Error("signals redirected away from the profile's endpoints must not use its transport")
	}
	if !maps.Equal(got.Logs.Headers, map[string]string{"X-Tenant": "a"}) {
		t.Errorf("redirected log headers = %v, want only the explicit headers", got.Logs.Headers)
	}
	if got.Metrics.HTTPClient == nil {
		t.Error("metrics still go to the profile's endpoint and must use its transport")
	}
}

func TestApply_WrappedClientDoesNotFollowRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		elsewhere.Add(1)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	got, err := Apply(Input{TraceEmitter: types.TraceEmitterConfig{Type: "otel", Endpoint: origin.URL}, Getenv: noEnv},
		Profile{name: "signing-platform", RequiredProtocol: otlp.ProtocolHTTPProtobuf, WrapTransport: identityWrap})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	resp, err := got.Traces.HTTPClient.Post(origin.URL+"/v1/traces", "application/x-protobuf", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || elsewhere.Load() != 0 {
		t.Errorf("status %d, %d requests reached the redirect target; want the redirect returned unfollowed", resp.StatusCode, elsewhere.Load())
	}
}

func TestApply_RejectsContradictions(t *testing.T) {
	headerProfile := Profile{name: "header-platform", Signals: map[otlp.Signal]SignalDefaults{
		otlp.SignalLogs: {Headers: map[string]string{"x-h": "v"}},
	}}
	cases := map[string]struct {
		cfg     types.TraceEmitterConfig
		headers map[string]string
		profile Profile
		want    string
	}{
		"required protocol versus explicit grpc": {
			cfg:     types.TraceEmitterConfig{Type: "otel", Protocol: otlp.ProtocolGRPC},
			profile: Profile{name: "http-platform", RequiredProtocol: otlp.ProtocolHTTPProtobuf},
			want:    `platform "http-platform" requires protocol http/protobuf, but traceEmitter.protocol is "grpc"`,
		},
		"signing on default grpc": {
			cfg:     types.TraceEmitterConfig{Type: "otel"},
			profile: Profile{name: "signing-platform", WrapTransport: identityWrap},
			want:    "requires protocol http/protobuf",
		},
		"header injection on grpc": {
			cfg:     types.TraceEmitterConfig{Type: "otel", Protocol: otlp.ProtocolGRPC},
			profile: headerProfile,
			want:    "requires protocol http/protobuf",
		},
		"explicit Authorization with signing": {
			cfg:     types.TraceEmitterConfig{Type: "otel", Protocol: otlp.ProtocolHTTPProtobuf},
			headers: map[string]string{"authorization": "Bearer t"},
			profile: Profile{name: "signing-platform", WrapTransport: identityWrap},
			want:    "owns the Authorization header",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Apply(Input{TraceEmitter: tc.cfg, Headers: tc.headers, Getenv: noEnv}, tc.profile)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}

	if _, err := Apply(Input{TraceEmitter: types.TraceEmitterConfig{Type: "otel"}, Getenv: noEnv}, Profile{name: "attr-only", ResourceAttrs: []attribute.KeyValue{attribute.String("a", "b")}}); err != nil {
		t.Errorf("a profile that only adds attributes must work over grpc: %v", err)
	}
}
