package kanban

// session.go is the credential layer for the LIVE request path: the
// *http.Client every MCP tool call goes through (mcptools.Server's
// doJSON) takes its transport from here.
//
// What used to happen (measured 2026-11-24): the bridge logged in once at
// startup and never again, so a gateway restart, a rotated dashboard
// password or a webapp sign-out turned every tool call into the backend's
// own `auth: Unauthorized` until a human restarted the bridge. The naive
// fix is worse: the dashboard throttles password login at 10 attempts per
// 60s per IP (hermes_cli/dashboard_auth/routes.py, _PW_RATE_MAX_ATTEMPTS),
// so a bridge that re-logs in on every 401 — or a systemd restart loop on
// a rejected login — keeps the window full and never authenticates again.
//
// This file therefore does three things, and nothing else:
//
//  1. re-authenticates INSIDE the request path: the Client's single-flight
//     login, then exactly one retry of the request the board refused;
//  2. NAMES the cause instead of passing the backend's word through. A
//     rejected password, a throttled login, an unreachable dashboard and a
//     session cookie the origin will not accept on /api/* are four
//     different repairs, and they used to be one string: `auth:
//     Unauthorized`;
//  3. shuts a GATE after a refusal. While it is shut no login POST is sent
//     (the tool call fails immediately with the named cause) and the waits
//     grow from 15s to 5m, so this bridge can never be the thing that keeps
//     the throttle tripped.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Login backoff. Two sequences because the two failures need different
// answers: a dashboard that REFUSED the credential is on the other side of
// its throttle, so the bridge must slow down hard; a dashboard that did not
// answer at all (restarting, refused the socket) throttled nothing, so
// recovery should be fast.
const (
	// loginRefusalFloor is the wait after the first refused login. At 15s
	// the bridge can make at most 4 attempts a minute — well inside the
	// dashboard's 10 per 60s — and each further refusal doubles it.
	loginRefusalFloor = 15 * time.Second
	// loginThrottleWindow is the dashboard's own sliding window
	// (_PW_RATE_WINDOW_SEC = 60). A 429 says "too many attempts in the
	// last 60s", so waiting the window out is the only useful response.
	loginThrottleWindow = 60 * time.Second
	// loginMaxBackoff caps both sequences.
	loginMaxBackoff = 5 * time.Minute
	// loginTransportFloor is the first wait after the dashboard failed to
	// answer AT ALL (dial refused, DNS, a 502 while the gateway boots).
	loginTransportFloor = 2 * time.Second
	// loginTransportMax caps that short sequence: long enough that a dead
	// dashboard is not hammered, short enough that a gateway restart heals
	// in seconds.
	loginTransportMax = 10 * time.Second
)

// AuthReason is the stable machine-readable cause of an authentication
// failure. It is deliberately narrower than the backend's error kinds: each
// value has exactly one repair.
type AuthReason string

const (
	// ReasonLoginRefused: the dashboard answered the login with a refusal —
	// a wrong or rotated password, or no password provider at all. The
	// credential is wrong, not stale.
	ReasonLoginRefused AuthReason = "login_refused"
	// ReasonLoginThrottled: HTTP 429 from the login route. The credential
	// may be right; this IP made too many attempts too fast.
	ReasonLoginThrottled AuthReason = "login_throttled"
	// ReasonLoginBackoff: the gate is shut after an earlier failure, so no
	// login POST was made for this call. Carries the last cause and the
	// wait.
	ReasonLoginBackoff AuthReason = "login_backoff"
	// ReasonOriginUnreachable: the dashboard did not answer (dial failure,
	// DNS, or a 5xx while it starts). Nothing was throttled.
	ReasonOriginUnreachable AuthReason = "origin_unreachable"
	// ReasonSessionRejected: the login SUCCEEDED and the board still
	// answered 401 for the very next request. The cookie is not accepted
	// on /api/* — a credential-shape problem, not a password problem.
	ReasonSessionRejected AuthReason = "session_rejected"
)

