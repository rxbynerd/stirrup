package types

// ClassifyForQuarantine inspects a slice of recordings the eval miner
// is about to bake into a suite and returns the QuarantineFlags that
// the resulting QuarantinedSuite should carry. The function is pure:
// the input recordings are not mutated.
//
// Flag semantics (see EvalSuite.QuarantineFlags godoc for the
// per-flag contract):
//
//   - QuarantineUnscrubbedSecretEvent: V0.1 has no upstream signal
//     for "a SecretRedactedInOutput fired during this run" — the
//     recording itself does not carry that bit. The flag is reserved
//     for future control-plane scoring; today it never fires from
//     the local-OSS path. The local path's defence-in-depth lives
//     in security.Scrub at trace write time (#270), so a
//     post-scrub recording does not contain raw secret-shaped
//     substrings; the QuarantineUnscrubbedSecretEvent flag will
//     light up once the control plane attaches its own scrub-event
//     metadata to recordings.
//
//   - QuarantineLargePayload: fires if any recording carries a
//     turn whose total content (model output + tool I/O) exceeds
//     DefaultLargePayloadBytes, or a tool call whose output exceeds
//     the same limit individually. The threshold is conservative —
//     enough room for ordinary conversation turns, tight enough to
//     catch attached config dumps and log captures.
//
//   - QuarantinePIIClassification: V0.1 reserves the flag without a
//     local heuristic. The harness has no PII classifier today, and
//     a regex-based stand-in would either over-block (every
//     conversation mentions names) or under-block (semantic PII is
//     not regex-shaped). Future control-plane scoring populates
//     this from upstream metadata.
//
// The returned slice is freshly allocated; the order matches the
// constant declaration order in eval.go so a textual diff between
// two miner runs over comparable data is stable.
func ClassifyForQuarantine(recordings []RunRecording) []QuarantineFlag {
	var flags []QuarantineFlag
	if hasLargePayload(recordings, DefaultLargePayloadBytes) {
		flags = append(flags, QuarantineLargePayload)
	}
	return flags
}

// hasLargePayload reports whether any recording carries a turn
// (sum of modelOutput + tool I/O sizes) or a tool call (output size
// alone) exceeding limit bytes.
func hasLargePayload(recordings []RunRecording, limit int) bool {
	for _, rec := range recordings {
		// Archive stream sizes are evaluated independently from transcript
		// payload sizes: spilled sandbox output is intentionally absent from
		// ToolCallRecord.Output, but remains relevant to quarantine policy.
		for _, command := range rec.CommandOutputs {
			if command.Stdout.ScrubbedBytes > int64(limit) || command.Stderr.ScrubbedBytes > int64(limit) {
				return true
			}
		}
		for _, turn := range rec.Turns {
			turnSize := 0
			for _, blk := range turn.ModelOutput {
				turnSize += len(blk.Text) + len(blk.Input)
			}
			for _, tc := range turn.ToolCalls {
				turnSize += len(tc.Input) + len(tc.Output)
				if len(tc.Output) > limit {
					return true
				}
			}
			if turnSize > limit {
				return true
			}
		}
	}
	return false
}
