package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func post(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"stream":true}`)))
	return rec
}

func TestChatCompletions_SuccessMode_StreamsOneFinalMessage(t *testing.T) {
	must := require.New(t)
	rec := post(t, newHandler("success"), "/chat/completions")
	must.Equal(http.StatusOK, rec.Code)

	body := rec.Body.String()
	tests := []struct {
		name string
		want string
	}{
		{name: "final message", want: finalMessage},
		{name: "stop finish reason", want: `"finish_reason":"stop"`},
		{name: "prompt tokens", want: `"prompt_tokens":10`},
		{name: "done terminator", want: "data: [DONE]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Contains(body, tc.want)
		})
	}
}

func TestChatCompletions_EmptyMode_StreamsMessageWithoutContent(t *testing.T) {
	must := require.New(t)
	rec := post(t, newHandler("empty"), "/chat/completions")
	must.Equal(http.StatusOK, rec.Code)

	body := rec.Body.String()
	must.NotContains(body, finalMessage)
	must.Contains(body, `"content":""`)
	must.Contains(body, `"finish_reason":"stop"`)
}

func TestChatCompletions_ErrorMode_RejectsWithBadRequest(t *testing.T) {
	must := require.New(t)
	rec := post(t, newHandler("error"), "/chat/completions")
	must.Equal(http.StatusBadRequest, rec.Code)
	must.Contains(rec.Body.String(), "scripted provider failure")
}

func TestHandler_UnknownPath_ReturnsNotFound(t *testing.T) {
	must := require.New(t)
	rec := post(t, newHandler("success"), "/v1/other")
	must.Equal(http.StatusNotFound, rec.Code)
}

func TestRun_InvalidMode_ReturnsExitCodeTwo(t *testing.T) {
	must := require.New(t)
	must.Equal(2, run([]string{"-mode", "flaky"}, io.Discard))
}
