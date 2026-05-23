package provider

import (
	"context"
	"fmt"
	"slices"

	"github.com/rxbynerd/stirrup/harness/internal/tool/toolname"
	"github.com/rxbynerd/stirrup/types"
)

// NormalizingAdapter wraps a ProviderAdapter and applies per-request
// tool-name normalization between the agentic loop and the wrapped
// adapter's wire serialisation. It is the single source of truth for
// "the provider sees a name the provider accepts" — adapters
// themselves remain naïve and serialise whatever string is on
// types.ToolDefinition / types.ContentBlock.
//
// Lifecycle of a single Stream call:
//
//  1. Build a toolname.Mapping from the names in params.Tools using
//     the policy for the wrapped provider type. Two distinct internal
//     names that normalise to the same external name fail the call
//     before any wire request is issued — silently aliasing would
//     route a tool call to the wrong handler on the inbound path.
//  2. Rewrite params.Tools[*].Name and the Name on every tool_use
//     ContentBlock in params.Messages to the external (normalised)
//     form so the adapter's allowlist wire types receive a string
//     that matches what was declared.
//  3. Forward the rewritten params to the inner adapter.
//  4. Intercept the streamed events: on tool_call, translate
//     event.Name back to the internal name so the loop's
//     ToolRegistry.Resolve continues to work unchanged. Every other
//     event is forwarded verbatim.
//
// Wrapping is applied at factory build time and sits on the outside
// of any BatchAdapter wrap so the loop's batch-mode detection still
// observes batch state through the wrapper's LastBatchID
// pass-through.
type NormalizingAdapter struct {
	inner        ProviderAdapter
	providerType string
}

// NewNormalizingAdapter constructs a NormalizingAdapter that applies
// the policy registered for providerType to every request. A
// providerType the toolname package does not recognise still gets a
// safe (strictest) policy from toolname.PolicyFor — the wrapper never
// no-ops.
func NewNormalizingAdapter(inner ProviderAdapter, providerType string) *NormalizingAdapter {
	return &NormalizingAdapter{inner: inner, providerType: providerType}
}

// Compile-time assertion that NormalizingAdapter satisfies the
// ProviderAdapter contract. Kept in this form (rather than `var _
// ProviderAdapter = &NormalizingAdapter{}`) so the linter's
// suggestion to use the concrete value does not subtly weaken the
// guard — see CLAUDE.md "Known false positives".
var _ ProviderAdapter = (*NormalizingAdapter)(nil)

// Stream rewrites params, forwards to the inner adapter, and
// reverse-rewrites tool_call events on the inbound side.
func (a *NormalizingAdapter) Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	mapping, err := a.buildMapping(params.Tools)
	if err != nil {
		return nil, fmt.Errorf("tool name normalization: %w", err)
	}

	rewritten := a.rewriteParams(params, mapping)
	innerCh, err := a.inner.Stream(ctx, rewritten)
	if err != nil {
		return nil, err
	}

	// Fan the inner stream through a translator goroutine. Buffer of 1
	// keeps the translator from blocking the inner producer past one
	// event when the loop's consumer pauses; matches the rate-control
	// pattern in streamEventsToResult.
	out := make(chan types.StreamEvent, 1)
	go func() {
		defer close(out)
		for ev := range innerCh {
			if ev.Type == "tool_call" {
				ev.Name = mapping.Reverse(ev.Name)
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				// Drain the inner channel so the inner adapter's
				// goroutine can exit cleanly; otherwise an unbuffered
				// send on the producer side would leak the goroutine
				// when the consumer abandoned the stream.
				for range innerCh {
				}
				return
			}
		}
	}()
	return out, nil
}

// Unwrap returns the inner ProviderAdapter the normalizer wraps. It
// is intended for tests that need to assert on the concrete adapter
// type (e.g. that batch wrapping is present) without coupling them to
// the wrapper's existence. Production code should not unwrap — the
// normalizer is the single source of truth for the on-wire name
// invariant and bypassing it would be a regression.
func (a *NormalizingAdapter) Unwrap() ProviderAdapter {
	return a.inner
}

// LastBatchID exposes the wrapped adapter's batch identifier when the
// inner adapter implements batchModeAdapter. The loop type-asserts
// against this method to populate TurnTrace.BatchID; without the
// pass-through, wrapping a BatchAdapter would silently downgrade every
// batch turn's trace to TurnModeStreaming.
//
// The interface is duck-typed (defined in core/loop.go) so we do not
// import it here; the loop's type assertion succeeds via Go's
// structural interface satisfaction on this single-method shape.
func (a *NormalizingAdapter) LastBatchID() string {
	type lastBatchID interface {
		LastBatchID() string
	}
	if b, ok := a.inner.(lastBatchID); ok {
		return b.LastBatchID()
	}
	return ""
}

// buildMapping centralises the policy lookup so tests can construct a
// NormalizingAdapter against a fake inner adapter and exercise the
// same translation logic the production wiring uses.
func (a *NormalizingAdapter) buildMapping(tools []types.ToolDefinition) (*toolname.Mapping, error) {
	if len(tools) == 0 {
		return &toolname.Mapping{
			ExternalFor: map[string]string{},
			InternalFor: map[string]string{},
		}, nil
	}
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return toolname.Build(names, toolname.PolicyFor(a.providerType))
}

// rewriteParams returns a copy of params with tool names normalised on
// both the tool definitions and any tool_use ContentBlocks carried in
// the message history. The Messages and Tools slices are cloned so
// mutating the wrapper's view cannot race with a caller that retains
// the original params (notably the batch adapter, which marshals
// params on a background goroutine).
func (a *NormalizingAdapter) rewriteParams(params types.StreamParams, mapping *toolname.Mapping) types.StreamParams {
	out := params
	out.Tools = make([]types.ToolDefinition, len(params.Tools))
	for i, t := range params.Tools {
		t.Name = mapping.Translate(t.Name)
		out.Tools[i] = t
	}
	out.Messages = cloneMessagesWithTranslatedToolNames(params.Messages, mapping)
	return out
}

// cloneMessagesWithTranslatedToolNames returns a copy of messages with
// every tool_use ContentBlock's Name translated through the mapping.
// Other block types pass through unchanged; the slice copy makes the
// rewrite safe against caller-side aliasing.
func cloneMessagesWithTranslatedToolNames(messages []types.Message, mapping *toolname.Mapping) []types.Message {
	if len(messages) == 0 {
		return messages
	}
	out := make([]types.Message, len(messages))
	for i, m := range messages {
		out[i] = m
		if len(m.Content) == 0 {
			continue
		}
		blocks := slices.Clone(m.Content)
		for j, b := range blocks {
			if b.Type == "tool_use" {
				blocks[j].Name = mapping.Translate(b.Name)
			}
		}
		out[i].Content = blocks
	}
	return out
}
