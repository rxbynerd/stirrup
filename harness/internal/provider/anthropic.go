package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

const (
	anthropicAPIURL     = "https://api.anthropic.com/v1/messages"
	anthropicAPIVersion = "2023-06-01"
	maxToolInputSize    = 10 * 1024 * 1024 // 10 MB cap on streamed tool input JSON
)

// AnthropicAdapter implements ProviderAdapter for the Anthropic Messages API.
type AnthropicAdapter struct {
	apiKey     string
	httpClient *http.Client
	baseURL    string                 // overridable for testing
	Tracer     oteltrace.Tracer       // optional, set by factory for span instrumentation
	Metrics    *observability.Metrics // optional, set by factory for metric recording (nil means no recording)
}

// NewAnthropicAdapter creates an adapter for the Anthropic Messages API.
// The HTTP client is configured with explicit timeouts to prevent unbounded
// connections. The overall timeout is generous (120s) because streaming
// responses can be long-lived; transport-level timeouts are tighter.
func NewAnthropicAdapter(apiKey string) *AnthropicAdapter {
	return &AnthropicAdapter{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
		baseURL: anthropicAPIURL,
	}
}

// anthropicRequest is the JSON body sent to the Anthropic Messages API.
type anthropicRequest struct {
	Model       string                 `json:"model"`
	System      string                 `json:"system,omitempty"`
	Messages    []types.Message        `json:"messages"`
	Tools       []types.ToolDefinition `json:"tools,omitempty"`
	MaxTokens   int                    `json:"max_tokens"`
	Temperature float64                `json:"temperature"`
	Stream      bool                   `json:"stream"`
}

// SSE event types from the Anthropic API.
type sseContentBlockStart struct {
	Index        int             `json:"index"`
	ContentBlock sseContentBlock `json:"content_block"`
}

type sseContentBlock struct {
	Type  string          `json:"type"` // "text" | "tool_use"
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Text  string          `json:"text,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type sseContentBlockDelta struct {
	Index int      `json:"index"`
	Delta sseDelta `json:"delta"`
}

type sseDelta struct {
	Type        string `json:"type"` // "text_delta" | "input_json_delta"
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

type sseMessageDelta struct {
	Delta struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage,omitempty"`
}

// Stream sends a streaming request to the Anthropic Messages API and returns
// a channel of StreamEvents. The channel is closed when the stream ends or
// an error occurs. Cancelling the context terminates the stream.
func (a *AnthropicAdapter) Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	start := time.Now()
	metricAttrs := metric.WithAttributes(
		attribute.String("provider.type", "anthropic"),
		attribute.String("provider.model", params.Model),
	)

	reqBody := anthropicRequest{
		Model:       params.Model,
		System:      params.System,
		Messages:    params.Messages,
		Tools:       params.Tools,
		MaxTokens:   params.MaxTokens,
		Temperature: params.Temperature,
		Stream:      true,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		a.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL, strings.NewReader(string(bodyBytes)))
	if err != nil {
		a.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("anthropic-version", anthropicAPIVersion)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		a.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("execute request: %w", err)
	}

	// Record HTTP-level metadata on the span from context (the provider.stream
	// span created by the loop), when OTel instrumentation is enabled.
	if a.Tracer != nil {
		span := oteltrace.SpanFromContext(ctx)
		span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
		if resp.StatusCode == 429 {
			retryAfter := resp.Header.Get("Retry-After")
			span.AddEvent("rate_limited", oteltrace.WithAttributes(
				attribute.String("retry_after", retryAfter),
			))
		}
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		a.recordLatency(ctx, start, metricAttrs)
		if len(body) > 0 {
			return nil, fmt.Errorf("anthropic API returned status %d: %s", resp.StatusCode, body)
		}
		return nil, fmt.Errorf("anthropic API returned status %d", resp.StatusCode)
	}

	ch := make(chan types.StreamEvent, 64)
	go func() {
		a.consumeSSE(ctx, resp, ch, start, metricAttrs)
		a.recordLatency(ctx, start, metricAttrs)
	}()
	return ch, nil
}

// recordLatency records the total provider request latency to the
// ProviderLatency histogram. Safe to call when Metrics is nil.
func (a *AnthropicAdapter) recordLatency(ctx context.Context, start time.Time, attrs metric.MeasurementOption) {
	if a.Metrics == nil {
		return
	}
	a.Metrics.ProviderLatency.Record(ctx, float64(time.Since(start).Milliseconds()), attrs)
}

