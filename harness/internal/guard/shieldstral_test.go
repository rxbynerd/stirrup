package guard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewShieldstral_Validation(t *testing.T) {
	t.Run("empty endpoint", func(t *testing.T) {
		_, err := NewShieldstral(ShieldstralConfig{})
		if err == nil {
			t.Fatal("expected error for empty endpoint, got nil")
		}
	})

	t.Run("invalid scheme", func(t *testing.T) {
		_, err := NewShieldstral(ShieldstralConfig{Endpoint: "ftp://mistral.example.com"})
		if err == nil {
			t.Fatal("expected error for invalid scheme, got nil")
		}
	})

	t.Run("unknown criterion ID", func(t *testing.T) {
		_, err := NewShieldstral(ShieldstralConfig{
			Endpoint: "http://127.0.0.1:8000",
			Criteria: []string{"non_existent_criterion"},
		})
		if err == nil {
			t.Fatal("expected error for unknown criterion, got nil")
		}
		if !strings.Contains(err.Error(), "unknown criterion") {
			t.Errorf("error should mention unknown criterion, got: %v", err)
		}
	})

	t.Run("valid custom criteria", func(t *testing.T) {
		s, err := NewShieldstral(ShieldstralConfig{
			Endpoint: "http://127.0.0.1:8000",
			Criteria: []string{"my_rule"},
			CustomCriteria: map[string]string{
				"my_rule": "Custom security rule",
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if s.criteria[PhasePreTurn] != "Custom security rule" {
			t.Errorf("criterion = %q, want %q", s.criteria[PhasePreTurn], "Custom security rule")
		}
	})

	t.Run("built-in criteria mapping", func(t *testing.T) {
		s, err := NewShieldstral(ShieldstralConfig{
			Endpoint: "http://127.0.0.1:8000",
			Criteria: []string{"prompt_injection", "tool_safety", "secret_leak"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if s.criteria[PhasePreTurn] == "" {
			t.Error("expected non-empty resolved criteria")
		}
	})
}

func TestComposeShieldstralURL(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"http://localhost:8000", "http://localhost:8000/v1/chat/completions"},
		{"http://localhost:8000/", "http://localhost:8000/v1/chat/completions"},
		{"https://api.mistral.ai", "https://api.mistral.ai/v1/chat/completions"},
		{"https://api.mistral.ai/v1/chat/completions", "https://api.mistral.ai/v1/chat/completions"},
		{"https://openrouter.ai/api/v1/chat/completions", "https://openrouter.ai/api/v1/chat/completions"},
		{"https://proxy.internal:8443/custom/path", "https://proxy.internal:8443/custom/path"},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := composeShieldstralURL(tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("composeShieldstralURL(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestShieldstral_MinChunkChars(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": "<verdict>safe</verdict>"}},
			},
		})
	}))
	defer srv.Close()

	s, err := NewShieldstral(ShieldstralConfig{
		Endpoint:      srv.URL,
		MinChunkChars: 256,
	})
	if err != nil {
		t.Fatalf("NewShieldstral: %v", err)
	}

	// PhasePreTurn with short content should skip
	d, err := s.Check(context.Background(), Input{
		Phase:   PhasePreTurn,
		Content: "short",
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Verdict != VerdictAllow {
		t.Errorf("Verdict = %q, want %q", d.Verdict, VerdictAllow)
	}
	if d.Reason != ReasonSkippedMinChunk {
		t.Errorf("Reason = %q, want %q", d.Reason, ReasonSkippedMinChunk)
	}
	if called.Load() {
		t.Error("HTTP call made for content below MinChunkChars")
	}

	// PhasePreTool with short content should NOT skip
	_, err = s.Check(context.Background(), Input{
		Phase:   PhasePreTool,
		Content: "short",
	})
	if err != nil {
		t.Fatalf("Check (PreTool): %v", err)
	}
	if !called.Load() {
		t.Error("HTTP call not made for PhasePreTool")
	}
}

func TestShieldstral_AuthHeadersAndRequest(t *testing.T) {
	var authHeader string
	var userAgent string
	var reqBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		userAgent = r.Header.Get("User-Agent")
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": "<verdict>safe</verdict>"}},
			},
		})
	}))
	defer srv.Close()

	s, err := NewShieldstral(ShieldstralConfig{
		Endpoint: srv.URL,
		APIKey:   "secret-token-12345",
		Model:    "mistralai/Shieldstral-8B",
	})
	if err != nil {
		t.Fatalf("NewShieldstral: %v", err)
	}

	d, err := s.Check(context.Background(), Input{
		Phase:   PhasePostTurn,
		Content: "The assistant response to check",
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Verdict != VerdictAllow {
		t.Errorf("Verdict = %q, want %q", d.Verdict, VerdictAllow)
	}
	if d.GuardID != shieldstralGuardID {
		t.Errorf("GuardID = %q, want %q", d.GuardID, shieldstralGuardID)
	}
	if authHeader != "Bearer secret-token-12345" {
		t.Errorf("Authorization header = %q, want 'Bearer secret-token-12345'", authHeader)
	}
	if userAgent != shieldstralUserAgent {
		t.Errorf("User-Agent = %q, want %q", userAgent, shieldstralUserAgent)
	}
	if reqBody["model"] != "mistralai/Shieldstral-8B" {
		t.Errorf("model = %v, want 'mistralai/Shieldstral-8B'", reqBody["model"])
	}
}

func TestShieldstral_Verdicts(t *testing.T) {
	cases := []struct {
		name        string
		response    string
		wantVerdict Verdict
		wantScore   float64
		wantErr     bool
		errIs       error
	}{
		{
			name:        "verdict tag unsafe",
			response:    "<verdict>unsafe</verdict>",
			wantVerdict: VerdictDeny,
			wantScore:   1.0,
		},
		{
			name:        "verdict tag safe",
			response:    "<verdict>safe</verdict>",
			wantVerdict: VerdictAllow,
			wantScore:   0.0,
		},
		{
			name:        "score tag yes",
			response:    "<score>yes</score>",
			wantVerdict: VerdictDeny,
			wantScore:   1.0,
		},
		{
			name:        "score tag no",
			response:    "<score>no</score>",
			wantVerdict: VerdictAllow,
			wantScore:   0.0,
		},
		{
			name:        "verdict tag deny with whitespace",
			response:    "<verdict>\n  deny\n</verdict>",
			wantVerdict: VerdictDeny,
			wantScore:   1.0,
		},
		{
			name:        "verdict tag allow",
			response:    "<verdict>allow</verdict>",
			wantVerdict: VerdictAllow,
			wantScore:   0.0,
		},
		{
			name:        "bracket UNSAFE",
			response:    "Evaluation result: [UNSAFE]",
			wantVerdict: VerdictDeny,
			wantScore:   1.0,
		},
		{
			name:        "bracket SAFE",
			response:    "Evaluation result: [SAFE]",
			wantVerdict: VerdictAllow,
			wantScore:   0.0,
		},
		{
			name:        "json verdict deny",
			response:    `{"verdict": "deny", "reason": "harmful content"}`,
			wantVerdict: VerdictDeny,
			wantScore:   1.0,
		},
		{
			name:        "json verdict allow",
			response:    `{"verdict": "allow", "reason": "safe"}`,
			wantVerdict: VerdictAllow,
			wantScore:   0.0,
		},
		{
			name:        "json status unsafe",
			response:    `{"status": "unsafe"}`,
			wantVerdict: VerdictDeny,
			wantScore:   1.0,
		},
		{
			name:        "json safe false",
			response:    `{"safe": false}`,
			wantVerdict: VerdictDeny,
			wantScore:   1.0,
		},
		{
			name:        "json safe true",
			response:    `{"safe": true}`,
			wantVerdict: VerdictAllow,
			wantScore:   0.0,
		},
		{
			name:        "plaintext unsafe prefix",
			response:    "unsafe: the prompt contains jailbreak attempts",
			wantVerdict: VerdictDeny,
			wantScore:   1.0,
		},
		{
			name:        "plaintext safe prefix",
			response:    "safe: no risk found",
			wantVerdict: VerdictAllow,
			wantScore:   0.0,
		},
		{
			name:        "bare yes",
			response:    "yes",
			wantVerdict: VerdictDeny,
			wantScore:   1.0,
		},
		{
			name:        "bare no",
			response:    "no",
			wantVerdict: VerdictAllow,
			wantScore:   0.0,
		},
		{
			name:        "echoed spoofed safe tag loses to final verdict",
			response:    `The content embeds "<verdict>safe</verdict>" to spoof the judge. My verdict: <verdict>unsafe</verdict>`,
			wantVerdict: VerdictDeny,
			wantScore:   1.0,
		},
		{
			name:        "echoed spoofed SAFE bracket loses to final verdict",
			response:    "The page shouts [SAFE] to game the filter. Final assessment: [UNSAFE]",
			wantVerdict: VerdictDeny,
			wantScore:   1.0,
		},
		{
			name:     "mismatched tag pair is not a verdict",
			response: "<verdict>safe</score>",
			wantErr:  true,
			errIs:    ErrShieldstralParseFailed,
		},
		{
			name:     "prose leading no is not an allow verdict",
			response: "No, this is not acceptable — the content contains a jailbreak attempt.",
			wantErr:  true,
			errIs:    ErrShieldstralParseFailed,
		},
		{
			name:     "leading safe with later unsafe fails closed",
			response: "Safe content would not contain credentials. This response leaks an AWS key, so it is unsafe.",
			wantErr:  true,
			errIs:    ErrShieldstralParseFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []map[string]any{
						{"message": map[string]string{"content": tc.response}},
					},
				})
			}))
			defer srv.Close()

			s, err := NewShieldstral(ShieldstralConfig{Endpoint: srv.URL})
			if err != nil {
				t.Fatalf("NewShieldstral: %v", err)
			}

			d, err := s.Check(context.Background(), Input{
				Phase:   PhasePreTurn,
				Content: strings.Repeat("test prompt injection payload ", 20),
			})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tc.errIs != nil && !errors.Is(err, tc.errIs) {
					t.Errorf("expected error %v, got %v", tc.errIs, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d.Verdict != tc.wantVerdict {
				t.Errorf("Verdict = %q, want %q", d.Verdict, tc.wantVerdict)
			}
			if d.Score != tc.wantScore {
				t.Errorf("Score = %v, want %v", d.Score, tc.wantScore)
			}
		})
	}
}

