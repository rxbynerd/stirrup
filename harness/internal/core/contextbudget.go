package core

import (
	"fmt"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/types"
)

const (
	// calibrationMinEstimate is the smallest request estimate, in tokens,
	// whose reported input updates the calibration. Below it the provider's
	// fixed per-request overhead dominates the ratio.
	calibrationMinEstimate = 1_000

	// calibrationMinRatio and calibrationMaxRatio bound the provider tokens
	// counted per estimated token.
	calibrationMinRatio = 0.5
	calibrationMaxRatio = 2.0

	// calibrationSmoothing is the weight of the newest observation in the
	// running ratio.
	calibrationSmoothing = 0.5
)

// tokenCalibration is a run's measured ratio of provider-reported input
// tokens to the loop's estimate for the same request. The zero value
// applies no correction.
type tokenCalibration struct {
	ratio float64
}

// observe folds one request's estimate and the provider's reported input
// for it into the ratio. The first observation replaces the neutral prior;
// later ones are smoothed so one unusual turn does not swing the budget.
func (c *tokenCalibration) observe(estimated, reported int) {
	if estimated < calibrationMinEstimate || reported <= 0 {
		return
	}
	observed := min(max(float64(reported)/float64(estimated), calibrationMinRatio), calibrationMaxRatio)
	if c.ratio == 0 {
		c.ratio = observed
		return
	}
	c.ratio = c.ratio*(1-calibrationSmoothing) + observed*calibrationSmoothing
}

// factor returns the provider tokens counted per estimated token.
func (c tokenCalibration) factor() float64 {
	if c.ratio == 0 {
		return 1
	}
	return c.ratio
}

// contextWindow is the context window a turn is budgeted against.
type contextWindow struct {
	tokens int
	// source names the window in operator-facing messages.
	source string
	// remedy is the configuration change that raises the window.
	remedy string
	// cappedFrom is a contextStrategy.maxTokens larger than the registered
	// window that replaced it, else 0.
	cappedFrom int
}

// effectiveContextWindow returns the smaller of an explicit and a
// registered context window, ignoring either when it is zero; zero when
// neither is set.
func effectiveContextWindow(explicit, registered int) int {
	switch {
	case explicit > 0 && registered > 0:
		return min(explicit, registered)
	case explicit > 0:
		return explicit
	default:
		return max(registered, 0)
	}
}

// resolveContextWindow returns the context window for a turn on the
// selected provider and model; an empty provider name is the default
// provider.
func (l *AgenticLoop) resolveContextWindow(config *types.RunConfig, providerName, model string) contextWindow {
	explicit := config.ContextStrategy.MaxTokens
	registered := 0
	if l.ContextWindows != nil {
		providerType := config.Provider.Type
		if providerName != "" {
			providerType = providerTypeFor(config, providerName)
		}
		registered = l.ContextWindows.Resolve(providerType, model).ContextWindow
	}
	switch effectiveContextWindow(explicit, registered) {
	case 0:
		return contextWindow{
			tokens: defaultMaxContextTokens,
			source: fmt.Sprintf("the default %d-token context window", defaultMaxContextTokens),
			remedy: "set contextStrategy.maxTokens to the model's context window",
		}
	case explicit:
		return contextWindow{
			tokens: explicit,
			source: fmt.Sprintf("contextStrategy.maxTokens=%d", explicit),
			remedy: "raise contextStrategy.maxTokens",
		}
	default:
		w := contextWindow{
			tokens: registered,
			source: fmt.Sprintf("the %d-token context window registered for %s", registered, model),
			remedy: "use a model with a larger context window",
		}
		if explicit > registered {
			w.cappedFrom = explicit
		}
		return w
	}
}

// turnBudget is the context budget for one provider request.
type turnBudget struct {
	window contextWindow
	ratio  float64
	// prepare is the budget handed to the context strategy and the fit
	// check. Its room after the response reserve is in estimated tokens:
	// the window's room divided by ratio.
	prepare contextpkg.TokenBudget
	// structuredResults reports whether the provider the request is
	// routed to sends tool results' structured envelope, which the
	// request's estimate counts.
	structuredResults bool
}

// newTurnBudget builds the budget for a request whose full history,
// system prompt and tool definitions are estimated at currentTokens.
func newTurnBudget(window contextWindow, reserve int, calibration tokenCalibration, currentTokens int) turnBudget {
	ratio := calibration.factor()
	room := window.tokens - reserve
	if room > 0 {
		room = int(float64(room) / ratio)
	}
	return turnBudget{
		window: window,
		ratio:  ratio,
		prepare: contextpkg.TokenBudget{
			MaxTokens:          reserve + room,
			CurrentTokens:      currentTokens,
			ReserveForResponse: reserve,
		},
	}
}

// describeLimit names the window, reserve and calibration behind the fit
// limit for an operator-facing message.
func (b turnBudget) describeLimit() string {
	s := fmt.Sprintf("%s allows %d after the %d-token response reserve and a safety margin",
		b.window.source, max(contextpkg.FitLimit(b.prepare), 0), b.prepare.ReserveForResponse)
	if b.ratio != 1 {
		s += fmt.Sprintf(", at a measured %.2f provider tokens per estimated token", b.ratio)
	}
	return s
}
