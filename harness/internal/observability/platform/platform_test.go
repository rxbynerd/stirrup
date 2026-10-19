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

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"

	"github.com/rxbynerd/stirrup/harness/internal/observability/otlp"
	"github.com/rxbynerd/stirrup/types"
)

// registerTestPlatform adds a platform to the registry for one test.
func registerTestPlatform(t *testing.T, name string, def definition) {
	t.Helper()
	require.NotContains(t, registry, name, "test platform %q collides with a registered platform", name)
	registry[name] = def
	t.Cleanup(func() { delete(registry, name) })
}

// TestRequiredProtocol_MatchesRegisteredProfiles pins the validation-time table of
// required protocols to each registered profile's RequiredProtocol.
func TestRequiredProtocol_MatchesRegisteredProfiles(t *testing.T) {
	_, deps := staticAWS("eu-west-2")
	opts := map[string]map[string]string{"cloudwatch": {"logGroup": "/stirrup/agents"}}
	for _, name := range sortedKeys(registry) {
		t.Run(name, func(t *testing.T) {
			p, err := Resolve(context.Background(), name, opts[name], deps)
			require.NoError(t, err, "resolve %q", name)
			want := p.RequiredProtocol
			got := types.RequiredTraceEmitterProtocol(name)
			require.Equal(t, want, got, "types must require the profile's protocol for %q", name)
		})
	}
}

