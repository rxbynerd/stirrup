package jsonextract

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var v extractionVectors
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode vectors: %v", err)
	}
	if len(v.Cases) == 0 {
		t.Fatal("vectors file has no cases")
	}
	for _, tc := range v.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			if (tc.Error == "") == (len(tc.Members) == 0) {
				t.Fatalf("case must set exactly one of members and error")
			}
			members, err := ObjectWithNonce(tc.Input, tc.Nonce)
			if tc.Error != "" {
				want, ok := vectorErrors[tc.Error]
				if !ok {
					t.Fatalf("unknown error name %q", tc.Error)
				}
				if !errors.Is(err, want) {
					t.Fatalf("ObjectWithNonce = %v, %v; want error %v", members, err, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("ObjectWithNonce: %v", err)
			}
			got, err := json.Marshal(members)
			if err != nil {
				t.Fatalf("marshal members: %v", err)
			}
			if !sameJSON(got, tc.Members) {
				t.Fatalf("members = %s, want %s", got, tc.Members)
			}
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
			members, err := ObjectWithNonce(in, nonce)
			if !errors.Is(err, ErrScanBudget) {
				t.Fatalf("ObjectWithNonce = %v, %v; want ErrScanBudget", members, err)
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("extraction took %s; the scan budget is not bounding work", elapsed)
			}
		})
	}
}
