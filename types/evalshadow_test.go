package types

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEvalJudge_ValidateShadow(t *testing.T) {
	leaf := EvalJudge{Type: "file-exists", Paths: []string{"a"}}
	shadowLeaf := leaf
	shadowLeaf.Shadow = true
	composite := func(judges ...EvalJudge) EvalJudge {
		return EvalJudge{Type: "composite", Require: "all", Judges: judges}
	}
	decisionLeaf := EvalJudge{Type: "diff-review", Criteria: "c", LLM: &JudgeLLMConfig{Provider: JudgeProviderDecision, Model: "jev-latest"}}
	shadowDecision := decisionLeaf
	shadowDecision.Shadow = true
	command := EvalJudge{Type: "test-command", Command: "true"}
	shadowCommand := command
	shadowCommand.Shadow = true
	shadowComposite := func(judges ...EvalJudge) EvalJudge {
		c := composite(judges...)
		c.Shadow = true
		return c
	}

	cases := []struct {
		name     string
		judge    EvalJudge
		topLevel bool
		wantErr  string
	}{
		{name: "plain top-level judge", judge: leaf, topLevel: true},
		{name: "shadow top-level judge", judge: shadowLeaf, topLevel: true, wantErr: "only valid on a composite sub-judge"},
		{name: "shadow sub-judge position", judge: shadowLeaf, topLevel: false},
		{name: "shadow beside a deciding judge", judge: composite(leaf, shadowLeaf), topLevel: true},
		{name: "composite of shadows", judge: composite(shadowLeaf, shadowLeaf), topLevel: true, wantErr: "at least one sub-judge that is not a shadow"},
		{name: "nested composite of shadows", judge: composite(leaf, composite(shadowLeaf)), topLevel: true, wantErr: "sub-judge 2: composite judge needs"},
		{name: "top-level decision judge", judge: decisionLeaf, topLevel: true, wantErr: `provider "decision" is usable only on a shadow judge`},
		{name: "deciding decision sub-judge", judge: composite(leaf, decisionLeaf), topLevel: true, wantErr: `sub-judge 2: llm provider "decision"`},
		{name: "shadow decision sub-judge", judge: composite(leaf, shadowDecision), topLevel: true},
		{name: "shadow test-command", judge: composite(leaf, shadowCommand), topLevel: true, wantErr: `sub-judge 2: a "test-command" judge cannot be a shadow`},
		{name: "test-command inside a shadow composite", judge: composite(leaf, shadowComposite(command)), topLevel: true, wantErr: `sub-judge 2: sub-judge 1: a "test-command" judge cannot be a shadow or sit inside one`},
		{name: "deciding test-command beside a shadow", judge: composite(command, shadowLeaf), topLevel: true},
		{name: "every side-effect-free type as a shadow", judge: composite(leaf,
			EvalJudge{Type: "file-exists", Shadow: true},
			EvalJudge{Type: "file-contains", Shadow: true},
			EvalJudge{Type: "diff-review", Shadow: true},
			EvalJudge{Type: "tool-trace", Shadow: true},
			shadowComposite(leaf)), topLevel: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.judge.ValidateShadow(tc.topLevel)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestEvalJudge_ShadowJSON(t *testing.T) {
	t.Run("decodes the shadow flag per sub-judge", func(t *testing.T) {
		var j EvalJudge
		require.NoError(t, json.Unmarshal([]byte(`{"type":"composite","judges":[{"type":"file-exists","paths":["a"]},{"type":"file-exists","paths":["b"],"shadow":true}]}`), &j))
		require.False(t, j.Judges[0].Shadow, "first sub-judge should not be a shadow")
		require.True(t, j.Judges[1].Shadow, "second sub-judge should be a shadow")
		require.NoError(t, j.ValidateShadow(true))
	})

	t.Run("rejects a decoded deciding decision sub-judge", func(t *testing.T) {
		var deciding EvalJudge
		require.NoError(t, json.Unmarshal([]byte(`{"type":"composite","judges":[{"type":"file-exists","paths":["a"]},{"type":"diff-review","criteria":"c","llm":{"provider":"decision","model":"jev-latest"}}]}`), &deciding))
		require.ErrorContains(t, deciding.ValidateShadow(true), "usable only on a shadow judge")
	})

	t.Run("omits the shadow flag when unset", func(t *testing.T) {
		data, err := json.Marshal(EvalJudge{Type: "file-exists"})
		require.NoError(t, err)
		require.NotContains(t, string(data), `"shadow"`)
	})
}
