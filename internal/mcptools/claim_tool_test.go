package mcptools

// claim_tool_test.go exercises TicketClaim and TicketBlock against a
// scripted kanban REST backend (httptest) and the fake hermes CLI in
// testdata/ (selected by ticket id: task-ok exits 0, task-fail prints to
// stderr and exits 1, task-hang spins until killed). HERMES_BIN is
// swapped per test and the process-level bin cache reset through the
// exported hook, so each case sees a clean resolution.
//
// Coverage per the T6 spec: success, failure, timeout, binary-missing,
// validation rejection — plus preflight rejection (todo/blocked),
// already-claimed, and the ticket_block typed/untyped/fallback matrix.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RKelln/hermes-kanban-mcp/internal/kanban"
)

// claimReq is one captured backend request.
type claimReq struct {
	method string
	path   string
	query  string
	body   string
}

// claimBackend is a scripted kanban REST backend for the claim/block
// tests. GET /tasks/{id} answers from orders, returning the first body
// on the first GET, the second on the second, and repeating the last
// thereafter (so the success path can serve "ready" for the preflight
// and "running" for the authoritative re-read). PATCH /tasks/{id}
// records the call and answers 200 with an empty object. 404s for
// unknown ids.
type claimBackend struct {
	mu     sync.Mutex
	orders map[string][]string
	reqs   []claimReq
}

func newClaimBackend() *claimBackend {
	return &claimBackend{orders: map[string][]string{}}
}

