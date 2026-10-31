package resultsink

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

const publishOK = `{"messageIds":["1234567890"]}`

func newTestPubSubSink(t *testing.T, srv *httptest.Server, opts PubSubSinkOptions) *PubSubSink {
	t.Helper()
	if opts.Topic == "" {
		opts.Topic = testTopic
	}
	opts.CredentialSource = &tokenSource{token: "test-token"}
	opts.HTTPClient = srv.Client()
	opts.EndpointBaseURL = srv.URL
	sink, err := NewPubSubSink(opts)
	require.New(t).NoError(err)
	sink.retryDelays = noRetryDelay
	return sink
}

// publishedMessage is the wire shape of one message in a publish request,
// decoded independently of the sink's own request types.
type publishedMessage struct {
	Data        string            `json:"data"`
	Attributes  map[string]string `json:"attributes"`
	OrderingKey string            `json:"orderingKey"`
}

func decodePublish(t *testing.T, body []byte) publishedMessage {
	t.Helper()
	must := require.New(t)
	var req struct {
		Messages []publishedMessage `json:"messages"`
	}
	must.NoError(json.Unmarshal(body, &req), "body=%s", body)
	must.Len(req.Messages, 1)
	return req.Messages[0]
}

func TestPubSubSink_Emit_PublishesRunResultMessage(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	logs := captureWarnings(t)
	backend := &scriptedServer{statuses: []int{http.StatusOK}, okBody: publishOK}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	attributes := map[string]string{"tenant": "team-a", "env": "prod"}
	sink := newTestPubSubSink(t, srv, PubSubSinkOptions{Attributes: attributes, OrderingKey: "team-a"})
	attributes["tenant"] = "mutated"
	result := sampleResult()
	must.NoError(sink.Emit(context.Background(), result))

	reqs := backend.recorded()
	must.Len(reqs, 1)
	req := reqs[0]
	check.Equal(http.MethodPost, req.method)
	check.Equal("/v1/projects/acme-prod/topics/stirrup-results:publish", req.path)
	check.Equal("Bearer test-token", req.auth)
	check.Equal("application/json", req.ctype)

	msg := decodePublish(t, req.body)
	check.Equal(map[string]string{"tenant": "team-a", "env": "prod", "runId": "run-42"}, msg.Attributes,
		"static attributes are copied at construction and runId is always set")
	check.Equal("team-a", msg.OrderingKey)
	data, err := base64.StdEncoding.DecodeString(msg.Data)
	must.NoError(err)
	var decoded types.RunResult
	must.NoError(json.Unmarshal(data, &decoded))
	check.Equal(result, decoded)
	check.Contains(logs.String(), `"messageId":"1234567890"`, "a successful publish logs the message ID")
}

func TestPubSubSink_Emit_OmitsUnsetOrderingKey(t *testing.T) {
	must := require.New(t)
	captureWarnings(t)
	backend := &scriptedServer{statuses: []int{http.StatusOK}, okBody: publishOK}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	must.NoError(newTestPubSubSink(t, srv, PubSubSinkOptions{}).Emit(context.Background(), sampleResult()))

	reqs := backend.recorded()
	must.Len(reqs, 1)
	must.NotContains(string(reqs[0].body), "orderingKey")
	must.Equal(map[string]string{"runId": "run-42"}, decodePublish(t, reqs[0].body).Attributes)
}

func TestPubSubSink_Emit_EscapesTopicPathSegments(t *testing.T) {
	must := require.New(t)
	captureWarnings(t)
	backend := &scriptedServer{statuses: []int{http.StatusOK}, okBody: publishOK}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	sink := newTestPubSubSink(t, srv, PubSubSinkOptions{Topic: "projects/example.com:acme/topics/results%v1"})
	must.NoError(sink.Emit(context.Background(), sampleResult()))

	reqs := backend.recorded()
	must.Len(reqs, 1)
	must.Equal("/v1/projects/example.com:acme/topics/results%25v1:publish", reqs[0].path)
}

func TestPubSubSink_Emit_RetriesTransientFailureThenSucceeds(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	logs := captureWarnings(t)
	backend := &scriptedServer{statuses: []int{http.StatusServiceUnavailable, http.StatusOK}, okBody: publishOK}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	must.NoError(newTestPubSubSink(t, srv, PubSubSinkOptions{}).Emit(context.Background(), sampleResult()))

	reqs := backend.recorded()
	must.Len(reqs, 2)
	check.Equal(reqs[0].body, reqs[1].body, "every attempt publishes the same message")
	check.Contains(logs.String(), "delivery attempt failed; retrying")
}