// consumeSSE reads SSE events from the response body and sends StreamEvents
// to the channel. It closes the channel and the response body when done.
//
// streamStart and metricAttrs are forwarded for ProviderTTFB measurement: the
// first non-empty stream event observed marks "time to first byte" for this
// request. TTFB is recorded at most once per stream.
func (a *AnthropicAdapter) consumeSSE(ctx context.Context, resp *http.Response, ch chan<- types.StreamEvent, streamStart time.Time, metricAttrs metric.MeasurementOption) {
	defer close(ch)
	defer func() { _ = resp.Body.Close() }()

	// emitEvent sends an event on the output channel and records TTFB on the
	// first non-empty event observed. Closes around ttfbRecorded so each call
	// site does not need to check.
	ttfbRecorded := false
	emitEvent := func(ev types.StreamEvent) {
		if !ttfbRecorded && a.Metrics != nil {
			a.Metrics.ProviderTTFB.Record(ctx, float64(time.Since(streamStart).Milliseconds()), metricAttrs)
			ttfbRecorded = true
		}
		ch <- ev
	}

	// Track in-flight content blocks by index for tool_use JSON accumulation.
	type blockState struct {
		blockType string
		id        string
		name      string
		jsonBuf   strings.Builder
	}
	blocks := make(map[int]*blockState)

	scanner := bufio.NewScanner(resp.Body)
	var currentEvent string

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			emitEvent(types.StreamEvent{Type: "error", Error: ctx.Err()})
			return
		default:
		}

		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")

		switch currentEvent {
		case "content_block_start":
			var cbs sseContentBlockStart
			if err := json.Unmarshal([]byte(data), &cbs); err != nil {
				emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse content_block_start: %w", err)})
				return
			}
			blocks[cbs.Index] = &blockState{
				blockType: cbs.ContentBlock.Type,
				id:        cbs.ContentBlock.ID,
				name:      cbs.ContentBlock.Name,
			}

		case "content_block_delta":
			var cbd sseContentBlockDelta
			if err := json.Unmarshal([]byte(data), &cbd); err != nil {
				emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse content_block_delta: %w", err)})
				return
			}
			bs := blocks[cbd.Index]
			if bs == nil {
				continue
			}
			switch cbd.Delta.Type {
			case "text_delta":
				emitEvent(types.StreamEvent{
					Type: "text_delta",
					Text: cbd.Delta.Text,
				})
			case "input_json_delta":
				if bs.jsonBuf.Len()+len(cbd.Delta.PartialJSON) > maxToolInputSize {
					emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("tool input exceeds %d byte limit", maxToolInputSize)})
					return
				}
				bs.jsonBuf.WriteString(cbd.Delta.PartialJSON)
			}

		case "content_block_stop":
			// Parse the index from the data to find which block stopped.
			var stopData struct {
				Index int `json:"index"`
			}
			if err := json.Unmarshal([]byte(data), &stopData); err != nil {
				emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse content_block_stop: %w", err)})
				return
			}
			bs := blocks[stopData.Index]
			if bs != nil && bs.blockType == "tool_use" {
				var input map[string]any
				raw := bs.jsonBuf.String()
				if raw != "" {
					if err := json.Unmarshal([]byte(raw), &input); err != nil {
						emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse tool input JSON: %w", err)})
						return
					}
				}
				emitEvent(types.StreamEvent{
					Type:  "tool_call",
					ID:    bs.id,
					Name:  bs.name,
					Input: input,
				})
			}
			delete(blocks, stopData.Index)

		case "message_delta":
			var md sseMessageDelta
			if err := json.Unmarshal([]byte(data), &md); err != nil {
				emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse message_delta: %w", err)})
				return
			}
			ev := types.StreamEvent{
				Type:       "message_complete",
				StopReason: md.Delta.StopReason,
			}
			if md.Usage != nil {
				ev.OutputTokens = md.Usage.OutputTokens
			}
			emitEvent(ev)

		case "message_stop":
			// Stream is done; the goroutine will exit and close the channel.
			return
		}

		currentEvent = ""
	}

	if err := scanner.Err(); err != nil {
		emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("read SSE stream: %w", err)})
	}
}
