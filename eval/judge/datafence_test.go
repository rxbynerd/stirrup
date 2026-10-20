package judge

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
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
	must := require.New(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "datafence_vectors.json"))
	must.NoError(err, "read vectors")
	var v fenceVectors
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	must.NoError(dec.Decode(&v), "decode vectors")
	must.NotEmpty(v.Fences, "vectors file has no fence cases")
	must.NotEmpty(v.Neutralise, "vectors file has no neutralise cases")

	for _, fv := range v.Fences {
		t.Run(fv.Name, func(t *testing.T) {
			must := require.New(t)
			entropy, err := hex.DecodeString(fv.Entropy)
			must.NoError(err, "entropy %q", fv.Entropy)
			f, err := newDataFence(bytes.NewReader(entropy))
			must.NoError(err)
			must.Equal(fv.Nonce, f.nonce)
			for _, m := range fv.Markers {
				must.Equal(m.Open, f.open(m.Label), "open(%s)", m.Label)
				must.Equal(m.Close, f.close(m.Label), "close(%s)", m.Label)
				must.Equal(m.Notice, f.notice(m.Label), "notice(%s)", m.Label)
			}
			for _, w := range fv.Wrap {
				must.Equal(w.Wrapped, f.wrap(w.Label, w.Content), "wrap(%s, %q)", w.Label, w.Content)
			}
		})
	}

	for i, n := range v.Neutralise {
		t.Run(fmt.Sprintf("neutralise vector %d", i), func(t *testing.T) {
			must := require.New(t)
			must.Equal(n.Output, neutraliseFenceMarkers(n.Input), "input %q", n.Input)
			must.NotContains(n.Output, "<<<")
		})
	}
}

func TestDataFence_NoncesDifferAndHaveFixedLength(t *testing.T) {
	must := require.New(t)
	a, err := newDataFence(rand.Reader)
	must.NoError(err)
	b, err := newDataFence(rand.Reader)
	must.NoError(err)
	must.NotEqual(a.nonce, b.nonce, "two fences share a nonce")
	must.Len(a.nonce, 2*fenceNonceBytes)
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestDataFence_EntropyFailureReturnsError(t *testing.T) {
	t.Run("exhausted reader", func(t *testing.T) {
		must := require.New(t)
		_, err := newDataFence(bytes.NewReader(nil))
		must.Error(err)
	})
	t.Run("reader error is wrapped", func(t *testing.T) {
		must := require.New(t)
		sentinel := errors.New("no entropy")
		_, err := newDataFence(errReader{sentinel})
		must.ErrorIs(err, sentinel)
	})
}
