package trace

import (
	"context"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

func TestEmitters_RecordGitHandoffReachesRunTrace(t *testing.T) {
	want := types.GitHandoff{Branch: "stirrup/run-1", SHA: "bbb", BaseSHA: "aaa", Dirty: true, Error: "dirty"}
	builders := stopDetailsEmitterBuilders(t)
	for _, name := range []string{"jsonl", "otel", "gcs"} {
		t.Run(name, func(t *testing.T) {
			e := builders[name](t)
			recorder, ok := e.(GitHandoffRecorder)
			if !ok {
				t.Fatalf("%T does not implement GitHandoffRecorder", e)
			}

			e.Start("run-1", &types.RunConfig{RunID: "run-1"})
			handoff := want
			recorder.RecordGitHandoff(handoff)
			handoff.SHA = "mutated"
			got, err := e.Finish(context.Background(), "handoff_failed")
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if got.Git == nil || *got.Git != want {
				t.Errorf("Git = %+v, want %+v", got.Git, want)
			}

			e.Start("run-2", &types.RunConfig{RunID: "run-2"})
			got, err = e.Finish(context.Background(), "success")
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if got.Git != nil {
				t.Errorf("Git after restart = %+v, want nil", got.Git)
			}
		})
	}
}
