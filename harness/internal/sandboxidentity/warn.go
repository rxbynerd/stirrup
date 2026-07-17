package sandboxidentity

import (
	"log/slog"
	"time"
)

// WarnIfExpiresBeforeBudget emits a scrub-safe slog.Warn (issue #516 CF-2)
// when a token's reported expiry is earlier than the run's configured
// wall-clock budget. The env is frozen at sandbox creation, so a token that
// expires mid-run would fail authentication for any git operation issued
// after expiry (e.g. a hooks.postRun push); this is a heads-up for the
// operator/control-plane implementer, not a fail-closed condition — the v1
// posture (see docs/deployment.md) is that the control plane is expected to
// size exp to the run's budget, and this warning surfaces the case where it
// did not.
//
// Deliberately logs only the two Unix timestamps being compared, never the
// token itself — this call site must not receive the token value at all
// (see Exchange's Result, which separates Token from ExpiresAt).
//
// Uses the package-level slog logger (not a caller-supplied *slog.Logger)
// because this runs in BuildLoopWithTransport before the run's
// scrub-wrapped structured logger is constructed (factory step 3, ahead of
// step 5's observability.NewLoggerWithExport) — the same constraint
// types.warnGitProxyAllowlistGap already works under. The warning content
// itself is scrub-safe by construction (two Unix-second integers), so the
// absence of the scrub-wrapping is not a leak risk.
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