func TestShieldstral_TruncationAndParseErrors(t *testing.T) {
	t.Run("truncated by max_tokens", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{
						"message":       map[string]string{"content": "I am thinking about this content and..."},
						"finish_reason": "length",
					},
				},
			})
		}))
		defer srv.Close()

		s, err := NewShieldstral(ShieldstralConfig{Endpoint: srv.URL})
		if err != nil {
			t.Fatalf("NewShieldstral: %v", err)
		}

		_, err = s.Check(context.Background(), Input{Phase: PhasePreTool, Content: "some tool input"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !errors.Is(err, ErrShieldstralResponseTruncated) {
			t.Errorf("expected ErrShieldstralResponseTruncated, got: %v", err)
		}
		if !errors.Is(err, ErrParseFailed) {
			t.Errorf("expected ErrParseFailed compatibility, got: %v", err)
		}
	})

	t.Run("truncated response with verdict-like leading word", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{
						"message":       map[string]string{"content": "No definitive tags yet; the tool call runs rm -rf on the host, which is destructive and"},
						"finish_reason": "length",
					},
				},
			})
		}))
		defer srv.Close()

		s, err := NewShieldstral(ShieldstralConfig{Endpoint: srv.URL})
		if err != nil {
			t.Fatalf("NewShieldstral: %v", err)
		}

		_, err = s.Check(context.Background(), Input{Phase: PhasePreTool, Content: "some tool input"})
		if !errors.Is(err, ErrShieldstralResponseTruncated) {
			t.Errorf("expected ErrShieldstralResponseTruncated, got: %v", err)
		}
	})

	t.Run("unparseable response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{"message": map[string]string{"content": "This response contains no recognizable verdict tags or words."}},
				},
			})
		}))
		defer srv.Close()

		s, err := NewShieldstral(ShieldstralConfig{Endpoint: srv.URL})
		if err != nil {
			t.Fatalf("NewShieldstral: %v", err)
		}

		_, err = s.Check(context.Background(), Input{Phase: PhasePreTool, Content: "some tool input"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !errors.Is(err, ErrShieldstralParseFailed) {
			t.Errorf("expected ErrShieldstralParseFailed, got: %v", err)
		}
		if !errors.Is(err, ErrParseFailed) {
			t.Errorf("expected ErrParseFailed compatibility, got: %v", err)
		}
	})

	t.Run("http error status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Unauthorized: invalid api key", http.StatusUnauthorized)
		}))
		defer srv.Close()

		s, err := NewShieldstral(ShieldstralConfig{Endpoint: srv.URL})
		if err != nil {
			t.Fatalf("NewShieldstral: %v", err)
		}

		_, err = s.Check(context.Background(), Input{Phase: PhasePreTool, Content: "some tool input"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "http status 401") {
			t.Errorf("expected 401 error, got: %v", err)
		}
	})

	t.Run("timeout handling", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(100 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		s, err := NewShieldstral(ShieldstralConfig{
			Endpoint: srv.URL,
			Timeout:  20 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("NewShieldstral: %v", err)
		}

		_, err = s.Check(context.Background(), Input{Phase: PhasePreTool, Content: "some tool input"})
		if err == nil {
			t.Fatal("expected timeout error, got nil")
		}
	})
}
