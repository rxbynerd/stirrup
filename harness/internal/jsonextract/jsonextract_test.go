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
	must := require.New(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "nonce_extraction_vectors.json"))
	must.NoError(err, "read vectors")

	var v extractionVectors
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	must.NoError(dec.Decode(&v), "decode vectors")
	must.NotEmpty(v.Cases, "vectors file has no cases")

	for _, tc := range v.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			must := require.New(t)
			hasError := tc.Error != ""
			hasMembers := len(tc.Members) > 0
			must.NotEqual(hasError, hasMembers, "case must set exactly one of members and error")

			members, err := ObjectWithNonce(tc.Input, tc.Nonce)
			if hasError {
				want, ok := vectorErrors[tc.Error]
				must.True(ok, "unknown error name %q", tc.Error)
				must.ErrorIs(err, want)
				return
			}

			must.NoError(err)
			got, err := json.Marshal(members)
			must.NoError(err, "marshal members")
			must.True(sameJSON(got, tc.Members), "members = %s, want %s", got, tc.Members)
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
			must := require.New(t)
			start := time.Now()
			_, err := ObjectWithNonce(in, nonce)
			must.ErrorIs(err, ErrScanBudget)

			elapsed := time.Since(start)
			must.LessOrEqual(elapsed, 5*time.Second, "the scan budget is not bounding work")
		})
	}
}
