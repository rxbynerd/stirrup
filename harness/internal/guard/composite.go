package guard

import (
	"context"
	"strings"
	"sync"
	"time"
)

// verdictRank ranks verdicts so we can pick the strongest aggregated
// outcome under the precedence Deny > Spotlight > Allow. An unknown
// verdict ranks below Allow so it cannot accidentally win an aggregation.
func verdictRank(v Verdict) int {
	switch v {
	case VerdictDeny:
		return 3
	case VerdictAllowSpot:
		return 2
	case VerdictAllow:
		return 1
	default:
		return 0
	}
}

// Sequential runs its Guards in order and short-circuits on the first
// VerdictDeny. If no guard denies, the LAST allow / spotlight decision
// is returned so a spotlight verdict from any guard naturally
// propagates. Errors abort the chain.
//
// Aggregation precedence: Deny > Spotlight > Allow.
type Sequential struct {
	Guards []GuardRail
	ID     string
}

// Check iterates the guards in order. The first VerdictDeny wins; an
// error from any guard propagates and stops the chain. With no guards,
// the result is a vacuous allow tagged with the composite's ID.
func (s Sequential) Check(ctx context.Context, in Input) (*Decision, error) {
	id := s.ID
	if id == "" {
		id = "sequential"
	}
	if len(s.Guards) == 0 {
		return &Decision{Verdict: VerdictAllow, GuardID: id}, nil
	}

	var last *Decision
	for _, g := range s.Guards {
		// Honour cancellation between sub-guards; downstream RPCs may
		// be expensive and we do not want to dispatch one after the
		// caller's deadline has fired.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		d, err := g.Check(ctx, in)
		if err != nil {
			return nil, err
		}
		if d != nil && d.Verdict == VerdictDeny {
			return d, nil
		}
		last = d
	}
	if last == nil {
		// All guards returned nil decisions without erroring. Treat as
		// a tagged allow rather than panicking — adapter authors should
		// never do this, but the composite's job is to stay sane.
		return &Decision{Verdict: VerdictAllow, GuardID: id}, nil
	}
	return last, nil
}

// Parallel fans guards out concurrently and aggregates their verdicts
// under the precedence Deny > Spotlight > Allow. The returned Decision
// names the strongest guard's GuardID; Reason is the comma-joined
// reasons of every contributing guard at the strongest rank; Latency
// is the maximum child latency (parallel wall time, not summed work).
//
// A deny from any guard wins outright, even if siblings errored. If
// every non-error guard allows and at least one guard errored, the
// first error is returned.
type Parallel struct {
	Guards []GuardRail
	ID     string
}

// Check fans out across all guards, waits for every one to settle,
// then aggregates. We do not cancel siblings on the first deny: each
// guard's verdict is interesting for traces and metrics, and adapter
// authors expect Check to either run to completion or honour ctx.
func (p Parallel) Check(ctx context.Context, in Input) (*Decision, error) {
	id := p.ID
	if id == "" {
		id = "parallel"
	}
	if len(p.Guards) == 0 {
		return &Decision{Verdict: VerdictAllow, GuardID: id}, nil
	}

	type slot struct {
		decision *Decision
		err      error
	}
	results := make([]slot, len(p.Guards))

	var wg sync.WaitGroup
	wg.Add(len(p.Guards))
	for i, g := range p.Guards {
		go func(i int, g GuardRail) {
			defer wg.Done()
			d, err := g.Check(ctx, in)
			results[i] = slot{decision: d, err: err}
		}(i, g)
	}
	wg.Wait()

	var (
		strongest     *Decision
		strongestRank int
		reasons       []string
		maxLatency    time.Duration
		firstErr      error
		anyDeny       bool
	)
	for _, r := range results {
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
		if r.decision == nil {
			continue
		}
		if r.decision.Latency > maxLatency {
			maxLatency = r.decision.Latency
		}
		rank := verdictRank(r.decision.Verdict)
		if rank > strongestRank {
			strongestRank = rank
			strongest = r.decision
			reasons = reasons[:0]
			if r.decision.Reason != "" {
				reasons = append(reasons, r.decision.Reason)
			}
		} else if rank == strongestRank && rank > 0 {
			if r.decision.Reason != "" {
				reasons = append(reasons, r.decision.Reason)
			}
		}
		if r.decision.Verdict == VerdictDeny {
			anyDeny = true
		}
	}

	// A deny outranks any error: the safest action is to stop the run,
	// and we already have a structured reason from the denying guard.
	if !anyDeny && firstErr != nil {
		return nil, firstErr
	}
	if strongest == nil {
		return &Decision{Verdict: VerdictAllow, GuardID: id}, nil
	}

	out := *strongest
	if len(reasons) > 0 {
		out.Reason = strings.Join(reasons, ", ")
	}
	out.Latency = maxLatency
	return &out, nil
}
