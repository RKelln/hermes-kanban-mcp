package kanban

// Tests for the live credential path (session.go + Client.HTTPClient):
// re-authentication on 401, and the NAMED failures that replace the
// backend's `auth: Unauthorized`. The login-pacing tests replace the old
// "next call retries the login immediately" contract: an immediate retry is
// exactly what defeated the dashboard's 10-attempts-per-60s throttle on
// 2026-11-24, so they advance the injected clock instead of sleeping.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// authBackend is an httptest dashboard: a password-login route whose
// behaviour each test sets, and an /api/* surface that only answers 200 when
// the request carries the session cookie.
type authBackend struct {
	ts        *httptest.Server
	loginHits atomic.Int32
	apiHits   atomic.Int32

	mu          sync.Mutex
	loginStatus int    // 0 => 200
	loginBody   string // response body for a non-2xx login
	retryAfter  string // Retry-After header on the login response
	apiStatus   int    // 0 => 200 with {"ok":true}
	apiBody     string // body for a non-200 API response
}

func newAuthBackend(t *testing.T) *authBackend {
	t.Helper()
	b := &authBackend{}
	b.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/password-login" && r.Method == http.MethodPost {
			b.loginHits.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			b.mu.Lock()
			status, body, retry := b.loginStatus, b.loginBody, b.retryAfter
			b.mu.Unlock()
			if retry != "" {
				w.Header().Set("Retry-After", retry)
			}
			if status == 0 {
				http.SetCookie(w, &http.Cookie{Name: "access_token", Value: "acc", Path: "/"})
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
				return
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		b.apiHits.Add(1)
		if _, err := r.Cookie("access_token"); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthenticated","detail":"Unauthorized","reason":"no_cookie","login_url":"/login"}`))
			return
		}
		b.mu.Lock()
		status, body := b.apiStatus, b.apiBody
		b.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(b.ts.Close)
	return b
}

// client builds a Client pointed at the fake, with an injectable clock.
func (b *authBackend) client(t *testing.T, clock *atomic.Int64) *Client {
	t.Helper()
	c, err := NewForBaseURL(b.ts.URL+"/api/plugins/kanban/", "user", "pass")
	if err != nil {
		t.Fatalf("NewForBaseURL: %v", err)
	}
	base := time.Unix(1_700_000_000, 0)
	c.clock = func() time.Time { return base.Add(time.Duration(clock.Load()) * time.Millisecond) }
	return c
}

// getBoards issues one authenticated GET /boards through the live transport
// and returns the status and body, as a tool call would see them.
func getBoards(t *testing.T, c *Client) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		c.baseURL+"boards", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := c.HTTPClient().Do(req)
	if err != nil {
		t.Fatalf("GET boards: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// detailOf pulls the named failure sentence out of a written body.
func detailOf(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body is not a detail envelope (%q): %v", body, err)
	}
	return env.Detail
}

// TestLoginStripsPluginMount verifies the login POST is sent to the
// dashboard ROOT (/auth/password-login), not under the plugin mount
// (/api/plugins/kanban/...), when baseURL carries the plugin prefix.
//
// Regression for the bug found by the live smoke (t_eec004f0): the old code
// TrimRight'ed the trailing slash BEFORE the HasSuffix check, so the
// slash-terminated pluginMount never matched and the login POST went to
// .../kanban/auth/password-login — a route behind the session middleware,
// which answers 401.
func TestLoginStripsPluginMount(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// baseURL with the plugin mount suffix, exactly as the env example ships it.
	c, err := NewForBaseURL(srv.URL+"/api/plugins/kanban/", "u", "p")
	if err != nil {
		t.Fatalf("NewForBaseURL: %v", err)
	}
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if gotPath != "/auth/password-login" {
		t.Fatalf("login POST went to %q, want %q (plugin mount must be stripped)", gotPath, "/auth/password-login")
	}
}

// TestLoginBareOrigin keeps the bare-origin contract intact: a baseURL
// without the plugin mount is left untouched.
func TestLoginBareOrigin(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, err := NewForBaseURL(srv.URL, "u", "p")
	if err != nil {
		t.Fatalf("NewForBaseURL: %v", err)
	}
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if gotPath != "/auth/password-login" {
		t.Fatalf("login POST went to %q, want %q", gotPath, "/auth/password-login")
	}
}

// TestHTTPClientReauthenticatesAfterSessionLoss is the survivability
// acceptance criterion at unit level: the board refuses a request, the
// client logs in ONCE, retries, and the caller sees data — with no restart
// and no second login.
func TestHTTPClientReauthenticatesAfterSessionLoss(t *testing.T) {
	var clock atomic.Int64
	b := newAuthBackend(t)
	c := b.client(t, &clock)

	status, body := getBoards(t, c)
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200 after one re-login + retry", status, body)
	}
	if got := b.loginHits.Load(); got != 1 {
		t.Fatalf("login POSTs = %d, want exactly 1 (single refresh)", got)
	}
	if got := b.apiHits.Load(); got != 2 {
		t.Fatalf("API hits = %d, want 2 (refused + retried)", got)
	}
}

