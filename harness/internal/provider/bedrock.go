package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdkretry "github.com/aws/aws-sdk-go-v2/aws/retry"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
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
	client bedrockConverseStreamer

	// AdapterDeps carries the factory-injected Tracer/Metrics/Logger.
	// NewBedrockAdapter maps RetryPolicy onto the SDK retryer, so setting
	// it after construction has no effect.
	AdapterDeps

	// Registry is resolved through ResolveBedrockQuirks on every Stream
	// call; nil falls back to quirks.DefaultRegistry().
	Registry *quirks.Registry

	// credentials and region are captured from the resolved AWS config so
	// Probe can validate the credential chain without issuing a billable
	// request. Both are nil/empty when a test builds the adapter around a
	// mock client, which leaves Probe a no-op skip.
	credentials aws.CredentialsProvider
	region      string
}

// NewBedrockAdapter creates an adapter that uses the ConverseStream API.
// Region and profile are optional overrides for the default AWS credential chain.
// credProvider, when non-nil, overrides the default credential chain — used
// for cross-cloud federation (e.g. GKE Workload Identity → STS
// AssumeRoleWithWebIdentity).
//
// retryPolicy's MaxAttempts and MaxDelay are mapped onto the AWS SDK's own
// Standard retryer rather than DoWithRetry, since ConverseStream goes
// through the SDK's transport with no raw *http.Client seam. MaxAttempts < 1
// swaps in aws.NopRetryer so an explicit "no retries" policy is honoured
// rather than falling back to the SDK's default of 3 attempts.
func NewBedrockAdapter(region, profile string, credProvider aws.CredentialsProvider, retryPolicy RetryPolicy) (*BedrockAdapter, error) {
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
	opts = append(opts, config.WithRetryer(func() aws.Retryer {
		if retryPolicy.MaxAttempts < 1 {
			return aws.NopRetryer{}
		}
		return sdkretry.NewStandard(func(o *sdkretry.StandardOptions) {
			o.MaxAttempts = retryPolicy.MaxAttempts
			if retryPolicy.MaxDelay > 0 {
				o.Backoff = sdkretry.NewExponentialJitterBackoff(retryPolicy.MaxDelay)
			}
		})
	}))
	// The SDK exempts Bedrock Runtime from its default read timeout, so a
	// silent ConverseStream would otherwise hang until the run deadline.
	opts = append(opts, config.WithHTTPClient(
		awshttp.NewBuildableClient().WithReadTimeout(defaultStreamIdleTimeout),
	))

	cfg, err := config.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	client := bedrockruntime.NewFromConfig(cfg)
	return &BedrockAdapter{
		client:      client,
		AdapterDeps: AdapterDeps{RetryPolicy: retryPolicy},
		Registry:    quirks.DefaultRegistry(),
		credentials: cfg.Credentials,
		region:      cfg.Region,
	}, nil
}

// Probe validates that the AWS credential chain resolves, as a cheap
// dry-run reachability check. It deliberately does not call a Bedrock API
// (ConverseStream is billable, and there is no zero-cost reachability
// operation), so a region typo or revoked Bedrock-invoke permission only
// surfaces on the first real turn. A nil credentials provider (the
// mock-client test path) returns nil so the preflight records the step
// without a spurious failure.
func (b *BedrockAdapter) Probe(ctx context.Context) error {
	if b.credentials == nil {
		return nil
	}
	if _, err := b.credentials.Retrieve(ctx); err != nil {
		return fmt.Errorf("resolve AWS credentials (region %q): %w", b.region, err)
	}
	return nil
}