func TestResolve_EmptyPlatformIsZeroProfile(t *testing.T) {
	p, err := Resolve(context.Background(), "", nil, Deps{})
	require.NoError(t, err)
	require.Empty(t, p.name, "empty platform must resolve to the zero Profile, got %+v", p)
	require.Empty(t, p.RequiredProtocol)
	require.Nil(t, p.Signals)
	require.Nil(t, p.ResourceAttrs)
	require.Nil(t, p.WrapTransport)
	require.Nil(t, p.WrapSpanExporter)

	_, err = Resolve(context.Background(), "", map[string]string{"k": "v"}, Deps{})
	require.Error(t, err, "options without a platform must be rejected")
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
	require.NoError(t, err)
	require.Equal(t, "test-platform", p.name)
	require.Equal(t, map[string]string{"needed": "x", "extra": "y"}, gotOpts, "builder must receive the options unchanged")

	cases := map[string]struct {
		platform string
		opts     map[string]string
		want     string
	}{
		"unknown platform": {"no-such-platform", nil, `unknown traceEmitter.platform "no-such-platform"`},
		"unknown option":   {"test-platform", map[string]string{"needed": "x", "typo": "y"}, `does not accept option "typo" (accepted: extra, needed)`},
		"missing required": {"test-platform", map[string]string{"extra": "y"}, `requires option "needed"`},
		"empty required":   {"test-platform", map[string]string{"needed": ""}, `requires option "needed"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Resolve(ctx, tc.platform, tc.opts, Deps{})
			require.ErrorContains(t, err, tc.want)
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
	require.EqualError(t, err, `platform "failing-platform": no region`)
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
	require.NoError(t, err)
	for sig, s := range map[otlp.Signal]otlp.Settings{otlp.SignalTraces: got.Traces, otlp.SignalMetrics: got.Metrics, otlp.SignalLogs: got.Logs} {
		t.Run(string(sig), func(t *testing.T) {
			require.Equal(t, "https://gw.example/otlp", s.Endpoint, "endpoint must come from the trace endpoint")
			require.Equal(t, otlp.ProtocolHTTPProtobuf, s.Protocol, "protocol must come from the trace protocol")
			require.Equal(t, headers, s.Headers, "headers must come from the explicit headers")
			require.Nil(t, s.HTTPClient, "no profile must add a transport")
		})
	}
	require.Equal(t, "delta", got.Metrics.Temporality, "temporality must reach the metric exporter")
	require.Empty(t, got.Traces.Temporality, "temporality must not reach the trace exporter")
	require.Empty(t, got.Platform)
	require.Nil(t, got.ResourceAttrs)
	require.Nil(t, got.WrapSpanExporter)
}

func TestApply_DefaultsWithoutAnyEndpoint(t *testing.T) {
	got, err := Apply(Input{TraceEmitter: types.TraceEmitterConfig{Type: "otel"}, Getenv: noEnv}, Profile{})
	require.NoError(t, err)
	require.Equal(t, defaultTraceEndpoint, got.Traces.Endpoint)
	require.Empty(t, got.Metrics.Endpoint, "metrics endpoint must stay unset")
	require.Empty(t, got.Logs.Endpoint, "logs endpoint must stay unset")
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
			require.NoError(t, err)
			require.Equal(t, tc.traces, got.Traces.Endpoint, "traces endpoint")
			require.Equal(t, tc.metrics, got.Metrics.Endpoint, "metrics endpoint")
			require.Equal(t, tc.logs, got.Logs.Endpoint, "logs endpoint")
			require.Equal(t, tc.protocol, got.Traces.Protocol, "traces protocol")
			require.Equal(t, tc.protocol, got.Metrics.Protocol, "metrics protocol")
			require.Equal(t, tc.protocol, got.Logs.Protocol, "logs protocol")
			require.True(t, maps.Equal(tc.logHeaders, got.Logs.Headers), "log headers = %v, want %v", got.Logs.Headers, tc.logHeaders)
			require.True(t, maps.Equal(tc.metricHdrs, got.Metrics.Headers), "metric headers = %v, want %v (profile log headers must not leak)", got.Metrics.Headers, tc.metricHdrs)
			require.Equal(t, "test-platform", got.Platform, "platform name must pass through")
			require.Len(t, got.ResourceAttrs, 1, "resource attributes must pass through")
		})
	}
}

func TestApply_DoesNotMutateInputs(t *testing.T) {
	cfg := types.TraceEmitterConfig{Type: "otel", Headers: map[string]string{"X-Tenant": "a"}}
	headers := map[string]string{"X-Tenant": "a"}
	_, err := Apply(Input{TraceEmitter: cfg, Headers: headers, Getenv: noEnv}, signalProfile())
	require.NoError(t, err)
	require.Empty(t, cfg.Endpoint, "config must not be modified")
	require.Empty(t, cfg.Protocol, "config must not be modified")
	require.Empty(t, cfg.MetricsEndpoint, "config must not be modified")
	require.Equal(t, map[string]string{"X-Tenant": "a"}, headers, "explicit headers must not be modified")
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
	got, err := Apply(Input{TraceEmitter: types.TraceEmitterConfig{Type: "otel", Endpoint: "https://backend.example"}, Getenv: noEnv}, p)
	require.NoError(t, err)
	for _, s := range []otlp.Settings{got.Traces, got.Metrics, got.Logs} {
		require.NotNil(t, s.HTTPClient, "signed signals need their own client")
		require.Equal(t, exportRequestTimeout, s.HTTPClient.Timeout, "signed signals need a client with an explicit timeout")
		require.NotSame(t, http.DefaultClient, s.HTTPClient, "http.DefaultClient must not be used")
	}
	require.Len(t, wrapped, 3, "WrapTransport must run for each of the three signals, got %v", wrapped)
}

func TestApply_WrapTransportOnlyForTheProfilesEndpoints(t *testing.T) {
	p := signalProfile()
	p.WrapTransport = identityWrap
	got, err := Apply(Input{
		TraceEmitter: types.TraceEmitterConfig{Type: "otel", Endpoint: "https://collector.example"},
		Headers:      map[string]string{"X-Tenant": "a"},
		Getenv:       envOf(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://local:4318"}),
	}, p)
	require.NoError(t, err)
	require.Nil(t, got.Traces.HTTPClient, "redirected traces must not use the profile's transport")
	require.Nil(t, got.Logs.HTTPClient, "redirected logs must not use the profile's transport")
	require.Equal(t, map[string]string{"X-Tenant": "a"}, got.Logs.Headers, "redirected log headers must be only the explicit headers")
	require.NotNil(t, got.Metrics.HTTPClient, "metrics still go to the profile's endpoint and must use its transport")
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
	require.NoError(t, err)
	resp, err := got.Traces.HTTPClient.Post(origin.URL+"/v1/traces", "application/x-protobuf", strings.NewReader("payload"))
	require.NoError(t, err, "POST to the redirecting origin")
	_ = resp.Body.Close()
	require.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode, "the redirect must be returned, not followed")
	require.Zero(t, elsewhere.Load(), "no request may reach the redirect target")
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
		"http platform without a trace endpoint": {
			cfg:     types.TraceEmitterConfig{Type: "otel"},
			profile: Profile{name: "http-platform", RequiredProtocol: otlp.ProtocolHTTPProtobuf},
			want:    `platform "http-platform" exports over http/protobuf and has no trace endpoint`,
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
			require.ErrorContains(t, err, tc.want)
		})
	}

	_, err := Apply(Input{TraceEmitter: types.TraceEmitterConfig{Type: "otel"}, Getenv: noEnv}, Profile{name: "attr-only", ResourceAttrs: []attribute.KeyValue{attribute.String("a", "b")}})
	require.NoError(t, err, "a profile that only adds attributes must work over grpc")
}
