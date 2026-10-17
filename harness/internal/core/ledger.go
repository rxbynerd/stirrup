package core

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/types"
)

// maxSpawnsPerRun bounds the sub-agents one top-level run may start.
const maxSpawnsPerRun = 8

// TokenLedger is the run-wide token account of a top-level run. Every
// provider call made on the run's credentials is charged to it: the
// parent's turns, every sub-agent's turns, and auxiliary calls (summary,
// llm-judge, cloud-judge). MaxTokenBudget is enforced against its total.
// It is safe for concurrent use.
type TokenLedger struct {
	mu        sync.Mutex
	limit     *int
	total     types.TokenUsage
	auxiliary types.TokenUsage
	external  types.TokenUsage
	spawns    int
}

// NewTokenLedger returns an empty ledger enforcing limit; a nil limit
// leaves the run unbounded.
func NewTokenLedger(limit *int) *TokenLedger {
	l := &TokenLedger{}
	if limit != nil {
		v := *limit
		l.limit = &v
	}
	return l
}

// Charge records the usage of one agentic turn.
func (l *TokenLedger) Charge(usage types.TokenUsage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total.Add(usage)
}

// ChargeAuxiliary records the usage of one auxiliary call. It counts
// toward the total and is also kept as a separate figure.
func (l *TokenLedger) ChargeAuxiliary(usage types.TokenUsage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total.Add(usage)
	l.auxiliary.Add(usage)
}

// ReportExternal records usage from a guard on its own endpoint. It is
// reported but never counts toward the total.
func (l *TokenLedger) ReportExternal(usage types.TokenUsage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.external.Add(usage)
}

// Total returns every charged token, auxiliary calls included.
func (l *TokenLedger) Total() types.TokenUsage {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total
}

// Auxiliary returns the charged usage of auxiliary calls.
func (l *TokenLedger) Auxiliary() types.TokenUsage {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.auxiliary
}

// External returns the reported, uncharged usage of external guards.
func (l *TokenLedger) External() types.TokenUsage {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.external
}

// CheckBudget reports whether the ledger total is within the run's
// limit, with the same semantics as TokenTracker.CheckBudget.
func (l *TokenLedger) CheckBudget() types.BudgetCheck {
	l.mu.Lock()
	defer l.mu.Unlock()
	tt := TokenTracker{total: l.total}
	return tt.CheckBudget(l.limit)
}

// AdmitSpawn admits one sub-agent or returns an error the model can
// read. It refuses once maxSpawnsPerRun spawns were admitted or no
// budget remains. The returned budget is the remainder at admission, a
// ceiling for the child rather than a reservation; nil means unbounded.
func (l *TokenLedger) AdmitSpawn() (*int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.spawns >= maxSpawnsPerRun {
		return nil, fmt.Errorf("spawn_agent refused: this run has already started %d sub-agents, the maximum per run", maxSpawnsPerRun)
	}
	var budget *int
	if l.limit != nil {
		used := l.total.Total()
		remaining := *l.limit - used
		if remaining <= 0 {
			return nil, fmt.Errorf("spawn_agent refused: the run's token budget is spent (%d of %d tokens used)", used, *l.limit)
		}
		budget = &remaining
	}
	l.spawns++
	return budget, nil
}

// ledgerBinding points auxiliary call sites, which are built before any
// run exists, at the ledger of the top-level run in progress. A loop and
// its sub-agents share one binding.
type ledgerBinding struct {
	current atomic.Pointer[TokenLedger]
}

func (b *ledgerBinding) chargeAuxiliary(usage types.TokenUsage) {
	if l := b.current.Load(); l != nil {
		l.ChargeAuxiliary(usage)
	}
}

func (b *ledgerBinding) reportExternal(usage types.TokenUsage) {
	if l := b.current.Load(); l != nil {
		l.ReportExternal(usage)
	}
}

// acquireLedger returns the ledger this run charges and a release func.
// A sub-agent shares the ledger its parent bound; a top-level run binds
// a fresh ledger for its own duration.
func (l *AgenticLoop) acquireLedger(config *types.RunConfig) (*TokenLedger, func()) {
	if l.ledger == nil {
		l.ledger = &ledgerBinding{}
	}
	if l.ParentRunID != "" {
		if shared := l.ledger.current.Load(); shared != nil {
			return shared, func() {}
		}
	}
	ledger := NewTokenLedger(config.MaxTokenBudget)
	l.ledger.current.Store(ledger)
	return ledger, func() { l.ledger.current.CompareAndSwap(ledger, nil) }
}

// activeLedger returns the ledger of the run in progress, or nil outside
// a run.
func (l *AgenticLoop) activeLedger() *TokenLedger {
	if l.ledger == nil {
		return nil
	}
	return l.ledger.current.Load()
}

// budgetExhausted applies both budgets a loop answers to: its own limit
// (a sub-agent's admitted budget) against its own turns, and the run's
// limit against the shared ledger.
func budgetExhausted(own *TokenTracker, ledger *TokenLedger, config *types.RunConfig) bool {
	return !own.CheckBudget(config.MaxTokenBudget).WithinBudget || !ledger.CheckBudget().WithinBudget
}

// chargingProvider charges the usage of every stream it serves. Usage is
// read from message_complete events, and the charge lands before the
// returned channel closes, so a caller that drains the stream observes
// it on return. Input is estimated from the request when a completed
// response reports none, matching the loop's own turn accounting.
type chargingProvider struct {
	inner  provider.ProviderAdapter
	charge func(types.TokenUsage)
	// structuredResults reports whether the adapter sends tool results'
	// wire envelope for a model; nil assumes it does, as the loop does.
	structuredResults func(model string) bool
}

type chargingOption func(*chargingProvider)

// withStructuredResults sets how the fallback input estimate decides
// whether tool results carry the wire envelope.
func withStructuredResults(fn func(model string) bool) chargingOption {
	return func(m *chargingProvider) { m.structuredResults = fn }
}

func newChargingProvider(inner provider.ProviderAdapter, charge func(types.TokenUsage), opts ...chargingOption) provider.ProviderAdapter {
	if inner == nil {
		return nil
	}
	m := &chargingProvider{inner: inner, charge: charge}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

func (m *chargingProvider) sendsStructuredResults(model string) bool {
	return m.structuredResults == nil || m.structuredResults(model)
}

func (m *chargingProvider) Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	in, err := m.inner.Stream(ctx, params)
	if err != nil {
		return nil, err
	}
	out := make(chan types.StreamEvent)
	go func() {
		defer close(out)
		var usage types.TokenUsage
		completed := false
		defer func() {
			if completed && usage.Input == 0 {
				usage.Input = estimateCurrentTokens(params.Messages, m.sendsStructuredResults(params.Model)) +
					estimateSystemPromptTokens(params.System) +
					estimateToolDefinitionTokens(params.Tools)
			}
			if usage != (types.TokenUsage{}) {
				m.charge(usage)
			}
		}()
		for ev := range in {
			if ev.Type == "message_complete" {
				usage.MergeEvent(ev)
				completed = true
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				for ev := range in {
					if ev.Type == "message_complete" {
						usage.MergeEvent(ev)
						completed = true
					}
				}
				return
			}
		}
	}()
	return out, nil
}
