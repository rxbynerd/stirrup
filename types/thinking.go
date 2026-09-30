package types

// IsThinkingBlock reports whether b carries model reasoning ("thinking" or
// "redacted_thinking") rather than conversation content.
func IsThinkingBlock(b ContentBlock) bool {
	return b.Type == "thinking" || b.Type == "redacted_thinking"
}

// StripThinkingBlocks returns messages with every thinking and
// redacted_thinking block removed. A provider may bind a thinking block to
// the exact history before it, so once any earlier message has been
// rewritten no block can be replayed safely; stripping all of them keeps
// the request valid.
//
// The input is never mutated. Messages without thinking blocks are shared
// with the input; the rest get a fresh Content slice. A message left with
// no content keeps its position so role alternation is unchanged.
func StripThinkingBlocks(messages []Message) []Message {
	var out []Message
	for i, msg := range messages {
		if !containsThinking(msg.Content) {
			if out != nil {
				out = append(out, msg)
			}
			continue
		}
		if out == nil {
			out = make([]Message, i, len(messages))
			copy(out, messages[:i])
		}
		kept := make([]ContentBlock, 0, len(msg.Content))
		for _, b := range msg.Content {
			if !IsThinkingBlock(b) {
				kept = append(kept, b)
			}
		}
		msg.Content = kept
		out = append(out, msg)
	}
	if out == nil {
		return messages
	}
	return out
}

func containsThinking(blocks []ContentBlock) bool {
	for _, b := range blocks {
		if IsThinkingBlock(b) {
			return true
		}
	}
	return false
}
