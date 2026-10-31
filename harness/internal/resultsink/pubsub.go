package resultsink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/types"
)

// DefaultPubSubEndpointBaseURL is the production Pub/Sub REST host.
const DefaultPubSubEndpointBaseURL = "https://pubsub.googleapis.com"

// maxPubSubResponseBytes bounds how much of a Pub/Sub response is read.
const maxPubSubResponseBytes = 64 * 1024

// maxStatusErrorBodyBytes bounds the response body an error carries,
// matching the GCS client's diagnostic cap.
const maxStatusErrorBodyBytes = 4 * 1024

// PubSubSink publishes the RunResult as one Pub/Sub message whose data is
// the RunResult JSON. A retry after a lost acknowledgement can publish
// the message twice, so delivery is at-least-once; subscribers
// deduplicate on the runId attribute.
type PubSubSink struct {
	publishPath      string
	orderingKey      string
	attributes       map[string]string
	credentialSource credential.Source
	httpClient       *http.Client
	endpointBaseURL  string
	retryDelays      []time.Duration
}

// PubSubSinkOptions configures a PubSubSink. Topic and CredentialSource
// are required.
type PubSubSinkOptions struct {
	// Topic is the full topic resource name, projects/{project}/topics/{topic}.
	Topic string

	// OrderingKey, when set, is copied onto the message.
	OrderingKey string

	// Attributes are copied verbatim onto the message alongside the
	// runId attribute.
	Attributes map[string]string

	CredentialSource credential.Source

	// HTTPClient is optional; a client bounded per request is
	// constructed when nil.
	HTTPClient *http.Client

	// EndpointBaseURL overrides the Pub/Sub host. Tests point it at an
	// httptest.NewServer; production callers leave it empty.
	EndpointBaseURL string
}

// NewPubSubSink validates opts and constructs the sink. Credential
// resolution is deferred to Emit so it is retried with the publish.
func NewPubSubSink(opts PubSubSinkOptions) (*PubSubSink, error) {
	publishPath, err := pubSubPublishPath(opts.Topic)
	if err != nil {
		return nil, fmt.Errorf("gcp-pubsub result sink: %w", err)
	}
	if opts.CredentialSource == nil {
		return nil, errors.New("gcp-pubsub result sink: credential source is required")
	}
	if _, reserved := opts.Attributes[types.PubSubRunIDAttribute]; reserved {
		return nil, fmt.Errorf("gcp-pubsub result sink: attribute %q is set by the harness", types.PubSubRunIDAttribute)
	}
	client := opts.HTTPClient
	if client == nil {
		client = newRemoteHTTPClient()
	}
	endpoint := opts.EndpointBaseURL
	if endpoint == "" {
		endpoint = DefaultPubSubEndpointBaseURL
	}
	return &PubSubSink{
		publishPath:      publishPath,
		orderingKey:      opts.OrderingKey,
		attributes:       maps.Clone(opts.Attributes),
		credentialSource: opts.CredentialSource,
		httpClient:       client,
		endpointBaseURL:  strings.TrimRight(endpoint, "/"),
		retryDelays:      defaultRetryDelays,
	}, nil
}

// pubSubPublishPath turns projects/{project}/topics/{topic} into the
// escaped publish path. types.ValidateRunConfig checks the full shape;
// this guards a caller that bypassed it from building an arbitrary path.
func pubSubPublishPath(topic string) (string, error) {
	parts := strings.Split(topic, "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "topics" || parts[1] == "" || parts[3] == "" {
		return "", fmt.Errorf("topic %q must have the form projects/{project}/topics/{topic}", topic)
	}
	return "/v1/projects/" + url.PathEscape(parts[1]) + "/topics/" + url.PathEscape(parts[3]) + ":publish", nil
}

type pubSubPublishRequest struct {
	Messages []pubSubMessage `json:"messages"`
}

type pubSubMessage struct {
	// Data is base64-encoded on the wire by encoding/json, as the
	// Pub/Sub REST API requires.
	Data        []byte            `json:"data"`
	Attributes  map[string]string `json:"attributes"`
	OrderingKey string            `json:"orderingKey,omitempty"`
}

type pubSubPublishResponse struct {
	MessageIDs []string `json:"messageIds"`
}

// Emit publishes result, retrying transient failures. A nil return means
// Pub/Sub acknowledged the message.
func (s *PubSubSink) Emit(ctx context.Context, result types.RunResult) error {
	if result.RunID == "" {
		return errors.New("gcp-pubsub result sink: RunResult has no runId for the message attribute")
	}
	data, err := marshalResult(result)
	if err != nil {
		return err
	}
	attributes := make(map[string]string, len(s.attributes)+1)
	maps.Copy(attributes, s.attributes)
	attributes[types.PubSubRunIDAttribute] = result.RunID
	body, err := json.Marshal(pubSubPublishRequest{Messages: []pubSubMessage{{
		Data:        data,
		Attributes:  attributes,
		OrderingKey: s.orderingKey,
	}}})
	if err != nil {
		return fmt.Errorf("gcp-pubsub result sink: marshal publish request: %w", err)
	}

	var messageID string
	err = deliver(ctx, "gcp-pubsub", s.retryDelays, func(ctx context.Context) error {
		id, err := s.publish(ctx, body)
		messageID = id
		return err
	})
	if err != nil {
		return fmt.Errorf("gcp-pubsub result sink: publish: %w", err)
	}
	slog.InfoContext(ctx, "resultSink: published RunResult", "sink", "gcp-pubsub",
		"publishPath", s.publishPath, "messageId", messageID, "runId", result.RunID)
	return nil
}

// publish sends one publish request and returns the message ID Pub/Sub
// assigned.
func (s *PubSubSink) publish(ctx context.Context, body []byte) (string, error) {
	bearer, err := resolveBearer(ctx, s.credentialSource)
	if err != nil {
		return "", err
	}
	token, err := bearer(ctx)
	if err != nil {
		return "", fmt.Errorf("acquire bearer token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpointBaseURL+s.publishPath, bytes.NewReader(body))
	if err != nil {
		return "", &permanentError{fmt.Errorf("build request: %w", err)}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("post to pubsub: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxPubSubResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		diagnostic, _ := types.CapFinalAssistantText(strings.ToValidUTF8(strings.TrimSpace(string(respBody)), "�"), maxStatusErrorBodyBytes)
		return "", &statusError{StatusCode: resp.StatusCode, Body: diagnostic}
	}
	// Past a 2xx, Pub/Sub has accepted the message, so a failure here is
	// not retried: another attempt would publish it twice.
	if err != nil {
		return "", &permanentError{fmt.Errorf("read publish response: %w", err)}
	}
	var parsed pubSubPublishResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", &permanentError{fmt.Errorf("decode publish response: %w", err)}
	}
	if len(parsed.MessageIDs) != 1 || parsed.MessageIDs[0] == "" {
		return "", &permanentError{fmt.Errorf("publish response carried %d message IDs, want 1", len(parsed.MessageIDs))}
	}
	return parsed.MessageIDs[0], nil
}
