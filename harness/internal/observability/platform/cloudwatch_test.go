package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability/otlp"
	"github.com/rxbynerd/stirrup/types"
)

// countingCredentials counts Retrieve calls on a static provider.
type countingCredentials struct {
	aws.CredentialsProvider
	calls atomic.Int32
}

func (c *countingCredentials) Retrieve(ctx context.Context) (aws.Credentials, error) {
	c.calls.Add(1)
	return c.CredentialsProvider.Retrieve(ctx)
}

func staticAWS(region string) (*countingCredentials, Deps) {
	creds := &countingCredentials{CredentialsProvider: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret-key", "session-token")}
	return creds, Deps{LoadAWSConfig: func(_ context.Context, pinned string) (aws.Config, error) {
		r := region
		if pinned != "" {
			r = pinned
		}
		return aws.Config{Region: r, Credentials: creds}, nil
	}}
}

func TestCloudWatch_Profile(t *testing.T) {
	_, deps := staticAWS("eu-west-2")
	p, err := Resolve(context.Background(), "cloudwatch", map[string]string{"logGroup": "/stirrup/agents"}, deps)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.RequiredProtocol != otlp.ProtocolHTTPProtobuf {
		t.Errorf("RequiredProtocol = %q", p.RequiredProtocol)
	}
	want := map[otlp.Signal]string{
		otlp.SignalTraces:  "https://xray.eu-west-2.amazonaws.com",
		otlp.SignalMetrics: "https://monitoring.eu-west-2.amazonaws.com",
		otlp.SignalLogs:    "https://logs.eu-west-2.amazonaws.com",
	}
	for sig, endpoint := range want {
		if got := p.Signals[sig].Endpoint; got != endpoint {
			t.Errorf("%s endpoint = %q, want %q", sig, got, endpoint)
		}
	}
	if len(p.Signals[otlp.SignalTraces].Headers) != 0 || len(p.Signals[otlp.SignalMetrics].Headers) != 0 {
		t.Error("log routing headers must apply to the logs signal only")
	}
	wantLogHeaders := map[string]string{"x-aws-log-group": "/stirrup/agents", "x-aws-log-stream": cloudwatchDefaultLogStream}
	if !maps.Equal(p.Signals[otlp.SignalLogs].Headers, wantLogHeaders) {
		t.Errorf("log headers = %v, want %v", p.Signals[otlp.SignalLogs].Headers, wantLogHeaders)
	}
	wantAttrs := []attribute.KeyValue{
		attribute.String("aws.service.type", "gen_ai_agent"),
		attribute.String("aws.log.group.names", "/stirrup/agents"),
	}
	if len(p.ResourceAttrs) != len(wantAttrs) {
		t.Fatalf("resource attributes = %v, want %v", p.ResourceAttrs, wantAttrs)
	}
	for i := range wantAttrs {
		if p.ResourceAttrs[i] != wantAttrs[i] {
			t.Errorf("resource attribute %d = %v, want %v", i, p.ResourceAttrs[i], wantAttrs[i])
		}
	}
	if p.WrapTransport == nil {
		t.Error("cloudwatch must sign requests")
	}
}

func TestCloudWatch_OptionsOverrideTheChain(t *testing.T) {
	_, deps := staticAWS("eu-west-2")
	p, err := Resolve(context.Background(), "cloudwatch", map[string]string{
		"region": "us-east-1", "logGroup": "/g", "logStream": "runs",
	}, deps)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := p.Signals[otlp.SignalTraces].Endpoint; got != "https://xray.us-east-1.amazonaws.com" {
		t.Errorf("region option must win over the chain, got %q", got)
	}
	if got := p.Signals[otlp.SignalLogs].Headers["x-aws-log-stream"]; got != "runs" {
		t.Errorf("log stream = %q, want runs", got)
	}
}

func TestCloudWatch_ResolveErrors(t *testing.T) {
	_, withRegion := staticAWS("eu-west-2")
	_, noRegion := staticAWS("")
	failing := Deps{LoadAWSConfig: func(context.Context, string) (aws.Config, error) {
		return aws.Config{}, errors.New("broken shared config")
	}}
	noCreds := Deps{LoadAWSConfig: func(context.Context, string) (aws.Config, error) {
		return aws.Config{Region: "eu-west-2"}, nil
	}}
	unavailableCreds := Deps{LoadAWSConfig: func(context.Context, string) (aws.Config, error) {
		return aws.Config{Region: "eu-west-2", Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{}, errors.New("no EC2 IMDS role found")
		})}, nil
	}}
	cases := map[string]struct {
		opts map[string]string
		deps Deps
		want string
	}{
		"missing logGroup":        {map[string]string{"region": "us-east-1"}, withRegion, `requires option "logGroup"`},
		"unknown option":          {map[string]string{"logGroup": "/g", "site": "x"}, withRegion, `does not accept option "site"`},
		"bad logGroup":            {map[string]string{"logGroup": "/g with space"}, withRegion, "logGroup"},
		"bad logStream":           {map[string]string{"logGroup": "/g", "logStream": "a:b"}, withRegion, "logStream"},
		"no region":               {map[string]string{"logGroup": "/g"}, noRegion, "no AWS region"},
		"bad region":              {map[string]string{"logGroup": "/g", "region": "evil.example.com/x"}, withRegion, "is not an AWS region name"},
		"config load fails":       {map[string]string{"logGroup": "/g"}, failing, "load AWS config: broken shared config"},
		"no credentials":          {map[string]string{"logGroup": "/g"}, noCreds, "no credentials provider"},
		"credentials unavailable": {map[string]string{"logGroup": "/g"}, unavailableCreds, "retrieve AWS credentials: no EC2 IMDS role found"},
		"china partition":         {map[string]string{"logGroup": "/g", "region": "cn-north-1"}, withRegion, "outside the aws and aws-us-gov partitions"},
		"iso partition":           {map[string]string{"logGroup": "/g", "region": "us-isob-east-1"}, withRegion, "outside the aws and aws-us-gov partitions"},
		"sovereign partition":     {map[string]string{"logGroup": "/g", "region": "eusc-de-east-1"}, withRegion, "outside the aws and aws-us-gov partitions"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Resolve(context.Background(), "cloudwatch", tc.opts, tc.deps)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestCloudWatch_ApplyContradictions(t *testing.T) {
	_, deps := staticAWS("eu-west-2")
	p, err := Resolve(context.Background(), "cloudwatch", map[string]string{"logGroup": "/g"}, deps)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := Apply(Input{TraceEmitter: types.TraceEmitterConfig{Type: "otel", Protocol: "grpc"}, Getenv: noEnv}, p); err == nil {
		t.Error("explicit grpc must be rejected")
	}
	if _, err := Apply(Input{
		TraceEmitter: types.TraceEmitterConfig{Type: "otel", Protocol: otlp.ProtocolHTTPProtobuf},
		Headers:      map[string]string{"Authorization": "Bearer t"},
		Getenv:       noEnv,
	}, p); err == nil || !strings.Contains(err.Error(), "Authorization") {
		t.Errorf("an explicit Authorization header must be rejected, got %v", err)
	}
}

// recordingTransport captures each request it receives with its body.
type recordingTransport struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.requests = append(r.requests, req)
	r.bodies = append(r.bodies, body)
	r.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

func TestSigV4Transport_SignsEachSignalForItsService(t *testing.T) {
	_, deps := staticAWS("eu-west-2")
	p, err := Resolve(context.Background(), "cloudwatch", map[string]string{"logGroup": "/g"}, deps)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	signingServices := map[otlp.Signal]string{
		otlp.SignalTraces:  "xray",
		otlp.SignalMetrics: "monitoring",
		otlp.SignalLogs:    "logs",
	}
	for sig, service := range signingServices {
		t.Run(string(sig), func(t *testing.T) {
			if want := "https://" + service + ".eu-west-2.amazonaws.com"; p.Signals[sig].Endpoint != want {
				t.Errorf("endpoint = %q, want the %s service host %q", p.Signals[sig].Endpoint, service, want)
			}
			rec := &recordingTransport{}
			rt := p.WrapTransport(sig, rec)
			payload := []byte("otlp-protobuf-payload-" + string(sig))
			req, _ := http.NewRequest(http.MethodPost, "https://example.amazonaws.com/v1/"+string(sig), bytes.NewReader(payload))
			req.Header.Set("Content-Type", "application/x-protobuf")
			req.Header.Set("x-aws-log-group", "/g")
			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			_ = resp.Body.Close()

			got := rec.requests[0]
			auth := got.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") {
				t.Errorf("Authorization = %q, want a SigV4 signature", auth)
			}
			if !strings.Contains(auth, "/eu-west-2/"+service+"/aws4_request") {
				t.Errorf("Authorization = %q, want scope eu-west-2/%s", auth, service)
			}
			if !strings.Contains(auth, "x-aws-log-group") {
				t.Errorf("Authorization = %q, want the routing header signed", auth)
			}
			if got.Header.Get("X-Amz-Date") == "" {
				t.Error("X-Amz-Date missing")
			}
			sum := sha256.Sum256(payload)
			if h := got.Header.Get("X-Amz-Content-Sha256"); h != hex.EncodeToString(sum[:]) {
				t.Errorf("X-Amz-Content-Sha256 = %q, want the payload hash", h)
			}
			if tok := got.Header.Get("X-Amz-Security-Token"); tok != "session-token" {
				t.Errorf("X-Amz-Security-Token = %q, want the session token", tok)
			}
			if !bytes.Equal(rec.bodies[0], payload) || got.ContentLength != int64(len(payload)) {
				t.Errorf("body after signing = %q (length %d), want %q", rec.bodies[0], got.ContentLength, payload)
			}
		})
	}
}

func TestSigV4Transport_ResignsEveryAttempt(t *testing.T) {
	creds, _ := staticAWS("eu-west-2")
	rec := &recordingTransport{}
	clock := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	rt := &sigV4Transport{
		base: rec, credentials: creds, signer: v4.NewSigner(), service: "xray", region: "eu-west-2",
		now: func() time.Time { clock = clock.Add(time.Minute); return clock },
	}
	for range 2 {
		req, _ := http.NewRequest(http.MethodPost, "https://xray.eu-west-2.amazonaws.com/v1/traces", bytes.NewReader([]byte("same body")))
		if _, err := rt.RoundTrip(req); err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
	}
	if creds.calls.Load() != 2 {
		t.Errorf("credentials retrieved %d times, want once per attempt", creds.calls.Load())
	}
	first, second := rec.requests[0], rec.requests[1]
	if first.Header.Get("X-Amz-Date") == second.Header.Get("X-Amz-Date") {
		t.Error("a retry must carry a fresh X-Amz-Date")
	}
	if first.Header.Get("Authorization") == second.Header.Get("Authorization") {
		t.Error("a retry must carry a fresh signature")
	}
}

func TestSigV4Transport_CredentialErrorFailsTheRequest(t *testing.T) {
	rec := &recordingTransport{}
	rt := &sigV4Transport{
		base: rec, signer: v4.NewSigner(), service: "logs", region: "eu-west-2", now: time.Now,
		credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{}, errors.New("expired")
		}),
	}
	req, _ := http.NewRequest(http.MethodPost, "https://logs.eu-west-2.amazonaws.com/v1/logs", strings.NewReader("x"))
	if _, err := rt.RoundTrip(req); err == nil || !strings.Contains(err.Error(), "retrieve AWS credentials: expired") {
		t.Errorf("got %v", err)
	}
	if len(rec.requests) != 0 {
		t.Error("an unsigned request must not be sent")
	}
}

