package mcptools

// An authentication failure must reach the TOOL CALLER as a named cause,
// not as the backend's `auth: Unauthorized`. This test drives the real
// board_list tool over the real credential layer (kanban.Client.HTTPClient),
// so it exercises the seam that used to lose the diagnosis: doJSON →
// *RestError → RestErrorMessage → `auth: <detail>`.
//
// Measured on 2026-11-24: a stale cookie, a rotated password and a
// throttled login all produced the same `auth: Unauthorized`, which sent
// three lanes hunting the wrong credential. Each cause now has its own
// sentence, and MapError's 300-rune truncation must not eat it.

import (
	"context"

	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/RKelln/hermes-kanban-mcp/internal/kanban"
)

// authFake is a dashboard whose login and /api/* answers each test picks.
type authFake struct {
	ts        *httptest.Server
	mu        sync.Mutex
	loginCode int    // 0 => 200
	loginBody string // body for a non-200 login
	apiCode   int    // 0 => 200; non-zero => always answer this, cookie or not
	apiBody   string
}

func newAuthFake(t *testing.T) *authFake {
	t.Helper()
	f := &authFake{}
	f.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/password-login" {
			f.mu.Lock()
			code, body := f.loginCode, f.loginBody
			f.mu.Unlock()
			if code == 0 {
				http.SetCookie(w, &http.Cookie{Name: "access_token", Value: "acc", Path: "/"})
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
				return
			}
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
			return
		}
		if _, err := r.Cookie("access_token"); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthenticated","detail":"Unauthorized","reason":"no_cookie"}`))
			return
		}
		f.mu.Lock()
		code, body := f.apiCode, f.apiBody
		f.mu.Unlock()
		if code != 0 {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"boards":[]}`))
	}))
	t.Cleanup(f.ts.Close)
	return f
}

// toolError runs board_list through a client wired exactly as main.go wires
// it (NewForBaseURL → Login → HTTPClient → NewServerWithClient) and returns
// the tool-visible error text.
func toolError(t *testing.T, f *authFake) string {
	t.Helper()
	client, err := kanban.NewForBaseURL(f.ts.URL+"/api/plugins/kanban/", "user", "pass")
	if err != nil {
		t.Fatalf("NewForBaseURL: %v", err)
	}
	// Startup login outcome is deliberately ignored: an install with a bad
	// credential still serves, and says why on every call.
	_ = client.Login(context.Background())

	srv := NewServerWithClient(client.HTTPClient(), f.ts.URL+"/api/plugins/kanban", "hermes-kanban-mcp")
	res := srv.BoardList(context.Background(), BoardListInput{})
	if !res.IsError {
		t.Fatalf("board_list succeeded, want an auth error result")
	}
	if len(res.Content) != 1 || res.Content[0].Text == "" {
		t.Fatalf("result has no text content: %+v", res)
	}
	return res.Content[0].Text
}

// TestBoardListNamesABadPassword: the password is wrong. The tool text must
// say so, name the environment variables, and NOT be the bare
// `auth: Unauthorized`.
func TestBoardListNamesABadPassword(t *testing.T) {
	f := newAuthFake(t)
	f.loginCode = http.StatusUnauthorized
	f.loginBody = `{"detail":"Invalid credentials"}`

	got := toolError(t, f)
	if !strings.HasPrefix(got, "auth: ") {
		t.Fatalf("tool error = %q, want the stable `auth: ` kind prefix", got)
	}
	if got == "auth: Unauthorized" {
		t.Fatalf("tool error is still the bare %q", got)
	}
	for _, want := range []string{"kanban login REFUSED", "KANBAN_USERNAME/KANBAN_PASSWORD", "HTTP 401", "Invalid credentials"} {
		if !strings.Contains(got, want) {
			t.Errorf("tool error %q does not mention %q", got, want)
		}
	}
}

// TestBoardListNamesAThrottledLogin: 429 from the login route is the
// dashboard's own throttle, and the text has to say that rather than
// blaming the password.
func TestBoardListNamesAThrottledLogin(t *testing.T) {
	f := newAuthFake(t)
	f.loginCode = http.StatusTooManyRequests
	f.loginBody = `{"detail":"Too many login attempts. Try again shortly."}`

	got := toolError(t, f)
	if !strings.HasPrefix(got, "auth: ") {
		t.Fatalf("tool error = %q, want the `auth: ` kind prefix", got)
	}
	for _, want := range []string{"THROTTLED", "HTTP 429", "10-attempts-per-60s-per-IP"} {
		if !strings.Contains(got, want) {
			t.Errorf("tool error %q does not mention %q", got, want)
		}
	}
}

// TestBoardListNamesARejectedSession: the login succeeds and the board still
// refuses the cookie — the shape a bind whose credential is not a cookie
// produces. The text must name no_cookie and the credential that WOULD work,
// because that is the difference between "fix the password" and "fix the
// bind".
func TestBoardListNamesARejectedSession(t *testing.T) {
	f := newAuthFake(t)
	f.apiCode = http.StatusUnauthorized
	f.apiBody = `{"error":"unauthenticated","detail":"Unauthorized","reason":"no_cookie"}`

	got := toolError(t, f)
	if !strings.HasPrefix(got, "auth: ") {
		t.Fatalf("tool error = %q, want the `auth: ` kind prefix", got)
	}
	if got == "auth: Unauthorized" {
		t.Fatalf("tool error is still the bare %q", got)
	}
	for _, want := range []string{"session REJECTED", "no_cookie", "X-Hermes-Session-Token"} {
		if !strings.Contains(got, want) {
			t.Errorf("tool error %q does not mention %q", got, want)
		}
	}
}

// TestBoardListWritesNothingAboveTheResultCap keeps the read-path rule
// honest: a named failure is a single line well inside the 2 KB result
// budget, so naming causes cannot push the tool surface past its cap.
func TestBoardListWritesNothingAboveTheResultCap(t *testing.T) {
	f := newAuthFake(t)
	f.loginCode = http.StatusUnauthorized
	f.loginBody = `{"detail":"Invalid credentials"}`

	got := toolError(t, f)
	if n := len([]rune(got)); n > maxResultTextRunes {
		t.Fatalf("auth failure text is %d runes, above the %d-rune result budget", n, maxResultTextRunes)
	}
}
