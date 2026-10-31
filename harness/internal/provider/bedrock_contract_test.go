package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirkstest"
	"github.com/rxbynerd/stirrup/types"
)

type capturedBedrockRequest struct {
	path string
	body []byte
}

// captureBedrockConverseRequest streams params through a BedrockAdapter
// backed by the real SDK client pointed at a local server, so the captured
// body is the SDK's own serialisation of the ConverseStream input.
func captureBedrockConverseRequest(t *testing.T, params types.StreamParams) capturedBedrockRequest {
	t.Helper()
	must := require.New(t)
	captured := make(chan capturedBedrockRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case captured <- capturedBedrockRequest{path: r.URL.EscapedPath(), body: body}:
		default:
			t.Error("the SDK sent more than one request")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Amzn-ErrorType", "ValidationException")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"captured by contract test"}`))
	}))
	defer srv.Close()

	client := bedrockruntime.New(bedrockruntime.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(srv.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "contract-test-key-id", SecretAccessKey: "contract-test-secret"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
	adapter := &BedrockAdapter{client: client}
	_, err := adapter.Stream(context.Background(), params)
	must.Error(err, "the contract server answers 400, so Stream must fail")

	select {
	case req := <-captured:
		return req
	default:
		t.Fatal("the SDK never reached the contract server")
		return capturedBedrockRequest{}
	}
}

func bedrockContractParams(model string) types.StreamParams {
	return types.StreamParams{
		Model:       model,
		System:      "You are helpful.",
		Messages:    contractFixtureMessages(),
		Tools:       []types.ToolDefinition{contractFixtureTool()},
		MaxTokens:   4096,
		Temperature: types.Float64Ptr(0.5),
	}
}

// TestBedrockContract_ClaudeSonnet46KeepsTemperature pins the ConverseStream
// body for a Claude model that accepts sampling params.
func TestBedrockContract_ClaudeSonnet46KeepsTemperature(t *testing.T) {
	must := require.New(t)
	req := captureBedrockConverseRequest(t, bedrockContractParams("anthropic.claude-sonnet-4-6"))
	must.Equal("/model/anthropic.claude-sonnet-4-6/converse-stream", req.path)
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath("testdata", "quirks", "bedrock", "anthropic.claude-sonnet-4-6", "request.json"), req.body)
}

// TestBedrockContract_ClaudeSonnet55OmitsTemperature pins the ConverseStream
// body for a Claude 5.x model behind a cross-region inference profile: the
// same request as the Sonnet 4.6 fixture with inferenceConfig.temperature
// absent, because the anthropic rules mark the model as rejecting it.
func TestBedrockContract_ClaudeSonnet55OmitsTemperature(t *testing.T) {
	must := require.New(t)
	req := captureBedrockConverseRequest(t, bedrockContractParams("us.anthropic.claude-sonnet-5-5"))
	must.Equal("/model/us.anthropic.claude-sonnet-5-5/converse-stream", req.path)
	quirkstest.AssertWireEqual(t, quirkstest.JoinPath("testdata", "quirks", "bedrock", "us.anthropic.claude-sonnet-5-5", "request.json"), req.body)
}
