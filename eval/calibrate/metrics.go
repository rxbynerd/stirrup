package calibrate

import (
	"math"
	"slices"

	"github.com/rxbynerd/stirrup/types"
)

// wilsonZ is the standard normal quantile for a two-sided 95% interval.
const wilsonZ = 1.959964

// Rate is a proportion K/N with its Wilson score 95% interval.
type Rate struct {
	K     int     `json:"k"`
	N     int     `json:"n"`
	Value float64 `json:"value"`
	Low   float64 `json:"low"`
	High  float64 `json:"high"`
}

// wilson returns k/n with its Wilson score interval, or nil when n is 0 and
// the proportion is undefined.
func wilson(k, n int) *Rate {
	if n <= 0 {
		return nil
	}
	p := float64(k) / float64(n)
	nf := float64(n)
	z2 := wilsonZ * wilsonZ
	denom := 1 + z2/nf
	center := (p + z2/(2*nf)) / denom
	half := wilsonZ * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf)) / denom
	return &Rate{K: k, N: n, Value: p, Low: math.Max(0, center-half), High: math.Min(1, center+half)}
}

// Confusion counts decided judgments by label and verdict, with "pass" as
// the positive class.
type Confusion struct {
	TruePositive  int `json:"truePositive"`
	FalseNegative int `json:"falseNegative"`
	TrueNegative  int `json:"trueNegative"`
	FalsePositive int `json:"falsePositive"`
}

func (c Confusion) total() int {
	return c.TruePositive + c.FalseNegative + c.TrueNegative + c.FalsePositive
}

// kappa is Cohen's kappa between the labels and the verdicts, or nil when
// chance agreement is total and kappa is undefined.
func (c Confusion) kappa() *float64 {
	n := float64(c.total())
	if n == 0 {
		return nil
	}
	observed := float64(c.TruePositive+c.TrueNegative) / n
	labelPass, verdictPass := float64(c.TruePositive+c.FalseNegative), float64(c.TruePositive+c.FalsePositive)
	chance := (labelPass*verdictPass + (n-labelPass)*(n-verdictPass)) / (n * n)
	if chance >= 1 {
		return nil
	}
	k := (observed - chance) / (1 - chance)
	return &k
}

// Latency summarises the judgments that made a successful model call.
type Latency struct {
	Calls  int     `json:"calls"`
	MeanMs float64 `json:"meanMs"`
	P95Ms  int64   `json:"p95Ms"`
}

// Tokens totals the tokens reported by every judgment's record.
type Tokens struct {
	Input  int `json:"input"`
	Output int `json:"output"`
}

// Prices are USD per million tokens.
type Prices struct {
	InputPerMTok  float64 `json:"inputPerMTok"`
	OutputPerMTok float64 `json:"outputPerMTok"`
}

// Cost is the estimated spend of Tokens at Prices.
type Cost struct {
	Prices
	InputUSD  float64 `json:"inputUsd"`
	OutputUSD float64 `json:"outputUsd"`
	TotalUSD  float64 `json:"totalUsd"`
}

// Metrics summarise a calibration's judgments.
type Metrics struct {
	Judgments int       `json:"judgments"`
	Decided   int       `json:"decided"`
	Errors    int       `json:"errors"`
	Confusion Confusion `json:"confusion"`

	// TPR is the share of pass-labelled cases judged pass; TNR the share of
	// fail-labelled cases judged fail. Both and Accuracy count only decided
	// judgments.
	TPR      *Rate `json:"tpr,omitempty"`
	TNR      *Rate `json:"tnr,omitempty"`
	Accuracy *Rate `json:"accuracy,omitempty"`

	// Kappa is Cohen's kappa against the labels; nil when undefined.
	Kappa *float64 `json:"kappa,omitempty"`

	// ErrorRate is the share of all judgments with Status error.
	ErrorRate *Rate `json:"errorRate,omitempty"`

	// AdversarialFlipRate is the share of decided adversarial judgments
	// whose verdict is the case's injection target.
	AdversarialFlipRate *Rate `json:"adversarialFlipRate,omitempty"`
	AdversarialErrors   int   `json:"adversarialErrors"`

	Latency   *Latency `json:"latency,omitempty"`
	Tokens    Tokens   `json:"tokens"`
	Cost      *Cost    `json:"cost,omitempty"`
	CacheHits int      `json:"cacheHits"`
}

