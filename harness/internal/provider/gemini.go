package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
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

	// maxScannerBuffer caps the per-line buffer used for the SSE scanner.
	// Vertex chunks are typically small, but a final chunk that bundles
	// usage metadata with a long tool-call args blob can be sizeable.
	// 16 MiB matches the upper bound the wire protocol could plausibly
	// produce and keeps a single malformed line from OOMing the run.
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

	// baseURLOverride is set by tests to point the adapter at an
	// httptest.Server. Production runs leave it empty so the URL is
	// derived from projectID + location every call.
	baseURLOverride string

	// streamCounter is incremented per Stream call to namespace the
	// synthesised tool-call IDs across concurrent requests on the same
	// adapter instance. Vertex does not echo tool-call IDs through
	// functionResponse, so the harness fabricates IDs of the form
	// "gemini-{streamN}-{partIdx}".
	streamCounter atomic.Int64

	// Tracer and Metrics are field-injected by the factory after
	// construction. Both nil-safe at every call site.
	Tracer  oteltrace.Tracer
	Metrics *observability.Metrics
}

// NewGeminiAdapter creates an adapter for Vertex AI's
// :streamGenerateContent endpoint. The HTTP client mirrors the
// timeout shape used by the other adapters (120s overall, 10s TLS,
// 30s response-header, 90s idle).
//
// bearer is invoked on every Stream call to fetch a fresh Google OAuth2
// access token. The credential layer is responsible for caching and
// refreshing (see credential.bearerFromTokenSource, which wraps an
// oauth2.ReuseTokenSource); the adapter just forwards the resulting
// string into the Authorization header.
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
	}
}

