package calibrate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval/golden"
	"github.com/rxbynerd/stirrup/eval/judge"
	"github.com/rxbynerd/stirrup/types"
)

const seedPath = "../golden/diff-review-seed.json"

var fenceNonce = regexp.MustCompile(`<<<UNTRUSTED_DIFF_([0-9a-f]{32})>>>`)

// scriptedClient answers each call with the next verdict of a script,
// repeating the last, and records every prompt. "malformed" answers with
// text that is not a verdict.
type scriptedClient struct {
	mu      sync.Mutex
	script  []string
	prompts []string
}

func (c *scriptedClient) Complete(_ context.Context, req judge.JudgeRequest) (judge.JudgeResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	verdict := c.script[min(len(c.prompts), len(c.script)-1)]
	c.prompts = append(c.prompts, req.User)
	text := "no verdict here"
	if verdict != "malformed" {
		nonce := ""
		if m := fenceNonce.FindStringSubmatch(req.User); m != nil {
			nonce = m[1]
		}
		text = `{"nonce":"` + nonce + `","reasoning":"scripted","verdict":"` + verdict + `","feedback":"scripted"}`
	}
	return judge.JudgeResponse{Text: text, Model: "scripted-model", StopReason: "end_turn", InputTokens: 50, OutputTokens: 5}, nil
}

func (c *scriptedClient) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.prompts)
}

