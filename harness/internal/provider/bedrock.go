package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

// bedrockConverseStreamer is the minimal interface needed from the Bedrock
// runtime client. Defined here for testability — tests inject a mock.
type bedrockConverseStreamer interface {
	ConverseStream(ctx context.Context, params *bedrockruntime.ConverseStreamInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error)
}

// bedrockEventReader abstracts the event stream reader so tests can supply
// a channel-based mock without needing the full SDK event stream machinery.
type bedrockEventReader interface {
	Events() <-chan brtypes.ConverseStreamOutput
	Close() error
	Err() error
}

// BedrockAdapter implements ProviderAdapter for AWS Bedrock's ConverseStream API.
type BedrockAdapter struct {
	client  bedrockConverseStreamer
	Tracer  oteltrace.Tracer       // optional, set by factory for span instrumentation
	Metrics *observability.Metrics // optional, set by factory for metric recording (nil means no recording)
}

// NewBedrockAdapter creates an adapter that uses the ConverseStream API.
// Region and profile are optional overrides for the default AWS credential chain.
// credProvider, when non-nil, is injected into the AWS config to override the
// default credential chain — used for cross-cloud federation scenarios
// (e.g. GKE Workload Identity → STS AssumeRoleWithWebIdentity).
func NewBedrockAdapter(region, profile string, credProvider aws.CredentialsProvider) (*BedrockAdapter, error) {
	var opts []func(*config.LoadOptions) error
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	if credProvider != nil {
		opts = append(opts, config.WithCredentialsProvider(credProvider))
	}

	cfg, err := config.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	client := bedrockruntime.NewFromConfig(cfg)
	return &BedrockAdapter{client: client}, nil
}

// Stream sends a ConverseStream request to Bedrock and returns a channel of
// StreamEvents. The channel is closed when the stream ends or an error occurs.
func (b *BedrockAdapter) Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	start := time.Now()
	metricAttrs := metric.WithAttributes(
		attribute.String("provider.type", "bedrock"),
		attribute.String("provider.model", params.Model),
	)

	input, err := buildConverseStreamInput(params)
	if err != nil {
		b.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("build converse input: %w", err)
	}

	output, err := b.client.ConverseStream(ctx, input)
	if err != nil {
		// Record the error as a span event when OTel instrumentation is enabled.
		if b.Tracer != nil {
			span := oteltrace.SpanFromContext(ctx)
			span.AddEvent("bedrock_error")
		}
		b.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("bedrock ConverseStream: %w", err)
	}

	ch := make(chan types.StreamEvent, 64)
	go func() {
		consumeBedrockStreamMetered(ctx, output.GetStream(), ch, b.Metrics, start, metricAttrs)
		b.recordLatency(ctx, start, metricAttrs)
	}()
	return ch, nil
}

// recordLatency records the total provider request latency to the
// ProviderLatency histogram. Safe to call when Metrics is nil.
func (b *BedrockAdapter) recordLatency(ctx context.Context, start time.Time, attrs metric.MeasurementOption) {
	if b.Metrics == nil {
		return
	}
	b.Metrics.ProviderLatency.Record(ctx, float64(time.Since(start).Milliseconds()), attrs)
}

// consumeBedrockStream reads events from the Bedrock event stream and
// translates them into stirrup StreamEvents. It closes ch when done.
//
// Equivalent to consumeBedrockStreamMetered with nil metrics; preserved as a
// stable entry point for tests.
func consumeBedrockStream(ctx context.Context, stream bedrockEventReader, ch chan<- types.StreamEvent) {
	consumeBedrockStreamMetered(ctx, stream, ch, nil, time.Time{}, metric.WithAttributes())
}

