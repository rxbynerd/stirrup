package calibrate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"

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
	s, err := golden.Load(seedPath)
	if err != nil {
		t.Fatal(err)
	}
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
	requireGit(t)
	t.Setenv("CALIBRATE_TEST_KEY", "k")
	set := loadSeed(t)
	oracle := &scriptedClient{script: labels(set, 2)}
	cache, err := judge.NewFileCache(t.TempDir(), judge.FileCacheOptions{Mode: judge.CacheReadThrough})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Judge: testJudge(), Repeats: 2, Options: judge.Options{Cache: cache, CacheMode: judge.CacheReadThrough, ClientFactory: oracle.factory()}}

	js, err := Run(context.Background(), set, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(js) != 48 || oracle.calls() != 48 {
		t.Fatalf("%d judgments from %d model calls, want 48 of each", len(js), oracle.calls())
	}
	m := Summarize(js, nil)
	if m.Accuracy == nil || m.Accuracy.K != 48 || m.Kappa == nil || !near(*m.Kappa, 1) || m.AdversarialFlipRate.K != 0 || m.AdversarialFlipRate.N != 16 || m.Errors != 0 {
		t.Errorf("oracle metrics = %+v", m)
	}
	for i, j := range js {
		if j.Sample != i%2 || j.Record == nil || j.Record.CacheStatus != types.JudgeCacheStored {
			t.Fatalf("judgment %d = %+v, want sample %d stored in the cache", i, j, i%2)
		}
	}

	replayed := &scriptedClient{script: []string{"fail"}}
	cfg.Options.ClientFactory = replayed.factory()
	again, err := Run(context.Background(), set, cfg)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if replayed.calls() != 0 || Summarize(again, nil).CacheHits != 48 {
		t.Errorf("repeat run made %d model calls and %d cache hits, want 0 and 48", replayed.calls(), Summarize(again, nil).CacheHits)
	}
	for i := range again {
		if again[i].Verdict != js[i].Verdict {
			t.Fatalf("replayed judgment %d = %s, recorded %s", i, again[i].Verdict, js[i].Verdict)
		}
	}
}