func TestSigV4Transport_WithholdsCredentialProcessOutput(t *testing.T) {
	rt := &sigV4Transport{
		base: &recordingTransport{}, signer: v4.NewSigner(), service: "xray", region: "eu-west-2", now: time.Now,
		credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{}, fmt.Errorf("failed to refresh cached credentials, %w", &processcreds.ProviderError{
				Err: errors.New(`parse failed of process output: {"AccessKeyId":"ASIAEXAMPLE","SecretAccessKey":"s3cr3t-key"}`),
			})
		}),
	}
	req, _ := http.NewRequest(http.MethodPost, "https://xray.eu-west-2.amazonaws.com/v1/traces", strings.NewReader("x"))
	_, err := rt.RoundTrip(req)
	if err == nil || strings.Contains(err.Error(), "s3cr3t-key") || strings.Contains(err.Error(), "ASIAEXAMPLE") {
		t.Errorf("error must report the failure without the process output, got %v", err)
	}
}

func TestSigV4Transport_BodyReadErrorFailsTheRequest(t *testing.T) {
	creds, _ := staticAWS("eu-west-2")
	rec := &recordingTransport{}
	rt := &sigV4Transport{base: rec, credentials: creds, signer: v4.NewSigner(), service: "xray", region: "eu-west-2", now: time.Now}
	req, _ := http.NewRequest(http.MethodPost, "https://xray.eu-west-2.amazonaws.com/v1/traces", io.NopCloser(failingReader{}))
	if _, err := rt.RoundTrip(req); err == nil || !strings.Contains(err.Error(), "read request body for signing") {
		t.Errorf("got %v, want a body read error", err)
	}
	if len(rec.requests) != 0 {
		t.Error("a request whose body could not be read must not be sent")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestSigV4Transport_Bodies(t *testing.T) {
	emptyHash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	cases := map[string]struct {
		body     func() io.Reader
		want     []byte
		wantHash string
	}{
		"unknown length": {body: func() io.Reader { return io.MultiReader(strings.NewReader("ab"), strings.NewReader("c")) }, want: []byte("abc")},
		"no body":        {body: func() io.Reader { return nil }, want: nil, wantHash: emptyHash},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			creds, _ := staticAWS("eu-west-2")
			rec := &recordingTransport{}
			rt := &sigV4Transport{base: rec, credentials: creds, signer: v4.NewSigner(), service: "xray", region: "eu-west-2", now: time.Now}
			req, _ := http.NewRequest(http.MethodPost, "https://xray.eu-west-2.amazonaws.com/v1/traces", tc.body())
			if req.Body != nil {
				req.ContentLength = -1
			}
			if _, err := rt.RoundTrip(req); err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			got := rec.requests[0]
			if got.ContentLength != int64(len(tc.want)) || !bytes.Equal(rec.bodies[0], tc.want) {
				t.Errorf("sent body %q with ContentLength %d, want %q", rec.bodies[0], got.ContentLength, tc.want)
			}
			if tc.wantHash != "" && got.Header.Get("X-Amz-Content-Sha256") != tc.wantHash {
				t.Errorf("X-Amz-Content-Sha256 = %q, want %q", got.Header.Get("X-Amz-Content-Sha256"), tc.wantHash)
			}
			replay, err := got.GetBody()
			if err != nil {
				t.Fatalf("GetBody: %v", err)
			}
			if b, _ := io.ReadAll(replay); !bytes.Equal(b, tc.want) {
				t.Errorf("GetBody replays %q, want %q", b, tc.want)
			}
		})
	}
}