// buildURL renders the endpoint URL for one Stream call. baseURLOverride
// short-circuits the host derivation when set (tests). For production
// runs, "global" routes to aiplatform.googleapis.com; every other
// location goes to the regional subdomain.
func (g *GeminiAdapter) buildURL(model string) string {
	// url.PathEscape every path component so a value containing slashes,
	// percent signs, or other reserved bytes cannot rewrite the URL
	// shape. projectID and location are validated by
	// gcpProjectIDPattern / gcpLocationPattern (no slashes possible),
	// but model arrives from RunConfig.ModelRouter.Model — its
	// validation lives in validateGeminiProviderFields. Even with that
	// validation in place we escape here too, so the URL builder is
	// defence-in-depth and never relies on a callers-validate-everything
	// invariant.
	projID := url.PathEscape(g.projectID)
	loc := url.PathEscape(g.location)
	mdl := url.PathEscape(model)

	if g.baseURLOverride != "" {
		// Tests inject a httptest server URL; we still substitute project
		// and model so the test can assert the path was built correctly.
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

	body, _, err := BuildGenerateContentRequest(params, g.safety)
	if err != nil {
		g.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("build request: %w", err)
	}
	// NOTE: BuildGenerateContentRequest returns a per-call id->name map for
	// outbound tool_result correlation. The marshaller already populates
	// functionResponse.name for the same call's outbound history, so the
	// adapter does not need it. Future work that surfaces tool-call IDs
	// across turns (e.g. issue #93 follow-up) would consume this map here.

	// Acquire a fresh Google access token via the bearer closure. The
	// credential layer wraps oauth2.ReuseTokenSource so a cached token is
	// returned with no IO when still valid; only on expiry does the
	// closure round-trip to the OAuth2 endpoint.
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

	resp, err := g.httpClient.Do(req)
	if err != nil {
		g.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("execute request: %w", err)
	}

	// Annotate the provider.stream span (created by the loop) with the
	// HTTP status. The 429 add-event mirrors the Anthropic adapter so
	// rate-limit retries surface uniformly across providers.
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
			// Scrub the error body at the point of construction.
			// LogScrubber already runs downstream at the agentic loop's
			// error boundary, but defence-in-depth: any future
			// instrumentation between here and that boundary (test
			// logging, retry wrapper, telemetry hook) would otherwise
			// surface unscrubbed Vertex diagnostic strings (project IDs,
			// quota identifiers, internal trace IDs).
			safeBody := security.Scrub(string(bodyBytes))
			return nil, fmt.Errorf("vertex AI returned status %d: %s", resp.StatusCode, safeBody)
		}
		return nil, fmt.Errorf("vertex AI returned status %d", resp.StatusCode)
	}

	streamN := g.streamCounter.Add(1)

	ch := make(chan types.StreamEvent, 64)
	go func() {
		g.consumeSSE(ctx, resp, ch, start, metricAttrs, streamN)
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
// canonical stop-reason vocabulary the agentic loop's stop-reason switch
// understands. STOP becomes "end_turn"; the caller is responsible for
// remapping STOP → "tool_use" when functionCall parts were observed in
// the same stream (Vertex sets STOP for both end-of-turn and
// tool-dispatch turns). MAX_TOKENS maps to the loop's existing keyword.
// SAFETY/RECITATION get explicit suffixes ("safety_blocked" /
// "recitation_blocked") because they are operationally meaningful — an
// operator looking at a run's outcome should be able to distinguish a
// safety block from a generic refusal. Everything else passes through
// lowercased so unknown future enums surface in the trace verbatim.
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

// toolCallBuf tracks the in-flight functionCall at one part-index slot.
// Vertex emits intermediate willContinue=true chunks carrying partialArgs
// snapshots (each a complete JSON object representing the cumulative
// argument state up to that point) and a finalising chunk with
// willContinue=false carrying the full args (or, on a single-shot call,
// just args directly). The adapter retains the most recently observed
// non-empty argument blob and emits the tool_call when willContinue=false
// (or when the stream terminates on a finishReason without a closing
// chunk, in which case we treat the latest snapshot as final).
//
// The retained blob is bounded at maxToolInputSize (10 MB) to mirror the
// Anthropic adapter's safety cap.
type toolCallBuf struct {
	name string
	args []byte
}

// consumeSSE reads SSE events from the response body and emits StreamEvents
// to the channel. It closes the channel and the response body when done.
//
// The Vertex wire format used here:
//
//   - Each non-empty `data: <json>` line is a generateContentChunk.
//   - Candidates[*].Content.Parts hold either Text (text_delta) or
//     FunctionCall (tool-call accumulation). Other part types are ignored.
//   - FunctionCall parts arrive in two patterns:
//     1) Streamed: one or more chunks with WillContinue=true and
//     PartialArgs, then a final chunk with WillContinue=false and either
//     Args or PartialArgs populated.
//     2) Single: one chunk with neither WillContinue nor partial markers,
//     just Args.
//   - finishReason on Candidates[0] terminates the turn — emit
//     message_complete and exit.
//   - usageMetadata, when present on the terminal chunk, supplies
//     CandidatesTokenCount as the OutputTokens for the stop event.
//
// streamN is the per-stream counter used to namespace synthesised tool-call
// IDs ("gemini-{streamN}-{partIdx}"). Vertex never echoes tool-call IDs
// through functionResponse, so we generate IDs that only need to be unique
// within this Stream call and the bookkeeping the marshaller does for
// outbound tool_result correlation.
func (g *GeminiAdapter) consumeSSE(
	ctx context.Context,
	resp *http.Response,
	ch chan<- types.StreamEvent,
	streamStart time.Time,
	metricAttrs metric.MeasurementOption,
	streamN int64,
) {
	defer close(ch)
	defer func() { _ = resp.Body.Close() }()

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

	// Tool-call accumulation slots, keyed by function name. Vertex
	// includes the function name on every chunk of a streamed call, so
	// keying by name lets two simultaneous calls with willContinue=true
	// (interleaved chunks) accumulate into separate slots without
	// colliding. A monotonic part-index counter would not work here:
	// before either call closes both arrive at index 0 and the second
	// call's partial args overwrite the first's.
	//
	// nextPartIdx is preserved purely for tool-call ID synthesis — it
	// gives each emitted call a unique sequence number within this
	// stream. It is not used to address slots in toolBufs.
	toolBufs := make(map[string]*toolCallBuf)
	nextPartIdx := 0
	// sawFunctionCall flips true the first time a functionCall part is
	// observed in this stream. The loop's stop-reason switch dispatches
	// tools only when StopReason == "tool_use"; Vertex however reports
	// finishReason=STOP even on tool-call turns. Remapping STOP → tool_use
	// when at least one functionCall part was emitted bridges the two
	// vocabularies.
	sawFunctionCall := false

	// emitToolCall finalises the buffer at the named slot (if present)
	// and emits a tool_call StreamEvent. The slot is cleared so a
	// follow-up chunk with the same name does not double-emit. The ID
	// is synthesised from streamN and the monotonic nextPartIdx counter
	// (incremented by the caller after a successful emit).
	emitToolCall := func(name string, fullArgs []byte, idForSeq int) bool {
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
			Type:  "tool_call",
			ID:    fmt.Sprintf("gemini-%d-%d", streamN, idForSeq),
			Name:  name,
			Input: input,
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

		// Walk the first candidate's parts. Vertex always returns at most
		// one candidate per chunk in our request configuration (we never
		// ask for multiple), so additional candidates are ignored.
		if len(chunk.Candidates) > 0 {
			cand := chunk.Candidates[0]
			if cand.Content != nil {
				for _, part := range cand.Content.Parts {
					switch {
					case part.Text != "":
						emitEvent(types.StreamEvent{
							Type: "text_delta",
							Text: part.Text,
						})
					case part.FunctionCall != nil:
						sawFunctionCall = true
						fc := part.FunctionCall
						// Slot key is the function name. Vertex sends the
						// name on every chunk of a streamed call, so two
						// simultaneous calls (e.g. tool_a / tool_b each
						// with willContinue=true) accumulate cleanly into
						// distinct slots — keying by a monotonic counter
						// would collide them at index 0 until the first
						// closes.
						//
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

						// Each chunk's partialArgs (or args, on the final
						// chunk) is a *cumulative* JSON object snapshot:
						// later snapshots supersede earlier ones rather
						// than concatenating onto them. Retain the most
						// recent non-empty payload; the size cap guards
						// against an oversized blob in any single chunk.
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

						if !fc.WillContinue {
							// Final chunk for this call: emit, then
							// advance the sequence counter used for ID
							// synthesis. nextPartIdx is no longer the
							// slot key — emitting deletes the named
							// slot directly.
							if !emitToolCall(buf.name, buf.args, nextPartIdx) {
								return
							}
							nextPartIdx++
						}
					}
				}
			}

			if cand.FinishReason != "" {
				// Drain any still-open tool-call buffers as a defensive
				// measure: a server that sets finishReason without first
				// closing a willContinue buffer would otherwise leave
				// the call invisible to the loop. Iteration order over
				// a Go map is non-deterministic, but each buffer here
				// represents a distinct tool name — order between
				// drained calls is not load-bearing.
				for _, buf := range toolBufs {
					if !emitToolCall(buf.name, buf.args, nextPartIdx) {
						return
					}
					nextPartIdx++
				}

				stop := mapGeminiFinishReason(cand.FinishReason)
				// Vertex returns finishReason=STOP for both plain
				// end-of-turn responses and tool-call turns. The agentic
				// loop dispatches tools only when StopReason=="tool_use",
				// so we promote STOP → tool_use whenever functionCall
				// parts were emitted during the stream.
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