func (c *scriptedClient) factory() judge.ClientFactory {
	return func(types.JudgeLLMConfig, string) (judge.JudgeClient, error) { return c, nil }
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func loadSeed(t *testing.T) *golden.Set {
	t.Helper()
	must := require.New(t)
	s, err := golden.Load(seedPath)
	must.NoError(err)
	return s
}

// labels returns the seed's labels, each repeated repeats times, in the
// order Run judges them.
func labels(s *golden.Set, repeats int) []string {
	var out []string
	for _, c := range s.Cases {
		for range repeats {
			out = append(out, c.Label)
		}
	}
	return out
}

func testJudge() types.JudgeLLMConfig {
	return types.JudgeLLMConfig{Provider: types.JudgeProviderAnthropic, Model: "test-judge", APIKeyRef: "secret://CALIBRATE_TEST_KEY"}
}

func TestRun_OracleJudgeOverTheSeedSet(t *testing.T) {
	must := require.New(t)
	requireGit(t)
	t.Setenv("CALIBRATE_TEST_KEY", "k")
	set := loadSeed(t)
	oracle := &scriptedClient{script: labels(set, 2)}
	cache, err := judge.NewFileCache(t.TempDir(), judge.FileCacheOptions{Mode: judge.CacheReadThrough})
	must.NoError(err)
	cfg := Config{Judge: testJudge(), Repeats: 2, Options: judge.Options{Cache: cache, CacheMode: judge.CacheReadThrough, ClientFactory: oracle.factory()}}

	js, err := Run(context.Background(), set, cfg)
	must.NoError(err)
	must.Len(js, 48)
	must.Equal(48, oracle.calls())

	m := Summarize(js, nil)
	must.NotNil(m.Accuracy)
	must.Equal(48, m.Accuracy.K)
	must.NotNil(m.Kappa)
	must.True(near(*m.Kappa, 1), "kappa = %v, want 1", *m.Kappa)
	must.Equal(0, m.AdversarialFlipRate.K)
	must.Equal(16, m.AdversarialFlipRate.N)
	must.Equal(0, m.Errors)

	for i, j := range js {
		must.Equal(i%2, j.Sample, "judgment %d sample", i)
		must.NotNil(j.Record, "judgment %d record", i)
		must.Equal(types.JudgeCacheStored, j.Record.CacheStatus, "judgment %d cache status", i)
	}

	replayed := &scriptedClient{script: []string{"fail"}}
	cfg.Options.ClientFactory = replayed.factory()
	again, err := Run(context.Background(), set, cfg)
	must.NoError(err)
	must.Zero(replayed.calls(), "model calls on the repeat run")
	must.Equal(48, Summarize(again, nil).CacheHits, "cache hits on the repeat run")
	for i := range again {
		must.Equal(js[i].Verdict, again[i].Verdict, "replayed judgment %d", i)
	}
}

func TestRun_AlwaysPassJudgeOverTheSeedSet(t *testing.T) {
	must := require.New(t)
	requireGit(t)
	t.Setenv("CALIBRATE_TEST_KEY", "k")
	set := loadSeed(t)
	js, err := Run(context.Background(), set, Config{Judge: testJudge(), Options: judge.Options{ClientFactory: (&scriptedClient{script: []string{"pass"}}).factory()}})
	must.NoError(err)
	m := Summarize(js, nil)

	t.Run("true positive and negative rates", func(t *testing.T) {
		must := require.New(t)
		must.Equal(12, m.TPR.K)
		must.Equal(12, m.TPR.N)
		must.Equal(0, m.TNR.K)
		must.Equal(12, m.TNR.N)
	})

	t.Run("kappa", func(t *testing.T) {
		must := require.New(t)
		must.NotNil(m.Kappa)
		must.True(near(*m.Kappa, 0), "kappa = %v, want 0 for a judge that always passes", *m.Kappa)
	})

	t.Run("adversarial flip rate", func(t *testing.T) {
		must := require.New(t)
		must.Equal(6, m.AdversarialFlipRate.K, "six adversarial cases target pass")
		must.Equal(8, m.AdversarialFlipRate.N)
	})
}

func TestRun_ReconstructsEveryChangeForTheJudge(t *testing.T) {
	must := require.New(t)
	requireGit(t)
	t.Setenv("CALIBRATE_TEST_KEY", "k")
	set := loadSeed(t)
	client := &scriptedClient{script: []string{"pass"}}
	_, err := Run(context.Background(), set, Config{Judge: testJudge(), Options: judge.Options{ClientFactory: client.factory()}})
	must.NoError(err)

	for i, c := range set.Cases {
		t.Run(c.ID, func(t *testing.T) {
			must := require.New(t)
			prompt := client.prompts[i]
			must.Contains(prompt, c.Criteria)
			for _, line := range strings.Split(c.Diff, "\n") {
				changed := (strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++ ")) || (strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "--- "))
				if !changed {
					continue
				}
				must.Contains(prompt, "\n"+neutralised(line)+"\n", "the judge's diff is missing %q", neutralised(line))
				if strings.Contains(line, "<<<") {
					must.NotContains(prompt, line, "the planted fence marker reached the judge unneutralised")
				}
			}
		})
	}
}

// neutralised is s as the data fence carries it, with every run of '<'
// split into pairs by spaces so that no "<<<" survives.
func neutralised(s string) string {
	var b strings.Builder
	run := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '<' {
			if run == 2 {
				b.WriteByte(' ')
				run = 0
			}
			run++
		} else {
			run = 0
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func TestRun_MaterialisesCreatesDeletesAndFixtures(t *testing.T) {
	must := require.New(t)
	requireGit(t)
	t.Setenv("CALIBRATE_TEST_KEY", "k")
	dir := t.TempDir()
	writeFixture(t, dir+"/ws/before/kept.txt", "same\n")
	writeFixture(t, dir+"/ws/before/old.txt", "remove me\n")
	writeFixture(t, dir+"/ws/after/kept.txt", "same\n")
	writeFixture(t, dir+"/ws/after/nested/new.txt", "fixture addition\n")
	writeFixture(t, dir+"/gone/before/only.txt", "only line\n")
	src := `{"version":1,"name":"m","cases":[
	  {"id":"diff","criteria":"c1","label":"pass","diff":"--- /dev/null\n+++ b/created.txt\n@@ -0,0 +1 @@\n+created line\n--- a/deleted.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-deleted line\n"},
	  {"id":"fixture","criteria":"c2","label":"fail","workspace":"ws"},
	  {"id":"delete-all","criteria":"c3","label":"pass","workspace":"gone"}]}`
	set, err := golden.Parse([]byte(src), dir)
	must.NoError(err)

	client := &scriptedClient{script: []string{"pass", "fail", "pass"}}
	js, err := Run(context.Background(), set, Config{Judge: testJudge(), Options: judge.Options{ClientFactory: client.factory()}})
	must.NoError(err)
	must.Len(js, 3)
	must.Equal("pass", js[0].Verdict)
	must.Equal("fail", js[1].Verdict)
	must.Equal("pass", js[2].Verdict)

	cases := []struct {
		name  string
		wants []string
	}{
		{"diff", []string{"+created line", "-deleted line", "deleted file mode"}},
		{"fixture", []string{"+fixture addition", "-remove me", "nested/new.txt"}},
		{"delete-all", []string{"-only line", "deleted file mode"}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			for _, want := range tc.wants {
				must.Contains(client.prompts[i], want)
			}
		})
	}
	must.NotContains(client.prompts[1], "kept.txt", "an unchanged fixture file appears in the judge's diff")
}

func TestRun_RecordsJudgeErrorsAndContinues(t *testing.T) {
	must := require.New(t)
	requireGit(t)
	t.Setenv("CALIBRATE_TEST_KEY", "k")
	set := loadSeed(t)
	client := &scriptedClient{script: []string{"malformed", "pass"}}
	js, err := Run(context.Background(), set, Config{Judge: testJudge(), Options: judge.Options{ClientFactory: client.factory()}})
	must.NoError(err)
	must.Len(js, 24)
	must.Equal(types.JudgeStatusError, js[0].Verdict)
	must.NotEmpty(js[0].Reason)
	must.Equal(types.JudgeStatusPass, js[1].Verdict)

	m := Summarize(js, nil)
	must.Equal(1, m.Errors)
	must.Equal(23, m.Decided)
	must.Equal(1, m.ErrorRate.K)
}

func TestRun_StopsWhenCancelled(t *testing.T) {
	must := require.New(t)
	requireGit(t)
	t.Setenv("CALIBRATE_TEST_KEY", "k")
	ctx, cancel := context.WithCancel(context.Background())
	client := &cancellingClient{cancel: cancel, after: 3}
	js, err := Run(ctx, loadSeed(t), Config{Judge: testJudge(), Options: judge.Options{ClientFactory: func(types.JudgeLLMConfig, string) (judge.JudgeClient, error) { return client, nil }}})
	must.ErrorIs(err, context.Canceled)
	must.Len(js, 3)
}

// cancellingClient passes every case and cancels the run after a number
// of calls, failing that call when failOnCancel is set.
type cancellingClient struct {
	cancel       func()
	after        int
	calls        int
	failOnCancel bool
}

func (c *cancellingClient) Complete(ctx context.Context, req judge.JudgeRequest) (judge.JudgeResponse, error) {
	c.calls++
	if c.calls == c.after {
		c.cancel()
		if c.failOnCancel {
			return judge.JudgeResponse{}, ctx.Err()
		}
	}
	nonce := fenceNonce.FindStringSubmatch(req.User)[1]
	return judge.JudgeResponse{Text: `{"nonce":"` + nonce + `","reasoning":"r","verdict":"pass","feedback":"f"}`, Model: "m", StopReason: "end_turn"}, nil
}

func TestRun_DecisionJudgeOverTheSeedSet(t *testing.T) {
	must := require.New(t)
	requireGit(t)
	var mu sync.Mutex
	var states []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			State string `json:"state"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		states = append(states, body.State)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"model":"jev-stub","answers":{"verdict":{"type":"choice","choice":"pass","probabilities":{"pass":0.8,"fail":0.2},"confidence":0.6}},"usage":{"input_tokens":300,"output_tokens":4}}`)
	}))
	defer srv.Close()
	cfg := types.JudgeLLMConfig{Provider: types.JudgeProviderDecision, Model: "jev-latest", BaseURL: srv.URL}

	js, err := Run(context.Background(), loadSeed(t), Config{Judge: cfg})
	must.NoError(err)
	must.Len(js, 24)
	must.Len(states, 24)
	for i, j := range js {
		must.Equal(types.JudgeStatusPass, j.Verdict, "judgment %d verdict", i)
		must.NotNil(j.Record, "judgment %d record", i)
		must.Equal(types.JudgeProviderDecision, j.Record.Provider, "judgment %d provider", i)
		must.Equal("jev-stub", j.Record.ServedModel, "judgment %d served model", i)
		must.Regexp(fenceNonce, states[i], "request %d state is not fenced", i)
	}

	m := Summarize(js, nil)
	must.Equal(24*300, m.Tokens.Input)
	must.Equal(0, m.TNR.K)
}

