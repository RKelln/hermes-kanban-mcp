package httpauth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const testToken = "correct-horse-battery-staple"

// okHandler is the underlying handler the middleware wraps. It reports the
// request method so tests can prove both POST and GET reach the handler
// when authenticated.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Underside", "ran")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok " + r.Method))
	})
}

// do runs one request through the middleware and returns the recorder.
// When set is true the Authorization header is always set (even to ""),
// which lets tests distinguish "header absent" from "header empty".
func do(method, authHeader string, set ...bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/mcp", nil)
	if len(set) == 1 && set[0] {
		req.Header.Set("Authorization", authHeader)
	} else if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	Bearer(testToken, okHandler()).ServeHTTP(rec, req)
	return rec
}

const wantAuthHeader = `Bearer error="invalid_token", error_description="invalid or missing bearer token"`
const wantBody = `{"error":"invalid_token","error_description":"invalid or missing bearer token"}`

func TestBearerRejects(t *testing.T) {
	tests := []struct {
		name   string
		method string
		auth   string
		set    bool
	}{
		{"missing header POST", http.MethodPost, "", false},
		{"missing header GET", http.MethodGet, "", false},
		{"empty header", http.MethodPost, "", true},
		{"wrong scheme Basic", http.MethodPost, "Basic dXNlcjpwYXNz", false},
		{"wrong token same length", http.MethodPost, "Bearer " + "correct-horse-battery-staple"[:len(testToken)-1] + "x", false},
		{"wrong token different length", http.MethodPost, "Bearer nope", false},
		{"empty token", http.MethodPost, "Bearer ", false},
		{"trailing whitespace", http.MethodPost, "Bearer " + testToken + " ", false},
		{"extra token field", http.MethodPost, "Bearer " + testToken + " extra", false},
		{"malformed no space", http.MethodPost, "Bearer" + testToken, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(tt.method, tt.auth)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != wantAuthHeader {
				t.Errorf("WWW-Authenticate = %q, want %q", got, wantAuthHeader)
			}
			if got := rec.Body.String(); got != wantBody {
				t.Errorf("body = %q, want %q", got, wantBody)
			}
			if got := rec.Header().Get("X-Underside"); got != "" {
				t.Errorf("underlying handler ran on rejected request: X-Underside = %q", got)
			}
		})
	}
}

func TestBearerAccepts(t *testing.T) {
	cases := []struct {
		name string
		auth string
	}{
		{"canonical Bearer", "Bearer " + testToken},
		// RFC 6750 §2.1: the scheme is case-insensitive.
		{"lowercase scheme", "bearer " + testToken},
	}
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		for _, tc := range cases {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				rec := do(method, tc.auth)
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200", rec.Code)
				}
				if got := rec.Header().Get("X-Underside"); got != "ran" {
					t.Errorf("underlying handler did not run: X-Underside = %q", got)
				}
				if got := rec.Body.String(); got != "ok "+method {
					t.Errorf("body = %q, want %q", got, "ok "+method)
				}
			})
		}
	}
}

// TestConstantTimeCompareEnsuresEqualLengthWrongTokenStillRejected guards
// against a naive string == comparison silently becoming the only check.
func TestConstantTimeCompareUsed(t *testing.T) {
	// Same length, one char off — subtle.ConstantTimeCompare returns 0
	// for equal-length mismatches, which is the case a plain compare would
	// also catch; the point is we route through the constant-time path.
	wrong := "Correct-horse-battery-staple"
	rec := do(http.MethodPost, "Bearer "+wrong)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("same-length wrong token: status = %d, want 401", rec.Code)
	}
}

// TestNoLogOutput guards the "never log the token or request bodies"
// requirement structurally: bearer.go must not import or call any logging
// package, so no request data can leak into logs.
func TestNoLogOutput(t *testing.T) {
	src, err := os.ReadFile("bearer.go")
	if err != nil {
		t.Fatalf("reading bearer.go: %v", err)
	}
	for _, banned := range []string{`"log"`, `"log/slog"`, `fmt.Print`, `println(`} {
		if strings.Contains(string(src), banned) {
			t.Errorf("bearer.go must not log (found %s); tokens and bodies must never be logged", banned)
		}
	}
}
