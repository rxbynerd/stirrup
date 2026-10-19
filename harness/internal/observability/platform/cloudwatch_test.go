package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
	"github.com/stretchr/testify/require"
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

func TestCloudWatch_Profile_ResolvesSignalsAndResource(t *testing.T) {
	_, deps := staticAWS("eu-west-2")
	p, err := Resolve(context.Background(), "cloudwatch", map[string]string{"logGroup": "/stirrup/agents"}, deps)
	require.NoError(t, err)
	require.Equal(t, otlp.ProtocolHTTPProtobuf, p.RequiredProtocol)

	wantEndpoints := map[otlp.Signal]string{
		otlp.SignalTraces:  "https://xray.eu-west-2.amazonaws.com",
		otlp.SignalMetrics: "https://monitoring.eu-west-2.amazonaws.com",
		otlp.SignalLogs:    "https://logs.eu-west-2.amazonaws.com",
	}
	for sig, endpoint := range wantEndpoints {
		t.Run(string(sig)+" endpoint", func(t *testing.T) {
			require.Equal(t, endpoint, p.Signals[sig].Endpoint)
		})
	}

	require.Empty(t, p.Signals[otlp.SignalTraces].Headers, "log routing headers must apply to the logs signal only")
	require.Empty(t, p.Signals[otlp.SignalMetrics].Headers, "log routing headers must apply to the logs signal only")
	wantLogHeaders := map[string]string{"x-aws-log-group": "/stirrup/agents", "x-aws-log-stream": cloudwatchDefaultLogStream}
	require.Equal(t, wantLogHeaders, p.Signals[otlp.SignalLogs].Headers)

	wantAttrs := []attribute.KeyValue{
		attribute.String("aws.service.type", "gen_ai_agent"),
		attribute.String("aws.log.group.names", "/stirrup/agents"),
	}
	require.Equal(t, wantAttrs, p.ResourceAttrs)
	require.NotNil(t, p.WrapTransport, "cloudwatch must sign requests")
}

func TestCloudWatch_Options_OverrideAWSConfigChain(t *testing.T) {
	_, deps := staticAWS("eu-west-2")
	p, err := Resolve(context.Background(), "cloudwatch", map[string]string{
		"region": "us-east-1", "logGroup": "/g", "logStream": "runs",
	}, deps)
	require.NoError(t, err)
	require.Equal(t, "https://xray.us-east-1.amazonaws.com", p.Signals[otlp.SignalTraces].Endpoint, "region option must win over the chain")
	require.Equal(t, "runs", p.Signals[otlp.SignalLogs].Headers["x-aws-log-stream"], "logStream option must set the log stream header")
}