func TestRun_CancelledDuringTheLastJudgmentFails(t *testing.T) {
	must := require.New(t)
	requireGit(t)
	t.Setenv("CALIBRATE_TEST_KEY", "k")
	set := loadSeed(t)
	ctx, cancel := context.WithCancel(context.Background())
	client := &cancellingClient{cancel: cancel, after: len(set.Cases), failOnCancel: true}
	js, err := Run(ctx, set, Config{Judge: testJudge(), Options: judge.Options{ClientFactory: func(types.JudgeLLMConfig, string) (judge.JudgeClient, error) { return client, nil }}})
	must.ErrorIs(err, context.Canceled)
	must.Len(js, len(set.Cases)-1, "the interrupted judgment is not recorded")
}

func TestRun_FailsWhenACaseCannotBeMaterialised(t *testing.T) {
	must := require.New(t)
	requireGit(t)
	t.Setenv("CALIBRATE_TEST_KEY", "k")
	dir := t.TempDir()
	writeFixture(t, dir+"/ws/before/a.txt", "a\n")
	writeFixture(t, dir+"/ws/after/a.txt", "b\n")
	src := `{"version":1,"name":"m","cases":[
	  {"id":"first","criteria":"c","label":"pass","diff":"--- a/f\n+++ b/f\n@@ -1 +1 @@\n-x\n+y\n"},
	  {"id":"fixture","criteria":"c","label":"pass","workspace":"ws"}]}`
	set, err := golden.Parse([]byte(src), dir)
	must.NoError(err)
	must.NoError(os.RemoveAll(filepath.Join(dir, "ws")))

	js, err := Run(context.Background(), set, Config{Judge: testJudge(), Options: judge.Options{ClientFactory: (&scriptedClient{script: []string{"pass"}}).factory()}})

	must.Error(err)
	must.True(strings.HasPrefix(err.Error(), "case fixture: workspace"), "error = %v", err)
	must.Len(js, 1)
	must.Equal("first", js[0].Case)
}

func TestRun_FailsWhenTheWorkDirIsUnusable(t *testing.T) {
	must := require.New(t)
	notADir := filepath.Join(t.TempDir(), "file")
	writeFixture(t, notADir, "x")

	js, err := Run(context.Background(), loadSeed(t), Config{Judge: testJudge(), WorkDir: notADir})

	must.ErrorContains(err, "creating workspace")
	must.Empty(js)
}
