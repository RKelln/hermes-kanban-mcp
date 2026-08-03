package kanban

// Verification suite for the session-core ticket (t_72a4087c), exercised
// with go test -race -count=10 and left here as a reference for t_add72655
// (which owns the canonical suite). Scenarios: 16-goroutine single-flight
// lazy login, concurrent-401 single refresh, retry-once semantics, 4xx
// no_cookie, permanent auth failure, login failure propagation, proactive
// TTL re-login, wire shape (payload/Content-Type/no Authorization), ctx
// cancellation, MapError mapping, and the New contract.

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

// fakeBackend stands in for the kanban REST gateway: it counts login
// POSTs, hands out session cookies when logins are allowed, optionally
// hooks successful logins (onLogin), and delegates every other request to
// api with an authed flag (whether the request carried a session cookie).
type fakeBackend struct {
	ts           *httptest.Server
	loginHits    atomic.Int32
	loginAllowed atomic.Bool
	onLogin      func()

	api func(w http.ResponseWriter, r *http.Request, authed bool)

	mu          sync.Mutex
	loginAuth   string
	loginBody   []byte
	cts         []string // Content-Type of each api request, in order
	lastReqAuth string
}

func newFakeBackend(api func(w http.ResponseWriter, r *http.Request, authed bool)) *fakeBackend {
	f := &fakeBackend{api: api}
	f.loginAllowed.Store(true)
	f.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The client joins relative paths onto baseURL, so with the test
		// baseURL override the login endpoint is /auth/password-login.
		if r.URL.Path == "/auth/password-login" && r.Method == http.MethodPost {
			f.loginHits.Add(1)
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.loginAuth = r.Header.Get("Authorization")
			f.loginBody = body
			f.mu.Unlock()
			if !f.loginAllowed.Load() {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"unauthenticated","reason":"no_cookie"}`))
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "access_token", Value: "acc", Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "refresh_token", Value: "ref", Path: "/"})
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"access_token":"acc","refresh_token":"ref"}`))
			if f.onLogin != nil {
				f.onLogin()
			}
			return
		}
		f.mu.Lock()
		f.cts = append(f.cts, r.Header.Get("Content-Type"))
		f.lastReqAuth = r.Header.Get("Authorization")
		f.mu.Unlock()
		authed := false
		if _, err := r.Cookie("access_token"); err == nil {
			authed = true
		}
		f.api(w, r, authed)
	}))
	return f
}

func (f *fakeBackend) client() *Client {
	c := New("user", "pass")
	// httptest URLs have no trailing slash; baseURL+path joining needs one.
	c.baseURL = f.ts.URL + "/"
	return c
}

// TestDoSingleFlightLazyLogin: 16 concurrent callers on a cold client must
// trigger exactly one login POST (the acceptance scenario).
func TestDoSingleFlightLazyLogin(t *testing.T) {
	var okHits atomic.Int32
	f := newFakeBackend(func(w http.ResponseWriter, r *http.Request, authed bool) {
		if !authed {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthenticated","reason":"no_cookie"}`))
			return
		}
		okHits.Add(1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	})
	defer f.ts.Close()
	c := f.client()

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.do(context.Background(), http.MethodGet, "boards", nil)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: do: %v", i, err)
		}
	}
	if got := f.loginHits.Load(); got != 1 {
		t.Fatalf("single-flight broken: %d login POSTs for %d concurrent callers, want 1", got, n)
	}
	if got := okHits.Load(); got != n {
		t.Fatalf("got %d successful API calls, want %d", got, n)
	}
}