func TestCloudWatch_Resolve_RejectsInvalidConfig(t *testing.T) {
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
		opts    map[string]string
		deps    Deps
		wantErr string
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
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestCloudWatch_Apply_RejectsConflictingSettings(t *testing.T) {
	_, deps := staticAWS("eu-west-2")
	p, err := Resolve(context.Background(), "cloudwatch", map[string]string{"logGroup": "/g"}, deps)
	require.NoError(t, err)

	t.Run("explicit grpc protocol", func(t *testing.T) {
		_, err := Apply(Input{TraceEmitter: types.TraceEmitterConfig{Type: "otel", Protocol: "grpc"}, Getenv: noEnv}, p)
		require.Error(t, err)
	})
	t.Run("explicit Authorization header", func(t *testing.T) {
		_, err := Apply(Input{
			TraceEmitter: types.TraceEmitterConfig{Type: "otel", Protocol: otlp.ProtocolHTTPProtobuf},
			Headers:      map[string]string{"Authorization": "Bearer t"},
			Getenv:       noEnv,
		}, p)
		require.ErrorContains(t, err, "Authorization")
	})
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
	require.NoError(t, err)

	signingServices := map[otlp.Signal]string{
		otlp.SignalTraces:  "xray",
		otlp.SignalMetrics: "monitoring",
		otlp.SignalLogs:    "logs",
	}
	for sig, service := range signingServices {
		t.Run(string(sig), func(t *testing.T) {
			require.Equal(t, "https://"+service+".eu-west-2.amazonaws.com", p.Signals[sig].Endpoint)

			rec := &recordingTransport{}
			rt := p.WrapTransport(sig, rec)
			payload := []byte("otlp-protobuf-payload-" + string(sig))
			req, _ := http.NewRequest(http.MethodPost, "https://example.amazonaws.com/v1/"+string(sig), bytes.NewReader(payload))
			req.Header.Set("Content-Type", "application/x-protobuf")
			req.Header.Set("x-aws-log-group", "/g")
			resp, err := rt.RoundTrip(req)
			require.NoError(t, err)
			_ = resp.Body.Close()

			got := rec.requests[0]
			auth := got.Header.Get("Authorization")
			require.True(t, strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/"),
				"Authorization = %q, want a SigV4 signature", auth)
			require.Contains(t, auth, "/eu-west-2/"+service+"/aws4_request")
			require.Contains(t, auth, "x-aws-log-group", "the routing header must be signed")
			require.NotEmpty(t, got.Header.Get("X-Amz-Date"))
			sum := sha256.Sum256(payload)
			require.Equal(t, hex.EncodeToString(sum[:]), got.Header.Get("X-Amz-Content-Sha256"))
			require.Equal(t, "session-token", got.Header.Get("X-Amz-Security-Token"))
			require.Equal(t, payload, rec.bodies[0], "body must be unchanged after signing")
			require.Equal(t, int64(len(payload)), got.ContentLength)
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
		_, err := rt.RoundTrip(req)
		require.NoError(t, err)
	}
	require.EqualValues(t, 2, creds.calls.Load(), "credentials must be retrieved once per attempt")

	first, second := rec.requests[0], rec.requests[1]
	require.NotEqual(t, first.Header.Get("X-Amz-Date"), second.Header.Get("X-Amz-Date"), "a retry must carry a fresh X-Amz-Date")
	require.NotEqual(t, first.Header.Get("Authorization"), second.Header.Get("Authorization"), "a retry must carry a fresh signature")
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
	_, err := rt.RoundTrip(req)
	require.ErrorContains(t, err, "retrieve AWS credentials: expired")
	require.Empty(t, rec.requests, "an unsigned request must not be sent")
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
	require.Error(t, err)
	require.NotContains(t, err.Error(), "s3cr3t-key", "the error must not include the process output")
	require.NotContains(t, err.Error(), "ASIAEXAMPLE", "the error must not include the process output")
}

func TestSigV4Transport_BodyReadErrorFailsTheRequest(t *testing.T) {
	creds, _ := staticAWS("eu-west-2")
	rec := &recordingTransport{}
	rt := &sigV4Transport{base: rec, credentials: creds, signer: v4.NewSigner(), service: "xray", region: "eu-west-2", now: time.Now}
	req, _ := http.NewRequest(http.MethodPost, "https://xray.eu-west-2.amazonaws.com/v1/traces", io.NopCloser(failingReader{}))
	_, err := rt.RoundTrip(req)
	require.ErrorContains(t, err, "read request body for signing")
	require.Empty(t, rec.requests, "a request whose body could not be read must not be sent")
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestSigV4Transport_SignsUnknownLengthAndEmptyBodies(t *testing.T) {
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
			_, err := rt.RoundTrip(req)
			require.NoError(t, err)

			got := rec.requests[0]
			require.Equal(t, int64(len(tc.want)), got.ContentLength)
			// Compared as strings so that a nil body and an empty body match.
			require.Equal(t, string(tc.want), string(rec.bodies[0]))
			if tc.wantHash != "" {
				require.Equal(t, tc.wantHash, got.Header.Get("X-Amz-Content-Sha256"))
			}
			replay, err := got.GetBody()
			require.NoError(t, err)
			b, _ := io.ReadAll(replay)
			require.Equal(t, string(tc.want), string(b), "GetBody must replay the body")
		})
	}
}

func TestCloudWatch_SignsWithTheCurrentTime(t *testing.T) {
	_, deps := staticAWS("eu-west-2")
	p, err := Resolve(context.Background(), "cloudwatch", map[string]string{"logGroup": "/g"}, deps)
	require.NoError(t, err)

	rec := &recordingTransport{}
	req, _ := http.NewRequest(http.MethodPost, "https://xray.eu-west-2.amazonaws.com/v1/traces", strings.NewReader("x"))
	_, err = p.WrapTransport(otlp.SignalTraces, rec).RoundTrip(req)
	require.NoError(t, err)

	signedAt, err := time.Parse("20060102T150405Z", rec.requests[0].Header.Get("X-Amz-Date"))
	require.NoError(t, err)
	require.WithinDuration(t, time.Now(), signedAt, time.Minute, "X-Amz-Date must be the signing time")
}

func TestCloudWatch_AcceptsGovCloudRegions(t *testing.T) {
	_, deps := staticAWS("")
	p, err := Resolve(context.Background(), "cloudwatch", map[string]string{"logGroup": "/g", "region": "us-gov-west-1"}, deps)
	require.NoError(t, err)
	require.Equal(t, "https://xray.us-gov-west-1.amazonaws.com", p.Signals[otlp.SignalTraces].Endpoint)
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
	require.NoError(t, err, "NewTraceExporter")
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	_, span := tp.Tracer("test").Start(context.Background(), "span")
	span.End()
	require.NoError(t, tp.Shutdown(context.Background()), "shutdown")
}

// exportOneMetric sends one data point through a real OTLP exporter built
// from s.
func exportOneMetric(t *testing.T, s otlp.Settings) {
	t.Helper()
	exporter, err := otlp.NewMetricExporter(context.Background(), s)
	require.NoError(t, err, "NewMetricExporter")
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)))
	counter, err := mp.Meter("test").Int64Counter("runs")
	require.NoError(t, err, "Int64Counter")
	counter.Add(context.Background(), 1)
	require.NoError(t, mp.Shutdown(context.Background()), "shutdown")
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
	require.NoError(t, err)
	p, base := servedLocally(p, srv)
	ex, err := Apply(Input{TraceEmitter: types.TraceEmitterConfig{Type: "otel"}, Getenv: noEnv}, p)
	require.NoError(t, err)

	exportOneSpan(t, ex.Traces)
	require.Equal(t, []string{"https://xray.eu-west-2.amazonaws.com/v1/traces"}, base.addressed(), "requests must address the X-Ray traces URL")
	require.Equal(t, "/v1/traces", path)
	require.Contains(t, auth, "/eu-west-2/xray/aws4_request", "the request must carry an xray SigV4 signature")
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
	require.NoError(t, err)
	p, base := servedLocally(p, srv)
	ex, err := Apply(Input{TraceEmitter: types.TraceEmitterConfig{Type: "otel"}}, p)
	require.NoError(t, err)

	exportOneSpan(t, ex.Traces)
	exportOneMetric(t, ex.Metrics)

	want := []string{
		"https://xray.eu-west-2.amazonaws.com/v1/traces",
		"https://monitoring.eu-west-2.amazonaws.com/v1/metrics",
	}
	require.Equal(t, want, base.addressed(), "trace and metric requests must stay signed over https")
	require.Empty(t, ex.Logs.Endpoint, "logs must follow the environment")
	require.Nil(t, ex.Logs.HTTPClient, "logs must not be signed")
	require.Empty(t, ex.Logs.Headers, "logs must not carry log group headers")
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

func TestCloudWatch_DefaultChain_LoadsEnvironmentCredentials(t *testing.T) {
	isolateAWSEnvironment(t, "AKIDEXAMPLE")
	p, err := Resolve(context.Background(), "cloudwatch", map[string]string{"logGroup": "/g"}, Deps{})
	require.NoError(t, err)
	require.Equal(t, "https://xray.eu-west-1.amazonaws.com", p.Signals[otlp.SignalTraces].Endpoint, "the endpoint must follow AWS_REGION")
}

func TestCloudWatch_DefaultChain_FailsWithoutCredentials(t *testing.T) {
	isolateAWSEnvironment(t, "")
	_, err := Resolve(context.Background(), "cloudwatch", map[string]string{"logGroup": "/g"}, Deps{})
	require.ErrorContains(t, err, "retrieve AWS credentials")
}

func TestCredentialsError_TimeoutNamesTheMissingChain(t *testing.T) {
	err := credentialsError(fmt.Errorf("retrieve: %w", context.DeadlineExceeded))
	require.ErrorContains(t, err, "found none within", "a timeout must explain that no credentials were found")
	require.ErrorContains(t, err, "AWS_PROFILE")
}
