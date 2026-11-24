// client.go implements the shared HTTP/session core for the kanban REST
// API: one *http.Client with a cookie jar and a 20s timeout, the
// single-flight password login, and the do() wrapper that endpoint
// methods build on. Sessions are cookie-based (no Authorization header is
// ever sent) and the jar is the only store of tokens. This is the PASSWORD
// path — the only upstream credential this bridge uses. (The gateway
// Webapp session token is the alternative for a LOOPBACK-bound origin and
// is deliberately not implemented here; see the t_51c5071f card.)
//
// Production tool calls reach the board through Client.HTTPClient(), whose
// transport (session.go) shares this client's jar and login state: a 401
// mid-run triggers one re-login and one retry, so an origin restart no
// longer needs a bridge restart, and every authentication failure names its
// cause instead of the backend's bare "Unauthorized".
//
// Login pacing lives here too: after a refused or unanswered login the
// client shuts a gate (nextLoginAttempt) and refuses to POST again until it
// reopens. The dashboard throttles password login at 10 attempts per 60s per
// IP, so an un-gated retry — from a request burst, or from a systemd restart
// loop — is what makes a credential problem permanent.
//
// Security constraint for this file: never log the password, session
// tokens, or any Set-Cookie value. There is deliberately no logging here at
// all; the password exists only in Client.password and the marshalled
// login payload, and cookies live only in the jar.
package kanban

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultBaseURL is the kanban REST API root served by the Hermes gateway.
// It is hardcoded per spec: not configurable, not derived from env.
const defaultBaseURL = "http://127.0.0.1:9119/api/plugins/kanban/"

// sessionTTL is how long the client trusts a session before proactively
// re-logging in. Chosen comfortably below the server-side session lifetime
// so requests rarely hit an expired session.
const sessionTTL = 50 * time.Minute

// defaultHTTPTimeout bounds every request issued through a kanban REST
// client: the login POST and, through HTTPClient(), every tool call.
const defaultHTTPTimeout = 20 * time.Second

// maxBodyRead bounds the response body read on every request. kanban
// payloads are small; the cap guards against unbounded reads while still
// letting error mapping and the no_cookie retry check see real bodies.
const maxBodyRead = 1 << 20 // 1 MiB

// ErrPermanentAuth is returned when a request fails authentication on the
// original attempt and on its single retry: the session could not be
// re-established (bad credentials, revoked account, or a login that the
// server keeps rejecting). The error wraps the *APIError of the final
// response, so errors.As can recover the status and kind.
var ErrPermanentAuth = errors.New("kanban: permanent authentication failure")

// Client is the shared HTTP/session core for the kanban REST API. One
// *http.Client carries a cookie jar (sessions are cookie-based) and a 20s
// timeout; login state is guarded by mu; http.Client and cookiejar.Jar
// are themselves concurrency-safe, so a Client is safe for concurrent
// use. The zero value is not usable: create clients with New or
// NewForBaseURL.
type Client struct {
	username string
	password string

	// httpClient is the single transport shared by all requests. Its Jar
	// collects the access/refresh cookies from the login response (the
	// http.Client applies Set-Cookie automatically); the 20s Timeout
	// bounds every request, logins included.
	httpClient *http.Client

	// baseURL is the API root. New fills it from defaultBaseURL; it is a
	// field rather than a const only so in-package tests can point a
	// client at an httptest server (and NewForBaseURL sets it from env).
	baseURL string

	// clock is time.Now. Injected by tests so the login backoff (which is
	// minutes long by design) can be exercised without sleeping.
	clock func() time.Time

	// mu guards the login state below; it also serializes the login
	// itself, which is what makes the login path single-flight.
	mu sync.Mutex
	// lastLoginAt is zero until the first successful login.
	lastLoginAt time.Time
	// loginFailures counts CONSECUTIVE failed login attempts (a refusal, an
	// unanswered dashboard, or a session the board then refused). It drives
	// the backoff and is reset by a success.
	loginFailures int
	// nextLoginAttempt is the earliest time another login POST may be sent.
	// While the gate is shut no POST is made: a call fails immediately with
	// a named *AuthError that carries the wait and the last cause.
	nextLoginAttempt time.Time
	// lastLoginErr is the cause of the last failed attempt, in words safe
	// to log and return (never the password).
	lastLoginErr error
}

