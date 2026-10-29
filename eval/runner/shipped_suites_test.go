package runner

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval/fixtureserver"
	"github.com/rxbynerd/stirrup/eval/spec"
	tracereader "github.com/rxbynerd/stirrup/types/trace"
)

// TestShippedSuites_DryRunValidates runs --dry-run validation over every
// shipped suite, so a suite whose merged RunConfig fails ValidateRunConfig
// is caught before an operator runs it.
func TestShippedSuites_DryRunValidates(t *testing.T) {
	must := require.New(t)
	paths, err := filepath.Glob(filepath.Join("..", "suites", "*.hcl"))
	must.NoError(err)
	must.NotEmpty(paths, "no shipped suites found")
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			must := require.New(t)
			suite, err := spec.LoadSuiteHCL(path)
			must.NoError(err, "LoadSuiteHCL")
			result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
			must.NoError(err, "RunSuite")
			must.NotEmpty(result.Tasks, "suite has no tasks")
			for _, task := range result.Tasks {
				t.Run(task.TaskID, func(t *testing.T) {
					must := require.New(t)
					must.Equal("pass", task.Outcome, "task error %q", task.Error)
				})
			}
		})
	}
}

// TestShippedSuites_ReplayFixturesLoad opens every recording a shipped
// suite names and checks it replays cleanly, and that every fixture file
// is named by some suite. A dry run validates only the RunConfig, so a
// renamed or malformed fixture would otherwise surface only when the
// harness binary runs.
func TestShippedSuites_ReplayFixturesLoad(t *testing.T) {
	must := require.New(t)
	paths, err := filepath.Glob(filepath.Join("..", "suites", "*.hcl"))
	must.NoError(err)
	referenced := map[string]bool{}
	for _, path := range paths {
		suite, err := spec.LoadSuiteHCL(path)
		must.NoError(err, "LoadSuiteHCL %s", path)
		var files []string
		if suite.RunConfig != nil && suite.RunConfig.Provider.ReplayFile != "" {
			files = append(files, suite.RunConfig.Provider.ReplayFile)
		}
		for _, task := range suite.Tasks {
			if task.RunConfigOverrides != nil && task.RunConfigOverrides.Provider != nil && task.RunConfigOverrides.Provider.ReplayFile != "" {
				files = append(files, task.RunConfigOverrides.Provider.ReplayFile)
			}
		}
		for _, file := range files {
			t.Run(filepath.Base(path)+"/"+filepath.Base(file), func(t *testing.T) {
				must := require.New(t)
				referenced[file] = true
				reader, err := tracereader.Open(file)
				must.NoError(err, "open recording")
				defer func() { _ = reader.Close() }()
				recording, err := reader.ReadRecording()
				must.NoError(err)
				must.NotEmpty(recording.Turns, "recording has no turns")
				must.Zero(reader.SkippedLines(), "recording has oversized lines")
				must.Zero(reader.MalformedLines(), "recording has malformed lines")
			})
		}
	}
	fixtures, err := filepath.Glob(filepath.Join("..", "suites", "fixtures", "*", "*.jsonl"))
	must.NoError(err)
	must.NotEmpty(fixtures, "no fixtures found")
	for _, fixture := range fixtures {
		abs, err := filepath.Abs(fixture)
		must.NoError(err)
		must.True(referenced[abs], "fixture %s is not named by any shipped suite", fixture)
	}
}

// TestShippedSuites_FixtureStreamsLoad opens every canned stream a shipped
// suite names and checks it holds at least one turn, and that every stream
// file is named by some suite. A dry run never opens the file, so a renamed
// or empty stream would otherwise surface only when the harness runs.
func TestShippedSuites_FixtureStreamsLoad(t *testing.T) {
	must := require.New(t)
	paths, err := filepath.Glob(filepath.Join("..", "suites", "*.hcl"))
	must.NoError(err)
	referenced := map[string]bool{}
	for _, path := range paths {
		suite, err := spec.LoadSuiteHCL(path)
		must.NoError(err, "LoadSuiteHCL %s", path)
		for _, task := range suite.Tasks {
			if task.FixtureServer == nil || task.FixtureServer.StreamFile == "" {
				continue
			}
			file := task.FixtureServer.StreamFile
			t.Run(filepath.Base(path)+"/"+task.ID, func(t *testing.T) {
				must := require.New(t)
				referenced[file] = true
				turns, err := fixtureserver.LoadTurns(file)
				must.NoError(err)
				must.NotEmpty(turns)
			})
		}
	}
	streams, err := filepath.Glob(filepath.Join("..", "suites", "fixtures", "*", "*.sse"))
	must.NoError(err)
	must.NotEmpty(streams, "no stream files found")
	for _, stream := range streams {
		abs, err := filepath.Abs(stream)
		must.NoError(err)
		must.True(referenced[abs], "stream file %s is not named by any shipped suite", stream)
	}
}