// TestHTTPClientSingleFlightOnColdStart: concurrent tool calls on a cold
// client must not each POST a login. This is what the transport would break
// if it had its own login rather than the Client's.
func TestHTTPClientSingleFlightOnColdStart(t *testing.T) {
	var clock atomic.Int64
	b := newAuthBackend(t)
	c := b.client(t, &clock)

	const n = 8
	var wg sync.WaitGroup
	statuses := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i], _ = getBoards(t, c)
		}(i)
	}
	wg.Wait()
	for i, s := range statuses {
		if s != http.StatusOK {
			t.Fatalf("caller %d: status %d, want 200", i, s)
		}
	}
	if got := b.loginHits.Load(); got != 1 {
		t.Fatalf("login POSTs = %d for %d concurrent callers, want 1 (single-flight)", got, n)
	}
}

// TestLoginRefusalIsNamed: a wrong/rotated password must name the
// credential and the status, never `auth: Unauthorized`.
func TestLoginRefusalIsNamed(t *testing.T) {
	var clock atomic.Int64
	b := newAuthBackend(t)
	b.loginStatus = http.StatusUnauthorized
	b.loginBody = `{"detail":"Invalid credentials"}`
	c := b.client(t, &clock)

	var authErr *AuthError
	if err := c.Login(context.Background()); !errors.As(err, &authErr) {
		t.Fatalf("Login error = %v, want *AuthError", err)
	}
	if authErr.Reason != ReasonLoginRefused {
		t.Fatalf("Reason = %q, want %q", authErr.Reason, ReasonLoginRefused)
	}
	for _, want := range []string{"REFUSED", "KANBAN_USERNAME/KANBAN_PASSWORD", "HTTP 401", "Invalid credentials"} {
		if !strings.Contains(authErr.Detail, want) {
			t.Errorf("Detail %q does not mention %q", authErr.Detail, want)
		}
	}

	// The same cause reaches a TOOL caller: the rewritten 401 body, which
	// mcptools renders as `auth: <detail>`.
	status, body := getBoards(t, c)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	detail := detailOf(t, body)
	if detail == "Unauthorized" || !strings.Contains(detail, "REFUSED") {
		t.Fatalf("tool-visible detail = %q, want the named login refusal", detail)
	}
	if got := b.loginHits.Load(); got != 1 {
		t.Fatalf("login POSTs = %d, want 1: the gate must refuse a second attempt", got)
	}
}

// TestLoginBackoffKeepsTheThrottleWindowClosed: repeated calls while the
// credential is wrong must NOT each POST a login. This is the crash-loop
// fix, and it is asserted as a POST count over a burst.
func TestLoginBackoffKeepsTheThrottleWindowClosed(t *testing.T) {
	var clock atomic.Int64
	b := newAuthBackend(t)
	b.loginStatus = http.StatusUnauthorized
	b.loginBody = `{"detail":"Invalid credentials"}`
	c := b.client(t, &clock)

	var authErr *AuthError
	if err := c.Login(context.Background()); !errors.As(err, &authErr) {
		t.Fatalf("Login error = %v, want *AuthError", err)
	}
	if authErr.RetryIn != loginRefusalFloor {
		t.Fatalf("RetryIn = %s, want %s", authErr.RetryIn, loginRefusalFloor)
	}

	// Five calls inside the gate: no POST, and the reason is the gate.
	for i := 0; i < 5; i++ {
		status, body := getBoards(t, c)
		if status != http.StatusUnauthorized {
			t.Fatalf("call %d: status = %d, want 401", i, status)
		}
		detail := detailOf(t, body)
		if !strings.Contains(detail, "backing off") || !strings.Contains(detail, "Invalid credentials") {
			t.Fatalf("call %d: detail = %q, want the backoff cause + the last failure", i, detail)
		}
	}
	if got := b.loginHits.Load(); got != 1 {
		t.Fatalf("login POSTs = %d, want 1: 5 calls inside the gate must not re-POST", got)
	}

	// Past the gate the client tries again — proof the credential is
	// re-tested, not abandoned.
	clock.Add(int64(loginRefusalFloor/time.Millisecond) + 1000)
	_ = c.Login(context.Background())
	if got := b.loginHits.Load(); got != 2 {
		t.Fatalf("login POSTs = %d, want 2 after the gate reopened", got)
	}
	// The second failure is paced further out.
	var second *AuthError
	err := c.Login(context.Background())
	if !errors.As(err, &second) || second.Reason != ReasonLoginBackoff {
		t.Fatalf("error after second refusal = %v, want the backoff gate", err)
	}
	if second.RetryIn <= authErr.RetryIn {
		t.Fatalf("second wait = %s, want more than the first (%s)", second.RetryIn, authErr.RetryIn)
	}
}

