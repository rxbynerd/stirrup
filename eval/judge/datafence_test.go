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
	raw, err := os.ReadFile(filepath.Join("testdata", "datafence_vectors.json"))
	require.NoError(t, err, "read vectors")
	var v fenceVectors
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&v), "decode vectors")
	require.NotEmpty(t, v.Fences, "vectors file has no fence cases")
	require.NotEmpty(t, v.Neutralise, "vectors file has no neutralise cases")

	for _, fv := range v.Fences {
		t.Run(fv.Name, func(t *testing.T) {
			entropy, err := hex.DecodeString(fv.Entropy)
			require.NoError(t, err, "entropy %q", fv.Entropy)
			f, err := newDataFence(bytes.NewReader(entropy))
			require.NoError(t, err)
			require.Equal(t, fv.Nonce, f.nonce)
			for _, m := range fv.Markers {
				require.Equal(t, m.Open, f.open(m.Label), "open(%s)", m.Label)
				require.Equal(t, m.Close, f.close(m.Label), "close(%s)", m.Label)
				require.Equal(t, m.Notice, f.notice(m.Label), "notice(%s)", m.Label)
			}
			for _, w := range fv.Wrap {
				require.Equal(t, w.Wrapped, f.wrap(w.Label, w.Content), "wrap(%s, %q)", w.Label, w.Content)
			}
		})
	}

	for i, n := range v.Neutralise {
		t.Run(fmt.Sprintf("neutralise vector %d", i), func(t *testing.T) {
			require.Equal(t, n.Output, neutraliseFenceMarkers(n.Input), "input %q", n.Input)
			require.NotContains(t, n.Output, "<<<")
		})
	}
}

func TestDataFence_NoncesDifferAndHaveFixedLength(t *testing.T) {
	a, err := newDataFence(rand.Reader)
	require.NoError(t, err)
	b, err := newDataFence(rand.Reader)
	require.NoError(t, err)
	require.NotEqual(t, a.nonce, b.nonce, "two fences share a nonce")
	require.Len(t, a.nonce, 2*fenceNonceBytes)
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestDataFence_EntropyFailureReturnsError(t *testing.T) {
	t.Run("exhausted reader", func(t *testing.T) {
		_, err := newDataFence(bytes.NewReader(nil))
		require.Error(t, err)
	})
	t.Run("reader error is wrapped", func(t *testing.T) {
		sentinel := errors.New("no entropy")
		_, err := newDataFence(errReader{sentinel})
		require.ErrorIs(t, err, sentinel)
	})
}