func (b *claimBackend) handler(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	id := strings.TrimPrefix(r.URL.Path, "/tasks/")
	switch r.Method {
	case http.MethodGet:
		prev := 0
		for _, rr := range b.reqs {
			if rr.method == http.MethodGet && rr.path == r.URL.Path {
				prev++
			}
		}
		b.reqs = append(b.reqs, claimReq{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery})
		bodies := b.orders[id]
		if len(bodies) == 0 {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"detail":"not found"}`)
			return
		}
		idx := prev
		if idx >= len(bodies) {
			idx = len(bodies) - 1
		}
		io.WriteString(w, bodies[idx])
	case http.MethodPatch:
		raw, _ := io.ReadAll(r.Body)
		b.reqs = append(b.reqs, claimReq{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(raw)})
		io.WriteString(w, `{}`)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// getCount returns how many GETs hit the given ticket id.
func (b *claimBackend) getCount(id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, rr := range b.reqs {
		if rr.method == http.MethodGet && strings.TrimPrefix(rr.path, "/tasks/") == id {
			n++
		}
	}
	return n
}

// patches returns every captured PATCH request.
func (b *claimBackend) patches() []claimReq {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []claimReq
	for _, rr := range b.reqs {
		if rr.method == http.MethodPatch {
			out = append(out, rr)
		}
	}
	return out
}

// newClaimToolServer wires a mcptools.Server to a scripted backend.
func newClaimToolServer(t *testing.T, backend *claimBackend) *Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(backend.handler))
	t.Cleanup(srv.Close)
	return NewServerWithClient(srv.Client(), srv.URL, testBoard)
}

// installFakeBin points HERMES_BIN at the testdata fake CLI and forces a
// fresh resolution on the next call.
func installFakeBin(t *testing.T) {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", "fake-hermes.sh"))
	if err != nil {
		t.Fatalf("resolve fake bin: %v", err)
	}
	t.Setenv("HERMES_BIN", abs)
	kanban.ResetCLIBinCacheForTests()
}

// breakBin points HERMES_BIN at a nonexistent binary so the
// claim/block-unavailable path fires.
func breakBin(t *testing.T) {
	t.Helper()
	t.Setenv("HERMES_BIN", "/nonexistent/hermes-cli")
	kanban.ResetCLIBinCacheForTests()
}

func decodeOut[T any](t *testing.T, res *ToolResult) T {
	t.Helper()
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	var out T
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, res.Content[0].Text)
	}
	return out
}

func TestTC_ClaimSuccess(t *testing.T) {
	installFakeBin(t)
	exp := time.Now().Unix() + 900
	backend := newClaimBackend()
	backend.orders["task-ok"] = []string{
		`{"task":{"id":"task-ok","title":"T","status":"ready"}}`,
		fmt.Sprintf(`{"task":{"id":"task-ok","title":"T","status":"running","assignee":"alice","claim_lock":"exp:1","claim_expires":%d}}`, exp),
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketClaim(context.Background(), TicketClaimInput{ID: "task-ok", Board: testBoard})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	if rendered := renderedSize(t, res); rendered > MaxToolResultBytes {
		t.Errorf("rendered result %d bytes > %d", rendered, MaxToolResultBytes)
	}
	out := decodeOut[TicketClaimOut](t, res)
	if out.ID != "task-ok" || out.Status != "running" || out.Assignee != "alice" {
		t.Errorf("out = %+v, want id task-ok status running assignee alice", out)
	}
	if out.ClaimExpires != exp {
		t.Errorf("claim_expires = %d, want %d", out.ClaimExpires, exp)
	}
	if out.ClaimTTLSeconds <= 0 || out.ClaimTTLSeconds > 900 {
		t.Errorf("claim_ttl_seconds = %d, want 1..900", out.ClaimTTLSeconds)
	}
	if out.Note != claimNote {
		t.Errorf("note = %q, want %q", out.Note, claimNote)
	}
	// authoritative state: exactly 2 GETs (preflight + re-read), no PATCH.
	if n := backend.getCount("task-ok"); n != 2 {
		t.Errorf("GET count = %d, want 2 (preflight + authoritative re-read)", n)
	}
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0", n)
	}
}

func TestTC_ClaimFailure(t *testing.T) {
	installFakeBin(t)
	backend := newClaimBackend()
	backend.orders["task-fail"] = []string{`{"task":{"id":"task-fail","title":"T","status":"ready"}}`}
	s := newClaimToolServer(t, backend)

	res := s.TicketClaim(context.Background(), TicketClaimInput{ID: "task-fail", Board: testBoard})
	if !res.IsError {
		t.Fatalf("expected IsError, got success: %s", res.Content[0].Text)
	}
	// first non-empty stderr line of the fake, verbatim.
	want := "error: task already claimed"
	if res.Content[0].Text != want {
		t.Errorf("error = %q, want %q", res.Content[0].Text, want)
	}
}

func TestTC_ClaimTimeout(t *testing.T) {
	installFakeBin(t)
	backend := newClaimBackend()
	backend.orders["task-hang"] = []string{`{"task":{"id":"task-hang","title":"T","status":"ready"}}`}
	s := newClaimToolServer(t, backend)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	res := s.TicketClaim(ctx, TicketClaimInput{ID: "task-hang", Board: testBoard})
	if !res.IsError {
		t.Fatalf("expected IsError, got success: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "deadline exceeded") {
		t.Errorf("error = %q, want it to mention deadline exceeded", res.Content[0].Text)
	}
}

func TestTC_ClaimBinaryMissing(t *testing.T) {
	breakBin(t)
	backend := newClaimBackend()
	backend.orders["task-ok"] = []string{`{"task":{"id":"task-ok","title":"T","status":"ready"}}`}
	s := newClaimToolServer(t, backend)

	res := s.TicketClaim(context.Background(), TicketClaimInput{ID: "task-ok", Board: testBoard})
	if !res.IsError {
		t.Fatalf("expected IsError, got success: %s", res.Content[0].Text)
	}
	want := "claim unavailable: hermes CLI not found at /nonexistent/hermes-cli (set HERMES_BIN)"
	if res.Content[0].Text != want {
		t.Errorf("error = %q, want exactly %q", res.Content[0].Text, want)
	}
	// fail-fast: the bin check precedes preflight, so the backend is
	// never consulted.
	if n := backend.getCount("task-ok"); n != 0 {
		t.Errorf("GET count = %d, want 0 (fail fast before preflight)", n)
	}
}

func TestTC_ClaimValidation(t *testing.T) {
	installFakeBin(t)
	backend := newClaimBackend()
	s := newClaimToolServer(t, backend)

	res := s.TicketClaim(context.Background(), TicketClaimInput{ID: "a b", Board: testBoard})
	if !res.IsError || res.Content[0].Text != `invalid_input: invalid ticket id "a b"` {
		t.Errorf("bad id: got %q", res.Content[0].Text)
	}
	res = s.TicketClaim(context.Background(), TicketClaimInput{ID: "task-ok", Board: "BOGUS"})
	if !res.IsError || res.Content[0].Text != `invalid_input: invalid board "BOGUS"` {
		t.Errorf("bad board: got %q", res.Content[0].Text)
	}
	res = s.TicketClaim(context.Background(), TicketClaimInput{ID: "a;rm -rf /", Board: testBoard})
	if !res.IsError {
		t.Errorf("injection id: expected IsError, got %q", res.Content[0].Text)
	}
	// nothing was exec'd or sent: the backend saw zero requests.
	if n := backend.getCount("task-ok"); n != 0 {
		t.Errorf("GET count = %d, want 0 (validation must not touch backend)", n)
	}
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0", n)
	}
}

func TestTC_ClaimPreflightRejects(t *testing.T) {
	installFakeBin(t)
	backend := newClaimBackend()
	backend.orders["task-todo"] = []string{`{"task":{"id":"task-todo","title":"T","status":"todo"}}`}
	backend.orders["task-blocked"] = []string{`{"task":{"id":"task-blocked","title":"T","status":"blocked"}}`}
	s := newClaimToolServer(t, backend)

	cases := map[string]string{
		"task-todo":    "cannot claim: ticket is todo, claim requires ready",
		"task-blocked": "cannot claim: ticket is blocked, claim requires ready",
	}
	for id, want := range cases {
		t.Run(id, func(t *testing.T) {
			res := s.TicketClaim(context.Background(), TicketClaimInput{ID: id, Board: testBoard})
			if !res.IsError || res.Content[0].Text != want {
				t.Errorf("error = %q, want %q", res.Content[0].Text, want)
			}
			// exactly one GET (preflight only): a leaked exec would
			// add a second GET for the re-read.
			if n := backend.getCount(id); n != 1 {
				t.Errorf("GET count = %d, want 1 (no exec on preflight rejection)", n)
			}
		})
	}
}

func TestTC_ClaimAlreadyClaimed(t *testing.T) {
	installFakeBin(t)
	exp := time.Now().Unix() + 600
	backend := newClaimBackend()
	backend.orders["task-running"] = []string{
		fmt.Sprintf(`{"task":{"id":"task-running","title":"T","status":"running","claim_lock":"experimance:123","claim_expires":%d}}`, exp),
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketClaim(context.Background(), TicketClaimInput{ID: "task-running", Board: testBoard})
	if !res.IsError {
		t.Fatalf("expected IsError, got success: %s", res.Content[0].Text)
	}
	want := fmt.Sprintf("already claimed (expires %d)", exp)
	if res.Content[0].Text != want {
		t.Errorf("error = %q, want %q", res.Content[0].Text, want)
	}
	if n := backend.getCount("task-running"); n != 1 {
		t.Errorf("GET count = %d, want 1 (no exec on already-claimed)", n)
	}
}

func TestTC_ClaimGetTaskFailure(t *testing.T) {
	installFakeBin(t)
	backend := newClaimBackend() // no orders: every GET 404s
	s := newClaimToolServer(t, backend)

	res := s.TicketClaim(context.Background(), TicketClaimInput{ID: "task-missing", Board: testBoard})
	if !res.IsError {
		t.Fatalf("expected IsError, got success: %s", res.Content[0].Text)
	}
	want := "not_found: not found"
	if res.Content[0].Text != want {
		t.Errorf("error = %q, want %q", res.Content[0].Text, want)
	}
}

func TestTC_BlockTypedCLI(t *testing.T) {
	installFakeBin(t)
	backend := newClaimBackend()
	s := newClaimToolServer(t, backend)

	res := s.TicketBlock(context.Background(), TicketBlockInput{ID: "task-ok", Reason: "need info", Kind: "needs_input", Board: testBoard})
	out := decodeOut[TicketBlockOut](t, res)
	if out.ID != "task-ok" || out.Status != "blocked" || !out.KindApplied {
		t.Errorf("out = %+v, want id task-ok status blocked kind_applied true", out)
	}
	if out.Note != "" {
		t.Errorf("note = %q, want empty", out.Note)
	}
	if rendered := renderedSize(t, res); rendered > MaxToolResultBytes {
		t.Errorf("rendered result %d bytes > %d", rendered, MaxToolResultBytes)
	}
	// CLI path: no REST traffic at all.
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0 (typed block via CLI)", n)
	}
}

func TestTC_BlockUntypedREST(t *testing.T) {
	installFakeBin(t)
	backend := newClaimBackend()
	s := newClaimToolServer(t, backend)

	res := s.TicketBlock(context.Background(), TicketBlockInput{ID: "task-blk", Reason: "just because", Board: testBoard})
	out := decodeOut[TicketBlockOut](t, res)
	if out.ID != "task-blk" || out.Status != "blocked" || out.KindApplied {
		t.Errorf("out = %+v, want id task-blk status blocked kind_applied false", out)
	}
	if out.Note != "" {
		t.Errorf("note = %q, want empty", out.Note)
	}
	patches := backend.patches()
	if len(patches) != 1 {
		t.Fatalf("PATCH count = %d, want 1", len(patches))
	}
	if patches[0].path != "/tasks/task-blk" {
		t.Errorf("PATCH path = %s, want /tasks/task-blk", patches[0].path)
	}
	if patches[0].query != "board=hermes-agent" {
		t.Errorf("PATCH query = %s, want board=hermes-agent", patches[0].query)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(patches[0].body), &body); err != nil {
		t.Fatalf("PATCH body not JSON: %v (%s)", err, patches[0].body)
	}
	if body["status"] != "blocked" || body["block_reason"] != "just because" {
		t.Errorf("PATCH body = %v, want status=blocked block_reason=just because", body)
	}
}

func TestTC_BlockFallbackWhenCLIUnavailable(t *testing.T) {
	breakBin(t)
	backend := newClaimBackend()
	s := newClaimToolServer(t, backend)

	res := s.TicketBlock(context.Background(), TicketBlockInput{ID: "task-blk", Reason: "waiting", Kind: "dependency", Board: testBoard})
	out := decodeOut[TicketBlockOut](t, res)
	if out.KindApplied {
		t.Errorf("kind_applied = true, want false (fallback)")
	}
	wantNote := "typed kind unavailable; recorded as untyped block"
	if out.Note != wantNote {
		t.Errorf("note = %q, want %q", out.Note, wantNote)
	}
	patches := backend.patches()
	if len(patches) != 1 {
		t.Fatalf("PATCH count = %d, want 1 (REST fallback engaged)", len(patches))
	}
	if !strings.Contains(patches[0].body, `"block_reason":"waiting"`) {
		t.Errorf("PATCH body = %s, want block_reason=waiting", patches[0].body)
	}
}

func TestTC_BlockCLIFailure(t *testing.T) {
	installFakeBin(t)
	backend := newClaimBackend()
	s := newClaimToolServer(t, backend)

	// task-fail exits 1 with stderr; that is a genuine CLI failure, NOT
	// an unavailability, so no REST fallback may engage.
	res := s.TicketBlock(context.Background(), TicketBlockInput{ID: "task-fail", Reason: "r", Kind: "transient", Board: testBoard})
	if !res.IsError {
		t.Fatalf("expected IsError, got success: %s", res.Content[0].Text)
	}
	want := "error: task already claimed"
	if res.Content[0].Text != want {
		t.Errorf("error = %q, want %q", res.Content[0].Text, want)
	}
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0 (genuine CLI failure, no fallback)", n)
	}
}

func TestTC_BlockValidation(t *testing.T) {
	installFakeBin(t)
	backend := newClaimBackend()
	s := newClaimToolServer(t, backend)

	res := s.TicketBlock(context.Background(), TicketBlockInput{ID: "task-ok", Reason: "r", Kind: "bogus", Board: testBoard})
	if !res.IsError || res.Content[0].Text != "invalid block kind: bogus" {
		t.Errorf("bad kind: got %q, want %q", res.Content[0].Text, "invalid block kind: bogus")
	}
	res = s.TicketBlock(context.Background(), TicketBlockInput{ID: "task-ok", Kind: "needs_input", Board: testBoard})
	if !res.IsError || res.Content[0].Text != "invalid_input: reason required" {
		t.Errorf("missing reason: got %q", res.Content[0].Text)
	}
	res = s.TicketBlock(context.Background(), TicketBlockInput{ID: "x;rm -rf /", Reason: "r", Kind: "dependency", Board: testBoard})
	if !res.IsError || res.Content[0].Text != `invalid_input: invalid ticket id "x;rm -rf /"` {
		t.Errorf("bad id: got %q", res.Content[0].Text)
	}
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0 (validation must not touch backend)", n)
	}
}
