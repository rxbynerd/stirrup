package guard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

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

func TestGraniteGuardianReportsResponseUsage(t *testing.T) {
	srv := newUsageGraniteServer(t, `{"choices":[{"message":{"content":"<score>no</score>"}}],"usage":{"prompt_tokens":412,"completion_tokens":7}}`)
	var reported []types.TokenUsage
	g, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint:    srv.URL,
		ReportUsage: func(u types.TokenUsage) { reported = append(reported, u) },
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if _, err := g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "benign output"}); err != nil {
		t.Fatalf("Check: %v", err)
	}
	want := types.TokenUsage{Input: 412, Output: 7}
	if len(reported) != 1 || reported[0] != want {
		t.Errorf("reported usage = %+v, want one report of %+v", reported, want)
	}
}

func TestGraniteGuardianReportsUsageOfUnparseableVerdict(t *testing.T) {
	srv := newUsageGraniteServer(t, `{"choices":[{"message":{"content":""},"finish_reason":"length"}],"usage":{"prompt_tokens":300,"completion_tokens":64}}`)
	var reported types.TokenUsage
	g, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint:    srv.URL,
		ReportUsage: func(u types.TokenUsage) { reported.Add(u) },
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if _, err := g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "benign output"}); !errors.Is(err, ErrResponseTruncated) {
		t.Fatalf("Check error = %v, want ErrResponseTruncated", err)
	}
	if want := (types.TokenUsage{Input: 300, Output: 64}); reported != want {
		t.Errorf("reported usage = %+v, want %+v", reported, want)
	}
}

func TestGraniteGuardianSkipsUsageReportWithoutUsage(t *testing.T) {
	srv := newUsageGraniteServer(t, `{"choices":[{"message":{"content":"<score>no</score>"}}],"usage":{"prompt_tokens":-5,"completion_tokens":0}}`)
	calls := 0
	g, err := NewGraniteGuardian(GraniteGuardianConfig{
		Endpoint:    srv.URL,
		ReportUsage: func(types.TokenUsage) { calls++ },
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if _, err := g.Check(context.Background(), Input{Phase: PhasePostTurn, Content: "benign output"}); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if calls != 0 {
		t.Errorf("ReportUsage called %d times for a response with no positive usage, want 0", calls)
	}
}
