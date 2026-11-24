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
	"fmt"
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
// the request carries the session cookie the LAST login issued.
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
	// token is the cookie value the /api surface accepts. Each login mints
	// a fresh one; invalidateSession rotates it, which is what a gateway
	// restart, a sign-out or a rotated signing key looks like from here: the
	// cookie in the jar is well-formed and no longer accepted.
	token string
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
			token := ""
			if status == 0 {
				token = fmt.Sprintf("acc-%d", b.loginHits.Load())
				b.token = token
			}
			b.mu.Unlock()
			if retry != "" {
				w.Header().Set("Retry-After", retry)
			}
			if status == 0 {
				http.SetCookie(w, &http.Cookie{Name: "access_token", Value: token, Path: "/"})
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
				return
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		b.apiHits.Add(1)
		b.mu.Lock()
		status, body, token := b.apiStatus, b.apiBody, b.token
		b.mu.Unlock()
		if ck, err := r.Cookie("access_token"); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthenticated","detail":"Unauthorized","reason":"no_cookie","login_url":"/login"}`))
			return
		} else if ck.Value != token {
			// Well-formed cookie, but not the one this origin issued: the
			// session was invalidated between calls.
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthenticated","detail":"Unauthorized","reason":"session_expired"}`))
			return
		}
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

// invalidateSession drops the session the fake issued: the cookie in the
// client's jar stays syntactically valid and stops being accepted.
func (b *authBackend) invalidateSession() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.token = "revoked"
}

// client builds a Client pointed at the fake, with an injectable clock. A
// nil clock keeps the real time.Now — the login gate has to hold on its own,
// not because a frozen clock made every later call look covered by the last
// login.
func (b *authBackend) client(t *testing.T, clock *atomic.Int64) *Client {
	t.Helper()
	c, err := NewForBaseURL(b.ts.URL+"/api/plugins/kanban/", "user", "pass")
	if err != nil {
		t.Fatalf("NewForBaseURL: %v", err)
	}
	if clock != nil {
		base := time.Unix(1_700_000_000, 0)
		c.clock = func() time.Time { return base.Add(time.Duration(clock.Load()) * time.Millisecond) }
	}
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

// TestReauthenticationAfterLoggedInSessionIsLost is the survivability
// acceptance criterion at unit level, in the shape the card asked for: the
// client has ALREADY logged in and served a call, then the origin
// invalidates that session, and the very next call logs in once, retries,
// and returns data — no restart, no second login.
//
// The prime call is what makes this more than a cold start: `apiHits == 3`
// is prime(1) + the refused request(2) + its single retry(3). A client that
// had never authenticated would produce 2, which is the signature the
// earlier version of this test asserted by accident.
//
// The real clock is deliberate: with a frozen one the retry's baseline
// equals lastLoginAt, the freshness check (not the login) would carry the
// request, and the client would never exercise a re-login at all (F2 of the
// t_51c5071f review).
func TestReauthenticationAfterLoggedInSessionIsLost(t *testing.T) {
	b := newAuthBackend(t)
	c := b.client(t, nil)

	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("prime login: %v", err)
	}
	if status, body := getBoards(t, c); status != http.StatusOK {
		t.Fatalf("prime call: status = %d (%s), want 200", status, body)
	}
	if got := b.loginHits.Load(); got != 1 {
		t.Fatalf("prime: login POSTs = %d, want 1", got)
	}

	// The origin drops the session; the jar's cookie is now worthless.
	b.invalidateSession()

	status, body := getBoards(t, c)
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200 after one re-login + retry", status, body)
	}
	if got := b.loginHits.Load(); got != 2 {
		t.Fatalf("login POSTs = %d, want exactly 2 (prime + one refresh)", got)
	}
	if got := b.apiHits.Load(); got != 3 {
		t.Fatalf("API hits = %d, want 3 (prime + refused + retried)", got)
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
// cookie session). That must name the credential mismatch, and the second
// call must fail without another login POST.
//
// NOTE this test proves the NAMING and only that: its frozen clock makes
// the second call's baseline equal lastLoginAt, so the freshness check —
// not the login gate — is what suppresses the second login, and this test
// stays green with the gate disabled. The gate itself is proved against a
// real clock by TestSessionRejectedLoginStormIsGated below.
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

// TestSessionRejectedLoginStormIsGated is the guard the frozen-clock test
// above cannot be: on a REAL clock, N calls in the session-REJECTED state
// must cost exactly ONE login POST, because it is nextLoginAttempt — not a
// coincidence of the injected clock — that refuses the rest.
//
// The measured shape, asserted here: call 1 is 401 → login → retry 401 →
// gate armed; calls 2..N short-circuit AT THE GATE, one API request each.
// So N calls cost one login and N+1 API requests. Disable the gate in
// ensureSession and this goes red with logins == N — that is the mutant the
// t_51c5071f review used to show the old test did not discriminate.
func TestSessionRejectedLoginStormIsGated(t *testing.T) {
	b := newAuthBackend(t)
	c := b.client(t, nil) // real clock: the gate has to hold on its own

	// The origin accepts the login and still refuses every /api request.
	b.apiStatus = http.StatusUnauthorized
	b.apiBody = `{"error":"unauthenticated","detail":"Unauthorized","reason":"no_cookie"}`

	const n = 5
	details := make([]string, n)
	for i := 0; i < n; i++ {
		status, body := getBoards(t, c)
		if status != http.StatusUnauthorized {
			t.Fatalf("call %d: status = %d (%s), want 401", i, status, body)
		}
		details[i] = detailOf(t, body)
	}

	// The discriminator: ONE login for the whole burst. With the gate
	// disabled (the review's mutant 2) every call re-logins, so this is N.
	if got := b.loginHits.Load(); got != 1 {
		t.Fatalf("login POSTs = %d for %d calls in the rejected state, want 1 (login storm); details: %v",
			got, n, details)
	}
	if got := b.apiHits.Load(); got != n+1 {
		t.Fatalf("API hits = %d for %d calls, want %d (first refused + retried, the rest refused once)",
			got, n, n+1)
	}

	// And the cause each call reports: the diagnosis first, the closed gate
	// after it.
	if !strings.Contains(details[0], "REJECTED") {
		t.Fatalf("call 0: detail = %q, want the REJECTED diagnosis", details[0])
	}
	for i := 1; i < n; i++ {
		if !strings.Contains(details[i], "backing off") {
			t.Fatalf("call %d: detail = %q, want the closed login gate named", i, details[i])
		}
	}
}

// recordedBody is a response body that reports whether it was closed: from
// outside a RoundTripper it is the only way to see that the layer closed a
// response it did not return.
type recordedBody struct {
	r      io.Reader
	closed atomic.Bool
}

func (b *recordedBody) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *recordedBody) Close() error               { b.closed.Store(true); return nil }

