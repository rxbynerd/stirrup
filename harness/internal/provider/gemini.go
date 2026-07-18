package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// Vertex AI host templates and the streaming endpoint path. The "global"
// location is special-cased because Vertex serves it from the unprefixed
// host rather than from a "global-aiplatform.googleapis.com" subdomain.
const (
	geminiAPIRegionalHost = "%s-aiplatform.googleapis.com"
	geminiAPIGlobalHost   = "aiplatform.googleapis.com"
	geminiAPIPathTemplate = "/v1/projects/%s/locations/%s/publishers/google/models/%s:streamGenerateContent?alt=sse"
	// geminiModelsPathTemplate has no model segment and no
	// :streamGenerateContent action, so the preflight probe's GET never
	// triggers a completion.
	geminiModelsPathTemplate = "/v1/projects/%s/locations/%s/publishers/google/models"

	// geminiMaxScannerBuffer caps the SSE per-line buffer; 16 MiB bounds
	// a final chunk that bundles usage metadata with a long tool-call
	// args blob, without letting a malformed line OOM the run.
	geminiMaxScannerBuffer = 16 * 1024 * 1024
)

// GeminiAdapter implements ProviderAdapter for Vertex AI's
// :streamGenerateContent endpoint. Auth is OAuth2 (a Google access token
// fetched via the bearer closure on every Stream call) — never an AI
// Studio API key. The adapter is text-and-tools only; multimodal input
// and Google's server-side built-in tools are out of scope.
type GeminiAdapter struct {
	bearer     credential.BearerTokenFunc
	projectID  string
	location   string
	safety     []types.GeminiSafetySetting
	httpClient *http.Client

	// baseURLOverride points the adapter at an httptest.Server; empty in
	// production, where the URL is derived from projectID + location.
	baseURLOverride string

	// streamCounter namespaces synthesised tool-call IDs
	// ("gemini-{streamN}-{partIdx}") across concurrent Stream calls on
	// the same adapter, since Vertex never echoes IDs through
	// functionResponse.
	streamCounter atomic.Int64

	// AdapterDeps carries the factory-injected Tracer/Metrics/RetryPolicy/
	// Logger; see its doc comment for the field-by-field contract.
	AdapterDeps

	// Registry resolves per-(provider, model) wire-shape and behaviour
	// overrides at the top of every Stream call. NewGeminiAdapter seeds
	// it with quirks.DefaultRegistry(); tests overwrite it directly.
	Registry *quirks.Registry
}

// NewGeminiAdapter creates an adapter for Vertex AI's
// :streamGenerateContent endpoint. bearer is invoked on every Stream
// call to fetch a fresh Google OAuth2 access token; caching and
// refreshing live in the credential layer
// (credential.bearerFromTokenSource).
func NewGeminiAdapter(
	bearer credential.BearerTokenFunc,
	projectID, location string,
	safety []types.GeminiSafetySetting,
) *GeminiAdapter {
	return &GeminiAdapter{
		bearer:    bearer,
		projectID: projectID,
		location:  location,
		safety:    safety,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
		Registry: quirks.DefaultRegistry(),
	}
}

// buildURL renders the endpoint URL for one Stream call. "global" routes
// to aiplatform.googleapis.com; every other location goes to the
// regional subdomain.
func (g *GeminiAdapter) buildURL(model string) string {
	// Escape every path component as defence-in-depth: model comes from
	// RunConfig.ModelRouter.Model and must not be able to rewrite the
	// URL shape even though validateGeminiProviderFields already checks it.
	projID := url.PathEscape(g.projectID)
	loc := url.PathEscape(g.location)
	mdl := url.PathEscape(model)

	if g.baseURLOverride != "" {
		// Tests inject an httptest server URL here.
		path := fmt.Sprintf(geminiAPIPathTemplate, projID, loc, mdl)
		return strings.TrimRight(g.baseURLOverride, "/") + path
	}
	host := geminiAPIGlobalHost
	if g.location != "global" {
		host = fmt.Sprintf(geminiAPIRegionalHost, g.location)
	}
	path := fmt.Sprintf(geminiAPIPathTemplate, projID, loc, mdl)
	return "https://" + host + path
}

