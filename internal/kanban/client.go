// client.go implements the shared HTTP/session core for the kanban REST
// API: one *http.Client with a cookie jar and a 20s timeout, lazy +
// proactive single-flight login, and the do() wrapper that endpoint
// methods build on. Sessions are cookie-based (no Authorization header is
// ever sent) and the jar is the only store of tokens.
//
// Security constraint for this file: never log the password, session
// tokens, or any Set-Cookie value. There is deliberately no logging here
// at all; the password exists only in Client.password and the marshalled
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
// use. The zero value is not usable: create clients with New.
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
	// client at an httptest server.
	baseURL string

	// mu guards lastLoginAt; it also serializes the login itself, which
	// is what makes the login path single-flight.
	mu          sync.Mutex
	lastLoginAt time.Time // zero until the first successful login
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
		httpClient: &http.Client{
			Jar:     jar,
			Timeout: 20 * time.Second,
		},
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
// again, do returns ErrPermanentAuth wrapping the mapped error.
func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	if err := c.ensureSession(ctx, time.Time{}); err != nil {
		return nil, err
	}
	sentAt := time.Now()
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
			// both errors so errors.Is and errors.As work on the result.
			return nil, fmt.Errorf("%w: %w", ErrPermanentAuth, MapError(status, respBody))
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
// established after that request (i.e. refreshed by a concurrent caller
// while we waited for the lock) qualifies, so the retry reuses that
// refresh instead of logging in again.
func (c *Client) ensureSession(ctx context.Context, baseline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.lastLoginAt.IsZero() && c.lastLoginAt.After(baseline) && time.Since(c.lastLoginAt) <= sessionTTL {
		return nil
	}
	return c.loginLocked(ctx)
}

// loginLocked performs the password-login POST and records the session
// timestamp on success. Callers must hold c.mu. The response cookies
// (access + refresh) land in the jar via http.Client's Set-Cookie
// handling; the JSON body is read (bounded) and discarded — nothing is
// logged. A non-2xx login maps through MapError and leaves the session
// state unchanged, so the next call retries the login.
func (c *Client) loginLocked(ctx context.Context) error {
	payload, err := json.Marshal(loginRequest{
		Provider: "basic",
		Username: c.username,
		Password: c.password,
	})
	if err != nil {
		return fmt.Errorf("kanban: marshal login payload: %w", err)
	}
	status, body, err := c.doOnce(ctx, http.MethodPost, "auth/password-login", payload)
	if err != nil {
		return err
	}
	if status >= 400 {
		return MapError(status, body)
	}
	c.lastLoginAt = time.Now()
	return nil
}

// doOnce performs a single HTTP request against baseURL+path and returns
// the status and fully-read body. It does no session management and no
// retries; that orchestration lives in do and loginLocked. Content-Type:
// application/json is set when body is present (non-empty). The response
// body is read eagerly (bounded by maxBodyRead) so the caller never owns
// a connection.
func (c *Client) doOnce(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return 0, nil, fmt.Errorf("kanban: build %s %s: %w", method, path, err)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("kanban: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
	if err != nil {
		return 0, nil, fmt.Errorf("kanban: read %s %s response: %w", method, path, err)
	}
	return resp.StatusCode, respBody, nil
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