// Stream sends a ConverseStream request to Bedrock and returns a channel of
// StreamEvents. The channel is closed when the stream ends or an error occurs.
func (b *BedrockAdapter) Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	start := time.Now()
	metricAttrs := metric.WithAttributes(
		attribute.String("provider.type", "bedrock"),
		attribute.String("provider.model", params.Model),
	)

	// Resolved once at the top so it is available on every early-return
	// path, not only inside the tool-choice branch below.
	logger := b.Logger
	if logger == nil {
		// This fallback bypasses the ScrubHandler-backed logger the
		// factory injects; only direct struct-literal construction (not
		// the production factory path) reaches it.
		logger = slog.Default()
	}

	// buildConverseStreamInput does not project ToolChoice onto the
	// ConverseStream request, so a non-auto ToolChoice is silently
	// downgraded to auto; warn so the downgrade is observable. Only the
	// static mode integer and adapter/model identifiers are logged.
	if params.ToolChoice != types.ToolChoiceAuto {
		logger.WarnContext(ctx, "bedrock tool-choice downgraded to auto: adapter does not support tool-choice",
			slog.String("provider.type", "bedrock"),
			slog.String("provider.model", params.Model),
			slog.Int("tool_choice", int(params.ToolChoice)),
		)
	}

	registry := b.Registry
	if registry == nil {
		registry = quirks.DefaultRegistry()
	}
	q, appliedRules := ResolveBedrockQuirks(registry, params.Model)
	logger.DebugContext(ctx, "bedrock quirks resolved",
		slog.String("provider.type", "bedrock"),
		slog.String("provider.model", params.Model),
		slog.Any("rules", ruleDescriptions(appliedRules)),
	)
	if span := oteltrace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		span.SetAttributes(attribute.StringSlice("provider.quirk.applied", ruleDescriptions(appliedRules)))
	}
	if q.BehaviourFlags.Anthropic.OmitSamplingParams && params.Temperature != nil {
		logger.WarnContext(ctx, "bedrock quirks suppressed caller temperature",
			slog.String("provider.type", "bedrock"),
			slog.String("provider.model", params.Model),
			slog.Any("quirk.rules", ruleDescriptions(appliedRules)),
		)
	}
	warnDroppedReasoningEffort(ctx, logger, "bedrock", params.ReasoningEffort, params.Model, q.BehaviourFlags.Anthropic.EffortLevels)

	// Converse exposes outputConfig.textFormat, but the adapter projects
	// no structured-output field, so the caller's prompt-only JSON
	// contract applies even where the anthropic rules advertise one.
	logDroppedOutputSchema(ctx, logger, "bedrock", params.Model, params.OutputSchema, quirks.OutputSchemaCapability{})

	input, err := buildConverseStreamInput(params, q)
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
		err = fmt.Errorf("bedrock ConverseStream: %w", err)
		if bedrockContextOverflow(err) {
			err = types.MarkContextOverflow(err)
		}
		return nil, err
	}

	inference := inferenceAttrs("bedrock", params.Model, "")
	inference.ServerAddress, inference.ServerPort = b.ServerEndpoint()
	ch := make(chan types.StreamEvent, 64)
	go consumeBedrockStreamMetered(ctx, output.GetStream(), ch, b.Metrics, start, metricAttrs, inference)
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
	consumeBedrockStreamMetered(ctx, stream, ch, nil, time.Time{}, metric.WithAttributes(), observability.InferenceAttrs{})
}

// consumeBedrockStreamMetered reads events from the Bedrock event stream and
// translates them into stirrup StreamEvents. It closes ch when done. When
// metrics is non-nil, records the time to first chunk on the first non-empty stream
// event observed (recorded at most once per stream), and records the request
// latency before closing ch, so a reader that sees the close can rely on the
// measurement existing.
func consumeBedrockStreamMetered(ctx context.Context, stream bedrockEventReader, ch chan<- types.StreamEvent, metrics *observability.Metrics, streamStart time.Time, metricAttrs metric.MeasurementOption, inference observability.InferenceAttrs) {
	defer close(ch)
	if metrics != nil {
		defer func() { // Deferred after close(ch) so it runs first.
			metrics.ProviderLatency.Record(ctx, float64(time.Since(streamStart).Milliseconds()), metricAttrs)
		}()
	}
	defer func() { _ = stream.Close() }()

	ttfbRecorded := false
	emitEvent := func(ev types.StreamEvent) {
		if !ttfbRecorded && metrics != nil {
			metrics.RecordFirstChunk(ctx, metricAttrs, inference, time.Since(streamStart))
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
				emitEvent(toolCallEvent(bs.id, bs.name, bs.jsonBuf.String()))
				delete(blocks, idx)
			}

		case *brtypes.ConverseStreamOutputMemberMessageStop:
			emitEvent(types.StreamEvent{
				Type:       "message_complete",
				StopReason: mapStopReason(ev.Value.StopReason),
			})

		case *brtypes.ConverseStreamOutputMemberMetadata:
			if ev.Value.Usage != nil {
				usageEv := types.StreamEvent{Type: "message_complete"}
				applyBedrockUsage(ev.Value.Usage, &usageEv)
				emitEvent(usageEv)
			}
		}
	}

	if err := stream.Err(); err != nil {
		err = fmt.Errorf("bedrock stream: %w", err)
		if bedrockContextOverflow(err) {
			err = types.MarkContextOverflow(err)
		}
		emitEvent(types.StreamEvent{Type: "error", Error: err})
	}
}

// applyBedrockUsage copies Converse usage onto a message_complete event.
// The whole prompt is totalTokens minus outputTokens, which holds whether
// or not inputTokens counts the cache figures; without totalTokens it
// falls back to inputTokens plus both cache figures.
func applyBedrockUsage(u *brtypes.TokenUsage, ev *types.StreamEvent) {
	output := clampTokens(int(aws.ToInt32(u.OutputTokens)))
	cacheRead := clampTokens(int(aws.ToInt32(u.CacheReadInputTokens)))
	cacheWrite := clampTokens(int(aws.ToInt32(u.CacheWriteInputTokens)))
	input := clampTokens(int(aws.ToInt32(u.InputTokens))) + cacheRead + cacheWrite
	if u.TotalTokens != nil {
		input = clampTokens(int(*u.TotalTokens)) - output
	}
	setEventUsage(ev, tokenReport{
		Input:      input,
		Output:     output,
		CacheRead:  cacheRead,
		CacheWrite: cacheWrite,
	})
}

