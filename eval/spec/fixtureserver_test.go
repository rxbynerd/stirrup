package spec

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// fixtureTaskHCL renders one task around a fixture_server body.
func fixtureTaskHCL(fixtureBody string) string {
	return "suite \"fixtures\" {\n" +
		"  task \"t1\" {\n" +
		"    mode   = \"execution\"\n" +
		"    prompt = \"p\"\n" +
		fixtureBody +
		"    judge {\n" +
		"      type    = \"test-command\"\n" +
		"      command = \"true\"\n" +
		"    }\n" +
		"  }\n" +
		"}\n"
}

// TestLoadSuiteHCL_FixtureServerStreamFileAnchoredToSuiteDir pins that a
// relative stream_file resolves against the suite file's directory, an
// absolute one passes through, and a task without the block has none.
func TestLoadSuiteHCL_FixtureServerStreamFileAnchoredToSuiteDir(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	src := "suite \"fixtures\" {\n" +
		"  task \"relative\" {\n" +
		"    mode   = \"execution\"\n" +
		"    prompt = \"p\"\n" +
		"    fixture_server {\n" +
		"      mode        = \"sse\"\n" +
		"      stream_file = \"fixtures/turns.sse\"\n" +
		"    }\n" +
		"    judge {\n" +
		"      type    = \"test-command\"\n" +
		"      command = \"true\"\n" +
		"    }\n" +
		"  }\n" +
		"  task \"absolute\" {\n" +
		"    mode   = \"execution\"\n" +
		"    prompt = \"p\"\n" +
		"    fixture_server {\n" +
		"      mode        = \"sse\"\n" +
		"      stream_file = \"/abs/turns.sse\"\n" +
		"    }\n" +
		"    judge {\n" +
		"      type    = \"test-command\"\n" +
		"      command = \"true\"\n" +
		"    }\n" +
		"  }\n" +
		"  task \"none\" {\n" +
		"    mode   = \"execution\"\n" +
		"    prompt = \"p\"\n" +
		"    judge {\n" +
		"      type    = \"test-command\"\n" +
		"      command = \"true\"\n" +
		"    }\n" +
		"  }\n" +
		"}\n"
	path := writeTemp(t, "fixtures.hcl", src)
	got, err := LoadSuiteHCL(path)
	must.NoError(err)
	must.Len(got.Tasks, 3)
	check.Equal(&types.FixtureServerConfig{Mode: "sse", StreamFile: filepath.Join(filepath.Dir(path), "fixtures/turns.sse")}, got.Tasks[0].FixtureServer)
	check.Equal(&types.FixtureServerConfig{Mode: "sse", StreamFile: "/abs/turns.sse"}, got.Tasks[1].FixtureServer)
	check.Nil(got.Tasks[2].FixtureServer)
}

func TestLoadSuiteHCL_FixtureServerRejectsInvalidBlocks(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown mode",
			body: "    fixture_server {\n      mode = \"grpc\"\n    }\n",
			want: `invalid fixture_server.mode "grpc"`,
		},
		{
			name: "sse without stream file",
			body: "    fixture_server {\n      mode = \"sse\"\n    }\n",
			want: `fixture_server mode "sse" requires stream_file`,
		},
		{
			name: "mcp with stream file",
			body: "    fixture_server {\n      mode        = \"mcp\"\n      stream_file = \"x.sse\"\n    }\n",
			want: `fixture_server mode "mcp" does not take stream_file`,
		},
		{
			name: "missing mode",
			body: "    fixture_server {\n      stream_file = \"x.sse\"\n    }\n",
			want: "mode",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			path := writeTemp(t, "bad.hcl", fixtureTaskHCL(tc.body))
			_, err := LoadSuiteHCL(path)
			must.ErrorContains(err, tc.want)
		})
	}
}