// consumeBedrockStreamMetered reads events from the Bedrock event stream and
// translates them into stirrup StreamEvents. It closes ch when done. When
// metrics is non-nil, records ProviderTTFB on the first non-empty stream
// event observed (TTFB is recorded at most once per stream).
func consumeBedrockStreamMetered(ctx context.Context, stream bedrockEventReader, ch chan<- types.StreamEvent, metrics *observability.Metrics, streamStart time.Time, metricAttrs metric.MeasurementOption) {
	defer close(ch)
	defer func() { _ = stream.Close() }()

	ttfbRecorded := false
	emitEvent := func(ev types.StreamEvent) {
		if !ttfbRecorded && metrics != nil {
			metrics.ProviderTTFB.Record(ctx, float64(time.Since(streamStart).Milliseconds()), metricAttrs)
			ttfbRecorded = true
		}
		ch <- ev
	}

	// Track in-flight tool_use blocks by content block index.
	type toolBlockState struct {
		id      string
		name    string
		jsonBuf strings.Builder
	}
	blocks := make(map[int32]*toolBlockState)

	for event := range stream.Events() {
		select {
		case <-ctx.Done():
			emitEvent(types.StreamEvent{Type: "error", Error: ctx.Err()})
			return
		default:
		}

		switch ev := event.(type) {
		case *brtypes.ConverseStreamOutputMemberContentBlockStart:
			idx := aws.ToInt32(ev.Value.ContentBlockIndex)
			switch start := ev.Value.Start.(type) {
			case *brtypes.ContentBlockStartMemberToolUse:
				blocks[idx] = &toolBlockState{
					id:   aws.ToString(start.Value.ToolUseId),
					name: aws.ToString(start.Value.Name),
				}
			}

		case *brtypes.ConverseStreamOutputMemberContentBlockDelta:
			idx := aws.ToInt32(ev.Value.ContentBlockIndex)
			switch delta := ev.Value.Delta.(type) {
			case *brtypes.ContentBlockDeltaMemberText:
				emitEvent(types.StreamEvent{
					Type: "text_delta",
					Text: delta.Value,
				})
			case *brtypes.ContentBlockDeltaMemberToolUse:
				if bs := blocks[idx]; bs != nil && delta.Value.Input != nil {
					bs.jsonBuf.WriteString(*delta.Value.Input)
				}
			}

		case *brtypes.ConverseStreamOutputMemberContentBlockStop:
			idx := aws.ToInt32(ev.Value.ContentBlockIndex)
			if bs := blocks[idx]; bs != nil {
				var input map[string]any
				raw := bs.jsonBuf.String()
				if raw != "" {
					if err := json.Unmarshal([]byte(raw), &input); err != nil {
						emitEvent(types.StreamEvent{
							Type:  "error",
							Error: fmt.Errorf("parse tool input JSON: %w", err),
						})
						return
					}
				}
				emitEvent(types.StreamEvent{
					Type:  "tool_call",
					ID:    bs.id,
					Name:  bs.name,
					Input: input,
				})
				delete(blocks, idx)
			}

		case *brtypes.ConverseStreamOutputMemberMessageStop:
			emitEvent(types.StreamEvent{
				Type:       "message_complete",
				StopReason: mapStopReason(ev.Value.StopReason),
			})

		case *brtypes.ConverseStreamOutputMemberMetadata:
			if ev.Value.Usage != nil && ev.Value.Usage.OutputTokens != nil {
				emitEvent(types.StreamEvent{
					Type:         "message_complete",
					OutputTokens: int(*ev.Value.Usage.OutputTokens),
				})
			}
		}
	}

	// Check for stream errors after the event channel drains.
	if err := stream.Err(); err != nil {
		emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("bedrock stream: %w", err)})
	}
}

// ---------------------------------------------------------------------------
// Request translation
// ---------------------------------------------------------------------------

// buildConverseStreamInput translates stirrup StreamParams into a Bedrock
// ConverseStreamInput.
func buildConverseStreamInput(params types.StreamParams) (*bedrockruntime.ConverseStreamInput, error) {
	input := &bedrockruntime.ConverseStreamInput{
		ModelId: aws.String(params.Model),
	}

	// System prompt.
	if params.System != "" {
		input.System = []brtypes.SystemContentBlock{
			&brtypes.SystemContentBlockMemberText{Value: params.System},
		}
	}

	// Messages.
	msgs, err := bedrockTranslateMessages(params.Messages)
	if err != nil {
		return nil, err
	}
	input.Messages = msgs

	// Inference config.
	input.InferenceConfig = &brtypes.InferenceConfiguration{}
	if params.MaxTokens > 0 {
		mt := int32(params.MaxTokens)
		input.InferenceConfig.MaxTokens = &mt
	}
	if params.Temperature > 0 {
		temp := float32(params.Temperature)
		input.InferenceConfig.Temperature = &temp
	}

	// Tools.
	if len(params.Tools) > 0 {
		tools, err := bedrockTranslateTools(params.Tools)
		if err != nil {
			return nil, err
		}
		input.ToolConfig = &brtypes.ToolConfiguration{
			Tools: tools,
		}
	}

	return input, nil
}