// AuthError is a named authentication failure: which credential, what the
// origin answered, and what to do about it.
//
// It exists because the old path surfaced the backend's `{"detail":
// "Unauthorized"}` verbatim (`auth: Unauthorized` in a tool result), which
// is true of a wrong password, an expired cookie, a throttled login and a
// loopback bind that wants a different credential — and names none of them.
// Detail is the one-line, secret-free sentence callers see; Reason is what
// tests and (future) structured callers key on.
type AuthError struct {
	Reason AuthReason
	// Status is the HTTP status that produced the failure, or 0 when the
	// dashboard never answered.
	Status int
	// RetryIn is how long until the bridge may try a login again; 0 when
	// the call failed for a reason that needs no new login.
	RetryIn time.Duration
	// Failures is the number of consecutive failed login attempts,
	// including this one.
	Failures int
	// Last is the cause of the previous failure, when Reason is
	// ReasonLoginBackoff.
	Last error
	// Detail is the full sentence; Error returns it unchanged.
	Detail string
}

// Error implements error.
func (e *AuthError) Error() string { return e.Detail }

// loginRefusalBackoff is the wait after the failures-th consecutive login
// the dashboard REFUSED: 15s, 30s, 1m, 2m, 4m, then 5m.
func loginRefusalBackoff(failures int) time.Duration {
	return escalate(loginRefusalFloor, loginMaxBackoff, failures)
}

// loginTransportBackoff is the wait after the failures-th consecutive
// login the dashboard did not answer: 2s, 4s, 8s, then 10s.
func loginTransportBackoff(failures int) time.Duration {
	return escalate(loginTransportFloor, loginTransportMax, failures)
}

// escalate doubles floor once per failure past the first, capped at max.
func escalate(floor, max time.Duration, failures int) time.Duration {
	d := floor
	for i := 1; i < failures && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}

// waitText renders a duration the way an operator reads it ("15s", "1m0s",
// "5m0s"), so a log line or tool error can be quoted into a fix.
func waitText(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	return d.Round(time.Second).String()
}

// reauthTransport is the transport of the *http.Client returned by
// Client.HTTPClient: it turns a 401 into (single-flight login, one retry)
// and turns every authentication failure into a body that names its cause.
//
// The naming half has to live here, not in the tool layer: mcptools maps a
// backend response through kanban.MapError, which renders `<kind>:
// <detail>` from the body. Rewriting the body is therefore the one place
// that reaches every tool — board_list, ticket_get, the claim path — with
// no per-tool knowledge of the credential.
type reauthTransport struct {
	c    *Client
	base http.RoundTripper
	// host is the host:port of the kanban API. Only that origin is retried
	// or renamed: a redirect to anywhere else is somebody else's business.
	host string
}

// RoundTrip implements http.RoundTripper.
func (t *reauthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if t.host == "" || req.URL.Host != t.host || resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}

	// A 401 means the request was not acted on, so retrying it cannot
	// double-apply a write.
	sentAt := t.c.now()
	if lerr := t.c.ensureSession(req.Context(), sentAt); lerr != nil {
		return nameAuthFailure(resp, authCause(lerr), false), nil
	}
	retry, ok := replayRequest(req, t.c)
	if !ok {
		return nameAuthFailure(resp,
			"kanban session was refreshed but the request could not be replayed (its body cannot be rewound), so it was not retried", false), nil
	}

	// From here the 401 is never returned: the response the caller gets is
	// the retry's (or an error). A RoundTripper owns every body it does not
	// hand back, so this one is drained and closed before the second
	// request. Closing it late (or never) leaks the whole connection: with
	// the real transport the socket and its readLoop stay alive for the
	// life of the process — one per re-auth, and one every gate cycle in the
	// session-REJECTED state (a misconfigured bind hit that path ~every 15s).
	drainAndClose(resp.Body)

	next, err := base.RoundTrip(retry)
	if err != nil {
		return nil, err
	}
	if next.StatusCode != http.StatusUnauthorized {
		return next, nil
	}
	// The login just succeeded and the board still refuses the session:
	// more logins cannot fix that, and trying is exactly how the throttle
	// gets tripped. Name it and shut the gate.
	return nameAuthFailure(next, t.c.recordSessionRejected().Detail, true), nil
}

