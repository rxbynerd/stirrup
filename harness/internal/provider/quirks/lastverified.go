package quirks

import (
	"regexp"
	"strings"
	"time"
)

// EvidenceLabelSince is the first LastVerified date the evidence-label
// convention in docs/provider-quirks.md#lastverified applies to; earlier
// rules gain a label when they are next re-verified.
var EvidenceLabelSince = Date("2026-10-29")

var evidenceLabel = regexp.MustCompile(`\b(probed|documented|inferred)\b`)

// LastVerifiedProblems reports how rule breaks the LastVerified
// convention as of now: a date in the future, or a rule dated under the
// convention whose Description does not say whether it is probed,
// documented or inferred. "not probed" alone says none of the three. A
// date one day ahead of now passes, since Date is UTC midnight and a
// rule author's local date can be ahead of UTC.
func LastVerifiedProblems(rule Rule, now time.Time) []string {
	var problems []string
	date := rule.LastVerified.Format("2006-01-02")
	if rule.LastVerified.After(now.Add(24 * time.Hour)) {
		problems = append(problems, "LastVerified "+date+" is in the future")
	}
	labelled := evidenceLabel.MatchString(strings.ReplaceAll(rule.Description, "not probed", ""))
	if !rule.LastVerified.Before(EvidenceLabelSince) && !labelled {
		problems = append(problems, "LastVerified "+date+" needs a Description saying whether the rule is probed, documented or inferred")
	}
	return problems
}
