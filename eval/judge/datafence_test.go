package judge

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testdata/datafence_vectors.json is a byte-identical copy of
// harness/internal/security/testdata/datafence_vectors.json; change both
// copies together.
type fenceVectors struct {
	Comment string `json:"comment"`
	Fences  []struct {
		Name    string `json:"name"`
		Entropy string `json:"entropy"`
		Nonce   string `json:"nonce"`
		Markers []struct {
			Label  string `json:"label"`
			Open   string `json:"open"`
			Close  string `json:"close"`
			Notice string `json:"notice"`
		} `json:"markers"`
		Wrap []struct {
			Label   string `json:"label"`
			Content string `json:"content"`
			Wrapped string `json:"wrapped"`
		} `json:"wrap"`
	} `json:"fences"`
	Neutralise []struct {
		Input  string `json:"input"`
		Output string `json:"output"`
	} `json:"neutralise"`
}

func TestDataFence_GoldenVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "datafence_vectors.json"))
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var v fenceVectors
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode vectors: %v", err)
	}
	if len(v.Fences) == 0 || len(v.Neutralise) == 0 {
		t.Fatal("vectors file has no fence or neutralise cases")
	}

	for _, fv := range v.Fences {
		t.Run(fv.Name, func(t *testing.T) {
			entropy, err := hex.DecodeString(fv.Entropy)
			if err != nil {
				t.Fatalf("entropy %q: %v", fv.Entropy, err)
			}
			f, err := newDataFence(bytes.NewReader(entropy))
			if err != nil {
				t.Fatalf("newDataFence: %v", err)
			}
			if f.nonce != fv.Nonce {
				t.Errorf("nonce = %q, want %q", f.nonce, fv.Nonce)
			}
			for _, m := range fv.Markers {
				if got := f.open(m.Label); got != m.Open {
					t.Errorf("open(%s) = %q, want %q", m.Label, got, m.Open)
				}
				if got := f.close(m.Label); got != m.Close {
					t.Errorf("close(%s) = %q, want %q", m.Label, got, m.Close)
				}
				if got := f.notice(m.Label); got != m.Notice {
					t.Errorf("notice(%s) = %q, want %q", m.Label, got, m.Notice)
				}
			}
			for _, w := range fv.Wrap {
				if got := f.wrap(w.Label, w.Content); got != w.Wrapped {
					t.Errorf("wrap(%s, %q) = %q, want %q", w.Label, w.Content, got, w.Wrapped)
				}
			}
		})
	}

	for _, n := range v.Neutralise {
		if got := neutraliseFenceMarkers(n.Input); got != n.Output {
			t.Errorf("neutraliseFenceMarkers(%q) = %q, want %q", n.Input, got, n.Output)
		}
		if strings.Contains(n.Output, "<<<") {
			t.Errorf("vector output %q still contains <<<", n.Output)
		}
	}
}

func TestDataFence_NoncesDiffer(t *testing.T) {
	a, err := newDataFence(rand.Reader)
	if err != nil {
		t.Fatalf("newDataFence: %v", err)
	}
	b, err := newDataFence(rand.Reader)
	if err != nil {
		t.Fatalf("newDataFence: %v", err)
	}
	if a.nonce == b.nonce {
		t.Fatalf("two fences share a nonce: %q", a.nonce)
	}
	if len(a.nonce) != 2*fenceNonceBytes {
		t.Fatalf("nonce %q is not %d hex characters", a.nonce, 2*fenceNonceBytes)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestDataFence_EntropyFailure(t *testing.T) {
	if _, err := newDataFence(bytes.NewReader(nil)); err == nil {
		t.Fatal("expected an error when entropy is exhausted")
	}
	sentinel := errors.New("no entropy")
	if _, err := newDataFence(errReader{sentinel}); !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want wrapped sentinel", err)
	}
}