func TestCloudWatch_SignsWithTheCurrentTime(t *testing.T) {
	_, deps := staticAWS("eu-west-2")
	p, err := Resolve(context.Background(), "cloudwatch", map[string]string{"logGroup": "/g"}, deps)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	rec := &recordingTransport{}
	req, _ := http.NewRequest(http.MethodPost, "https://xray.eu-west-2.amazonaws.com/v1/traces", strings.NewReader("x"))
	if _, err := p.WrapTransport(otlp.SignalTraces, rec).RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	signedAt, err := time.Parse("20060102T150405Z", rec.requests[0].Header.Get("X-Amz-Date"))
	if err != nil {
		t.Fatalf("X-Amz-Date: %v", err)
	}
	if skew := time.Since(signedAt); skew < -time.Minute || skew > time.Minute {
		t.Errorf("X-Amz-Date is %v from now, want the signing time", skew)
	}
}

func TestCloudWatch_AcceptsGovCloudRegions(t *testing.T) {
	_, deps := staticAWS("")
	p, err := Resolve(context.Background(), "cloudwatch", map[string]string{"logGroup": "/g", "region": "us-gov-west-1"}, deps)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := p.Signals[otlp.SignalTraces].Endpoint; got != "https://xray.us-gov-west-1.amazonaws.com" {
		t.Errorf("trace endpoint = %q", got)
	}
}

