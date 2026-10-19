package guard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func newUsageGraniteServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGraniteGuardian_ReportsResponseUsage(t *testing.T) {
	srv := newUsageGraniteServer(t, `{"choices":[{"message":{"content":"<score>no</score>"}}],"usage":{"prompt_tokens":412,"completion_tokens":7}}`)
	var reported []types.TokenUsage
	g, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint:    srv.URL,
		ReportUsage: func(u types.TokenUsage) { reported = append(reported, u) },
	})
	require.NoError(t, err)

	_, err = g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "benign output"})
	require.NoError(t, err)

	want := types.TokenUsage{Input: 412, Output: 7}
	require.Equal(t, []types.TokenUsage{want}, reported)
}

func TestGraniteGuardian_ReportsUsageOfUnparseableVerdict(t *testing.T) {
	srv := newUsageGraniteServer(t, `{"choices":[{"message":{"content":""},"finish_reason":"length"}],"usage":{"prompt_tokens":300,"completion_tokens":64}}`)
	var reported types.TokenUsage
	g, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint:    srv.URL,
		ReportUsage: func(u types.TokenUsage) { reported.Add(u) },
	})
	require.NoError(t, err)

	_, err = g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "benign output"})
	require.ErrorIs(t, err, ErrResponseTruncated)

	require.Equal(t, types.TokenUsage{Input: 300, Output: 64}, reported)
}

func TestGraniteGuardian_SkipsUsageReportWithoutPositiveUsage(t *testing.T) {
	srv := newUsageGraniteServer(t, `{"choices":[{"message":{"content":"<score>no</score>"}}],"usage":{"prompt_tokens":-5,"completion_tokens":0}}`)
	calls := 0
	g, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint:    srv.URL,
		ReportUsage: func(types.TokenUsage) { calls++ },
	})
	require.NoError(t, err)

	_, err = g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "benign output"})
	require.NoError(t, err)

	require.Zero(t, calls, "ReportUsage called for a response with no positive usage")
}
