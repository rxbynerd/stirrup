package spec

import (
	"testing"

	"github.com/stretchr/testify/require"
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

func TestLoadSuiteHCL_Trials_ParsesCount(t *testing.T) {
	t.Run("reads the attribute", func(t *testing.T) {
		must := require.New(t)
		got, err := LoadSuiteHCL(writeTemp(t, "trials.hcl", trialsSuiteSource("  trials = 3\n")))
		must.NoError(err)
		must.Equal(3, got.Trials)
	})

	t.Run("defaults to zero when the attribute is absent", func(t *testing.T) {
		must := require.New(t)
		got, err := LoadSuiteHCL(writeTemp(t, "no-trials.hcl", trialsSuiteSource("")))
		must.NoError(err)
		must.Equal(0, got.Trials)
	})
}

func TestLoadSuiteHCL_Trials_RejectsNonPositive(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{name: "zero", line: "  trials = 0\n"},
		{name: "negative", line: "  trials = -2\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, err := LoadSuiteHCL(writeTemp(t, "bad-trials.hcl", trialsSuiteSource(tc.line)))
			must.ErrorContains(err, "trials must be at least 1")
		})
	}
}

func TestLoadSuiteHCL_Trials_CapsAt20(t *testing.T) {
	t.Run("accepts 20", func(t *testing.T) {
		must := require.New(t)
		got, err := LoadSuiteHCL(writeTemp(t, "cap.hcl", trialsSuiteSource("  trials = 20\n")))
		must.NoError(err)
		must.Equal(20, got.Trials)
	})

	t.Run("rejects 21", func(t *testing.T) {
		must := require.New(t)
		_, err := LoadSuiteHCL(writeTemp(t, "over.hcl", trialsSuiteSource("  trials = 21\n")))
		must.ErrorContains(err, "trials must be at most 20, got 21")
	})
}

func TestLoadSuiteHCL_RequireHealthy_ParsesFlag(t *testing.T) {
	t.Run("reads true when set", func(t *testing.T) {
		must := require.New(t)
		got, err := LoadSuiteHCL(writeTemp(t, "require.hcl", trialsSuiteSource("  require_healthy = true\n")))
		must.NoError(err)
		must.True(got.RequireHealthy)
	})

	t.Run("is false when the attribute is absent", func(t *testing.T) {
		must := require.New(t)
		unset, err := LoadSuiteHCL(writeTemp(t, "no-require.hcl", trialsSuiteSource("")))
		must.NoError(err)
		must.False(unset.RequireHealthy)
	})
}