// modelsURL renders the publisher-model collection URL for a preflight
// probe: a metadata GET that spends no tokens. Mirrors buildURL's host
// derivation.
func (g *GeminiAdapter) modelsURL() string {
	projID := url.PathEscape(g.projectID)
	loc := url.PathEscape(g.location)
	path := fmt.Sprintf(geminiModelsPathTemplate, projID, loc)

	if g.baseURLOverride != "" {
		return strings.TrimRight(g.baseURLOverride, "/") + path
	}
	host := geminiAPIGlobalHost
	if g.location != "global" {
		host = fmt.Sprintf(geminiAPIRegionalHost, g.location)
	}
	return "https://" + host + path
}

// Stream sends a streaming request to Vertex AI and returns a channel of
// StreamEvents. The channel is closed when the stream ends, the context
// is cancelled, or an unrecoverable error occurs.
//
// On non-200 responses, Stream drains up to 4 KiB of the response body
// into the returned error so the operator can see Vertex's diagnostic
// without exfiltrating an unbounded payload to logs.
func (g *GeminiAdapter) Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	start := time.Now()
	metricAttrs := metric.WithAttributes(
		attribute.String("provider.type", "gemini"),
		attribute.String("provider.model", params.Model),
	)

	// Quirks are resolved once per stream; the same value drives both
	// send (BuildGenerateContentRequest) and parse (consumeSSE). The
	// nil-Registry guard tolerates callers that build the adapter
	// outside the factory without going through NewGeminiAdapter.
	registry := g.Registry
	if registry == nil {
		registry = quirks.DefaultRegistry()
	}
	q, appliedRules := registry.ResolveWithRules("gemini", params.Model)

	logger := g.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// Emitted even when the rule list is empty so an operator grepping
	// for the line can distinguish "no rule fired" from "suppressed".
	logger.DebugContext(ctx, "gemini quirks resolved",
		slog.String("provider.type", "gemini"),
		slog.String("provider.model", params.Model),
		slog.Any("rules", ruleDescriptions(appliedRules)),
	)

	// Mirrors the slog DEBUG line above so trace-only consumers also see
	// which rules fired. IsValid keeps the attribute slice allocation
	// off the hot path when no tracer is configured.
	if span := oteltrace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		span.SetAttributes(attribute.StringSlice("provider.quirk.applied", ruleDescriptions(appliedRules)))
	}

	body, _, err := BuildGenerateContentRequest(params, g.safety, q)
	if err != nil {
		g.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("build request: %w", err)
	}
	// BuildGenerateContentRequest also returns a per-call id->name map
	// for outbound tool_result correlation; the marshaller already
	// populates functionResponse.name from it, so the adapter itself
	// does not need it here.

	token, err := g.bearer(ctx)
	if err != nil {
		g.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("resolve bearer token: %w", err)
	}

	url := g.buildURL(params.Model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		g.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	// DoWithRetry only covers the pre-stream call; a failure after
	// streaming has begun is never replayed.
	resp, err := DoWithRetry(ctx, g.httpClient, req, RetryOptions{
		Policy:       g.RetryPolicy,
		Logger:       g.Logger,
		Metrics:      g.Metrics,
		ProviderType: "gemini",
		Model:        params.Model,
	})
	if err != nil {
		g.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("execute request: %w", err)
	}

	// The 429 add-event mirrors the Anthropic adapter so rate-limit
	// retries surface uniformly across providers.
	if g.Tracer != nil {
		span := oteltrace.SpanFromContext(ctx)
		span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfter := resp.Header.Get("Retry-After")
			span.AddEvent("rate_limited", oteltrace.WithAttributes(
				attribute.String("retry_after", retryAfter),
			))
		}
	}

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		g.recordLatency(ctx, start, metricAttrs)
		if len(bodyBytes) > 0 {
			// Scrub at construction time as defence-in-depth: the
			// LogScrubber runs downstream at the loop's error boundary,
			// but any instrumentation between here and there would
			// otherwise see unscrubbed Vertex diagnostics.
			safeBody := security.Scrub(string(bodyBytes))
			return nil, fmt.Errorf("vertex AI returned status %d: %s", resp.StatusCode, safeBody)
		}
		return nil, fmt.Errorf("vertex AI returned status %d", resp.StatusCode)
	}

	streamN := g.streamCounter.Add(1)

	ch := make(chan types.StreamEvent, 64)
	go func() {
		g.consumeSSE(ctx, resp, ch, start, metricAttrs, streamN, q, params.Model)
		g.recordLatency(ctx, start, metricAttrs)
	}()
	return ch, nil
}