// New returns a Client for the kanban REST API that authenticates with
// username/password through the cookie-session login. No HTTP Basic
// Authorization header is used: the session is established with
// POST auth/password-login and carried as cookies in the jar.
func New(username, password string) *Client {
	// cookiejar.New cannot fail with nil options; the error is dropped
	// deliberately (a nil jar would only mean cookie storage is off).
	jar, _ := cookiejar.New(nil)
	return &Client{
		username: username,
		password: password,
		baseURL:  defaultBaseURL,
		clock:    time.Now,
		httpClient: &http.Client{
			Jar:     jar,
			Timeout: defaultHTTPTimeout,
		},
	}
}

// NewForBaseURL returns a Client for the kanban REST API at baseURL. It
// does no I/O: the first request — or Login — performs the password login,
// so a dashboard that is down or a password that is wrong is a NAMED error
// at the call site instead of a reason to exit the process.
//
// A baseURL with no host is refused here: every request would otherwise be
// a relative-URL error that names neither the setting nor the cause.
func NewForBaseURL(baseURL, username, password string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("kanban base URL %q: %w", baseURL, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("kanban base URL %q has no host", baseURL)
	}
	c := New(username, password)
	c.baseURL = baseURL
	return c, nil
}

// now reads the client clock (time.Now unless a test replaced it).
func (c *Client) now() time.Time {
	if c.clock == nil {
		return time.Now()
	}
	return c.clock()
}

// Login performs one password login now, subject to the backoff gate, and
// returns a named *AuthError on failure. The bridge calls it once at startup
// so a credential problem is a journal line immediately; it is NOT a
// liveness requirement — the client keeps serving and retries under the
// gate, which is what stops a systemd restart loop from defeating the
// dashboard's login throttle.
func (c *Client) Login(ctx context.Context) error {
	return c.ensureSession(ctx, time.Time{})
}

// HTTPClient returns the *http.Client every REST call must be issued
// through. It shares this Client's cookie jar and login state, so a 401
// triggers one single-flight re-login and one retry (session.go) and an
// authentication failure comes back as a 401 whose detail names its cause —
// the tool layer then renders `auth: <cause>` instead of `auth:
// Unauthorized`.
func (c *Client) HTTPClient() *http.Client {
	tr := &reauthTransport{c: c, base: c.httpClient.Transport}
	if tr.base == nil {
		tr.base = http.DefaultTransport
	}
	if u, err := url.Parse(c.baseURL); err == nil {
		tr.host = u.Host
	}
	return &http.Client{
		Jar:       c.httpClient.Jar,
		Timeout:   c.httpClient.Timeout,
		Transport: tr,
	}
}

// do sends method to baseURL+path with an optional JSON body and returns
// the response body for any 2xx status. It guarantees a fresh session
// first (lazy login on the first request, proactive re-login once the
// session is older than sessionTTL) and honours ctx cancellation.
//
// Content-Type: application/json is set when body is present. API
// failures (status >= 400) are mapped through MapError; transport
// failures (DNS, connect, cancellation) are returned as the underlying
// error wrapped with method/path context.
//
// Auth handling: a 401 response, or any 4xx whose body carries
// "no_cookie", triggers exactly one session refresh followed by exactly
// one retry of the original request. If the retry fails authentication
// again, do returns ErrPermanentAuth wrapping the mapped error — whose
// detail NAMES the cause (the login was refused, the login was throttled,
// the dashboard was unreachable, or the session cookie is not accepted).
func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	if err := c.ensureSession(ctx, time.Time{}); err != nil {
		return nil, err
	}
	sentAt := c.now()
	status, respBody, err := c.doOnce(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if isAuthFailure(status, respBody) {
		if err := c.ensureSession(ctx, sentAt); err != nil {
			return nil, err
		}
		status, respBody, err = c.doOnce(ctx, method, path, body)
		if err != nil {
			return nil, err
		}
		if isAuthFailure(status, respBody) {
			// Both the original attempt and the retry failed
			// authentication: the session cannot be re-established. Wrap
			// both errors so errors.Is and errors.As work on the result,
			// and name the cause in the detail the tool layer renders.
			rejected := c.recordSessionRejected()
			return nil, fmt.Errorf("%w: %w", ErrPermanentAuth,
				MapError(status, detailBody(namedDetail(respBody, rejected.Detail, true))))
		}
	}
	if status >= 400 {
		return nil, MapError(status, respBody)
	}
	return respBody, nil
}

