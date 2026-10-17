package core

import "github.com/rxbynerd/stirrup/types"

// streamsThinking reports whether the run forwards reasoning text to the
// transport as thinking_delta events. It gates only the transport: thinking
// blocks are persisted in history either way.
func streamsThinking(config *types.RunConfig) bool {
	return config.ReasoningSummary != types.ReasoningSummaryOff
}
