package core

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/rxbynerd/stirrup/types"
)

const (
	// windDownTurnsRemaining counts the turn about to start.
	windDownTurnsRemaining = 2
	windDownBudgetFraction = 0.9
)

// Trigger names recorded on TurnTrace.WindDown and the wind_downs metric.
const (
	windDownTriggerTurns  = "turns"
	windDownTriggerTokens = "tokens"
	windDownTriggerTime   = "time"
)

const (
	windDownText         = "Budget is nearly exhausted. Stop starting new work, bring the workspace to a consistent state, and finish with a summary of what is done and what remains."
	windDownReadOnlyText = "Budget is nearly exhausted. Summarise findings now; make no further tool calls unless essential."
)

// windDownState is the per-run wind-down bookkeeping. It is reset at run
// start, so verification re-entries share one instruction per run.
type windDownState struct {
	start time.Time
	sent  bool
}

func (l *AgenticLoop) now() time.Time {
	if l.clock != nil {
		return l.clock()
	}
	return time.Now()
}

// windDownTrigger names the first budget threshold crossed at the start
// of turn, or "" when none is. The turn threshold skips an inner loop's
// first turn, which has no work to wind down yet. The time threshold
// measures the share of the window from run start to the ctx deadline.
func (l *AgenticLoop) windDownTrigger(ctx context.Context, config *types.RunConfig, turn int, tokens types.TokenUsage) string {
	if turn > 0 && config.MaxTurns-turn <= windDownTurnsRemaining {
		return windDownTriggerTurns
	}
	if b := config.MaxTokenBudget; b != nil && *b > 0 &&
		float64(tokens.Total()) >= float64(*b)*windDownBudgetFraction {
		return windDownTriggerTokens
	}
	if deadline, ok := ctx.Deadline(); ok && !l.windDown.start.IsZero() {
		window := deadline.Sub(l.windDown.start)
		elapsed := l.now().Sub(l.windDown.start)
		if window > 0 && float64(elapsed) >= float64(window)*windDownBudgetFraction {
			return windDownTriggerTime
		}
	}
	return ""
}

// windDownIfDue appends the wind-down instruction when the run opted in,
// none has been sent this run, and a budget threshold is crossed. It
// returns the history and the trigger name, empty when nothing was sent.
func (l *AgenticLoop) windDownIfDue(ctx context.Context, config *types.RunConfig, turn int, tokens types.TokenUsage, messages []types.Message) ([]types.Message, string) {
	if !config.WindDownEnabled() || l.windDown.sent {
		return messages, ""
	}
	trigger := l.windDownTrigger(ctx, config, turn, tokens)
	if trigger == "" {
		return messages, ""
	}
	l.windDown.sent = true

	text := windDownText
	if types.IsReadOnlyMode(config.Mode) {
		text = windDownReadOnlyText
	}
	messages = injectSyntheticUserText(messages, text)

	l.Metrics.WindDowns.Add(ctx, 1, l.metricAttrs(
		attribute.String("run.mode", config.Mode),
		attribute.String("wind_down.trigger", trigger),
	))
	l.Logger.Warn("budget nearly exhausted; asking the model to wind down", "turn", turn, "trigger", trigger)
	_ = l.Transport.Emit(types.HarnessEvent{
		Type: "warning",
		Message: fmt.Sprintf(
			"turn %d: the %s budget is nearly exhausted; told the model to wind down and summarise",
			turn, trigger,
		),
	})
	return messages, trigger
}
