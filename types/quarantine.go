package types

// ClassifyForQuarantine inspects recordings the eval miner is about to
// bake into a suite and returns the QuarantineFlags the resulting
// QuarantinedSuite should carry. Pure; does not mutate recordings.
//
// Today only QuarantineLargePayload is computed locally.
// QuarantineUnscrubbedSecretEvent and QuarantinePIIClassification are
// reserved for future control-plane scoring and never fire from this
// path.
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