// recordLatency records the total provider request latency to the
// ProviderLatency histogram. Safe to call when Metrics is nil.
func (g *GeminiAdapter) recordLatency(ctx context.Context, start time.Time, attrs metric.MeasurementOption) {
	if g.Metrics == nil {
		return
	}
	g.Metrics.ProviderLatency.Record(ctx, float64(time.Since(start).Milliseconds()), attrs)
}

// mapGeminiFinishReason translates Vertex's finishReason enum onto the
// stop-reason vocabulary the agentic loop understands. The caller
// remaps STOP → "tool_use" when functionCall parts were observed (see
// docs/providers.md). Unknown reasons pass through lowercased.
func mapGeminiFinishReason(in string) string {
	switch in {
	case "":
		return ""
	case "STOP":
		return "end_turn"
	case "MAX_TOKENS":
		return "max_tokens"
	case "SAFETY":
		return "safety_blocked"
	case "RECITATION":
		return "recitation_blocked"
	default:
		return strings.ToLower(in)
	}
}

// toolCallBuf tracks the in-flight functionCall at one part-index slot;
// see docs/providers.md for the willContinue/partialArgs accumulation
// wire format. The retained blob is bounded at maxToolInputSize (10 MB)
// to mirror the Anthropic adapter's safety cap. thoughtSignature holds
// the latest non-empty value observed for the part, since Vertex treats
// it as a property of the part rather than of the call.
type toolCallBuf struct {
	name             string
	args             []byte
	thoughtSignature string
}

