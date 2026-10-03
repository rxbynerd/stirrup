package calibrate

import (
	"fmt"
	"io"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/rxbynerd/stirrup/types"
)

// ReportSchemaVersion identifies the JSON form of Report.
const ReportSchemaVersion = 1

// Report is a calibration's result.
type Report struct {
	SchemaVersion int           `json:"schemaVersion"`
	Golden        string        `json:"golden"`
	Set           string        `json:"set"`
	Judge         JudgeIdentity `json:"judge"`
	Cases         int           `json:"cases"`
	Repeats       int           `json:"repeats"`
	Metrics       Metrics       `json:"metrics"`

	// UnstableCases are the cases whose decided verdicts differ across
	// repeats.
	UnstableCases []string   `json:"unstableCases,omitempty"`
	Judgments     []Judgment `json:"judgments"`
}

// JudgeIdentity names the judge a report measured. It carries no
// credential reference.
type JudgeIdentity struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	BaseURL    string `json:"baseUrl,omitempty"`
	ConfigHash string `json:"configHash,omitempty"`
}

// NewReport assembles the report of judgments made on cases cases of the
// set named setName, loaded from goldenPath, with cfg judged repeats times.
func NewReport(goldenPath, setName string, cases int, cfg types.JudgeLLMConfig, repeats int, judgments []Judgment, prices *Prices) Report {
	id := JudgeIdentity{Provider: cfg.EffectiveProvider(), Model: cfg.Model, BaseURL: displayURL(cfg.BaseURL)}
	for _, j := range judgments {
		if j.Record != nil && j.Record.ConfigHash != "" {
			id.ConfigHash = j.Record.ConfigHash
			break
		}
	}
	return Report{
		SchemaVersion: ReportSchemaVersion,
		Golden:        goldenPath,
		Set:           setName,
		Judge:         id,
		Cases:         cases,
		Repeats:       max(repeats, 1),
		Metrics:       Summarize(judgments, prices),
		UnstableCases: unstableCases(judgments),
		Judgments:     judgments,
	}
}

// displayURL drops any userinfo, query and fragment from a base URL, so a
// gateway token carried in one never reaches a report.
func displayURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable)"
	}
	u.User, u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = nil, "", false, "", ""
	return u.String()
}

// WriteText renders r for a terminal.
func WriteText(w io.Writer, r Report) error {
	var b strings.Builder
	m := r.Metrics
	name := r.Set
	if name == "" {
		name = r.Golden
	}
	fmt.Fprintf(&b, "Judge calibration: %s (%s x %s = %s)\n", name, plural(r.Cases, "case"), plural(r.Repeats, "repeat"), plural(m.Judgments, "judgment"))
	fmt.Fprintf(&b, "Judge: %s %s", r.Judge.Provider, r.Judge.Model)
	if r.Judge.BaseURL != "" {
		fmt.Fprintf(&b, " at %s", r.Judge.BaseURL)
	}
	if r.Judge.ConfigHash != "" {
		fmt.Fprintf(&b, " (config %s)", shortHash(r.Judge.ConfigHash))
	}
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "Decided %d of %s; %s\n\n", m.Decided, plural(m.Judgments, "judgment"), plural(m.Errors, "error"))

	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  metric\tvalue\t95% CI\tn")
	rateRow(tw, "TPR (pass judged pass)", m.TPR)
	rateRow(tw, "TNR (fail judged fail)", m.TNR)
	rateRow(tw, "Accuracy", m.Accuracy)
	if m.Kappa != nil {
		_, _ = fmt.Fprintf(tw, "  Cohen's kappa\t%.3f\t\t%d\n", *m.Kappa, m.Decided)
	} else {
		_, _ = fmt.Fprintf(tw, "  Cohen's kappa\tundefined\t\t%d\n", m.Decided)
	}
	rateRow(tw, "Adversarial flip rate", m.AdversarialFlipRate)
	rateRow(tw, "Error rate", m.ErrorRate)
	_ = tw.Flush()

	c := m.Confusion
	fmt.Fprintf(&b, "\nConfusion (label/verdict): pass/pass %d, pass/fail %d, fail/fail %d, fail/pass %d\n",
		c.TruePositive, c.FalseNegative, c.TrueNegative, c.FalsePositive)
	if m.AdversarialErrors > 0 {
		fmt.Fprintf(&b, "Adversarial judgments that errored (not in the flip rate): %d\n", m.AdversarialErrors)
	}
	if m.Latency != nil {
		fmt.Fprintf(&b, "Latency: mean %.0f ms, p95 %d ms over %s\n", m.Latency.MeanMs, m.Latency.P95Ms, plural(m.Latency.Calls, "model call"))
	} else {
		b.WriteString("Latency: no live model calls\n")
	}
	fmt.Fprintf(&b, "Tokens: %d input, %d output", m.Tokens.Input, m.Tokens.Output)
	if m.CacheHits > 0 {
		fmt.Fprintf(&b, " (including %s served from the cache)", plural(m.CacheHits, "judgment"))
	}
	b.WriteString("\n")
	if m.Cost != nil {
		fmt.Fprintf(&b, "Estimated cost: $%.4f (input $%.4f + output $%.4f at $%g/$%g per million tokens)\n",
			m.Cost.TotalUSD, m.Cost.InputUSD, m.Cost.OutputUSD, m.Cost.InputPerMTok, m.Cost.OutputPerMTok)
	}
	if len(r.UnstableCases) > 0 {
		fmt.Fprintf(&b, "Unstable across repeats: %s\n", strings.Join(r.UnstableCases, ", "))
	}

	var wrong, errored []string
	for _, j := range r.Judgments {
		id := j.Case
		if r.Repeats > 1 {
			id = fmt.Sprintf("%s #%d", j.Case, j.Sample)
		}
		switch {
		case j.Verdict == types.JudgeStatusError:
			errored = append(errored, fmt.Sprintf("  %s: %s", id, excerpt(j.Reason)))
		case j.Verdict != j.Label:
			line := fmt.Sprintf("  %s: label %s, verdict %s", id, j.Label, j.Verdict)
			if j.Adversarial {
				line += " (followed the injection)"
			}
			wrong = append(wrong, line)
		}
	}
	if len(wrong) > 0 {
		fmt.Fprintf(&b, "\nDisagreements with the label:\n%s\n", strings.Join(wrong, "\n"))
	}
	if len(errored) > 0 {
		fmt.Fprintf(&b, "\nErrors:\n%s\n", strings.Join(errored, "\n"))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func rateRow(w io.Writer, name string, r *Rate) {
	if r == nil {
		_, _ = fmt.Fprintf(w, "  %s\tn/a\t\t0\n", name)
		return
	}
	_, _ = fmt.Fprintf(w, "  %s\t%.1f%%\t[%.1f%%, %.1f%%]\t%d\n", name, 100*r.Value, 100*r.Low, 100*r.High, r.N)
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func excerpt(s string) string {
	const limit = 160
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > limit {
		s = string(r[:limit]) + "..."
	}
	return s
}
