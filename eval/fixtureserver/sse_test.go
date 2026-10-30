package fixtureserver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func writeStream(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "turns.sse")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func post(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	must := require.New(t)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(`{}`))
	must.NoError(err)
	resp, err := (&http.Client{}).Do(req)
	must.NoError(err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	must.NoError(err)
	return resp, string(body)
}

func TestLoadTurns_SplitsOnSeparatorLine(t *testing.T) {
	must := require.New(t)
	turns, err := LoadTurns(writeStream(t, "data: a\n\ndata: b\n\n---\n\ndata: c\n---\n"))
	must.NoError(err)
	must.Equal([]string{"data: a\n\ndata: b\n\n", "data: c\n\n"}, turns)
}

func TestLoadTurns_NormalizesCRLFLineEndings(t *testing.T) {
	must := require.New(t)
	turns, err := LoadTurns(writeStream(t, "data: a\r\n\r\ndata: b\r\n\r\n---\r\ndata: c\r\n"))
	must.NoError(err)
	must.Equal([]string{"data: a\n\ndata: b\n\n", "data: c\n\n"}, turns)
}

func TestLoadTurns_Errors(t *testing.T) {
	tests := []struct {
		name string
		path func(t *testing.T) string
		want string
	}{
		{"empty path", func(*testing.T) string { return "" }, "stream file is required"},
		{"missing file", func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") }, "stream file"},
		{"directory", func(t *testing.T) string { return t.TempDir() }, "not a regular file"},
		{"no turn", func(t *testing.T) string { return writeStream(t, "\n---\n\n") }, "holds no turn"},
		{"oversized", func(t *testing.T) string { return writeStream(t, strings.Repeat("x", maxStreamFileBytes+1)) }, "byte cap"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, err := LoadTurns(tc.path(t))
			must.ErrorContains(err, tc.want)
		})
	}
}

func TestStart_SSE_ServesTurnsInOrderThenRefuses(t *testing.T) {
	must := require.New(t)
	server, err := Start(types.FixtureServerConfig{
		Mode:       types.FixtureModeSSE,
		StreamFile: writeStream(t, "data: one\n\n---\ndata: two\n\n"),
	})
	must.NoError(err)
	defer func() { _ = server.Close() }()
	must.Regexp(`^http://127\.0\.0\.1:\d+$`, server.URL)

	resp, body := post(t, server.URL+"/chat/completions")
	must.Equal(http.StatusOK, resp.StatusCode)
	must.Equal("text/event-stream", resp.Header.Get("Content-Type"))
	must.Equal("data: one\n\n", body)

	_, body = post(t, server.URL+"/responses")
	must.Equal("data: two\n\n", body)

	resp, body = post(t, server.URL+"/chat/completions")
	must.Equal(http.StatusBadRequest, resp.StatusCode)
	must.Contains(body, "request 3 but the stream file holds 2 turn(s)")
}

// recordingWriter notes each write and how many flushes had happened when
// it was made.
type recordingWriter struct {
	*httptest.ResponseRecorder
	flushes        int
	flushesAtWrite []int
	writes         []string
}

func (w *recordingWriter) Write(b []byte) (int, error) {
	w.writes = append(w.writes, string(b))
	w.flushesAtWrite = append(w.flushesAtWrite, w.flushes)
	return w.ResponseRecorder.Write(b)
}

func (w *recordingWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func (w *recordingWriter) Flush() { w.flushes++ }

// TestNewSSEHandler_WritesAndFlushesEachRecordSeparately pins that a turn is
// written one SSE record at a time with a flush after each, so a client
// reads split arguments as separate events rather than one buffered body.
func TestNewSSEHandler_WritesAndFlushesEachRecordSeparately(t *testing.T) {
	must := require.New(t)
	w := &recordingWriter{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(`{}`))
	newSSEHandler([]string{"data: first\n\ndata: second\n\n"}).ServeHTTP(w, req)

	must.Equal([]string{"data: first\n\n", "data: second\n\n"}, w.writes)
	must.Equal([]int{0, 1}, w.flushesAtWrite)
	must.Equal(2, w.flushes)
}

func TestStart_RejectsUnknownMode(t *testing.T) {
	must := require.New(t)
	_, err := Start(types.FixtureServerConfig{Mode: "grpc"})
	must.ErrorContains(err, `unknown fixture server mode "grpc"`)
}

func TestServer_Close_StopsListening(t *testing.T) {
	must := require.New(t)
	server, err := Start(types.FixtureServerConfig{
		Mode:       types.FixtureModeSSE,
		StreamFile: writeStream(t, "data: one\n\n"),
	})
	must.NoError(err)
	must.NoError(server.Close())

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, strings.NewReader(`{}`))
	must.NoError(err)
	resp, err := (&http.Client{}).Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	must.Error(err)
}