// replayRequest clones req for the retry and re-reads the cookie jar.
//
// http.Client applies the jar to the ORIGINAL request before this transport
// sees it, so the header on req carries the pre-login cookies. After a
// successful login the retry must rebuild it from the jar, or it would
// re-send the very cookie that was just rejected.
func replayRequest(req *http.Request, c *Client) (*http.Request, bool) {
	r := req.Clone(req.Context())
	if req.Body != nil {
		if req.GetBody == nil {
			return nil, false
		}
		body, err := req.GetBody()
		if err != nil {
			return nil, false
		}
		r.Body = body
	}
	r.Header.Del("Cookie")
	if c.httpClient.Jar != nil {
		for _, ck := range c.httpClient.Jar.Cookies(r.URL) {
			r.AddCookie(ck)
		}
	}
	return r, true
}

// authCause renders any error as the cause half of a named failure.
func authCause(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// drainAndClose releases a response body this transport will not return.
// The drain is bounded because the payload is a 401 envelope (tiny) and a
// body past the bound could not be a recycled connection anyway; reading
// the remainder first is what lets net/http put the connection back in the
// pool instead of dropping the socket.
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	const maxDrain = 4 << 10
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxDrain))
	_ = body.Close()
}

// nameAuthFailure replaces a 401 body with a detail that names the cause.
// backendDetail says whether the response's own envelope belongs in the
// sentence: it does when the BOARD refused a session (its reason field is
// the diagnosis), and it does not when the LOGIN failed — the 401 on the
// wire is then just the request the login could not rescue, and quoting it
// would point the reader at the wrong credential, which is the bug this
// file exists to fix.
func nameAuthFailure(resp *http.Response, cause string, backendDetail bool) *http.Response {
	const maxDetailRead = 512
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxDetailRead))
	_ = resp.Body.Close()

	if resp.Header == nil {
		resp.Header = http.Header{}
	}
	body := detailBody(namedDetail(raw, cause, backendDetail))
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return resp
}

// namedDetail composes the cause-first sentence every auth failure carries.
// The backend's own shape is kept in parentheses, so nothing the server said
// is lost — but the cause comes first, because MapError truncates the
// message at 300 runes and the actionable half must survive. `reason:
// no_cookie` is pulled out of the raw envelope separately: it lives beside
// `detail`, not inside it, and it is the one field that distinguishes "the
// gate never looked at this credential" from "the credential was refused".
func namedDetail(raw []byte, cause string, backendDetail bool) string {
	detail := strings.TrimSpace(cause)
	if detail == "" {
		detail = "kanban authentication failed without a usable cause"
	}
	var notes []string
	if backendDetail {
		if backend := detailMessage(raw); backend != "" && !strings.Contains(detail, backend) {
			notes = append(notes, backend)
		}
		if bytes.Contains(raw, []byte(`"no_cookie"`)) && !strings.Contains(detail, "no_cookie") {
			notes = append(notes, "reason=no_cookie")
		}
	}
	if len(notes) > 0 {
		detail = fmt.Sprintf("%s (backend: %s)", detail, strings.Join(notes, ", "))
	}
	return detail
}

// detailBody wraps a sentence in the FastAPI error envelope MapError reads,
// so a rewritten failure travels through the tool layer as `<kind>: <detail>`.
// The marshal cannot fail for a map[string]string; the empty body it would
// degrade to maps to the status text, which is no worse than the original.
func detailBody(detail string) []byte {
	body, err := json.Marshal(map[string]string{"detail": detail})
	if err != nil {
		return nil
	}
	return body
}
