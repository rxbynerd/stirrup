package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/types"
)

func TestLoadSuite_Valid(t *testing.T) {
	dir := t.TempDir()
	suite := types.EvalSuite{
		ID:          "test-suite",
		Description: "a test suite",
		Tasks: []types.EvalTask{
			{ID: "t1", Prompt: "hello", Mode: "execution"},
		},
	}
	path := filepath.Join(dir, "suite.json")
	writeJSONFile(t, path, suite)

	got, err := loadSuite(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "test-suite" {
		t.Errorf("ID = %q, want %q", got.ID, "test-suite")
	}
	if len(got.Tasks) != 1 {
		t.Errorf("got %d tasks, want 1", len(got.Tasks))
	}
}

func TestLoadSuite_Missing(t *testing.T) {
	_, err := loadSuite("/nonexistent/suite.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadSuite_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	os.WriteFile(path, []byte("not json"), 0o644)

	_, err := loadSuite(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoadResult_Valid(t *testing.T) {
	dir := t.TempDir()
	result := eval.SuiteResult{
		SuiteID:  "s1",
		RunID:    "r1",
		PassRate: 0.5,
		Tasks: []eval.TaskResult{
			{TaskID: "t1", Outcome: "pass"},
			{TaskID: "t2", Outcome: "fail"},
		},
	}
	path := filepath.Join(dir, "result.json")
	writeJSONFile(t, path, result)

	got, err := loadResult(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.RunID != "r1" {
		t.Errorf("RunID = %q, want %q", got.RunID, "r1")
	}
	if len(got.Tasks) != 2 {
		t.Errorf("got %d tasks, want 2", len(got.Tasks))
	}
}

func TestLoadResult_Missing(t *testing.T) {
	_, err := loadResult("/nonexistent/result.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestWriteJSON_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")

	original := eval.SuiteResult{
		SuiteID:  "s1",
		RunID:    "r1",
		PassRate: 0.75,
	}

	if err := writeJSON(path, original); err != nil {
		t.Fatalf("writeJSON error: %v", err)
	}

	got, err := loadResult(path)
	if err != nil {
		t.Fatalf("loadResult error: %v", err)
	}

	if got.SuiteID != original.SuiteID || got.RunID != original.RunID {
		t.Errorf("round-trip mismatch: got %+v", got)
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