// BedrockQuirksTarget maps a Bedrock model id onto the anthropic rules:
// anthropic.<id>, optionally behind one leading label such as a
// cross-region prefix and after the last "/" of an ARN, resolves as
// <id> on the anthropic provider type. Any other id resolves as provider
// type bedrock, which no built-in rule names.
func BedrockQuirksTarget(model string) quirks.Target {
	id := model[strings.LastIndex(model, "/")+1:]
	if prefix, rest, ok := strings.Cut(id, "."); ok && prefix != "anthropic" {
		id = rest
	}
	if claude, ok := strings.CutPrefix(id, "anthropic."); ok && claude != "" {
		return quirks.Target{ProviderType: "anthropic", Endpoint: quirks.EndpointBedrock, Model: claude}
	}
	return quirks.Target{ProviderType: "bedrock", Model: model}
}

// ResolveBedrockQuirks resolves model at BedrockQuirksTarget and keeps
// only Anthropic.OmitSamplingParams, with the rules that changed it: the
// Converse request carries none of the Messages API features the other
// anthropic flags gate, so reporting them would misdescribe the request.
func ResolveBedrockQuirks(registry *quirks.Registry, model string) (quirks.ProviderQuirks, []quirks.Rule) {
	target := BedrockQuirksTarget(model)
	_, matched := registry.ResolveWithRules(target)
	q := quirks.NewRegistry(nil).Resolve(target)
	replay := quirks.NewRegistry(nil).Resolve(target)
	honoured := make([]quirks.Rule, 0, len(matched))
	for _, rule := range matched {
		before := replay.BehaviourFlags.Anthropic.OmitSamplingParams
		rule.Apply(&replay)
		if replay.BehaviourFlags.Anthropic.OmitSamplingParams != before {
			honoured = append(honoured, rule)
		}
	}
	q.BehaviourFlags.Anthropic.OmitSamplingParams = replay.BehaviourFlags.Anthropic.OmitSamplingParams
	return q, honoured
}

// buildConverseStreamInput translates stirrup StreamParams into a Bedrock
// ConverseStreamInput, dropping temperature where q marks the model as
// rejecting sampling params.
func buildConverseStreamInput(params types.StreamParams, q quirks.ProviderQuirks) (*bedrockruntime.ConverseStreamInput, error) {
	input := &bedrockruntime.ConverseStreamInput{
		ModelId: aws.String(params.Model),
	}

	if params.System != "" {
		input.System = []brtypes.SystemContentBlock{
			&brtypes.SystemContentBlockMemberText{Value: params.System},
		}
	}

	msgs, err := bedrockTranslateMessages(params.Messages)
	if err != nil {
		return nil, err
	}
	input.Messages = msgs

	input.InferenceConfig = &brtypes.InferenceConfiguration{}
	if params.MaxTokens > 0 {
		mt := int32(params.MaxTokens)
		input.InferenceConfig.MaxTokens = &mt
	}
	// A non-nil Temperature, including an explicit 0.0 for greedy
	// decoding, is forwarded unless the model rejects sampling params.
	if params.Temperature != nil && !q.BehaviourFlags.Anthropic.OmitSamplingParams {
		temp := float32(*params.Temperature)
		input.InferenceConfig.Temperature = &temp
	}

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
	dropped := false
	for _, msg := range msgs {
		role, err := mapRole(msg.Role)
		if err != nil {
			return nil, err
		}
		blocks, err := bedrockTranslateContentBlocks(msg.Content)
		if err != nil {
			return nil, fmt.Errorf("message (role=%s): %w", msg.Role, err)
		}
		if len(blocks) == 0 && hasToolSearchBlocks(msg.Content) {
			dropped = true
			continue
		}
		// Converse requires alternating roles, so the messages on either
		// side of a dropped one are joined.
		if last := len(out) - 1; dropped && last >= 0 && out[last].Role == role {
			out[last].Content = append(out[last].Content, blocks...)
		} else {
			out = append(out, brtypes.Message{
				Role:    role,
				Content: blocks,
			})
		}
		dropped = false
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
			inputMap := map[string]any{}
			if err := json.Unmarshal(types.NormalizeToolInput(cb.Input), &inputMap); err != nil {
				return nil, fmt.Errorf("unmarshal tool_use input: %w", err)
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
