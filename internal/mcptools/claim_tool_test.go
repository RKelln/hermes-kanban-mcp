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
	// profiles is the roster served by GET /profiles. A nil slice makes
	// the endpoint 404, which is how tests exercise the "roster could not
	// be read" branch. Names mirror the installed-profile roster the
	// dispatcher checks with profile_exists().
	profiles []string
}

func newClaimBackend() *claimBackend {
	return &claimBackend{
		orders:   map[string][]string{},
		profiles: []string{"default", "alice", "bob", "carol", "dave"},
	}
}

// profileProbes counts GETs to /profiles.
func (b *claimBackend) profileProbes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, rr := range b.reqs {
		if rr.path == "/profiles" {
			n++
		}
	}
	return n
}

func (b *claimBackend) handler(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/profiles" {
		b.reqs = append(b.reqs, claimReq{method: r.Method, path: r.URL.Path})
		if b.profiles == nil {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"detail":"no roster"}`)
			return
		}
		var sb strings.Builder
		sb.WriteString(`{"profiles":[`)
		for i, p := range b.profiles {
			if i > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, `{"name":%q}`, p)
		}
		sb.WriteString(`]}`)
		io.WriteString(w, sb.String())
		return
	}
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
	case http.MethodPost:
		// Comment posts (the complete-path handoff writes its comment before
		// the review transition). Recorded so a test can assert the comment
		// precedes the CLI handoff; 200 with an empty object, like the real
		// backend's comment route.
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

// snapshot returns every captured request in order, so a test can assert
// the ORDER of the traffic (e.g. the comment precedes the handoff) and
// scan the whole wire for a marker that must not appear.
func (b *claimBackend) snapshot() []claimReq {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]claimReq, len(b.reqs))
	copy(out, b.reqs)
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

// TestTC_ClaimRefusalPerStatus is the t_6053ef34 replacement for the old
// blanket "cannot claim: ticket is X, claim requires ready". Every case here
// asserts the FULL error text, so a future edit that collapses the reasons back
// into one generic line fails loudly. Each case also asserts exactly one GET (the
// preflight) and no PATCH: a refusal must never exec the CLI.
func TestTC_ClaimRefusalPerStatus(t *testing.T) {
	installFakeBin(t)
	backend := newClaimBackend()
	backend.orders["task-triage"] = []string{
		`{"task":{"id":"task-triage","title":"T","status":"triage"}}`}
	backend.orders["task-todo-parents"] = []string{
		`{"task":{"id":"task-todo-parents","title":"T","status":"todo"},"links":{"parents":["t_aaa111","t_bbb222"],"children":[]}}`}
	// The same fact carried only on the task dict: the projection's links are
	// preferred, but a backend that omits links must not turn the refusal back
	// into "todo, no parents" (which would send the caller to a human for a
	// card that is simply gated).
	backend.orders["task-todo-dict"] = []string{
		`{"task":{"id":"task-todo-dict","title":"T","status":"todo","parents":["t_ccc333"]}}`}
	backend.orders["task-todo-orphan"] = []string{
		`{"task":{"id":"task-todo-orphan","title":"T","status":"todo"}}`}
	backend.orders["task-sched"] = []string{
		`{"task":{"id":"task-sched","title":"T","status":"scheduled"}}`}
	backend.orders["task-blocked"] = []string{
		`{"task":{"id":"task-blocked","title":"T","status":"blocked"}}`}
	// running with no live lock (expired): the dispatcher's, not the caller's.
	backend.orders["task-running"] = []string{
		`{"task":{"id":"task-running","title":"T","status":"running","claim_expires":1}}`}
	backend.orders["task-review"] = []string{
		`{"task":{"id":"task-review","title":"T","status":"review"}}`}
	backend.orders["task-done"] = []string{
		`{"task":{"id":"task-done","title":"T","status":"done"}}`}
	s := newClaimToolServer(t, backend)

	cases := []struct{ id, want string }{
		{"task-triage", "cannot claim: ticket is triage, an internal intake lane where a specifier rewrites the card; no agent action exists, so surface it to a human"},
		{"task-todo-parents", "cannot claim: ticket is todo, waiting on parent dependencies: t_aaa111, t_bbb222 (the kernel promotes it to ready when they complete)"},
		{"task-todo-dict", "cannot claim: ticket is todo, waiting on parent dependencies: t_ccc333 (the kernel promotes it to ready when they complete)"},
		{"task-todo-orphan", "cannot claim: ticket is todo with no parent dependencies; the kernel should have promoted it to ready, so surface it to a human"},
		{"task-sched", "cannot claim: ticket is scheduled, waiting on its scheduled time and not dispatchable now"},
		{"task-blocked", "cannot claim: ticket is blocked and must be unblocked first; ticket_get shows the block_reason, and this bridge has no unblock tool"},
		{"task-running", "cannot claim: ticket is running without a live claim lock; the dispatcher owns it, so wait for ticket_events or surface it to a human"},
		{"task-review", "cannot claim: ticket is in review, the reviewer holds it; wait for the verdict with ticket_events"},
		{"task-done", "cannot claim: ticket is done, there is nothing left to claim"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			res := s.TicketClaim(context.Background(), TicketClaimInput{ID: tc.id, Board: testBoard})
			if !res.IsError {
				t.Fatalf("expected IsError, got success: %s", res.Content[0].Text)
			}
			if res.Content[0].Text != tc.want {
				t.Errorf("error = %q, want %q", res.Content[0].Text, tc.want)
			}
			if n := backend.getCount(tc.id); n != 1 {
				t.Errorf("GET count = %d, want 1 (preflight only, no exec)", n)
			}
		})
	}
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0 (a refusal must not write)", n)
	}
}

// TestTC_ClaimTriageRefusalNamesNoVerb pins the specific failure the incident
// hit: the triage refusal must not suggest a tool or a "next step" the caller
// can take, because no agent-facing verb exists for that lane. Naming one is
// what sent the reporting agent hunting (t_a6e3ba60, 2026-10-18).
func TestTC_ClaimTriageRefusalNamesNoVerb(t *testing.T) {
	installFakeBin(t)
	backend := newClaimBackend()
	backend.orders["task-triage"] = []string{
		`{"task":{"id":"task-triage","title":"T","status":"triage"}}`}
	s := newClaimToolServer(t, backend)

	res := s.TicketClaim(context.Background(), TicketClaimInput{ID: "task-triage", Board: testBoard})
	if !res.IsError {
		t.Fatalf("expected IsError, got success: %s", res.Content[0].Text)
	}
	msg := res.Content[0].Text
	if strings.Contains(msg, "ticket_") {
		t.Errorf("triage refusal names a tool (%q): no bridge tool exists for that lane, so naming one sends the caller hunting again", msg)
	}
	for _, want := range []string{"internal intake lane", "surface it to a human"} {
		if !strings.Contains(msg, want) {
			t.Errorf("triage refusal does not say %q: %q", want, msg)
		}
	}
	if strings.ContainsAny(msg, "\r\n") {
		t.Errorf("refusal is not one line: %q", msg)
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
