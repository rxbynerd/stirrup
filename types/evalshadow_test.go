package types

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEvalJudgeValidateShadow(t *testing.T) {
	leaf := EvalJudge{Type: "file-exists", Paths: []string{"a"}}
	shadowLeaf := leaf
	shadowLeaf.Shadow = true
	composite := func(judges ...EvalJudge) EvalJudge {
		return EvalJudge{Type: "composite", Require: "all", Judges: judges}
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.judge.ValidateShadow(tc.topLevel)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("ValidateShadow: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("ValidateShadow error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestEvalJudgeShadowWireShape(t *testing.T) {
	var j EvalJudge
	if err := json.Unmarshal([]byte(`{"type":"composite","judges":[{"type":"file-exists","paths":["a"]},{"type":"file-exists","paths":["b"],"shadow":true}]}`), &j); err != nil {
		t.Fatal(err)
	}
	if j.Judges[0].Shadow || !j.Judges[1].Shadow {
		t.Fatalf("decoded shadow flags = %v, %v; want false, true", j.Judges[0].Shadow, j.Judges[1].Shadow)
	}
	if err := j.ValidateShadow(true); err != nil {
		t.Errorf("ValidateShadow: %v", err)
	}

	data, err := json.Marshal(EvalJudge{Type: "file-exists"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"shadow"`) {
		t.Errorf("a judge that is not a shadow serialises the flag: %s", data)
	}
}
