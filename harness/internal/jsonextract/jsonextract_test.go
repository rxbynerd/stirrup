package jsonextract

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// nonce_extraction_vectors.json is mirrored byte-for-byte in eval/judge/testdata; change both copies together.
type extractionVectors struct {
	Comment string `json:"comment"`
	Cases   []struct {
		Name    string          `json:"name"`
		Input   string          `json:"input"`
		Nonce   string          `json:"nonce"`
		Members json.RawMessage `json:"members"`
		Error   string          `json:"error"`
	} `json:"cases"`
}

var vectorErrors = map[string]error{
	"empty_nonce": ErrEmptyNonce,
	"no_match":    ErrNoNonceObject,
	"conflict":    ErrConflictingObjects,
}

func TestObjectWithNonce_Vectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "nonce_extraction_vectors.json"))
	require.NoError(t, err, "read vectors")

	var v extractionVectors
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&v), "decode vectors")
	require.NotEmpty(t, v.Cases, "vectors file has no cases")

	for _, tc := range v.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			hasError := tc.Error != ""
			hasMembers := len(tc.Members) > 0
			require.NotEqual(t, hasError, hasMembers, "case must set exactly one of members and error")

			members, err := ObjectWithNonce(tc.Input, tc.Nonce)
			if hasError {
				want, ok := vectorErrors[tc.Error]
				require.True(t, ok, "unknown error name %q", tc.Error)
				require.ErrorIs(t, err, want)
				return
			}

			require.NoError(t, err)
			got, err := json.Marshal(members)
			require.NoError(t, err, "marshal members")
			require.True(t, sameJSON(got, tc.Members), "members = %s, want %s", got, tc.Members)
		})
	}
}

// TestObjectWithNonce_ScanBudgetFailsClosed pins that exhausting the scan
// budget is an error rather than "no object" or an earlier match, so a
// verdict is never chosen from a partial scan.
func TestObjectWithNonce_ScanBudgetFailsClosed(t *testing.T) {
	const nonce = "0123456789abcdef0123456789abcdef"
	verdict := `{"nonce":"` + nonce + `","verdict":"deny"}`
	for name, in := range map[string]string{
		"braces_before_match": strings.Repeat("{", 200_000) + verdict,
		"braces_after_match":  verdict + strings.Repeat("{", 200_000),
	} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			_, err := ObjectWithNonce(in, nonce)
			require.ErrorIs(t, err, ErrScanBudget)

			elapsed := time.Since(start)
			require.LessOrEqual(t, elapsed, 5*time.Second, "the scan budget is not bounding work")
		})
	}
}
