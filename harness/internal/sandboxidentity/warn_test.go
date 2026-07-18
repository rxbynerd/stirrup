package sandboxidentity

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestWarnIfExpiresBeforeBudget is the CF-2 carry-forward test (issue
// #516): the harness warns (scrub-safe) when the token's reported expiry
// is earlier than the run's configured wall-clock budget.
func TestWarnIfExpiresBeforeBudget(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		expiresAt   *int64
		budgetSecs  int
		wantWarning bool
	}{
		{
			name:        "expires before budget warns",
			expiresAt:   int64Ptr(now.Add(5 * time.Minute).Unix()),
			budgetSecs:  3600,
			wantWarning: true,
		},
		{
			name:        "expires after budget is silent",
			expiresAt:   int64Ptr(now.Add(2 * time.Hour).Unix()),
			budgetSecs:  3600,
			wantWarning: false,
		},
		{
			name:        "nil expiry is silent",
			expiresAt:   nil,
			budgetSecs:  3600,
			wantWarning: false,
		},
		{
			name:        "zero budget is silent (nothing to compare against)",
			expiresAt:   int64Ptr(now.Unix()),
			budgetSecs:  0,
			wantWarning: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			prevDefault := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			defer slog.SetDefault(prevDefault)

			WarnIfExpiresBeforeBudget(tc.expiresAt, tc.budgetSecs, now)

			gotWarning := strings.Contains(buf.String(), "expires before the run's configured wall-clock budget")
			if gotWarning != tc.wantWarning {
				t.Errorf("warning emitted = %v, want %v (log output: %q)", gotWarning, tc.wantWarning, buf.String())
			}
		})
	}
}