// TestDoRetriesOnceAfterSessionExpiry: a server-side session revocation
// must produce exactly one re-login and exactly one retry.
func TestDoRetriesOnceAfterSessionExpiry(t *testing.T) {
	var sessionValid atomic.Bool
	sessionValid.Store(true)
	var apiHits atomic.Int32
	f := newFakeBackend(func(w http.ResponseWriter, r *http.Request, authed bool) {
		apiHits.Add(1)
		if !sessionValid.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthenticated","reason":"no_cookie"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	})
	f.onLogin = func() { sessionValid.Store(true) }
	defer f.ts.Close()
	c := f.client()

	if _, err := c.do(context.Background(), http.MethodGet, "boards", nil); err != nil {
		t.Fatalf("prime session: %v", err)
	}
	if got := f.loginHits.Load(); got != 1 {
		t.Fatalf("prime: want 1 login, got %d", got)
	}

	// Server wipes sessions; the client still believes its session fresh.
	sessionValid.Store(false)
	body, err := c.do(context.Background(), http.MethodGet, "boards", nil)
	if err != nil {
		t.Fatalf("do after expiry: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("unexpected body %q", body)
	}
	if got := f.loginHits.Load(); got != 2 {
		t.Fatalf("want exactly 1 re-login (2 total), got %d", got)
	}
	if got := apiHits.Load(); got != 3 { // prime + original + single retry
		t.Fatalf("want original request retried exactly once (3 API hits), got %d", got)
	}
}

// TestDoRetriesOnNoCookie4xx: a 4xx (here 403, not 401) whose body carries
// the no_cookie reason must also refresh and retry.
func TestDoRetriesOnNoCookie4xx(t *testing.T) {
	var sessionValid atomic.Bool
	sessionValid.Store(true)
	f := newFakeBackend(func(w http.ResponseWriter, r *http.Request, authed bool) {
		if !sessionValid.Load() {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"unauthenticated","reason":"no_cookie"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	})
	f.onLogin = func() { sessionValid.Store(true) }
	defer f.ts.Close()
	c := f.client()

	if _, err := c.do(context.Background(), http.MethodGet, "boards", nil); err != nil {
		t.Fatalf("prime session: %v", err)
	}
	sessionValid.Store(false)
	if _, err := c.do(context.Background(), http.MethodGet, "boards", nil); err != nil {
		t.Fatalf("no_cookie 4xx should refresh and retry once: %v", err)
	}
	if got := f.loginHits.Load(); got != 2 {
		t.Fatalf("want 2 logins (prime + one refresh), got %d", got)
	}
}

// TestDoPermanentAuthAfterSecondFailure: a second auth failure must return
// ErrPermanentAuth wrapping the mapped error, with no third login.
func TestDoPermanentAuthAfterSecondFailure(t *testing.T) {
	f := newFakeBackend(func(w http.ResponseWriter, r *http.Request, authed bool) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthenticated","reason":"no_cookie"}`))
	})
	defer f.ts.Close()
	c := f.client()

	_, err := c.do(context.Background(), http.MethodGet, "boards", nil)
	if err == nil {
		t.Fatal("want error")
	}
	if !errors.Is(err, ErrPermanentAuth) {
		t.Fatalf("want ErrPermanentAuth, got %v", err)
	}
	if got := f.loginHits.Load(); got != 2 {
		t.Fatalf("want exactly 2 logins (initial + one refresh), got %d", got)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want wrapped *APIError, got %v", err)
	}
	if apiErr.Kind != KindAuth {
		t.Fatalf("want auth kind, got %s", apiErr.Kind)
	}
}

// TestDoLoginFailurePropagates: a refused login surfaces as a mapped auth
// error (not ErrPermanentAuth) and the next call retries the login.
func TestDoLoginFailurePropagates(t *testing.T) {
	f := newFakeBackend(func(w http.ResponseWriter, r *http.Request, authed bool) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	})
	f.loginAllowed.Store(false)
	defer f.ts.Close()
	c := f.client()

	_, err := c.do(context.Background(), http.MethodGet, "boards", nil)
	if err == nil {
		t.Fatal("want error")
	}
	if errors.Is(err, ErrPermanentAuth) {
		t.Fatalf("login failure is not a permanent request auth failure: %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != KindAuth {
		t.Fatalf("want mapped auth error, got %v", err)
	}
	if got := f.loginHits.Load(); got != 1 {
		t.Fatalf("want 1 login attempt, got %d", got)
	}
	if _, err := c.do(context.Background(), http.MethodGet, "boards", nil); err == nil {
		t.Fatal("want second call to fail while login is refused")
	}
	if got := f.loginHits.Load(); got != 2 {
		t.Fatalf("want login retried on next call, got %d attempts", got)
	}
}

// TestDoProactiveReloginAfterTTL: once the session is older than 50m the
// client re-logins before the next request.
func TestDoProactiveReloginAfterTTL(t *testing.T) {
	f := newFakeBackend(func(w http.ResponseWriter, r *http.Request, authed bool) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	})
	defer f.ts.Close()
	c := f.client()

	if _, err := c.do(context.Background(), http.MethodGet, "boards", nil); err != nil {
		t.Fatalf("prime: %v", err)
	}
	if got := f.loginHits.Load(); got != 1 {
		t.Fatalf("prime: want 1 login, got %d", got)
	}

	c.mu.Lock()
	c.lastLoginAt = time.Now().Add(-51 * time.Minute)
	c.mu.Unlock()

	if _, err := c.do(context.Background(), http.MethodGet, "boards", nil); err != nil {
		t.Fatalf("do after TTL: %v", err)
	}
	if got := f.loginHits.Load(); got != 2 {
		t.Fatalf("want proactive re-login after TTL, got %d logins", got)
	}
}

// TestDoWireShape: login payload, Content-Type rules, and the absence of
// any Authorization header.
func TestDoWireShape(t *testing.T) {
	f := newFakeBackend(func(w http.ResponseWriter, r *http.Request, authed bool) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	})
	defer f.ts.Close()
	c := f.client()

	if _, err := c.do(context.Background(), http.MethodPost, "tasks", []byte(`{"title":"x"}`)); err != nil {
		t.Fatalf("POST with body: %v", err)
	}
	if _, err := c.do(context.Background(), http.MethodGet, "boards", nil); err != nil {
		t.Fatalf("GET without body: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loginAuth != "" {
		t.Fatalf("login carried Authorization header %q; sessions are cookie-based", f.loginAuth)
	}
	if f.lastReqAuth != "" {
		t.Fatalf("api request carried Authorization header %q", f.lastReqAuth)
	}
	if len(f.cts) != 2 {
		t.Fatalf("want 2 api requests captured, got %d", len(f.cts))
	}
	if f.cts[0] != "application/json" {
		t.Fatalf("request with body: Content-Type = %q, want application/json", f.cts[0])
	}
	if f.cts[1] != "" {
		t.Fatalf("request without body: Content-Type = %q, want empty", f.cts[1])
	}
	var got loginRequest
	if err := json.Unmarshal(f.loginBody, &got); err != nil {
		t.Fatalf("login body not JSON: %v", err)
	}
	if got.Provider != "basic" || got.Username != "user" || got.Password != "pass" {
		t.Fatalf("unexpected login payload: %+v", got)
	}
}

// TestDoContentTypeAbsentWithoutBody: GET with nil body must not set
// Content-Type (checked via a second request so lastCT reflects it).
func TestDoContentTypeAbsentWithoutBody(t *testing.T) {
	f := newFakeBackend(func(w http.ResponseWriter, r *http.Request, authed bool) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	})
	defer f.ts.Close()
	c := f.client()

	if _, err := c.do(context.Background(), http.MethodGet, "boards", nil); err != nil {
		t.Fatalf("do: %v", err)
	}
	f.mu.Lock()
	ct := f.cts[0]
	f.mu.Unlock()
	if ct != "" {
		t.Fatalf("request without body set Content-Type %q, want empty", ct)
	}
}

// TestDoHonoursContextCancellation: a cancelled/deadlined ctx aborts the
// request and surfaces as context.DeadlineExceeded.
func TestDoHonoursContextCancellation(t *testing.T) {
	release := make(chan struct{})
	f := newFakeBackend(func(w http.ResponseWriter, r *http.Request, authed bool) {
		// Hold the request open until the client gives up (or the test
		// releases us — the stdlib transport doesn't always surface the
		// client's disconnect to the server, so this must not be the
		// only unblock path).
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	defer f.ts.Close()   // runs second
	defer close(release) // runs first (LIFO): unblocks the handler before shutdown
	c := f.client()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.do(ctx, http.MethodGet, "boards", nil)
	if err == nil {
		t.Fatal("want error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
}

// TestDoMapsAPIErrors: non-auth failures map through MapError with the
// right status and kind.
func TestDoMapsAPIErrors(t *testing.T) {
	f := newFakeBackend(func(w http.ResponseWriter, r *http.Request, authed bool) {
		switch {
		case strings.HasSuffix(r.URL.Path, "notfound"):
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"detail":"no such board"}`))
		case strings.HasSuffix(r.URL.Path, "boom"):
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"detail":"kaboom"}`))
		default:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"ok":true}`))
		}
	})
	defer f.ts.Close()
	c := f.client()

	_, err := c.do(context.Background(), http.MethodGet, "notfound", nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if apiErr.Status != http.StatusNotFound || apiErr.Kind != KindNotFound {
		t.Fatalf("unexpected mapping: %+v", apiErr)
	}
	if !strings.Contains(apiErr.Msg, "no such board") {
		t.Fatalf("msg %q lost the server detail", apiErr.Msg)
	}

	if _, err := c.do(context.Background(), http.MethodGet, "boom", nil); !errors.As(err, &apiErr) || apiErr.Kind != KindUnavailable {
		t.Fatalf("want unavailable kind for 500, got %v", err)
	}
}

// TestDoConcurrentRetriesShareOneRefresh: with the server session revoked
// behind the client's back, 16 concurrent callers each hit 401 and must
// still trigger exactly one refresh login between them.
func TestDoConcurrentRetriesShareOneRefresh(t *testing.T) {
	var sessionValid atomic.Bool
	sessionValid.Store(true)
	var okHits atomic.Int32
	f := newFakeBackend(func(w http.ResponseWriter, r *http.Request, authed bool) {
		if !sessionValid.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthenticated","reason":"no_cookie"}`))
			return
		}
		okHits.Add(1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	})
	f.onLogin = func() { sessionValid.Store(true) }
	defer f.ts.Close()
	c := f.client()

	if _, err := c.do(context.Background(), http.MethodGet, "boards", nil); err != nil {
		t.Fatalf("prime: %v", err)
	}
	if got := f.loginHits.Load(); got != 1 {
		t.Fatalf("prime: want 1 login, got %d", got)
	}

	sessionValid.Store(false) // server revokes; client-side session still looks fresh

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.do(context.Background(), http.MethodGet, "boards", nil)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if got := f.loginHits.Load(); got != 2 {
		t.Fatalf("concurrent retries must share one refresh: %d logins, want 2", got)
	}
	if got := okHits.Load(); got != n+1 { // prime request + 16 retries
		t.Fatalf("want %d successful API calls (prime + retries), got %d", n+1, got)
	}
}

// TestNewContract: the constructor hardcodes the gateway base URL, a 20s
// timeout, and a non-nil cookie jar.
func TestNewContract(t *testing.T) {
	c := New("u", "p")
	if c.baseURL != defaultBaseURL {
		t.Fatalf("base URL = %q, want hardcoded %q", c.baseURL, defaultBaseURL)
	}
	if c.httpClient.Timeout != 20*time.Second {
		t.Fatalf("timeout = %v, want 20s", c.httpClient.Timeout)
	}
	if c.httpClient.Jar == nil {
		t.Fatal("cookie jar must be configured")
	}
}