// recordedResponse is one response a recording base transport handed out.
type recordedResponse struct {
	req    *http.Request
	status int
	body   *recordedBody
}

// recordingTransport is a canned base RoundTripper: it answers each request
// through answer and keeps every body it handed out, so a test can assert
// which of them the layer above closed and which it passed on untouched.
type recordingTransport struct {
	answer func(*http.Request) (int, string, error)
	mu     sync.Mutex
	got    []*recordedResponse
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	status, body, err := rt.answer(req)
	if err != nil {
		return nil, err
	}
	rec := &recordedResponse{req: req, status: status, body: &recordedBody{r: strings.NewReader(body)}}
	rt.mu.Lock()
	rt.got = append(rt.got, rec)
	rt.mu.Unlock()
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{},
		Body:          rec.body,
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

// responses returns, in call order, the responses handed out for requests
// whose path is path.
func (rt *recordingTransport) responses(path string) []*recordedResponse {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	var out []*recordedResponse
	for _, r := range rt.got {
		if r.req.URL.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// TestRoundTripClosesAbandonedBodies: reauthTransport issues a second
// request of its own, so it owns the first response the moment it decides
// not to return it. An http.RoundTripper that drops a body instead of
// closing it leaks the connection with it — with the real transport, the
// socket and its readLoop, once per re-auth and once per 15s gate cycle in
// the session-REJECTED state (F1 of the t_51c5071f review).
//
// Each subtest fails on the pre-fix code, where the first 401 was abandoned
// unclosed; the recording base transport is what makes that visible.
// Reverting drainAndClose() in RoundTrip turns all three "first" assertions
// red — that negative control was run when this landed.
func TestRoundTripClosesAbandonedBodies(t *testing.T) {
	const boardsPath = "/api/plugins/kanban/boards"
	const loginPath = "/auth/password-login"

	const unauthorized = `{"detail":"Unauthorized","reason":"no_cookie"}`
	const ok = `{"ok":true}`

	// issue sends one board GET through the live transport, as a tool call
	// would. The caller owns the returned response.
	issue := func(t *testing.T, c *Client) (*http.Response, error) {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, c.baseURL+"boards", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		return c.HTTPClient().Do(req)
	}

	// newClient points a client's BASE transport at the recorder, so the
	// login POST and both board attempts pass through it.
	newClient := func(t *testing.T, rt *recordingTransport) *Client {
		t.Helper()
		c, err := NewForBaseURL("http://kanban.test/api/plugins/kanban/", "user", "pass")
		if err != nil {
			t.Fatalf("NewForBaseURL: %v", err)
		}
		c.httpClient.Transport = rt
		return c
	}

	t.Run("rescued", func(t *testing.T) {
		var attempts atomic.Int32
		rt := &recordingTransport{answer: func(req *http.Request) (int, string, error) {
			if req.URL.Path == loginPath {
				return http.StatusOK, ok, nil
			}
			if attempts.Add(1) == 1 {
				return http.StatusUnauthorized, unauthorized, nil
			}
			return http.StatusOK, ok, nil
		}}
		c := newClient(t, rt)

		resp, err := issue(t, c)
		if err != nil {
			t.Fatalf("GET boards: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 after re-login + retry", resp.StatusCode)
		}
		boards := rt.responses(boardsPath)
		if len(boards) != 2 {
			t.Fatalf("board requests = %d, want 2 (refused + retried)", len(boards))
		}
		if !boards[0].body.closed.Load() {
			t.Error("the abandoned 401 body was not closed: its connection leaks with it")
		}
		if boards[1].body.closed.Load() {
			t.Error("the returned 200 body was closed by the transport; the caller still needs it")
		}
	})

	t.Run("also_refused", func(t *testing.T) {
		rt := &recordingTransport{answer: func(req *http.Request) (int, string, error) {
			if req.URL.Path == loginPath {
				return http.StatusOK, ok, nil
			}
			return http.StatusUnauthorized, unauthorized, nil
		}}
		c := newClient(t, rt)

		resp, err := issue(t, c)
		if err != nil {
			t.Fatalf("GET boards: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want the named 401", resp.StatusCode)
		}
		raw, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(raw), "REJECTED") {
			t.Fatalf("body = %q, want the REJECTED diagnosis", raw)
		}
		boards := rt.responses(boardsPath)
		if len(boards) != 2 {
			t.Fatalf("board requests = %d, want 2 (refused, then refused again)", len(boards))
		}
		if !boards[0].body.closed.Load() {
			t.Error("the abandoned first 401 body was not closed: its connection leaks with it")
		}
		if !boards[1].body.closed.Load() {
			t.Error("the second 401 body was not closed by nameAuthFailure")
		}
	})

	t.Run("retry_failed", func(t *testing.T) {
		var attempts atomic.Int32
		boom := errors.New("dial tcp 127.0.0.1:9119: connection refused")
		rt := &recordingTransport{answer: func(req *http.Request) (int, string, error) {
			if req.URL.Path == loginPath {
				return http.StatusOK, ok, nil
			}
			if attempts.Add(1) > 1 {
				return 0, "", boom
			}
			return http.StatusUnauthorized, unauthorized, nil
		}}
		c := newClient(t, rt)

		resp, err := issue(t, c)
		if err == nil {
			resp.Body.Close()
			t.Fatal("want the retry's transport error")
		}
		if !errors.Is(err, boom) {
			t.Fatalf("error = %v, want it to wrap %v", err, boom)
		}
		boards := rt.responses(boardsPath)
		if len(boards) != 1 {
			t.Fatalf("recorded board responses = %d, want 1 (the refused attempt)", len(boards))
		}
		if !boards[0].body.closed.Load() {
			t.Error("the abandoned 401 body was not closed when the retry itself failed")
		}
	})
}