// ensureSession returns with a session usable for a request sent at or
// after baseline, logging in if needed. It is the single point of login
// and is single-flight: callers serialize on c.mu and the post-lock
// freshness check means a login completed by a concurrent caller while we
// waited satisfies everyone behind it. With a session-expired state, N
// concurrent callers therefore trigger exactly one login POST.
//
// baseline selects what counts as "fresh". On the first attempt it is the
// zero time: any session younger than sessionTTL qualifies. After an auth
// failure it is the time the failed request was sent: only a session
// established at or after that request (i.e. refreshed by a concurrent
// caller while we waited, or exactly coincident) qualifies, so the retry
// reuses that refresh instead of logging in again. "At or after", not
// "after": a coarse clock must not turn one refresh into a login per
// waiter, which is a login storm against the dashboard's throttle.
//
// A shut login gate wins over an expired session: it returns a named
// *AuthError with the wait and the last cause, and makes NO login POST.
func (c *Client) ensureSession(ctx context.Context, baseline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if !c.lastLoginAt.IsZero() && !c.lastLoginAt.Before(baseline) && now.Sub(c.lastLoginAt) <= sessionTTL {
		return nil
	}
	if wait := c.nextLoginAttempt.Sub(now); wait > 0 {
		return c.backoffErrorLocked(wait)
	}
	return c.loginLocked(ctx)
}

// backoffErrorLocked builds the named failure for a call that arrived while
// the login gate is shut. Callers must hold c.mu.
func (c *Client) backoffErrorLocked(wait time.Duration) *AuthError {
	last := "unknown"
	if c.lastLoginErr != nil {
		last = truncateRunes(c.lastLoginErr.Error(), 120)
	}
	return &AuthError{
		Reason:   ReasonLoginBackoff,
		RetryIn:  wait,
		Failures: c.loginFailures,
		Last:     c.lastLoginErr,
		Detail: fmt.Sprintf("kanban login backing off (%d failure(s)): no login POST for %s. Cause: %s",
			c.loginFailures, waitText(wait), truncateRunes(last, 200)),
	}
}

// loginLocked performs the password-login POST and records the outcome.
// Callers must hold c.mu. The response cookies (access + refresh) land in
// the jar via http.Client's Set-Cookie handling; the JSON body is read
// (bounded) and discarded — nothing is logged.
//
// A refusal does NOT simply return the backend's mapped error: it is
// classified into a named *AuthError that says which credential was
// rejected and how long the bridge will wait, and it shuts the gate. That
// is the whole fix for the 2026-11-24 crash-loop, where a rejected password
// plus systemd's 3s RestartSec kept the dashboard's 10-per-60s window full
// and the bridge never authenticated again even after the credential was
// fixed.
func (c *Client) loginLocked(ctx context.Context) error {
	payload, err := json.Marshal(loginRequest{
		Provider: "basic",
		Username: c.username,
		Password: c.password,
	})
	if err != nil {
		return fmt.Errorf("kanban: marshal login payload: %w", err)
	}
	status, body, header, err := c.doOnceWithHeaders(ctx, http.MethodPost, c.loginURL(), payload)
	now := c.now()
	if err != nil {
		failures := c.loginFailures + 1
		delay := loginTransportBackoff(failures)
		authErr := &AuthError{
			Reason:   ReasonOriginUnreachable,
			RetryIn:  delay,
			Failures: failures,
			Detail: fmt.Sprintf("kanban login failed: the dashboard did not answer (%s). Next attempt in %s; nothing was throttled, so a restart heals once it is back.",
				truncateRunes(err.Error(), 100), waitText(delay)),
		}
		c.recordLoginFailureLocked(authErr, now)
		return authErr
	}
	if status >= 200 && status < 300 {
		c.lastLoginAt = now
		c.loginFailures = 0
		c.nextLoginAttempt = time.Time{}
		c.lastLoginErr = nil
		return nil
	}
	authErr := classifyLoginFailure(status, body, header, c.loginFailures+1)
	c.recordLoginFailureLocked(authErr, now)
	return authErr
}

