// Package httpauth provides HTTP middleware for the kanban-mcp server:
// static bearer-token authentication and per-client-IP rate limiting.
// It is free of MCP-specific code and reusable as plain http.Handler
// wrappers.
package httpauth

import (
	"encoding/json"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Default limiter values used by the wiring layer (MCP_RATE_LIMIT).
const (
	// DefaultCallsPerMin is the default number of calls allowed per minute
	// per client IP.
	DefaultCallsPerMin = 60
	// DefaultBurst is the default token-bucket capacity: the number of
	// requests that may arrive back-to-back before limiting kicks in.
	DefaultBurst = 20
)

// rateLimitError is the JSON body sent with every 429 response.
type rateLimitError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// bucket is one client IP's token bucket.
type bucket struct {
	tokens float64
	last   time.Time
}

// RateLimiter is a per-remote-IP token-bucket rate limiter.
//
// Each client IP has an independent bucket holding up to burst tokens.
// Tokens refill continuously at callsPerMin/60 per second. Every request
// consumes one token; when none are available the request is rejected with
// 429 and a Retry-After header giving the seconds until the bucket refills
// at least one token.
//
// A RateLimiter created with callsPerMin <= 0 is disabled: Wrap returns the
// wrapped handler unchanged, no bucket state is allocated, and every request
// passes through.
//
// RateLimiter is safe for concurrent use by multiple goroutines.
type RateLimiter struct {
	ratePerSec float64
	burst      float64

	mu      sync.Mutex
	buckets map[string]*bucket

	// now returns the current time; tests replace it with a fake clock.
	now func() time.Time
}

// NewRateLimiter returns a RateLimiter allowing up to callsPerMin requests
// per minute per client IP, with the given burst capacity. A callsPerMin of
// 0 (or any negative value) disables limiting entirely.
func NewRateLimiter(callsPerMin, burst int) *RateLimiter {
	rl := &RateLimiter{
		burst:   float64(max(burst, 1)),
		buckets: make(map[string]*bucket),
		now:     time.Now,
	}
	if callsPerMin > 0 {
		rl.ratePerSec = float64(callsPerMin) / 60.0
	}
	return rl
}

// Wrap returns an http.Handler that rate-limits requests by client IP and
// delegates to next. When the limiter is disabled (callsPerMin <= 0), next
// is returned unchanged and all requests pass through.
func (rl *RateLimiter) Wrap(next http.Handler) http.Handler {
	if rl == nil || rl.ratePerSec <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, retryAfter := rl.allow(clientIP(r))
		if !ok {
			writeRateLimitExceeded(w, retryAfter)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP returns the host part of r.RemoteAddr (the TCP peer address).
// X-Forwarded-For is deliberately ignored: this server has no trusted-proxy
// convention in front of it, and honouring a client-supplied header would
// let callers forge their identity and bypass per-IP limits.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// No port (or empty RemoteAddr): use the raw value as the key.
		return r.RemoteAddr
	}
	return host
}

// allow reports whether a request from ip may proceed, and if not the
// duration to wait for at least one token. Rejected requests consume no
// tokens and do not advance the refill clock.
func (rl *RateLimiter) allow(ip string) (bool, time.Duration) {
	now := rl.now()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[ip]
	if !ok {
		b = &bucket{tokens: rl.burst, last: now}
		rl.buckets[ip] = b
	} else {
		b.refill(now, rl.ratePerSec, rl.burst)
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}

	// Seconds until at least one token is available. A Retry-After of 0
	// would invite immediate retry loops; the HTTP spec expects >= 1.
	wait := time.Duration(math.Ceil((1 - b.tokens) / rl.ratePerSec * float64(time.Second)))
	if wait < time.Second {
		wait = time.Second
	}
	return false, wait
}

// refill adds the tokens accrued since last, capped at burst. elapsed is
// computed from the injected clock, so tests drive refill deterministically.
func (b *bucket) refill(now time.Time, ratePerSec, burst float64) {
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(burst, b.tokens+elapsed*ratePerSec)
		b.last = now
	}
}

// writeRateLimitExceeded writes a 429 response with Retry-After and a JSON
// error body.
func writeRateLimitExceeded(w http.ResponseWriter, retryAfter time.Duration) {
	secs := int(math.Ceil(retryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(rateLimitError{
		Error:            "rate_limited",
		ErrorDescription: "rate limit exceeded",
	})
}
