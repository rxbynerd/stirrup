package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

const cmdTriageSchema = `{"type":"object","required":["title","labels"],"additionalProperties":false,"properties":{"title":{"type":"string"},"labels":{"type":"array","items":{"enum":["bug","feature"]}}}}`

// answerSequenceServer streams one Chat Completions answer per request,
// taking each from answers in turn and repeating the last.
func answerSequenceServer(t *testing.T, answers ...string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		idx := min(int(requests.Add(1))-1, len(answers)-1)
		chunk, err := json.Marshal(map[string]any{
			"id":      "resp",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": answers[idx]}, "finish_reason": nil}},
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		_, _ = fmt.Fprint(w, "data: "+`{"id":"resp","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func structuredRunConfig(t *testing.T, baseURL string) (*types.RunConfig, string) {
	t.Helper()
	config := runOutcomeTestConfig(t, baseURL)
	tracePath := filepath.Join(t.TempDir(), "trace.jsonl")
	config.TraceEmitter = types.TraceEmitterConfig{Type: "jsonl", FilePath: tracePath}
	config.OutputSchema = json.RawMessage(cmdTriageSchema)
	return config, tracePath
}

func TestRunWithConfig_StructuredOutputAfterCorrection(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv, requests := answerSequenceServer(t,
		"The title is Fix login and it is a bug.",
		"```json\n{\"title\": \"Fix login\", \"labels\": [\"bug\"]}\n```",
	)
	config, tracePath := structuredRunConfig(t, srv.URL)

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout := stdoutDone()

	must.NoError(runErr)
	const want = `{"title":"Fix login","labels":["bug"]}`
	check.EqualValues(2, requests.Load())

	result := decodeStirrupResult(t, stdout)
	check.Equal("success", result.Outcome)
	check.Equal(want, string(result.StructuredOutput)) //nolint:testifylint // exact compact bytes are the behaviour under test

	done := stdioDoneEvent(t, stdout)
	must.NotNil(done.Trace)
	check.Equal(want, string(done.Trace.StructuredOutput)) //nolint:testifylint // exact compact bytes are the behaviour under test

	rt := runFinishedTrace(t, tracePath)
	check.Equal(want, string(rt.StructuredOutput)) //nolint:testifylint // exact compact bytes are the behaviour under test
}

func TestRunWithConfig_StructuredOutputFailureExitsNonZero(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv, requests := answerSequenceServer(t, `{"title":"Fix login"}`)
	config, _ := structuredRunConfig(t, srv.URL)
	config.MaxTurns = 3

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout := stdoutDone()

	must.Error(runErr)
	check.Equal(1, classifyExitCode(runErr))
	check.EqualValues(3, requests.Load(), "the answer and two corrective attempts")

	result := decodeStirrupResult(t, stdout)
	check.Equal("structured_output_failed", result.Outcome)
	check.Contains(result.Error, "missing property 'labels'")
	check.Nil(result.StructuredOutput)

	done := stdioDoneEvent(t, stdout)
	check.Equal("structured_output_failed", done.StopReason)
}

func TestRunWithConfig_StructuredOutput_ResultLineStaysUnderCap(t *testing.T) {
	const resultLineCap = 250 * 1024
	prefix, suffix := `{"title":"`, `","labels":["bug"]}`
	document := func(size int) string {
		return prefix + strings.Repeat("<&>", (size-len(prefix)-len(suffix))/3) + suffix
	}
	tests := []struct {
		name        string
		doc         string
		wantOutcome string
	}{
		{name: "largest document", doc: document(200 << 10), wantOutcome: "success"},
		{name: "document over the cap", doc: document(260 << 10), wantOutcome: "structured_output_failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			srv, _ := answerSequenceServer(t, tc.doc)
			config, _ := structuredRunConfig(t, srv.URL)
			// An unregistered model keeps the default window, which fits the
			// oversized document; gpt-4o-mini's registered window does not.
			config.ModelRouter.Model = "unregistered-model"
			config.ResultSink = &types.ResultSinkConfig{Type: "stdout-json", MaxFinalAssistantTextBytes: 1 << 20}
			config.MaxTurns = 3

			stdoutDone := captureStdout(t)
			_ = runWithConfig(config, runOptions{outputMode: "json"})
			stdout := stdoutDone()

			for _, line := range strings.Split(stdout, "\n") {
				if strings.HasPrefix(line, "STIRRUP_RESULT ") {
					must.LessOrEqual(len(line)+1, resultLineCap, "STIRRUP_RESULT line including its newline")
				}
			}
			result := decodeStirrupResult(t, stdout)
			must.Equal(tc.wantOutcome, result.Outcome, "error %q", result.Error)
			if tc.wantOutcome == "success" {
				must.Equal(tc.doc, string(result.StructuredOutput))
			}
		})
	}
}

func structuredJobTask(t *testing.T, providerURL string) *pb.RunConfig {
	t.Helper()
	must := require.New(t)
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	var schemaDoc map[string]any
	must.NoError(json.Unmarshal([]byte(cmdTriageSchema), &schemaDoc), "parse schema")
	schema, err := structpb.NewStruct(schemaDoc)
	must.NoError(err, "build schema struct")
	task := followUpJobConfig(t, providerURL, 30, 0)
	task.FollowUpGrace = nil
	task.MaxTurns = 3
	task.OutputSchema = schema
	return task
}

func runStructuredJob(t *testing.T, task *pb.RunConfig) (*pb.HarnessEvent, error) {
	t.Helper()
	must := require.New(t)
	useTempMarkerPaths(t)
	srv := newScriptedControlPlane(task, func(int) *pb.ControlEvent { return nil })
	serveControlPlane(t, srv)

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()
	srv.waitDone(t, "the structured job run")
	var runErr error
	select {
	case runErr = <-errCh:
	case <-time.After(15 * time.Second):
		must.Fail("runJob did not return")
	}
	var done *pb.HarnessEvent
	for _, ev := range srv.events() {
		if ev.Type == "done" {
			done = ev
		}
	}
	return done, runErr
}

func TestRunJob_StructuredOutputOnDone(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	provider, requests := answerSequenceServer(t, "Not JSON yet.", `{"title":"Fix login","labels":["bug"]}`)
	sink := &stubResultSink{}
	installStubResultSink(t, sink)

	done, runErr := runStructuredJob(t, structuredJobTask(t, provider.URL))
	must.NoError(runErr)
	const want = `{"title":"Fix login","labels":["bug"]}`
	check.EqualValues(2, requests.Load())
	check.Equal("success", done.GetStopReason())
	check.Equal(want, done.GetTrace().GetStructuredOutput()) //nolint:testifylint // exact compact bytes are the behaviour under test
	must.Len(sink.calls, 1)
	check.Equal(want, string(sink.calls[0].StructuredOutput)) //nolint:testifylint // exact compact bytes are the behaviour under test
}

func TestRunJob_StructuredOutputFailureOnDone(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	provider, _ := answerSequenceServer(t, "still prose")
	sink := &stubResultSink{}
	installStubResultSink(t, sink)

	done, runErr := runStructuredJob(t, structuredJobTask(t, provider.URL))
	must.Error(runErr)
	check.Equal(1, classifyExitCode(runErr))
	check.Equal("structured_output_failed", done.GetStopReason())
	check.Empty(done.GetTrace().GetStructuredOutput())
	check.Contains(done.GetTrace().GetError(), "not a single JSON document")
	must.Len(sink.calls, 1)
	check.Equal("structured_output_failed", sink.calls[0].Outcome)
}

func writeSchemaFile(t *testing.T, content string) string {
	t.Helper()
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "schema.json")
	must.NoError(os.WriteFile(path, []byte(content), 0o600), "write schema")
	return path
}

func TestApplyOverrides_OutputSchemaFlag(t *testing.T) {
	t.Run("file sets compact schema", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		path := writeSchemaFile(t, "{\n  \"type\": \"object\",\n  \"required\": [\"title\"]\n}\n")
		must.NoError(cmd.ParseFlags([]string{"--output-schema", path}))
		cfg := baseFileConfig()
		must.NoError(applyOverrides(cmd, cfg, nil))
		must.Equal(`{"type":"object","required":["title"]}`, string(cfg.OutputSchema)) //nolint:testifylint // exact compact bytes are the behaviour under test
	})

	t.Run("empty value clears file schema", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		must.NoError(cmd.ParseFlags([]string{"--output-schema="}))
		cfg := baseFileConfig()
		cfg.OutputSchema = json.RawMessage(`{"type":"object"}`)
		must.NoError(applyOverrides(cmd, cfg, nil))
		must.Nil(cfg.OutputSchema)
	})

	t.Run("unset flag keeps file schema", func(t *testing.T) {
		must := require.New(t)
		cmd := newTestHarnessCommand()
		cfg := baseFileConfig()
		cfg.OutputSchema = json.RawMessage(`{"type":"object"}`)
		must.NoError(applyOverrides(cmd, cfg, nil))
		must.Equal(`{"type":"object"}`, string(cfg.OutputSchema)) //nolint:testifylint // exact compact bytes are the behaviour under test
	})

	tests := []struct {
		name     string
		path     func(t *testing.T) string
		wantCode int
	}{
		{
			name:     "malformed json is a parse error",
			path:     func(t *testing.T) string { return writeSchemaFile(t, `{"type":`) },
			wantCode: exitParse,
		},
		{
			name:     "missing file is an io error",
			path:     func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.json") },
			wantCode: exitIO,
		},
		{
			name:     "directory is an io error",
			path:     func(t *testing.T) string { return t.TempDir() },
			wantCode: exitIO,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cmd := newTestHarnessCommand()
			must.NoError(cmd.ParseFlags([]string{"--output-schema", tc.path(t)}))
			err := applyOverrides(cmd, baseFileConfig(), nil)
			var exitErr *exitError
			must.ErrorAs(err, &exitErr)
			must.Equal(tc.wantCode, exitErr.code)
		})
	}
}

