package router

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCandidates_StaticRouterYieldsItsOneSelection(t *testing.T) {
	must := require.New(t)

	got := Candidates(context.Background(), NewStaticRouter("anthropic", "claude-sonnet-4-6"), "execution")

	must.Equal([]ModelSelection{{Provider: "anthropic", Model: "claude-sonnet-4-6"}}, got)
}

func TestCandidates_PerModeRouterYieldsTheModeSelectionThenEveryOther(t *testing.T) {
	must := require.New(t)
	r := NewPerModeRouter(defaultSel, map[string]ModelSelection{"review": cheapSel, "planning": expensiveSel})

	got := Candidates(context.Background(), r, "review")

	must.Equal([]ModelSelection{cheapSel, defaultSel, expensiveSel}, got)
}

func TestCandidates_DynamicRouterYieldsEveryDistinctSelection(t *testing.T) {
	must := require.New(t)

	got := Candidates(context.Background(), NewDynamicRouter(standardConfig), "execution")

	must.Equal([]ModelSelection{defaultSel, cheapSel, expensiveSel}, got)
}

func TestCandidates_DynamicRouterCollapsesRepeatedSelections(t *testing.T) {
	must := require.New(t)
	cfg := standardConfig
	cfg.CheapSelection = defaultSel
	cfg.ExpensiveSelection = defaultSel

	got := Candidates(context.Background(), NewDynamicRouter(cfg), "execution")

	must.Equal([]ModelSelection{defaultSel}, got)
}