// consumeSSE reads SSE events from the response body and emits
// StreamEvents to the channel, closing both when done. See
// docs/providers.md for the Vertex generateContentChunk wire format.
// streamN namespaces synthesised tool-call IDs
// ("gemini-{streamN}-{partIdx}"), since Vertex never echoes them
// through functionResponse. q carries the resolved per-(provider,
// model) quirks driving ReplayFields parse-side recognition.
func (g *GeminiAdapter) consumeSSE(
	ctx context.Context,
	resp *http.Response,
	ch chan<- types.StreamEvent,
	streamStart time.Time,
	metricAttrs metric.MeasurementOption,
	streamN int64,
	q quirks.ProviderQuirks,
	model string,
) {
	defer close(ch)
	defer func() { _ = resp.Body.Close() }()

	// The ReplayFields accumulator collects every path matched across
	// every chunk in this stream; the deferred log below emits the
	// summary on any exit path, including an error mid-stream. Length
	// only — captured values (e.g. Gemini 3.x's thoughtSignature) are
	// provider-private and must not land in log sinks.
	logger := g.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// replayFieldsTotalBytes / replayFieldsCapped enforce the shared
	// maxReplayFieldBytes budget (see the constant in openai.go): once
	// the cap is hit, every later capture is dropped so the
	// accumulator stays a clean prefix and a hostile upstream cannot
	// balloon per-stream memory.
	replayFieldsCapture := map[string][]any{}
	replayFieldsTotalBytes := 0
	replayFieldsCapped := false
	defer func() {
		if len(replayFieldsCapture) == 0 {
			return
		}
		// Mirrors the slog summary onto the OTel span so trace-only
		// consumers see the rule fired without correlating with logs.
		if span := oteltrace.SpanFromContext(ctx); span.SpanContext().IsValid() {
			totalCount, totalLen := summarizeReplayCaptures(replayFieldsCapture)
			span.SetAttributes(
				attribute.Int("replay_fields_captured.count", totalCount),
				attribute.Int("replay_fields_captured.total_len", totalLen),
			)
		}
		logReplayFieldsCapture(ctx, logger, "gemini", model, replayFieldsCapture)
	}()

	ttfbRecorded := false
	emitEvent := func(ev types.StreamEvent) {
		if !ttfbRecorded && g.Metrics != nil {
			g.Metrics.ProviderTTFB.Record(ctx, float64(time.Since(streamStart).Milliseconds()), metricAttrs)
			ttfbRecorded = true
		}
		ch <- ev
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), geminiMaxScannerBuffer)

	// Slots are keyed by function name, not a monotonic part index:
	// Vertex sends the name on every chunk of a streamed call, so two
	// simultaneous willContinue=true calls accumulate into separate
	// slots instead of colliding at index 0 before either closes.
	//
	// nextPartIdx is used only for tool-call ID synthesis, not to
	// address slots in toolBufs.
	toolBufs := make(map[string]*toolCallBuf)
	nextPartIdx := 0
	// Vertex reports finishReason=STOP even on tool-call turns; remapping
	// STOP → tool_use when a functionCall part was seen bridges Vertex's
	// vocabulary to the loop's stop-reason switch.
	sawFunctionCall := false

	// emitToolCall finalises the named slot and emits a tool_call
	// StreamEvent, clearing the slot so a follow-up chunk does not
	// double-emit. thoughtSignature is the part-level Gemini 3.x
	// reasoning blob, propagated so the loop can round-trip it back to
	// Vertex; empty for 2.x responses.
	emitToolCall := func(name string, fullArgs []byte, idForSeq int, thoughtSignature string) bool {
		var input map[string]any
		argsBytes := fullArgs
		if len(argsBytes) == 0 {
			argsBytes = []byte("{}")
		}
		if err := json.Unmarshal(argsBytes, &input); err != nil {
			emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse tool input JSON: %w", err)})
			return false
		}
		emitEvent(types.StreamEvent{
			Type:             "tool_call",
			ID:               fmt.Sprintf("gemini-%d-%d", streamN, idForSeq),
			Name:             name,
			Input:            input,
			ThoughtSignature: thoughtSignature,
		})
		delete(toolBufs, name)
		return true
	}

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			emitEvent(types.StreamEvent{Type: "error", Error: ctx.Err()})
			return
		default:
		}

		line := scanner.Text()
		if line == "" {
			continue
		}
		// Vertex emits `data:` only, but tolerate `event:` lines (the SSE
		// spec permits them) to keep us forward-compatible with future
		// named events.
		if strings.HasPrefix(line, "event: ") || strings.HasPrefix(line, "event:") {
			continue
		}
		// Single-pass CutPrefix so a payload that itself starts with
		// "data:" (e.g. a JSON string value) is not corrupted by a
		// double TrimPrefix. Per the SSE spec the optional space after
		// the colon is part of the framing, not the payload, so trim
		// only the leading space(s).
		rest, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data := strings.TrimLeft(rest, " ")
		if data == "" {
			continue
		}

		var chunk generateContentChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse generateContent chunk: %w", err)})
			return
		}

		// Decode the same chunk bytes a second time as a generic map so
		// the path walker can descend through fields the typed chunk
		// struct does not know about. Gated by len(q.ReplayFields) > 0,
		// so the extra json.Unmarshal is zero-overhead for non-Gemini-3
		// streams.
		if !replayFieldsCapped && len(q.ReplayFields) > 0 {
			if captured := quirks.CaptureFromJSON([]byte(data), q.ReplayFields); len(captured) > 0 {
				for k, v := range captured {
					add := 0
					for _, item := range v {
						add += replayCaptureByteLen(item)
					}
					if replayFieldsTotalBytes+add > maxReplayFieldBytes {
						replayFieldsCapped = true
						// One WARN per stream; the captured values
						// themselves are never logged.
						logger.WarnContext(ctx, "replay field accumulator cap reached, truncating",
							slog.String("provider.type", "gemini"),
							slog.String("provider.model", model),
							slog.String("path", k),
							slog.Int("limit_bytes", maxReplayFieldBytes),
						)
						break
					}
					replayFieldsTotalBytes += add
					replayFieldsCapture[k] = append(replayFieldsCapture[k], v...)
				}
			}
		}

		// Walk the first candidate's parts. Vertex always returns at most
		// one candidate per chunk in our request configuration (we never
		// ask for multiple), so additional candidates are ignored.
		if len(chunk.Candidates) > 0 {
			cand := chunk.Candidates[0]
			if cand.Content != nil {
				for _, part := range cand.Content.Parts {
					switch {
					case part.Text != "":
						// TODO(#194): Gemini 3.x can also attach a
						// thoughtSignature to text parts; threading it onto
						// the assembled text ContentBlock needs plumbing
						// through streamEventsToResult. Deferred — the
						// tool_use case is load-bearing for multi-turn
						// agentic loops, so a text-part signature is
						// silently dropped here for now.
						emitEvent(types.StreamEvent{
							Type: "text_delta",
							Text: part.Text,
						})
					case part.FunctionCall != nil:
						sawFunctionCall = true
						fc := part.FunctionCall
						// A blank Name on a continuation chunk is a
						// protocol violation; surface it rather than
						// silently bucketing the chunk into a "" slot.
						if fc.Name == "" {
							emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("functionCall part missing name")})
							return
						}
						buf, ok := toolBufs[fc.Name]
						if !ok {
							buf = &toolCallBuf{name: fc.Name}
							toolBufs[fc.Name] = buf
						}

						// Each chunk's args/partialArgs is a *cumulative*
						// snapshot that supersedes earlier ones rather than
						// concatenating onto them.
						snapshot := fc.Args
						if len(snapshot) == 0 {
							snapshot = fc.PartialArgs
						}
						if len(snapshot) > 0 {
							if len(snapshot) > maxToolInputSize {
								emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("tool input exceeds %d byte limit", maxToolInputSize)})
								return
							}
							buf.args = snapshot
						}

						// thoughtSignature is a property of the part, not
						// the call; retain the most recent non-empty value.
						if part.ThoughtSignature != "" {
							buf.thoughtSignature = part.ThoughtSignature
						}

						// With streamFunctionCallArguments=false (the
						// default) WillContinue is never set, so this is
						// the only path production tool calls take; the
						// willContinue=true accumulation above is kept
						// defensively in case the flag is re-enabled.
						if !fc.WillContinue {
							// Emit, then advance the ID-synthesis counter.
							if !emitToolCall(buf.name, buf.args, nextPartIdx, buf.thoughtSignature) {
								return
							}
							nextPartIdx++
						}
					}
				}
			}

			if cand.FinishReason != "" {
				// Drain any still-open tool-call buffers: a server that
				// sets finishReason without closing a willContinue buffer
				// would otherwise leave the call invisible to the loop.
				for _, buf := range toolBufs {
					if !emitToolCall(buf.name, buf.args, nextPartIdx, buf.thoughtSignature) {
						return
					}
					nextPartIdx++
				}

				stop := mapGeminiFinishReason(cand.FinishReason)
				// Promote STOP → tool_use since Vertex uses STOP for both.
				if stop == "end_turn" && sawFunctionCall {
					stop = "tool_use"
				}
				ev := types.StreamEvent{
					Type:       "message_complete",
					StopReason: stop,
				}
				if chunk.UsageMetadata != nil {
					ev.OutputTokens = chunk.UsageMetadata.CandidatesTokenCount
					if ev.OutputTokens == 0 && chunk.UsageMetadata.TotalTokenCount > 0 {
						// Some Vertex deployments only populate the total;
						// derive the candidate count from that minus the
						// prompt count when possible.
						derived := chunk.UsageMetadata.TotalTokenCount - chunk.UsageMetadata.PromptTokenCount
						if derived > 0 {
							ev.OutputTokens = derived
						}
					}
				}
				emitEvent(ev)
				return
			}
		}

		// Vertex blocks the *prompt itself* with promptFeedback +
		// no candidates when the system prompt or user input trips a
		// safety policy. Without this branch the loop receives an
		// empty stream that closes silently — the agent then surfaces
		// the resulting "no model output" as a generic stall rather
		// than the safety_blocked verdict the operator needs to see.
		if chunk.PromptFeedback != nil && chunk.PromptFeedback.BlockReason != "" {
			emitEvent(types.StreamEvent{
				Type:       "message_complete",
				StopReason: "safety_blocked",
			})
			return
		}
	}

	if err := scanner.Err(); err != nil {
		emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("read SSE stream: %w", err)})
	}
}