// localAWS is the base transport under a test's signing transport: it
// records the URL each signed request was addressed to and serves it from
// srv instead.
type localAWS struct {
	srv  *httptest.Server
	mu   sync.Mutex
	urls []string
}

func (l *localAWS) RoundTrip(req *http.Request) (*http.Response, error) {
	l.mu.Lock()
	l.urls = append(l.urls, req.URL.String())
	l.mu.Unlock()
	target, err := url.Parse(l.srv.URL)
	if err != nil {
		return nil, err
	}
	local := req.Clone(req.Context())
	local.URL.Scheme, local.URL.Host = target.Scheme, target.Host
	return l.srv.Client().Transport.RoundTrip(local)
}

func (l *localAWS) addressed() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.urls)
}

// servedLocally returns p with its signed requests served from srv.
func servedLocally(p Profile, srv *httptest.Server) (Profile, *localAWS) {
	base := &localAWS{srv: srv}
	wrap := p.WrapTransport
	p.WrapTransport = func(sig otlp.Signal, _ http.RoundTripper) http.RoundTripper { return wrap(sig, base) }
	return p, base
}

// exportOneSpan sends one span through a real OTLP exporter built from s.
func exportOneSpan(t *testing.T, s otlp.Settings) {
	t.Helper()
	exporter, err := otlp.NewTraceExporter(context.Background(), s)
	if err != nil {
		t.Fatalf("NewTraceExporter: %v", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	_, span := tp.Tracer("test").Start(context.Background(), "span")
	span.End()
	if err := tp.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// exportOneMetric sends one data point through a real OTLP exporter built
// from s.
func exportOneMetric(t *testing.T, s otlp.Settings) {
	t.Helper()
	exporter, err := otlp.NewMetricExporter(context.Background(), s)
	if err != nil {
		t.Fatalf("NewMetricExporter: %v", err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)))
	counter, err := mp.Meter("test").Int64Counter("runs")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	counter.Add(context.Background(), 1)
	if err := mp.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestCloudWatch_ExporterSendsSignedRequests drives a real OTLP span
// exporter through Apply's settings to the profile's X-Ray endpoint,
// served by a local stand-in.
func TestCloudWatch_ExporterSendsSignedRequests(t *testing.T) {
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, deps := staticAWS("eu-west-2")
	p, err := Resolve(context.Background(), "cloudwatch", map[string]string{"logGroup": "/g"}, deps)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	p, base := servedLocally(p, srv)
	ex, err := Apply(Input{TraceEmitter: types.TraceEmitterConfig{Type: "otel"}, Getenv: noEnv}, p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	exportOneSpan(t, ex.Traces)
	if got := base.addressed(); !slices.Equal(got, []string{"https://xray.eu-west-2.amazonaws.com/v1/traces"}) {
		t.Errorf("requests addressed to %v, want the X-Ray traces URL", got)
	}
	if path != "/v1/traces" {
		t.Errorf("path = %q, want /v1/traces", path)
	}
	if !strings.Contains(auth, "/eu-west-2/xray/aws4_request") {
		t.Errorf("Authorization = %q, want an xray SigV4 signature", auth)
	}
}

// TestCloudWatch_OTLPEnvironmentCannotDowngradeTLS pins that a plaintext
// OTEL_EXPORTER_OTLP_ENDPOINT, which redirects only the log signal, leaves
// the signed trace and metric requests on https.
func TestCloudWatch_OTLPEnvironmentCannotDowngradeTLS(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, deps := staticAWS("eu-west-2")
	p, err := Resolve(context.Background(), "cloudwatch", map[string]string{"logGroup": "/g"}, deps)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	p, base := servedLocally(p, srv)
	ex, err := Apply(Input{TraceEmitter: types.TraceEmitterConfig{Type: "otel"}}, p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	exportOneSpan(t, ex.Traces)
	exportOneMetric(t, ex.Metrics)

	want := []string{
		"https://xray.eu-west-2.amazonaws.com/v1/traces",
		"https://monitoring.eu-west-2.amazonaws.com/v1/metrics",
	}
	if got := base.addressed(); !slices.Equal(got, want) {
		t.Errorf("signed requests addressed to %v, want %v", got, want)
	}
	if ex.Logs.Endpoint != "" || ex.Logs.HTTPClient != nil || len(ex.Logs.Headers) != 0 {
		t.Errorf("logs follow the environment unsigned and without log group headers, got %+v", ex.Logs)
	}
}

// isolateAWSEnvironment points the AWS default chain at static
// environment credentials only, with no shared config and no IMDS.
func isolateAWSEnvironment(t *testing.T, accessKey string) {
	t.Helper()
	missing := t.TempDir() + "/absent"
	for name, value := range map[string]string{
		"AWS_ACCESS_KEY_ID":                      accessKey,
		"AWS_SECRET_ACCESS_KEY":                  "secret-key",
		"AWS_SESSION_TOKEN":                      "",
		"AWS_REGION":                             "eu-west-1",
		"AWS_DEFAULT_REGION":                     "",
		"AWS_PROFILE":                            "",
		"AWS_CONFIG_FILE":                        missing,
		"AWS_SHARED_CREDENTIALS_FILE":            missing,
		"AWS_WEB_IDENTITY_TOKEN_FILE":            "",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI":     "",
		"AWS_EC2_METADATA_DISABLED":              "true",
	} {
		t.Setenv(name, value)
	}
}

func TestCloudWatch_DefaultChainLoadsTheEnvironment(t *testing.T) {
	isolateAWSEnvironment(t, "AKIDEXAMPLE")
	p, err := Resolve(context.Background(), "cloudwatch", map[string]string{"logGroup": "/g"}, Deps{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := p.Signals[otlp.SignalTraces].Endpoint; got != "https://xray.eu-west-1.amazonaws.com" {
		t.Errorf("trace endpoint = %q, want the AWS_REGION endpoint", got)
	}
}

func TestCloudWatch_DefaultChainWithoutCredentialsFailsResolution(t *testing.T) {
	isolateAWSEnvironment(t, "")
	_, err := Resolve(context.Background(), "cloudwatch", map[string]string{"logGroup": "/g"}, Deps{})
	if err == nil || !strings.Contains(err.Error(), "retrieve AWS credentials") {
		t.Errorf("got %v, want a credential retrieval error", err)
	}
}

func TestCredentialsError_TimeoutNamesTheMissingChain(t *testing.T) {
	err := credentialsError(fmt.Errorf("retrieve: %w", context.DeadlineExceeded))
	if err == nil || !strings.Contains(err.Error(), "found none within") || !strings.Contains(err.Error(), "AWS_PROFILE") {
		t.Fatalf("timeout error should explain that no credentials were found, got %v", err)
	}
}
