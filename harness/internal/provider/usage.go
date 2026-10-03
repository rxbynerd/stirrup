package provider

import (
	"math"

	"github.com/rxbynerd/stirrup/types"
)

// maxReportedTokens bounds a single provider-reported count so sums
// across fields and turns cannot overflow and every count fits the
// int32 fields of the gRPC RunTrace.
const maxReportedTokens = math.MaxInt32

// clampTokens maps an untrusted provider-reported count into
// [0, maxReportedTokens].
func clampTokens(n int) int {
	switch {
	case n < 0:
		return 0
	case n > maxReportedTokens:
		return maxReportedTokens
	}
	return n
}

// tokenReport is one provider usage report already projected onto the
// harness semantics: Input is the whole prompt and Reasoning is part of
// Output.
type tokenReport struct {
	Input, Output, CacheRead, CacheWrite, Reasoning int
}

// setEventUsage copies r onto a message_complete event. Each count is
// clamped, and the subsets are capped at their totals so CacheRead +
// CacheWrite <= Input and Reasoning <= Output hold whatever the provider
// sent.
func setEventUsage(ev *types.StreamEvent, r tokenReport) {
	input := clampTokens(r.Input)
	output := clampTokens(r.Output)
	cacheRead := min(clampTokens(r.CacheRead), input)
	ev.InputTokens = input
	ev.OutputTokens = output
	ev.CacheReadTokens = cacheRead
	ev.CacheWriteTokens = min(clampTokens(r.CacheWrite), input-cacheRead)
	ev.ReasoningTokens = min(clampTokens(r.Reasoning), output)
}
