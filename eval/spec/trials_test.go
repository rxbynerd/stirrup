package spec

import (
	"strings"
	"testing"
)

func trialsSuiteSource(trialsLine string) string {
	return "suite \"trials\" {\n" +
		trialsLine +
		"  task \"t1\" {\n" +
		"    prompt = \"p\"\n" +
		"    judge {\n" +
		"      type    = \"test-command\"\n" +
		"      command = \"true\"\n" +
		"    }\n" +
		"  }\n" +
		"}\n"
}

func TestLoadSuiteHCL_Trials(t *testing.T) {
	got, err := LoadSuiteHCL(writeTemp(t, "trials.hcl", trialsSuiteSource("  trials = 3\n")))
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	if got.Trials != 3 {
		t.Errorf("Trials = %d, want 3", got.Trials)
	}

	unset, err := LoadSuiteHCL(writeTemp(t, "no-trials.hcl", trialsSuiteSource("")))
	if err != nil {
		t.Fatalf("LoadSuiteHCL: %v", err)
	}
	if unset.Trials != 0 {
		t.Errorf("Trials = %d, want 0 when the attribute is absent", unset.Trials)
	}
}

func TestLoadSuiteHCL_TrialsRejectsNonPositive(t *testing.T) {
	for _, line := range []string{"  trials = 0\n", "  trials = -2\n"} {
		_, err := LoadSuiteHCL(writeTemp(t, "bad-trials.hcl", trialsSuiteSource(line)))
		if err == nil || !strings.Contains(err.Error(), "trials must be at least 1") {
			t.Errorf("%q: error = %v, want a trials validation error", strings.TrimSpace(line), err)
		}
	}
}
