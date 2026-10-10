package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	srv, requests := answerSequenceServer(t,
		"The title is Fix login and it is a bug.",
		"```json\n{\"title\": \"Fix login\", \"labels\": [\"bug\"]}\n```",
	)
	config, tracePath := structuredRunConfig(t, srv.URL)

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout := stdoutDone()

	if runErr != nil {
		t.Fatalf("runWithConfig error = %v, want nil", runErr)
	}
	const want = `{"title":"Fix login","labels":["bug"]}`
	if n := requests.Load(); n != 2 {
		t.Errorf("provider requests = %d, want 2", n)
	}
	result := decodeStirrupResult(t, stdout)
	if result.Outcome != "success" || string(result.StructuredOutput) != want {
		t.Errorf("RunResult outcome = %q, structuredOutput = %s; want success with %s", result.Outcome, result.StructuredOutput, want)
	}
	if done := stdioDoneEvent(t, stdout); done.Trace == nil || string(done.Trace.StructuredOutput) != want {
		t.Errorf("done trace = %+v, want structuredOutput %s", done.Trace, want)
	}
	if rt := runFinishedTrace(t, tracePath); string(rt.StructuredOutput) != want {
		t.Errorf("trace structuredOutput = %s, want %s", rt.StructuredOutput, want)
	}
}

func TestRunWithConfig_StructuredOutputFailureExitsNonZero(t *testing.T) {
	srv, requests := answerSequenceServer(t, `{"title":"Fix login"}`)
	config, _ := structuredRunConfig(t, srv.URL)
	config.MaxTurns = 3

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout := stdoutDone()

	if runErr == nil || classifyExitCode(runErr) != 1 {
		t.Errorf("runWithConfig error = %v (exit %d), want a non-zero exit", runErr, classifyExitCode(runErr))
	}
	if n := requests.Load(); n != 3 {
		t.Errorf("provider requests = %d, want the answer and two corrective attempts", n)
	}
	result := decodeStirrupResult(t, stdout)
	if result.Outcome != "structured_output_failed" {
		t.Errorf("RunResult outcome = %q, want structured_output_failed", result.Outcome)
	}
	if !strings.Contains(result.Error, "missing property 'labels'") {
		t.Errorf("RunResult error = %q, want the validation error", result.Error)
	}
	if result.StructuredOutput != nil {
		t.Errorf("RunResult structuredOutput = %s, want none", result.StructuredOutput)
	}
	if done := stdioDoneEvent(t, stdout); done.StopReason != "structured_output_failed" {
		t.Errorf("done stop reason = %q, want structured_output_failed", done.StopReason)
	}
}

func structuredJobTask(t *testing.T, providerURL string) *pb.RunConfig {
	t.Helper()
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	var schemaDoc map[string]any
	if err := json.Unmarshal([]byte(cmdTriageSchema), &schemaDoc); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	schema, err := structpb.NewStruct(schemaDoc)
	if err != nil {
		t.Fatalf("NewStruct: %v", err)
	}
	task := followUpJobConfig(t, providerURL, 30, 0)
	task.FollowUpGrace = nil
	task.MaxTurns = 3
	task.OutputSchema = schema
	return task
}

func runStructuredJob(t *testing.T, task *pb.RunConfig) (*pb.HarnessEvent, error) {
	t.Helper()
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
		t.Fatal("runJob did not return")
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
	provider, requests := answerSequenceServer(t, "Not JSON yet.", `{"title":"Fix login","labels":["bug"]}`)
	sink := &stubResultSink{}
	installStubResultSink(t, sink)

	done, runErr := runStructuredJob(t, structuredJobTask(t, provider.URL))
	if runErr != nil {
		t.Fatalf("runJob error = %v, want nil", runErr)
	}
	const want = `{"title":"Fix login","labels":["bug"]}`
	if n := requests.Load(); n != 2 {
		t.Errorf("provider requests = %d, want 2", n)
	}
	if done.GetStopReason() != "success" || done.GetTrace().GetStructuredOutput() != want {
		t.Errorf("done = %v, want success carrying structured_output %s", done, want)
	}
	if len(sink.calls) != 1 || string(sink.calls[0].StructuredOutput) != want {
		t.Errorf("RunResults = %+v, want one carrying the structured output", sink.calls)
	}
}

func TestRunJob_StructuredOutputFailureOnDone(t *testing.T) {
	provider, _ := answerSequenceServer(t, "still prose")
	sink := &stubResultSink{}
	installStubResultSink(t, sink)

	done, runErr := runStructuredJob(t, structuredJobTask(t, provider.URL))
	if runErr == nil || classifyExitCode(runErr) != 1 {
		t.Errorf("runJob error = %v (exit %d), want a non-zero exit", runErr, classifyExitCode(runErr))
	}
	if done.GetStopReason() != "structured_output_failed" || done.GetTrace().GetStructuredOutput() != "" {
		t.Errorf("done = %v, want structured_output_failed without structured_output", done)
	}
	if !strings.Contains(done.GetTrace().GetError(), "not a single JSON document") {
		t.Errorf("done.trace.error = %q, want the last validation error", done.GetTrace().GetError())
	}
	if len(sink.calls) != 1 || sink.calls[0].Outcome != "structured_output_failed" {
		t.Errorf("RunResults = %+v, want one structured_output_failed result", sink.calls)
	}
}

func writeSchemaFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write schema: %v", err)
	}
	return path
}

func TestApplyOverrides_OutputSchemaFlag(t *testing.T) {
	t.Run("file_sets_compact_schema", func(t *testing.T) {
		cmd := newTestHarnessCommand()
		path := writeSchemaFile(t, "{\n  \"type\": \"object\",\n  \"required\": [\"title\"]\n}\n")
		if err := cmd.ParseFlags([]string{"--output-schema", path}); err != nil {
			t.Fatalf("ParseFlags: %v", err)
		}
		cfg := baseFileConfig()
		if err := applyOverrides(cmd, cfg, nil); err != nil {
			t.Fatalf("applyOverrides: %v", err)
		}
		if string(cfg.OutputSchema) != `{"type":"object","required":["title"]}` {
			t.Errorf("OutputSchema = %s", cfg.OutputSchema)
		}
	})

	t.Run("empty_value_clears_file_schema", func(t *testing.T) {
		cmd := newTestHarnessCommand()
		if err := cmd.ParseFlags([]string{"--output-schema="}); err != nil {
			t.Fatalf("ParseFlags: %v", err)
		}
		cfg := baseFileConfig()
		cfg.OutputSchema = json.RawMessage(`{"type":"object"}`)
		if err := applyOverrides(cmd, cfg, nil); err != nil {
			t.Fatalf("applyOverrides: %v", err)
		}
		if cfg.OutputSchema != nil {
			t.Errorf("OutputSchema = %s, want cleared", cfg.OutputSchema)
		}
	})

	t.Run("unset_flag_keeps_file_schema", func(t *testing.T) {
		cmd := newTestHarnessCommand()
		cfg := baseFileConfig()
		cfg.OutputSchema = json.RawMessage(`{"type":"object"}`)
		if err := applyOverrides(cmd, cfg, nil); err != nil {
			t.Fatalf("applyOverrides: %v", err)
		}
		if string(cfg.OutputSchema) != `{"type":"object"}` {
			t.Errorf("OutputSchema = %s, want the file's schema", cfg.OutputSchema)
		}
	})

	for name, tc := range map[string]struct {
		path     func(t *testing.T) string
		wantCode int
	}{
		"malformed_json_is_a_parse_error": {func(t *testing.T) string { return writeSchemaFile(t, `{"type":`) }, exitParse},
		"missing_file_is_an_io_error":     {func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.json") }, exitIO},
		"directory_is_an_io_error":        {func(t *testing.T) string { return t.TempDir() }, exitIO},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := newTestHarnessCommand()
			if err := cmd.ParseFlags([]string{"--output-schema", tc.path(t)}); err != nil {
				t.Fatalf("ParseFlags: %v", err)
			}
			err := applyOverrides(cmd, baseFileConfig(), nil)
			var exitErr *exitError
			if !errors.As(err, &exitErr) || exitErr.code != tc.wantCode {
				t.Errorf("applyOverrides error = %v, want exit code %d", err, tc.wantCode)
			}
		})
	}
}

func TestRunRunConfig_OutputSchema(t *testing.T) {
	t.Run("validate_rejects_uncompilable_schema", func(t *testing.T) {
		path := writeSchemaFile(t, `{"type":"strng"}`)
		_, err := runRunConfigForTest(t, []string{"--validate", "--mode", "execution", "--prompt", "x", "--output-schema", path}, "")
		var exitErr *exitError
		if !errors.As(err, &exitErr) || exitErr.code != exitValidation || !strings.Contains(err.Error(), "outputSchema") {
			t.Errorf("run-config --validate error = %v, want an outputSchema validation failure", err)
		}
	})

	t.Run("validate_rejects_external_ref", func(t *testing.T) {
		path := writeSchemaFile(t, `{"$ref":"https://example.com/s.json"}`)
		_, err := runRunConfigForTest(t, []string{"--validate", "--mode", "execution", "--prompt", "x", "--output-schema", path}, "")
		if err == nil || !strings.Contains(err.Error(), "not a local fragment") {
			t.Errorf("run-config --validate error = %v, want the external reference rejected", err)
		}
	})

	t.Run("emits_schema_as_json_object", func(t *testing.T) {
		path := writeSchemaFile(t, cmdTriageSchema)
		out, err := runRunConfigForTest(t, []string{"--mode", "execution", "--output-schema", path}, "")
		if err != nil {
			t.Fatalf("run-config: %v", err)
		}
		var cfg types.RunConfig
		if err := json.Unmarshal([]byte(out), &cfg); err != nil {
			t.Fatalf("parse: %v", err)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, cfg.OutputSchema); err != nil || compact.String() != cmdTriageSchema {
			t.Errorf("OutputSchema = %s (%v), want %s", cfg.OutputSchema, err, cmdTriageSchema)
		}
	})
}

func TestBuildRunResult_CarriesStructuredOutput(t *testing.T) {
	res := buildRunResult(&types.RunTrace{
		ID:               "run-1",
		Outcome:          "success",
		StructuredOutput: json.RawMessage(`{"title":"Fix login"}`),
	}, types.DefaultMaxFinalAssistantTextBytes)
	if string(res.StructuredOutput) != `{"title":"Fix login"}` {
		t.Errorf("StructuredOutput = %s", res.StructuredOutput)
	}
}