func TestRunRunConfig_OutputSchema(t *testing.T) {
	t.Run("validate rejects uncompilable schema", func(t *testing.T) {
		must := require.New(t)
		path := writeSchemaFile(t, `{"type":"strng"}`)
		_, err := runRunConfigForTest(t, []string{"--validate", "--mode", "execution", "--prompt", "x", "--output-schema", path}, "")
		var exitErr *exitError
		must.ErrorAs(err, &exitErr)
		must.Equal(exitValidation, exitErr.code)
		must.ErrorContains(err, "outputSchema")
	})

	t.Run("validate rejects external ref", func(t *testing.T) {
		must := require.New(t)
		path := writeSchemaFile(t, `{"$ref":"https://example.com/s.json"}`)
		_, err := runRunConfigForTest(t, []string{"--validate", "--mode", "execution", "--prompt", "x", "--output-schema", path}, "")
		must.ErrorContains(err, "not a local fragment")
	})

	t.Run("emits schema as json object", func(t *testing.T) {
		must := require.New(t)
		path := writeSchemaFile(t, cmdTriageSchema)
		out, err := runRunConfigForTest(t, []string{"--mode", "execution", "--output-schema", path}, "")
		must.NoError(err)
		var cfg types.RunConfig
		must.NoError(json.Unmarshal([]byte(out), &cfg))
		var compact bytes.Buffer
		must.NoError(json.Compact(&compact, cfg.OutputSchema))
		must.Equal(cmdTriageSchema, compact.String()) //nolint:testifylint // exact compact bytes are the behaviour under test
	})
}

func TestBuildRunResult_CarriesStructuredOutput(t *testing.T) {
	must := require.New(t)
	res := buildRunResult(&types.RunTrace{
		ID:               "run-1",
		Outcome:          "success",
		StructuredOutput: json.RawMessage(`{"title":"Fix login"}`),
	}, types.DefaultMaxFinalAssistantTextBytes)
	must.Equal(`{"title":"Fix login"}`, string(res.StructuredOutput)) //nolint:testifylint // exact compact bytes are the behaviour under test
}
