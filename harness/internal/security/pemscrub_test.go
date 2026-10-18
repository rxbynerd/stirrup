package security

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// pemBodyLines returns n deterministic 64-character base64 lines, the
// shape of a PEM body.
func pemBodyLines(n int) []string {
	raw := make([]byte, 48*n)
	for i := range raw {
		raw[i] = byte(i*131 + i/7)
	}
	enc := base64.StdEncoding.EncodeToString(raw)
	lines := make([]string, n)
	for i := range lines {
		lines[i] = enc[i*64 : (i+1)*64]
	}
	return lines
}

func pemKey(label string, body []string, sep string) string {
	return "-----BEGIN " + label + "-----" + sep + strings.Join(body, sep) + sep + "-----END " + label + "-----"
}

// rsa4096PEMFixture is a PKCS#1 key the size of an RSA-4096 key.
func rsa4096PEMFixture() string {
	return pemKey("RSA PRIVATE KEY", pemBodyLines(51), "\n")
}

func assertNoPEMBody(t *testing.T, got string, body []string) {
	t.Helper()
	for _, line := range body {
		if strings.Contains(got, line) {
			t.Fatalf("key body line %q survived: %q", line, got)
		}
	}
}

func TestScrub_PEMKeyRedactedThroughEndLine(t *testing.T) {
	body := pemBodyLines(51)
	encrypted := "-----BEGIN RSA PRIVATE KEY-----\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC,0123456789ABCDEF0123456789ABCDEF\n\n" +
		strings.Join(body, "\n") + "\n-----END RSA PRIVATE KEY-----"
	indented := "    " + pemKey("EC PRIVATE KEY", body, "\n    ")
	cases := map[string]struct{ in, want string }{
		"pkcs1":     {"before\n" + rsa4096PEMFixture() + "\nafter", "before\n[REDACTED]\nafter"},
		"pkcs8":     {"key: " + pemKey("PRIVATE KEY", body, "\n") + " done", "key: [REDACTED] done"},
		"openssh":   {pemKey("OPENSSH PRIVATE KEY", body, "\n"), "[REDACTED]"},
		"crlf":      {pemKey("PRIVATE KEY", body, "\r\n") + "\r\n", "[REDACTED]\r\n"},
		"encrypted": {encrypted + "\n", "[REDACTED]\n"},
		"indented":  {"tls.key: |\n" + indented + "\nnext: 1", "tls.key: |\n    [REDACTED]\nnext: 1"},
		"json escapes": {
			`{"private_key": "` + pemKey("PRIVATE KEY", body, `\n`) + `\n", "client_email": "a@b.c"}`,
			`{"private_key": "[REDACTED]\n", "client_email": "a@b.c"}`,
		},
		"two keys": {
			pemKey("RSA PRIVATE KEY", body, "\n") + "\nbetween the keys\n" + pemKey("EC PRIVATE KEY", body, "\n"),
			"[REDACTED]\nbetween the keys\n[REDACTED]",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := Scrub(tc.in)
			assertNoPEMBody(t, got, body)
			if got != tc.want {
				t.Errorf("Scrub = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestScrub_PEMKeyWithoutEndLine pins the truncated case: the header and
// the base64 lines after it are redacted, and the prose after them is not
// swallowed.
func TestScrub_PEMKeyWithoutEndLine(t *testing.T) {
	body := pemBodyLines(20)
	cases := map[string]struct{ in, want string }{
		"header only": {"-----BEGIN RSA PRIVATE KEY-----", "[REDACTED]"},
		"header in prose": {
			"files starting -----BEGIN RSA PRIVATE KEY----- are keys",
			"files starting [REDACTED] are keys",
		},
		"truncated body": {
			"-----BEGIN PRIVATE KEY-----\n" + strings.Join(body, "\n") + "\n(output truncated)",
			"[REDACTED](output truncated)",
		},
		"truncated indented body": {
			"  -----BEGIN PRIVATE KEY-----\n  " + strings.Join(body, "\n  ") + "\n\nnext: value",
			"  [REDACTED]: value",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := Scrub(tc.in)
			assertNoPEMBody(t, got, body)
			if got != tc.want {
				t.Errorf("Scrub = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestScrub_PEMShapesStayLinear pins that Scrub stays linear on
// tool-result-sized PEM-shaped input, including headers whose body never
// reaches an END line, which an unanchored body scan revisits once per
// header if it can run past the next armour line.
func TestScrub_PEMShapesStayLinear(t *testing.T) {
	const size = 100 << 10
	key := rsa4096PEMFixture() + "\n"
	line := pemBodyLines(1)[0]
	shapes := map[string]string{
		"headers":                strings.Repeat("-----BEGIN RSA PRIVATE KEY-----", size/31),
		"BEGIN":                  strings.Repeat("-----BEGIN ", size/11),
		"END":                    strings.Repeat("-----END RSA PRIVATE KEY-----", size/29),
		"keys":                   strings.Repeat(key, size/len(key)),
		"headers with base64":    strings.Repeat("-----BEGIN A KEY-----\n"+line+"\n", size/87),
		"headers with prose":     strings.Repeat("-----BEGIN A KEY----- some words that never end ", size/48),
		"one header, hyphens":    "-----BEGIN A KEY-----" + strings.Repeat("A-", size/2),
		"one header, base64":     "-----BEGIN A KEY-----\n" + strings.Repeat(line+"\n", size/65),
		"one header, prose":      "-----BEGIN A KEY-----" + strings.Repeat("word ", size/5),
		"BEGIN headers, no KEY":  strings.Repeat("-----BEGIN "+strings.Repeat("w", 64), size/75),
		"header then many ENDs":  "-----BEGIN A KEY-----" + strings.Repeat("\n-----END B", size/11),
		"bodies split by dashes": strings.Repeat("-----BEGIN A KEY-----"+strings.Repeat("Q", 200)+"--", size/223),
	}
	for name, in := range shapes {
		start := time.Now()
		Scrub(in)
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("Scrub of 100 KiB of %q took %v", name, elapsed)
		}
	}
}

func TestDetectSensitive_PEMFindingCoversKeyBody(t *testing.T) {
	key := rsa4096PEMFixture()
	in := "cat id_rsa:\n" + key + "\n"
	var found bool
	for _, f := range DetectSensitive(in) {
		if f.Name != "secret/pem_private_key" {
			continue
		}
		found = true
		if f.Tier != TierLatch || in[f.Start:f.End] != key {
			t.Errorf("finding = %+v covering %q, want a latch finding over the whole key", f, in[f.Start:f.End])
		}
	}
	if !found {
		t.Fatal("no pem_private_key finding")
	}
}
