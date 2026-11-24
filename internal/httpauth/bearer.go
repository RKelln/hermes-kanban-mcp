// Package httpauth provides HTTP middleware for the kanban-mcp server:
// static bearer-token authentication and (in ratelimit.go) rate limiting.
//
// The bearer middleware intentionally does not use the MCP SDK's
// auth.RequireBearerToken: that helper emits text/plain error bodies and
// only sets WWW-Authenticate when resource/scope options are supplied.
// opencode's remote MCP config sends a static `Authorization` header with the
// `Bearer` scheme, so we enforce exactly that wire shape here.
//
// Two tokens, two capabilities. `MCP_BEARER_TOKEN` alone (the legacy
// deployment) opens the whole surface. When `MCP_WRITE_BEARER_TOKEN` is
// also configured, `MCP_BEARER_TOKEN` becomes the READ token: it still opens
// every read tool and ticket_comment, but the tools that reach the
// dispatcher — ticket_create above all, because a `ready` card is spawned as
// a real worker process within a tick — need the write token. The decision
// lives here; the tool handlers consult CapabilityFromContext (see
// internal/mcptools/capability.go) so the refusal is an in-band MCP tool
// error that names the variable, not a bare 401.
package httpauth

import (
	"context"
	"crypto/sha256"
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

// Capability is what a request's bearer token is allowed to do.
//
// The zero value is CapabilityFull on purpose: it is the legacy
// single-token deployment, and it is also what an in-process caller gets
// from a plain context (the unit tests, a hypothetical stdio transport). The
// HTTP middleware ALWAYS records an explicit capability on the request it
// admits, so over the wire a read-token request can never be mistaken for a
// full one.
type Capability int

const (
	// CapabilityFull opens the whole surface: the legacy single token.
	CapabilityFull Capability = iota
	// CapabilityWrite opens the whole surface: the split deployment's write
	// token.
	CapabilityWrite
	// CapabilityRead opens every read tool plus ticket_comment, and is
	// refused by the tools that can put work on the host's dispatcher.
	CapabilityRead
)

// AllowsWrite reports whether the capability may call a write tool.
// Only CapabilityRead is refused; CapabilityFull and CapabilityWrite both
// write.
func (c Capability) AllowsWrite() bool { return c != CapabilityRead }

// String renders the capability for error messages and logs.
func (c Capability) String() string {
	switch c {
	case CapabilityRead:
		return "read"
	case CapabilityWrite:
		return "write"
	default:
		return "full"
	}
}

// capabilityKey is the unexported context key. Unexported type, so no other
// package can overwrite the value by accident.
type capabilityKey struct{}

// WithCapability returns ctx carrying c. The bearer middleware is its only
// production caller.
func WithCapability(ctx context.Context, c Capability) context.Context {
	return context.WithValue(ctx, capabilityKey{}, c)
}

// CapabilityFromContext reports the capability the middleware recorded.
//
// A context without one is CapabilityFull — the legacy deployment. That is
// not a hole: the token check and the capability write happen in the same
// middleware call, so every request that reaches a tool handler over HTTP
// has been through it. It only means an in-process caller bypasses the split
// by (deliberately) not using the middleware at all.
func CapabilityFromContext(ctx context.Context) Capability {
	if c, ok := ctx.Value(capabilityKey{}).(Capability); ok {
		return c
	}
	return CapabilityFull
}

// Tokens is the configured bearer credentials.
//
// Write is optional. When it is empty the deployment is the legacy
// single-token one: Read opens the whole surface. When it is set, Read is
// the read-only token and Write opens everything.
type Tokens struct {
	Read  string
	Write string
}

// Split reports whether the read/write split is configured.
func (t Tokens) Split() bool { return t.Write != "" }

// capability reports what the Authorization header grants, and whether it is
// accepted at all. The comparison is a constant-time compare of SHA-256
// digests, so neither the value nor its length leaks through timing.
func (t Tokens) capability(header string) (Capability, bool) {
	presented, ok := bearerToken(header)
	if !ok {
		return CapabilityFull, false
	}
	got := sha256.Sum256([]byte(presented))
	if t.Write != "" && subtle.ConstantTimeCompare(got[:], digest(t.Write)) == 1 {
		return CapabilityWrite, true
	}
	if subtle.ConstantTimeCompare(got[:], digest(t.Read)) == 1 {
		if !t.Split() {
			return CapabilityFull, true
		}
		return CapabilityRead, true
	}
	return CapabilityFull, false
}

func digest(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Bearer returns a middleware that requires a valid static bearer token and
// records the capability it grants on the request context.
//
// Every request whose Authorization header is missing, malformed, or does
// not carry either configured token is rejected with 401, a JSON body, and
// a WWW-Authenticate challenge. Both comparisons are constant-time.
//
// The middleware never logs the token or request bodies.
//
// It wraps any http.Handler, so every method (POST, GET, …) reaching the
// wrapped handler is protected.
//
// A note on session lifetime: the MCP SDK builds a session's context from
// the request that CREATED it (initialize), so a split-deployment client
// keeps the capability of the token it opened the session with. That cannot
// escalate a read token — the middleware still has to admit the write token
// to create a write session — but a client holding both should use one token
// per session rather than mixing them.
func Bearer(tokens Tokens, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capability, ok := tokens.capability(r.Header.Get("Authorization"))
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("WWW-Authenticate", wwwAuthenticate)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, unauthorizedBody)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithCapability(r.Context(), capability)))
	})
}

// bearerToken extracts the credential from an Authorization header value.
//
// The scheme is matched case-insensitively per RFC 6750 / RFC 7235.
// Anything else — a missing or empty header, a non-Bearer scheme, an empty
// token, or embedded whitespace — yields false.
func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	// A single token only: reject "Bearer a b" and trailing whitespace.
	if strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}
