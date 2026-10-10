package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"go.opentelemetry.io/otel/attribute"

	"github.com/rxbynerd/stirrup/types"
)

const (
	// stallRepeats is how many times in a row a call pattern must occur
	// to count as a repetition. A pattern is a single call or a cycle of
	// up to maxStallCycleLength calls.
	stallRepeats = 3

	// maxStallCycleLength is the longest cycle of calls the detector
	// recognises.
	maxStallCycleLength = 4

	// stallWindowSize is the number of recent call signatures kept: enough
	// to hold the longest cycle repeated stallRepeats times.
	stallWindowSize = maxStallCycleLength * stallRepeats

	// maxConsecutiveFailures is the number of consecutive tool call failures
	// before the loop is terminated with a tool_failures outcome.
	maxConsecutiveFailures = 5
)

const stallNudgeText = "The last %d tool calls repeated the same pattern with the same results. Change approach, or if the task is complete, finish with a final answer."

// stallSignature identifies a call by its tool name, canonical input,
// success flag and output.
type stallSignature [sha256.Size]byte

const commandOutputRefPrefix = "stirrup://command-output/"

// commandOutputRefPattern matches a command-output store reference. The
// store derives each reference from the tool-use ID, so identical calls
// with identical output render different references.
var commandOutputRefPattern = regexp.MustCompile(regexp.QuoteMeta(commandOutputRefPrefix) + `[0-9a-f]+/`)

// stallDetector tracks repeated tool-call patterns and consecutive
// failures to detect when the agentic loop is making no progress. The
// first repetition yields a nudge for the model; a repetition after it
// ends the run.
type stallDetector struct {
	window           []stallSignature
	consecutiveFails int
	nudged           bool
	// pendingNudge holds nudge text the loop has not yet taken. While it
	// is set the window stays empty, because calls made before the model
	// sees the nudge cannot show whether it changed approach.
	pendingNudge string
}

// recordToolCall records a completed tool call. It returns a non-empty
// outcome when the run must end: "stalled" for a repetition after a
// nudge, "tool_failures" for too many consecutive failures. The first
// repetition returns no outcome and leaves a nudge for takeNudge.
func (s *stallDetector) recordToolCall(name string, input json.RawMessage, output string, success bool) string {
	if success {
		s.consecutiveFails = 0
	} else {
		s.consecutiveFails++
	}

	cycle := 0
	if s.pendingNudge == "" {
		if len(s.window) == stallWindowSize {
			copy(s.window, s.window[1:])
			s.window = s.window[:stallWindowSize-1]
		}
		s.window = append(s.window, stallSignatureOf(name, input, output, success))
		cycle = s.repeatedCycleLength()
	}

	switch {
	case cycle > 0 && s.nudged:
		return "stalled"
	case s.consecutiveFails >= maxConsecutiveFailures:
		return "tool_failures"
	case cycle > 0:
		s.nudged = true
		s.pendingNudge = fmt.Sprintf(stallNudgeText, cycle*stallRepeats)
		s.window = s.window[:0]
	}
	return ""
}

// takeNudge returns the nudge issued since it was last called, if any,
// and resumes repetition tracking.
func (s *stallDetector) takeNudge() (string, bool) {
	text := s.pendingNudge
	s.pendingNudge = ""
	return text, text != ""
}

// repeatedCycleLength returns the length of the shortest cycle that the
// most recent calls repeat stallRepeats times in a row, or 0 when there
// is none. Identical consecutive calls are a cycle of length 1.
func (s *stallDetector) repeatedCycleLength() int {
	for length := 1; length <= maxStallCycleLength; length++ {
		span := length * stallRepeats
		if len(s.window) < span {
			return 0
		}
		tail := s.window[len(s.window)-span:]
		periodic := true
		for k := length; k < span; k++ {
			if tail[k] != tail[k-length] {
				periodic = false
				break
			}
		}
		if periodic {
			return length
		}
	}
	return 0
}

func stallSignatureOf(name string, input json.RawMessage, output string, success bool) stallSignature {
	h := sha256.New()
	writeField := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	writeField([]byte(name))
	writeField(canonicalToolInput(input))
	writeField([]byte(stallComparableOutput(output)))
	if success {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	var sig stallSignature
	h.Sum(sig[:0])
	return sig
}

// stallComparableOutput removes the per-call part of command-output
// references so a repeated run_command whose output spilled to the
// archive still matches its earlier calls.
func stallComparableOutput(output string) string {
	if !strings.Contains(output, commandOutputRefPrefix) {
		return output
	}
	return commandOutputRefPattern.ReplaceAllLiteralString(output, commandOutputRefPrefix)
}

// canonicalToolInput re-encodes a JSON input with sorted object keys and
// numbers kept as written, so inputs that differ only in key order or
// whitespace compare equal. Input that is not a single JSON value is
// returned unchanged; it never equals a canonical form, which is always
// valid JSON.
func canonicalToolInput(input json.RawMessage) []byte {
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return input
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return input
	}
	out, err := json.Marshal(v)
	if err != nil {
		return input
	}
	return out
}

// nudgeStalledRun appends the stall nudge to the history and reports it
// through metrics, the log and the transport.
func (l *AgenticLoop) nudgeStalledRun(ctx context.Context, config *types.RunConfig, turn int, text string, messages []types.Message) []types.Message {
	messages = injectSyntheticUserText(messages, text)
	l.Metrics.StallNudges.Add(ctx, 1,
		l.metricAttrs(attribute.String("run.mode", config.Mode)),
	)
	l.Logger.Warn("repeated tool-call pattern; nudging the model", "turn", turn)
	_ = l.Transport.Emit(types.HarnessEvent{
		Type: "warning",
		Message: fmt.Sprintf(
			"turn %d completed a repeated tool-call pattern; told the model to change approach (a further repetition ends the run as stalled)",
			turn,
		),
	})
	return messages
}
