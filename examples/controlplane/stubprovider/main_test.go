package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func post(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"stream":true}`)))
	return rec
}

func TestSuccessModeStreamsOneFinalMessage(t *testing.T) {
	rec := post(t, newHandler("success"), "/chat/completions")
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	for _, want := range []string{finalMessage, `"finish_reason":"stop"`, `"prompt_tokens":10`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

func TestEmptyModeStreamsAMessageWithoutContent(t *testing.T) {
	rec := post(t, newHandler("empty"), "/chat/completions")
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(body, finalMessage) || !strings.Contains(body, `"content":""`) || !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("body is not an empty message that ends the turn:\n%s", body)
	}
}

func TestErrorModeRejectsWithBadRequest(t *testing.T) {
	rec := post(t, newHandler("error"), "/chat/completions")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "scripted provider failure") {
		t.Errorf("body = %s, want the scripted error message", rec.Body.String())
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	if rec := post(t, newHandler("success"), "/v1/other"); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestRunRejectsInvalidMode(t *testing.T) {
	if got := run([]string{"-mode", "flaky"}, io.Discard); got != 2 {
		t.Errorf("run = %d, want 2", got)
	}
}