// classifyLoginFailure turns a non-2xx login response into the cause an
// operator needs. Statuses are the dashboard's own (hermes_cli/
// dashboard_auth/routes.py auth_password_login): 401 bad credentials, 404 no
// password provider, 429 throttled, 503 credential store unreachable.
func classifyLoginFailure(status int, body []byte, header http.Header, failures int) *AuthError {
	said := backendSuffix(detailMessage(body))
	switch {
	case status == http.StatusTooManyRequests:
		delay := retryAfter(header, loginThrottleWindow)
		return &AuthError{
			Reason:   ReasonLoginThrottled,
			Status:   status,
			RetryIn:  delay,
			Failures: failures,
			Detail: fmt.Sprintf("kanban login THROTTLED (HTTP 429%s): the 10-attempts-per-60s-per-IP budget is spent; next attempt in %s (do not restart-loop).",
				said, waitText(delay)),
		}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		delay := loginRefusalBackoff(failures)
		return &AuthError{
			Reason:   ReasonLoginRefused,
			Status:   status,
			RetryIn:  delay,
			Failures: failures,
			Detail: fmt.Sprintf("kanban login REFUSED (HTTP %d%s): KANBAN_USERNAME/KANBAN_PASSWORD was rejected — wrong or rotated. The bridge retries in %s at first, backing off to %s.",
				status, said, waitText(delay), waitText(loginMaxBackoff)),
		}
	case status == http.StatusNotFound:
		delay := loginRefusalBackoff(failures)
		return &AuthError{
			Reason:   ReasonLoginRefused,
			Status:   status,
			RetryIn:  delay,
			Failures: failures,
			Detail: fmt.Sprintf("kanban login REFUSED (HTTP 404%s): the dashboard has no password provider registered, so KANBAN_USERNAME/KANBAN_PASSWORD cannot authenticate on this install. Next attempt in %s.",
				said, waitText(delay)),
		}
	default:
		delay := loginTransportBackoff(failures)
		return &AuthError{
			Reason:   ReasonOriginUnreachable,
			Status:   status,
			RetryIn:  delay,
			Failures: failures,
			Detail: fmt.Sprintf("kanban login failed (HTTP %d%s): the dashboard answered with no session (it may still be starting). Next attempt in %s.",
				status, said, waitText(delay)),
		}
	}
}

// recordLoginFailureLocked stores a failed attempt and shuts the gate for
// its RetryIn. Callers must hold c.mu.
func (c *Client) recordLoginFailureLocked(authErr *AuthError, now time.Time) {
	c.loginFailures = authErr.Failures
	c.nextLoginAttempt = now.Add(authErr.RetryIn)
	c.lastLoginErr = authErr
}

// recordSessionRejected is called when a login that JUST SUCCEEDED is
// followed by a 401: the cookie is not accepted on /api/*, which no amount
// of re-logging fixes. It names that cause and shuts the gate, so a
// cookie-shape problem (a loopback bind that wants the Webapp session
// token, a sign-out the account cannot recover from) degrades into a named
// error instead of a login storm.
func (c *Client) recordSessionRejected() *AuthError {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	failures := c.loginFailures + 1
	delay := loginRefusalBackoff(failures)
	authErr := &AuthError{
		Reason:   ReasonSessionRejected,
		Status:   http.StatusUnauthorized,
		RetryIn:  delay,
		Failures: failures,
		Detail: fmt.Sprintf("kanban session REJECTED: the login SUCCEEDED and the board still answered 401, so the cookie is not accepted on /api/* (a loopback bind wants X-Hermes-Session-Token; a gated bind takes only a cookie it issued). No login for %s.",
			waitText(delay)),
	}
	c.recordLoginFailureLocked(authErr, now)
	return authErr
}

// retryAfter reads a Retry-After response header in seconds, clamped to
// [loginRefusalFloor, loginMaxBackoff]. The dashboard does not send one
// today; a proxy in front of it might.
func retryAfter(header http.Header, fallback time.Duration) time.Duration {
	if header != nil {
		if v := strings.TrimSpace(header.Get("Retry-After")); v != "" {
			if secs, err := strconv.Atoi(v); err == nil {
				d := time.Duration(secs) * time.Second
				if d < loginRefusalFloor {
					return loginRefusalFloor
				}
				if d > loginMaxBackoff {
					return loginMaxBackoff
				}
				return d
			}
		}
	}
	return fallback
}

