package httpauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a deterministic clock for exercising refill behaviour without
// sleeping. RateLimiter.now is replaced with it in tests.
type fakeClock struct {
	t time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1_700_000_000, 0)}
}

func (f *fakeClock) Now() time.Time { return f.t }
func (f *fakeClock) advance(d time.Duration) {
	f.t = f.t.Add(d)
}

// countingHandler records how many times it was called.
func countingHandler(hits *int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.WriteHeader(http.StatusOK)
	})
}

// serve issues a request to h from the given remote address.
func serve(h http.Handler, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRateLimiterBurstThenLimit(t *testing.T) {
	clock := newFakeClock()
	rl := NewRateLimiter(60, 20)
	rl.now = clock.Now

	var hits int32
	h := rl.Wrap(countingHandler(&hits))

	// The first burst tokens all pass.
	for i := 0; i < 20; i++ {
		if rec := serve(h, "192.0.2.1:4000"); rec.Code != http.StatusOK {
			t.Fatalf("request %d within burst: got %d, want 200", i+1, rec.Code)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 20 {
		t.Fatalf("handler hits = %d, want 20", got)
	}

	// The next request exceeds the bucket and must not reach the handler.
	rec := serve(h, "192.0.2.1:4000")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("request past burst: got %d, want 429", rec.Code)
	}
	if got := atomic.LoadInt32(&hits); got != 20 {
		t.Fatalf("handler hits after 429 = %d, want 20 (429 must not reach the handler)", got)
	}
}

func TestRateLimiterRetryAfterAndBody(t *testing.T) {
	clock := newFakeClock()
	rl := NewRateLimiter(60, 1) // 1 token/s, burst 1
	rl.now = clock.Now

	var hits int32
	h := rl.Wrap(countingHandler(&hits))

	if rec := serve(h, "192.0.2.1:4000"); rec.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", rec.Code)
	}
	rec := serve(h, "192.0.2.1:4000")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: got %d, want 429", rec.Code)
	}

	// Retry-After must be present, numeric, and >= 1. From an empty bucket
	// at 1 token/s that is exactly 1 second.
	raw := rec.Header().Get("Retry-After")
	secs, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("Retry-After %q is not an integer: %v", raw, err)
	}
	if secs < 1 {
		t.Fatalf("Retry-After = %d, want >= 1", secs)
	}
	if raw != "1" {
		t.Errorf("Retry-After = %q, want \"1\"", raw)
	}

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("429 body is not valid JSON: %v", err)
	}
	if body["error"] != "rate_limited" {
		t.Errorf(`body["error"] = %q, want "rate_limited"`, body["error"])
	}
	if body["error_description"] == "" {
		t.Error(`body["error_description"] is empty`)
	}
}

func TestRateLimiterRefill(t *testing.T) {
	clock := newFakeClock()
	rl := NewRateLimiter(60, 1)
	rl.now = clock.Now

	var hits int32
	h := rl.Wrap(countingHandler(&hits))

	serve(h, "192.0.2.1:4000") // consume the single token
	if rec := serve(h, "192.0.2.1:4000"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after consuming the only token, got %d", rec.Code)
	}

	clock.advance(time.Second) // one token refills
	if rec := serve(h, "192.0.2.1:4000"); rec.Code != http.StatusOK {
		t.Fatalf("after 1s refill: got %d, want 200", rec.Code)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("handler hits = %d, want 2", got)
	}
}

func TestRateLimiterPartialRefillAcrossBurst(t *testing.T) {
	clock := newFakeClock()
	rl := NewRateLimiter(6, 20) // 0.1 token/s, burst 20
	rl.now = clock.Now

	var hits int32
	h := rl.Wrap(countingHandler(&hits))

	for i := 0; i < 20; i++ {
		serve(h, "192.0.2.1:4000")
	}

	// 5s later only 0.5 tokens have refilled: still limited, and Retry-After
	// reports the 5s needed to reach one full token.
	clock.advance(5 * time.Second)
	rec := serve(h, "192.0.2.1:4000")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after 5s: got %d, want 429 (0.5 tokens < 1)", rec.Code)
	}
	if raw := rec.Header().Get("Retry-After"); raw != "5" {
		t.Errorf("Retry-After = %q, want \"5\"", raw)
	}

	// 5s more -> 1.0 token: allowed.
	clock.advance(5 * time.Second)
	if rec := serve(h, "192.0.2.1:4000"); rec.Code != http.StatusOK {
		t.Fatalf("after 10s: got %d, want 200 (1 token refilled)", rec.Code)
	}
}

func TestRateLimiterIndependentBuckets(t *testing.T) {
	clock := newFakeClock()
	rl := NewRateLimiter(60, 1)
	rl.now = clock.Now

	var hits int32
	h := rl.Wrap(countingHandler(&hits))

	// 10.0.0.1 exhausts its bucket.
	serve(h, "10.0.0.1:4000")
	if rec := serve(h, "10.0.0.1:4000"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("10.0.0.1 second request: got %d, want 429", rec.Code)
	}
	// Same IP, different source port: still the same bucket (keyed by IP).
	if rec := serve(h, "10.0.0.1:5000"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("10.0.0.1 different port: got %d, want 429 (bucket keyed by IP, not IP:port)", rec.Code)
	}
	// A different IP has a fresh bucket.
	if rec := serve(h, "10.0.0.2:4000"); rec.Code != http.StatusOK {
		t.Fatalf("10.0.0.2 first request: got %d, want 200", rec.Code)
	}
	// IPv6 loopback parses correctly and is its own bucket.
	if rec := serve(h, "[::1]:4000"); rec.Code != http.StatusOK {
		t.Fatalf("[::1] first request: got %d, want 200", rec.Code)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("handler hits = %d, want 3", got)
	}
}

func TestRateLimiterDisabled(t *testing.T) {
	clock := newFakeClock()
	rl := NewRateLimiter(0, 20) // 0 disables limiting entirely
	rl.now = clock.Now

	var hits int32
	h := rl.Wrap(countingHandler(&hits))

	// Every request passes through regardless of rate or burst.
	for i := 0; i < 100; i++ {
		if rec := serve(h, "192.0.2.1:4000"); rec.Code != http.StatusOK {
			t.Fatalf("request %d with disabled limiter: got %d, want 200", i+1, rec.Code)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 100 {
		t.Fatalf("handler hits = %d, want 100", got)
	}
}

func TestRateLimiterConcurrent(t *testing.T) {
	// Real clock, loose assertion: the point is that the limiter is
	// race-free and that per-IP buckets stay independent under load.
	rl := NewRateLimiter(60, 20)
	var hits int32
	var unexpected int32
	h := rl.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))

	const clients = 8
	const perClient = 50
	var wg sync.WaitGroup
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			addr := "10.1.0." + strconv.Itoa(c+1) + ":4000"
			for i := 0; i < perClient; i++ {
				rec := serve(h, addr)
				if rec.Code != http.StatusOK && rec.Code != http.StatusTooManyRequests {
					atomic.AddInt32(&unexpected, 1)
					return
				}
			}
		}(c)
	}
	wg.Wait()

	if unexpected != 0 {
		t.Fatalf("%d responses with an unexpected status", unexpected)
	}
	allowed := atomic.LoadInt32(&hits)
	if allowed == 0 {
		t.Fatal("no requests were allowed at all")
	}
	// Each client has its own bucket of 20; refill over the run adds a
	// little. A count far above clients*burst would mean buckets are shared
	// or the limiter is not limiting.
	if allowed > int32(clients*25) {
		t.Fatalf("allowed %d requests, want <= %d (per-IP buckets)", allowed, clients*25)
	}
}