// translateMessages converts stirrup messages to Bedrock messages.
func bedrockTranslateMessages(msgs []types.Message) ([]brtypes.Message, error) {
	out := make([]brtypes.Message, 0, len(msgs))
	for _, msg := range msgs {
		role, err := mapRole(msg.Role)
		if err != nil {
			return nil, err
		}
		blocks, err := bedrockTranslateContentBlocks(msg.Content)
		if err != nil {
			return nil, fmt.Errorf("message (role=%s): %w", msg.Role, err)
		}
		out = append(out, brtypes.Message{
			Role:    role,
			Content: blocks,
		})
	}
	return out, nil
}

// translateContentBlocks converts stirrup ContentBlocks to Bedrock ContentBlocks.
func bedrockTranslateContentBlocks(blocks []types.ContentBlock) ([]brtypes.ContentBlock, error) {
	out := make([]brtypes.ContentBlock, 0, len(blocks))
	for _, cb := range blocks {
		switch cb.Type {
		case "text":
			out = append(out, &brtypes.ContentBlockMemberText{Value: cb.Text})

		case "tool_use":
			var inputMap map[string]any
			if len(cb.Input) > 0 {
				if err := json.Unmarshal(cb.Input, &inputMap); err != nil {
					return nil, fmt.Errorf("unmarshal tool_use input: %w", err)
				}
			}
			out = append(out, &brtypes.ContentBlockMemberToolUse{
				Value: brtypes.ToolUseBlock{
					ToolUseId: aws.String(cb.ID),
					Name:      aws.String(cb.Name),
					Input:     document.NewLazyDocument(inputMap),
				},
			})

		case "tool_result":
			resultBlock := brtypes.ToolResultBlock{
				ToolUseId: aws.String(cb.ToolUseID),
				Content: []brtypes.ToolResultContentBlock{
					&brtypes.ToolResultContentBlockMemberText{Value: cb.Content},
				},
			}
			if cb.IsError {
				resultBlock.Status = brtypes.ToolResultStatusError
			}
			out = append(out, &brtypes.ContentBlockMemberToolResult{Value: resultBlock})

		default:
			// Skip unknown content block types gracefully.
		}
	}
	return out, nil
}

// translateTools converts stirrup ToolDefinitions to Bedrock Tool specs.
func bedrockTranslateTools(tools []types.ToolDefinition) ([]brtypes.Tool, error) {
	out := make([]brtypes.Tool, 0, len(tools))
	for _, td := range tools {
		var schemaMap map[string]any
		if len(td.InputSchema) > 0 {
			if err := json.Unmarshal(td.InputSchema, &schemaMap); err != nil {
				return nil, fmt.Errorf("unmarshal tool %q input schema: %w", td.Name, err)
			}
		}
		out = append(out, &brtypes.ToolMemberToolSpec{
			Value: brtypes.ToolSpecification{
				Name:        aws.String(td.Name),
				Description: aws.String(td.Description),
				InputSchema: &brtypes.ToolInputSchemaMemberJson{
					Value: document.NewLazyDocument(schemaMap),
				},
			},
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// mapRole converts a stirrup role string to a Bedrock ConversationRole.
func mapRole(role string) (brtypes.ConversationRole, error) {
	switch role {
	case "user":
		return brtypes.ConversationRoleUser, nil
	case "assistant":
		return brtypes.ConversationRoleAssistant, nil
	default:
		return "", fmt.Errorf("unsupported message role: %q", role)
	}
}

// mapStopReason converts a Bedrock StopReason to a string for StreamEvent.
func mapStopReason(reason brtypes.StopReason) string {
	switch reason {
	case brtypes.StopReasonEndTurn:
		return "end_turn"
	case brtypes.StopReasonToolUse:
		return "tool_use"
	case brtypes.StopReasonMaxTokens:
		return "max_tokens"
	case brtypes.StopReasonStopSequence:
		return "stop_sequence"
	default:
		return string(reason)
	}
}