func TestPubSubSink_Emit_ReturnsErrorAfterExhaustingAttempts(t *testing.T) {
	must := require.New(t)
	logs := captureWarnings(t)
	backend := &scriptedServer{statuses: []int{http.StatusBadGateway}}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	err := newTestPubSubSink(t, srv, PubSubSinkOptions{}).Emit(context.Background(), sampleResult())

	must.ErrorContains(err, "after 3 attempt(s)")
	must.ErrorContains(err, "pubsub publish returned HTTP 502: backend says no")
	must.Len(backend.recorded(), 3)
	must.Contains(logs.String(), `"attempt":2`, "each retried attempt is logged")
}

func TestPubSubSink_Emit_DoesNotRetryPermanentFailure(t *testing.T) {
	must := require.New(t)
	backend := &scriptedServer{statuses: []int{http.StatusNotFound}}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	err := newTestPubSubSink(t, srv, PubSubSinkOptions{}).Emit(context.Background(), sampleResult())

	must.ErrorContains(err, "after 1 attempt(s)")
	must.ErrorContains(err, "HTTP 404")
	must.Len(backend.recorded(), 1)
}

func TestPubSubSink_Emit_RejectsResponseWithoutMessageID(t *testing.T) {
	must := require.New(t)
	backend := &scriptedServer{statuses: []int{http.StatusOK}, okBody: `{}`}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	err := newTestPubSubSink(t, srv, PubSubSinkOptions{}).Emit(context.Background(), sampleResult())

	must.ErrorContains(err, "carried 0 message IDs")
	must.Len(backend.recorded(), 1, "a malformed acknowledgement is not retried, which could publish twice")
}

func TestPubSubSink_Emit_RejectsMalformedAcknowledgement(t *testing.T) {
	cases := map[string]string{
		"non-JSON body": "not json",
		"two IDs":       `{"messageIds":["1","2"]}`,
		"empty ID":      `{"messageIds":[""]}`,
		"no messageIds": `{"other":1}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			backend := &scriptedServer{statuses: []int{http.StatusOK}, okBody: body}
			srv := httptest.NewServer(backend)
			defer srv.Close()

			err := newTestPubSubSink(t, srv, PubSubSinkOptions{}).Emit(context.Background(), sampleResult())

			must.Error(err)
			must.Len(backend.recorded(), 1, "a malformed acknowledgement is not retried")
		})
	}
}

func TestPubSubSink_Emit_RetriesCredentialFailure(t *testing.T) {
	must := require.New(t)
	captureWarnings(t)
	backend := &scriptedServer{statuses: []int{http.StatusOK}, okBody: publishOK}
	srv := httptest.NewServer(backend)
	defer srv.Close()
	sink := newTestPubSubSink(t, srv, PubSubSinkOptions{})
	sink.credentialSource = &tokenSource{err: errors.New("metadata server unavailable")}

	err := sink.Emit(context.Background(), sampleResult())

	must.ErrorContains(err, "after 3 attempt(s)")
	must.ErrorContains(err, "metadata server unavailable")
	must.Empty(backend.recorded(), "no request is sent without a credential")
}

func TestPubSubSink_Emit_CapsErrorBody(t *testing.T) {
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(strings.Repeat("x", 20*1024)))
	}))
	defer srv.Close()

	err := newTestPubSubSink(t, srv, PubSubSinkOptions{}).Emit(context.Background(), sampleResult())

	must.Error(err)
	must.Less(len(err.Error()), 5*1024)
}

func TestPubSubSink_Emit_RejectsResultWithoutRunID(t *testing.T) {
	must := require.New(t)
	backend := &scriptedServer{statuses: []int{http.StatusOK}, okBody: publishOK}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	err := newTestPubSubSink(t, srv, PubSubSinkOptions{}).Emit(context.Background(), types.RunResult{Outcome: "internal-error"})

	must.ErrorContains(err, "no runId")
	must.Empty(backend.recorded())
}

func TestNewPubSubSink_RejectsInvalidOptions(t *testing.T) {
	cases := []struct {
		name string
		opts PubSubSinkOptions
		want string
	}{
		{"bare topic", PubSubSinkOptions{Topic: "stirrup-results", CredentialSource: &tokenSource{}}, "must have the form"},
		{"extra segment", PubSubSinkOptions{Topic: testTopic + "/x", CredentialSource: &tokenSource{}}, "must have the form"},
		{"no credential", PubSubSinkOptions{Topic: testTopic}, "credential source is required"},
		{"runId attribute", PubSubSinkOptions{Topic: testTopic, CredentialSource: &tokenSource{}, Attributes: map[string]string{"runId": "x"}}, "set by the harness"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, err := NewPubSubSink(tc.opts)
			must.ErrorContains(err, tc.want)
		})
	}
}
