// Package httpauth provides HTTP middleware for the kanban-mcp server:
// static bearer-token authentication and (in ratelimit.go) rate limiting.
//
// The bearer middleware intentionally does not use the MCP SDK's
// auth.RequireBearerToken: that helper emits text/plain error bodies and
// only sets WWW-Authenticate when resource/scope options are supplied.
// opencode's remote MCP config sends a static `Authorization: Bearer <t>`
// header, so we enforce exactly that wire shape here.
package httpauth

import (
	"crypto/subtle"
	"io"
	"net/http"
	"strings"
)

// The exact wire shape opencode expects for a rejected request. These are
// deliberately raw string constants so the response bytes can never drift
// from the negotiated contract.
const (
	unauthorizedBody = `{"error":"invalid_token","error_description":"invalid or missing bearer token"}`
	// wwwAuthenticate is the challenge sent on every rejected request.
	wwwAuthenticate = `Bearer error="invalid_token", error_description="invalid or missing bearer token"`
)

// Bearer returns a middleware that requires a valid static bearer token.
//
// Every request whose Authorization header is missing, malformed, or does
// not carry the exact configured token is rejected with 401, a JSON body,
// and a WWW-Authenticate challenge. The token is compared with
// crypto/subtle.ConstantTimeCompare so the comparison does not leak timing
// information about the configured value.
//
// The middleware never logs the token or request bodies.
//
// It wraps any http.Handler, so every method (POST, GET, …) reaching the
// wrapped handler is protected.
func Bearer(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validBearer(r.Header.Get("Authorization"), want) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("WWW-Authenticate", wwwAuthenticate)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, unauthorizedBody)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// validBearer reports whether the Authorization header value is a
// well-formed `Bearer <token>` credential matching want.
//
// The scheme is matched case-insensitively per RFC 6750 / RFC 7235.
// Anything else — a missing or empty header, a non-Bearer scheme, an empty
// token, embedded whitespace, or a token that does not match — is invalid.
func validBearer(header string, want []byte) bool {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return false
	}
	// A single token only: reject "Bearer a b" and trailing whitespace.
	if strings.ContainsAny(token, " \t") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), want) == 1
}
