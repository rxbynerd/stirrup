package sandboxidentity

import (
	"log/slog"
	"time"
)

// WarnIfExpiresBeforeBudget warns when a token expires before the run's
// wall-clock budget. The sandbox env is frozen at creation, so an early
// expiry silently breaks late git operations (e.g. a hooks.postRun push).
// Advisory only: sizing exp to the budget is the control plane's job.
//
// Logs only the two Unix timestamps — never the token, which this call site
// never receives. Uses the package-level slog logger because it runs before
// the run's scrub-wrapped logger exists; two integers are scrub-safe.
func WarnIfExpiresBeforeBudget(expiresAt *int64, runBudgetSeconds int, now time.Time) {
	if expiresAt == nil || runBudgetSeconds <= 0 {
		return
	}
	budgetDeadline := now.Add(time.Duration(runBudgetSeconds) * time.Second).Unix()
	if *expiresAt < budgetDeadline {
		slog.Warn(
			"sandbox identity token expires before the run's configured wall-clock budget; git operations issued late in the run (e.g. a hooks.postRun push) may fail authentication",
			"tokenExpiresAtUnix", *expiresAt,
			"runBudgetDeadlineUnix", budgetDeadline,
		)
	}
}