// TestLoginThrottle429IsNamed: a 429 must say THROTTLED, quote the
// dashboard's window, and honour Retry-After.
func TestLoginThrottle429IsNamed(t *testing.T) {
	var clock atomic.Int64
	b := newAuthBackend(t)
	b.loginStatus = http.StatusTooManyRequests
	b.loginBody = `{"detail":"Too many login attempts. Try again shortly."}`
	b.retryAfter = "90"
	c := b.client(t, &clock)

	var authErr *AuthError
	if err := c.Login(context.Background()); !errors.As(err, &authErr) {
		t.Fatalf("Login error = %v, want *AuthError", err)
	}
	if authErr.Reason != ReasonLoginThrottled {
		t.Fatalf("Reason = %q, want %q", authErr.Reason, ReasonLoginThrottled)
	}
	if authErr.RetryIn != 90*time.Second {
		t.Fatalf("RetryIn = %s, want the Retry-After value (90s)", authErr.RetryIn)
	}
	for _, want := range []string{"THROTTLED", "10-attempts-per-60s-per-IP", "HTTP 429", "1m30s"} {
		if !strings.Contains(authErr.Detail, want) {
			t.Errorf("Detail %q does not mention %q", authErr.Detail, want)
		}
	}
	// The gate honours the header: a call one minute later still makes no POST.
	clock.Add(int64(60 * time.Second / time.Millisecond))
	_ = c.Login(context.Background())
	if got := b.loginHits.Load(); got != 1 {
		t.Fatalf("login POSTs = %d after 60s of a 90s Retry-After, want 1", got)
	}
}

// TestLoginUnreachableIsNamed: a dashboard that is not answering must name
// the transport cause and retry on the SHORT schedule, so a gateway restart
// heals in seconds rather than in five minutes.
func TestLoginUnreachableIsNamed(t *testing.T) {
	var clock atomic.Int64
	b := newAuthBackend(t)
	c := b.client(t, &clock)
	b.ts.Close() // nothing listening: dial refused

	var authErr *AuthError
	if err := c.Login(context.Background()); !errors.As(err, &authErr) {
		t.Fatalf("Login error = %v, want *AuthError", err)
	}
	if authErr.Reason != ReasonOriginUnreachable {
		t.Fatalf("Reason = %q, want %q", authErr.Reason, ReasonOriginUnreachable)
	}
	if authErr.RetryIn != loginTransportFloor {
		t.Fatalf("RetryIn = %s, want the short transport floor %s", authErr.RetryIn, loginTransportFloor)
	}
	if !strings.Contains(authErr.Detail, "did not answer") {
		t.Fatalf("Detail = %q, want it to say the dashboard did not answer", authErr.Detail)
	}
}

// TestSessionRejectedIsNamed: the login succeeds and the board still
// refuses the cookie (the shape a loopback-bound origin produces for a
// cookie session). That must name the credential mismatch, and must NOT
// re-login per call — the login storm is what keeps the throttle shut.
func TestSessionRejectedIsNamed(t *testing.T) {
	var clock atomic.Int64
	b := newAuthBackend(t)
	c := b.client(t, &clock)

	// Login succeeds (the fake sets the cookie), but the API refuses even
	// WITH it: a gated origin that did not sign this cookie, or a loopback
	// bind that wants the session-token header instead.
	b.apiStatus = http.StatusUnauthorized
	b.apiBody = `{"error":"unauthenticated","detail":"Unauthorized","reason":"no_cookie"}`

	status, body := getBoards(t, c)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	detail := detailOf(t, body)
	for _, want := range []string{"REJECTED", "no_cookie", "X-Hermes-Session-Token"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not mention %q", detail, want)
		}
	}

	// The second call must not log in again: the gate was armed.
	before := b.loginHits.Load()
	getBoards(t, c)
	if got := b.loginHits.Load(); got != before {
		t.Fatalf("login POSTs = %d after a rejected session, want %d (no login storm)", got, before)
	}
}