// Summarize computes the metrics of js. Token totals and the cost estimate
// include cache hits, so they describe what the judgments cost to make
// rather than what one invocation spent; latency covers only live calls.
func Summarize(js []Judgment, prices *Prices) Metrics {
	m := Metrics{Judgments: len(js)}
	advDecided, advFlips := 0, 0
	var latencies []int64
	for _, j := range js {
		if rec := j.Record; rec != nil {
			m.Tokens.Input += rec.InputTokens
			m.Tokens.Output += rec.OutputTokens
			if rec.CacheStatus == types.JudgeCacheHit {
				m.CacheHits++
			} else if j.Verdict != types.JudgeStatusError {
				latencies = append(latencies, rec.LatencyMs)
			}
		}
		if j.Verdict == types.JudgeStatusError {
			m.Errors++
			if j.Adversarial {
				m.AdversarialErrors++
			}
			continue
		}
		m.Decided++
		switch {
		case j.Label == types.JudgeStatusPass && j.Verdict == types.JudgeStatusPass:
			m.Confusion.TruePositive++
		case j.Label == types.JudgeStatusPass:
			m.Confusion.FalseNegative++
		case j.Verdict == types.JudgeStatusFail:
			m.Confusion.TrueNegative++
		default:
			m.Confusion.FalsePositive++
		}
		if j.Adversarial {
			advDecided++
			if j.Verdict == j.InjectionTarget {
				advFlips++
			}
		}
	}
	c := m.Confusion
	m.TPR = wilson(c.TruePositive, c.TruePositive+c.FalseNegative)
	m.TNR = wilson(c.TrueNegative, c.TrueNegative+c.FalsePositive)
	m.Accuracy = wilson(c.TruePositive+c.TrueNegative, c.total())
	m.Kappa = c.kappa()
	m.ErrorRate = wilson(m.Errors, m.Judgments)
	m.AdversarialFlipRate = wilson(advFlips, advDecided)
	m.Latency = summarizeLatency(latencies)
	if prices != nil {
		in := float64(m.Tokens.Input) * prices.InputPerMTok / 1e6
		out := float64(m.Tokens.Output) * prices.OutputPerMTok / 1e6
		m.Cost = &Cost{Prices: *prices, InputUSD: in, OutputUSD: out, TotalUSD: in + out}
	}
	return m
}

// summarizeLatency returns the mean and nearest-rank 95th percentile of ms,
// or nil when there are none.
func summarizeLatency(ms []int64) *Latency {
	if len(ms) == 0 {
		return nil
	}
	sorted := slices.Clone(ms)
	slices.Sort(sorted)
	var sum int64
	for _, v := range sorted {
		sum += v
	}
	rank := int(math.Ceil(0.95*float64(len(sorted)))) - 1
	return &Latency{Calls: len(sorted), MeanMs: float64(sum) / float64(len(sorted)), P95Ms: sorted[rank]}
}

// unstableCases lists, in first-seen order, the cases whose decided
// verdicts differ across repeats.
func unstableCases(js []Judgment) []string {
	verdicts := map[string]map[string]bool{}
	var order []string
	for _, j := range js {
		if j.Verdict == types.JudgeStatusError {
			continue
		}
		if verdicts[j.Case] == nil {
			verdicts[j.Case] = map[string]bool{}
			order = append(order, j.Case)
		}
		verdicts[j.Case][j.Verdict] = true
	}
	var out []string
	for _, id := range order {
		if len(verdicts[id]) > 1 {
			out = append(out, id)
		}
	}
	return out
}