// backendSuffix renders the dashboard's own detail for a message, quoted and
// capped so the actionable half of the sentence is never truncated away.
func backendSuffix(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return ""
	}
	return fmt.Sprintf(" %q", truncateRunes(detail, 60))
}

// loginURL returns the absolute URL of the password-login route. The
// route lives at the dashboard root (/auth/password-login), NOT under
// the plugin mount /api/plugins/kanban/ — that prefix sits behind the
// session auth middleware, so a login POST sent there would be rejected
// as unauthenticated before reaching the route. The root is derived from
// baseURL by stripping the plugin mount suffix when present, which keeps
// httptest fakes that override baseURL to a bare origin working
// unchanged.
func (c *Client) loginURL() string {
	const pluginMount = "/api/plugins/kanban/"
	root := c.baseURL
	if strings.HasSuffix(root, pluginMount) {
		root = strings.TrimSuffix(root, pluginMount)
	}
	if !strings.HasSuffix(root, "/") {
		root += "/"
	}
	return root + "auth/password-login"
}

// doOnce performs a single HTTP request against baseURL+path and returns
// the status and fully-read body. It does no session management and no
// retries; that orchestration lives in do and loginLocked. Content-Type:
// application/json is set when body is present (non-empty). The response
// body is read eagerly (bounded by maxBodyRead) so the caller never owns
// a connection. A body longer than the cap is an error, not a short
// read: see the overflow branch in doOnceWithHeaders.
//
// path is normally relative to baseURL; an absolute URL (scheme-prefixed,
// as produced by loginURL) is used as-is.
func (c *Client) doOnce(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	status, respBody, _, err := c.doOnceWithHeaders(ctx, method, path, body)
	return status, respBody, err
}

// doOnceWithHeaders is doOnce plus the response headers, which the login
// classifier needs for Retry-After.
func (c *Client) doOnceWithHeaders(ctx context.Context, method, path string, body []byte) (int, []byte, http.Header, error) {
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	target := c.baseURL + path
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		target = path
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("kanban: build %s %s: %w", method, path, err)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("kanban: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead+1))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("kanban: read %s %s response: %w", method, path, err)
	}
	if len(respBody) > maxBodyRead {
		// Refuse rather than return a clipped body: a truncated response
		// would decode to "unexpected end of JSON input" at the caller and
		// blame the backend for what is this cap.
		return 0, nil, nil, fmt.Errorf("kanban: %s %s response exceeds the %d-byte read cap", method, path, maxBodyRead)
	}
	return resp.StatusCode, respBody, resp.Header, nil
}

// ListBoards returns the boards visible to the session, optionally
// including archived boards. It backs the board_list MCP tool through
// the mcptools.BoardLister interface; decoding is lenient, so the extra
// fields the API returns per board (description, counts, total, ...) are
// dropped and only the consumed projection (slug, name, counts) is kept.
func (c *Client) ListBoards(ctx context.Context, includeArchived bool) ([]Board, error) {
	path := "boards"
	if includeArchived {
		path += "?include_archived=true"
	}
	body, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Boards []Board `json:"boards"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("kanban: decode GET %s response: %w", path, err)
	}
	return resp.Boards, nil
}

// isAuthFailure reports whether the response requires a session refresh:
// a 401, or any 4xx whose body carries the backend's "no_cookie" reason
// (e.g. {"error":"unauthenticated","reason":"no_cookie"}). The raw-body
// check also catches the FastAPI {"detail":"no_cookie ..."} envelope, so
// the retry decision needs no separate JSON parse.
func isAuthFailure(status int, body []byte) bool {
	if status == http.StatusUnauthorized {
		return true
	}
	return status >= 400 && status < 500 && bytes.Contains(body, []byte("no_cookie"))
}

// loginRequest is the wire shape of POST auth/password-login.
type loginRequest struct {
	Provider string `json:"provider"`
	Username string `json:"username"`
	Password string `json:"password"`
}