func TestRun_AlwaysPassJudgeOverTheSeedSet(t *testing.T) {
	requireGit(t)
	t.Setenv("CALIBRATE_TEST_KEY", "k")
	set := loadSeed(t)
	js, err := Run(context.Background(), set, Config{Judge: testJudge(), Options: judge.Options{ClientFactory: (&scriptedClient{script: []string{"pass"}}).factory()}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	m := Summarize(js, nil)
	if m.TPR.K != 12 || m.TPR.N != 12 || m.TNR.K != 0 || m.TNR.N != 12 {
		t.Errorf("TPR %+v TNR %+v, want 12/12 and 0/12", m.TPR, m.TNR)
	}
	if m.Kappa == nil || !near(*m.Kappa, 0) {
		t.Errorf("kappa = %v, want 0 for a judge that always passes", m.Kappa)
	}
	if m.AdversarialFlipRate.K != 6 || m.AdversarialFlipRate.N != 8 {
		t.Errorf("flip rate = %+v, want 6/8: six adversarial cases target pass", m.AdversarialFlipRate)
	}
}

func TestRun_ReconstructsEveryChangeForTheJudge(t *testing.T) {
	requireGit(t)
	t.Setenv("CALIBRATE_TEST_KEY", "k")
	set := loadSeed(t)
	client := &scriptedClient{script: []string{"pass"}}
	if _, err := Run(context.Background(), set, Config{Judge: testJudge(), Options: judge.Options{ClientFactory: client.factory()}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i, c := range set.Cases {
		prompt := client.prompts[i]
		if !strings.Contains(prompt, c.Criteria) {
			t.Errorf("%s: prompt does not carry the criteria", c.ID)
		}
		for _, line := range strings.Split(c.Diff, "\n") {
			changed := (strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++ ")) || (strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "--- "))
			if !changed {
				continue
			}
			if !strings.Contains(prompt, "\n"+neutralised(line)+"\n") {
				t.Errorf("%s: the judge's diff is missing %q", c.ID, neutralised(line))
			}
			if strings.Contains(line, "<<<") && strings.Contains(prompt, line) {
				t.Errorf("%s: the planted fence marker %q reached the judge unneutralised", c.ID, line)
			}
		}
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
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedClient{script: []string{"pass", "fail", "pass"}}
	js, err := Run(context.Background(), set, Config{Judge: testJudge(), Options: judge.Options{ClientFactory: client.factory()}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(js) != 3 || js[0].Verdict != "pass" || js[1].Verdict != "fail" || js[2].Verdict != "pass" {
		t.Fatalf("judgments = %+v", js)
	}
	for i, wants := range [][]string{
		{"+created line", "-deleted line", "deleted file mode"},
		{"+fixture addition", "-remove me", "nested/new.txt"},
		{"-only line", "deleted file mode"},
	} {
		for _, want := range wants {
			if !strings.Contains(client.prompts[i], want) {
				t.Errorf("case %d prompt is missing %q", i+1, want)
			}
		}
	}
	if strings.Contains(client.prompts[1], "kept.txt") {
		t.Error("an unchanged fixture file appears in the judge's diff")
	}
}

func TestRun_RecordsJudgeErrorsAndContinues(t *testing.T) {
	requireGit(t)
	t.Setenv("CALIBRATE_TEST_KEY", "k")
	set := loadSeed(t)
	client := &scriptedClient{script: []string{"malformed", "pass"}}
	js, err := Run(context.Background(), set, Config{Judge: testJudge(), Options: judge.Options{ClientFactory: client.factory()}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(js) != 24 || js[0].Verdict != types.JudgeStatusError || js[0].Reason == "" || js[1].Verdict != types.JudgeStatusPass {
		t.Fatalf("judgments = %+v, want an error then passes", js[:2])
	}
	if m := Summarize(js, nil); m.Errors != 1 || m.Decided != 23 || m.ErrorRate.K != 1 {
		t.Errorf("metrics = %+v", m)
	}
}

func TestRun_StopsWhenCancelled(t *testing.T) {
	requireGit(t)
	t.Setenv("CALIBRATE_TEST_KEY", "k")
	ctx, cancel := context.WithCancel(context.Background())
	client := &cancellingClient{cancel: cancel, after: 3}
	js, err := Run(ctx, loadSeed(t), Config{Judge: testJudge(), Options: judge.Options{ClientFactory: func(types.JudgeLLMConfig, string) (judge.JudgeClient, error) { return client, nil }}})
	if !errors.Is(err, context.Canceled) || len(js) != 3 {
		t.Fatalf("Run = %d judgments, err %v; want 3 and context.Canceled", len(js), err)
	}
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
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(js) != 24 || len(states) != 24 {
		t.Fatalf("%d judgments from %d requests, want 24", len(js), len(states))
	}
	for i, j := range js {
		if j.Verdict != types.JudgeStatusPass || j.Record == nil || j.Record.Provider != types.JudgeProviderDecision || j.Record.ServedModel != "jev-stub" {
			t.Fatalf("judgment %d = %+v", i, j)
		}
		if !fenceNonce.MatchString(states[i]) {
			t.Errorf("request %d state is not fenced: %q", i, states[i])
		}
	}
	if m := Summarize(js, nil); m.Tokens.Input != 24*300 || m.TNR.K != 0 {
		t.Errorf("metrics = %+v", m)
	}
}

func TestRun_CancelledDuringTheLastJudgmentFails(t *testing.T) {
	requireGit(t)
	t.Setenv("CALIBRATE_TEST_KEY", "k")
	set := loadSeed(t)
	ctx, cancel := context.WithCancel(context.Background())
	client := &cancellingClient{cancel: cancel, after: len(set.Cases), failOnCancel: true}
	js, err := Run(ctx, set, Config{Judge: testJudge(), Options: judge.Options{ClientFactory: func(types.JudgeLLMConfig, string) (judge.JudgeClient, error) { return client, nil }}})
	if !errors.Is(err, context.Canceled) || len(js) != len(set.Cases)-1 {
		t.Fatalf("Run = %d judgments, err %v; want %d and context.Canceled without the interrupted judgment", len(js), err, len(set.Cases)-1)
	}
}
