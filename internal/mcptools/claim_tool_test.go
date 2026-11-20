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
//
// It also pins the claim's identity contract (t_7f1caa0a): the lane label is
// ASSIGNED while the card is still ready, BEFORE the claim, so the kernel's
// run row carries an implementer and the review handoff inherits it. The
// fixtures task-assignfail / task-claimfail / task-claimfail-stuck exist for
// the failure edges of that pair.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RKelln/hermes-kanban-mcp/internal/config"
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

// noEnvClaimLabel clears the ambient lane label so a test observes the
// resolution it means to (the argument, or the configured default) instead of
// the operator's environment.
func noEnvClaimLabel(t *testing.T) {
	t.Helper()
	t.Setenv("MCP_CLAIM_WORKER", "")
}

// claimSeq renders the three GET bodies of a claim that has to WRITE the lane
// label: the preflight (ready, unassigned), the read-back that proves the write
// landed, and the authoritative post-claim read.
func claimSeq(id, label string, exp int64) []string {
	return []string{
		fmt.Sprintf(`{"task":{"id":%q,"title":"T","status":"ready"}}`, id),
		fmt.Sprintf(`{"task":{"id":%q,"title":"T","status":"ready","assignee":%q}}`, id, label),
		claimedRow(id, label, exp),
	}
}

// claimedRow is the post-claim body on its own: a card that already carried the
// label, so the tool skipped the write and the read-back.
func claimedRow(id, label string, exp int64) string {
	return fmt.Sprintf(`{"task":{"id":%q,"title":"T","status":"running","assignee":%q,"claim_lock":"exp:1","claim_expires":%d}}`, id, label, exp)
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
	// Pin the knob so the caller's environment cannot decide what this
	// asserts; the default world is four hours.
	t.Setenv(kanban.ClaimTTLEnv, "")
	noEnvClaimLabel(t)
	exp := time.Now().Unix() + kanban.DefaultClaimTTLSeconds
	backend := newClaimBackend()
	backend.orders["task-ok"] = claimSeq("task-ok", defaultClaimWorkerLabel, exp)
	s := newClaimToolServer(t, backend)

	res := s.TicketClaim(context.Background(), TicketClaimInput{ID: "task-ok", Board: testBoard})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	if rendered := renderedSize(t, res); rendered > MaxToolResultBytes {
		t.Errorf("rendered result %d bytes > %d", rendered, MaxToolResultBytes)
	}
	out := decodeOut[TicketClaimOut](t, res)
	// The label is the configured default here, and both fields come from the
	// post-claim re-read, not from what was asked.
	if out.ID != "task-ok" || out.Status != "running" || out.Assignee != defaultClaimWorkerLabel {
		t.Errorf("out = %+v, want id task-ok status running assignee %s", out, defaultClaimWorkerLabel)
	}
	if out.ClaimExpires != exp {
		t.Errorf("claim_expires = %d, want %d", out.ClaimExpires, exp)
	}
	// The reported TTL is what the bridge ASKED the CLI for.
	if out.ClaimTTLSeconds != kanban.DefaultClaimTTLSeconds {
		t.Errorf("claim_ttl_seconds = %d, want %d", out.ClaimTTLSeconds, kanban.DefaultClaimTTLSeconds)
	}
	if out.ClaimTTLSource != "default" {
		t.Errorf("claim_ttl_source = %q, want %q", out.ClaimTTLSource, "default")
	}
	wantNote := claimNote(kanban.ClaimTTLSettings{Seconds: kanban.DefaultClaimTTLSeconds, Source: "default"}) +
		"; " + claimIdentityNote(defaultClaimWorkerLabel, "the server default")
	if out.Note != wantNote {
		t.Errorf("note = %q, want %q", out.Note, wantNote)
	}
	// authoritative state: exactly 3 GETs (preflight, label read-back,
	// post-claim re-read), no PATCH.
	if n := backend.getCount("task-ok"); n != 3 {
		t.Errorf("GET count = %d, want 3 (preflight + label read-back + authoritative re-read)", n)
	}
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0", n)
	}
}

// TestTC_ClaimStampsLabelThenClaims is the identity contract of t_7f1caa0a:
// the label is ASSIGNED while the card is still ready and only then claimed,
// in that order, so the kernel's _claim_and_open_run copies it into the run
// row the review handoff records its implementer from.
func TestTC_ClaimStampsLabelThenClaims(t *testing.T) {
	installFakeBin(t)
	t.Setenv(kanban.ClaimTTLEnv, "")
	noEnvClaimLabel(t)
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("HERMES_FAKE_ARGV_LOG", argvLog)
	exp := time.Now().Unix() + kanban.DefaultClaimTTLSeconds
	backend := newClaimBackend()
	backend.orders["task-ok"] = claimSeq("task-ok", "codex-orchestrator", exp)
	s := newClaimToolServer(t, backend)

	res := s.TicketClaim(context.Background(), TicketClaimInput{
		ID: "task-ok", Board: testBoard, Worker: "codex-orchestrator",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	out := decodeOut[TicketClaimOut](t, res)
	if out.Assignee != "codex-orchestrator" || out.Status != "running" {
		t.Errorf("out = %+v, want assignee codex-orchestrator status running (read back from the ticket)", out)
	}
	if !strings.Contains(out.Note, `lane label "codex-orchestrator" assigned before the claim (the worker argument)`) {
		t.Errorf("note = %q, want it to name the label and its source", out.Note)
	}

	raw, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("fake CLI captured no argv (%v): the claim path did not reach the CLI", err)
	}
	argv := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(argv) != 2 {
		t.Fatalf("argv = %q, want exactly two invocations (assign then claim)", argv)
	}
	if want := "kanban --board " + testBoard + " assign task-ok codex-orchestrator"; argv[0] != want {
		t.Errorf("argv[0] = %q, want %q", argv[0], want)
	}
	if want := fmt.Sprintf("kanban --board %s claim --ttl %d task-ok", testBoard, kanban.DefaultClaimTTLSeconds); argv[1] != want {
		t.Errorf("argv[1] = %q, want %q", argv[1], want)
	}
}

// TestTC_ClaimLabelResolution pins the precedence: explicit worker, then
// MCP_CLAIM_WORKER, then the configured default. Every row asserts the label
// that actually reached the CLI's assign argv AND the source the note reports,
// so a silently changed precedence fails here.
func TestTC_ClaimLabelResolution(t *testing.T) {
	cases := []struct {
		name, worker, env, want, wantSource string
	}{
		{"the argument wins over the knob", "codex-orchestrator", "remote-mcp", "codex-orchestrator", "the worker argument"},
		{"the knob, when no argument", "", "framework-lane", "framework-lane", "MCP_CLAIM_WORKER"},
		{"the configured default, when neither", "", "", "remote-mcp", "the server default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			installFakeBin(t)
			noEnvClaimLabel(t)
			if tc.env != "" {
				t.Setenv("MCP_CLAIM_WORKER", tc.env)
			}
			argvLog := filepath.Join(t.TempDir(), "argv.log")
			t.Setenv("HERMES_FAKE_ARGV_LOG", argvLog)
			exp := time.Now().Unix() + kanban.DefaultClaimTTLSeconds
			backend := newClaimBackend()
			backend.orders["task-ok"] = claimSeq("task-ok", tc.want, exp)
			s := newClaimToolServer(t, backend)

			res := s.TicketClaim(context.Background(), TicketClaimInput{
				ID: "task-ok", Board: testBoard, Worker: tc.worker,
			})
			if res.IsError {
				t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
			}
			out := decodeOut[TicketClaimOut](t, res)
			if out.Assignee != tc.want {
				t.Errorf("assignee = %q, want the resolved label %q", out.Assignee, tc.want)
			}
			if want := claimIdentityNote(tc.want, tc.wantSource); !strings.Contains(out.Note, want) {
				t.Errorf("note = %q, want it to contain %q", out.Note, want)
			}
			raw, err := os.ReadFile(argvLog)
			if err != nil {
				t.Fatalf("fake CLI captured no argv: %v", err)
			}
			wantAssign := "kanban --board " + testBoard + " assign task-ok " + tc.want
			if !strings.Contains(string(raw), wantAssign) {
				t.Errorf("argv = %q, want it to contain %q", string(raw), wantAssign)
			}
		})
	}
}

// TestTC_ClaimLabelDefaultMatchesConfig ties the bridge's built-in fallback to
// internal/config's MCPClaimWorker default. Two independent copies of
// "remote-mcp" is exactly how the claim and the review handoff would end up
// naming different lanes.
func TestTC_ClaimLabelDefaultMatchesConfig(t *testing.T) {
	t.Setenv("KANBAN_USERNAME", "u")
	t.Setenv("KANBAN_PASSWORD", "p")
	t.Setenv("MCP_BEARER_TOKEN", "0123456789abcdef")
	t.Setenv("MCP_COMPLETE_MODE", "review")
	noEnvClaimLabel(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.MCPClaimWorker != defaultClaimWorkerLabel {
		t.Errorf("config MCPClaimWorker default = %q, tool default = %q: change both or neither", cfg.MCPClaimWorker, defaultClaimWorkerLabel)
	}
}

// TestTC_ClaimRefusesProfileLabel is the guard that has to fire BEFORE the
// write: a ready card assigned to an installed profile is claimed by the
// dispatcher within about a minute, so assigning one invites a second worker
// onto this lane's card. Zero CLI invocations, and the message names the
// roster and the reason.
func TestTC_ClaimRefusesProfileLabel(t *testing.T) {
	installFakeBin(t)
	noEnvClaimLabel(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["task-ok"] = []string{`{"task":{"id":"task-ok","title":"T","status":"ready"}}`}
	s := newClaimToolServer(t, backend)

	// Case-insensitive: the kernel canonicalises the assignee before writing.
	for _, label := range []string{"alice", "ALICE"} {
		res := s.TicketClaim(context.Background(), TicketClaimInput{
			ID: "task-ok", Board: testBoard, Worker: label,
		})
		if !res.IsError {
			t.Fatalf("label %q: expected IsError, got success: %s", label, res.Content[0].Text)
		}
		for _, want := range []string{"IS an installed profile", "dispatcher", "second worker", "default, alice"} {
			if !strings.Contains(res.Content[0].Text, want) {
				t.Errorf("label %q: error = %q, want it to mention %q", label, res.Content[0].Text, want)
			}
		}
	}
	assertNoCLI(t, readArgv())
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0", n)
	}
}

// TestTC_ClaimRefusesUnreadableRoster fails closed: without the roster nothing
// can tell a lane label from a spawnable profile, and the collision it would
// risk is silent by construction.
func TestTC_ClaimRefusesUnreadableRoster(t *testing.T) {
	installFakeBin(t)
	noEnvClaimLabel(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.profiles = nil // GET /profiles 404s
	backend.orders["task-ok"] = []string{`{"task":{"id":"task-ok","title":"T","status":"ready"}}`}
	s := newClaimToolServer(t, backend)

	res := s.TicketClaim(context.Background(), TicketClaimInput{
		ID: "task-ok", Board: testBoard, Worker: "codex-orchestrator",
	})
	if !res.IsError {
		t.Fatalf("expected IsError for an unreadable roster, got success: %s", res.Content[0].Text)
	}
	for _, want := range []string{"could not read the installed profile roster", "refused rather than risked"} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("error = %q, want it to mention %q", res.Content[0].Text, want)
		}
	}
	assertNoCLI(t, readArgv())
}

// TestTC_ClaimLabelAlreadyOnCardSkipsWrite covers the re-claim: the card
// already carries the label, so there is nothing to write (and nothing to undo
// if the claim fails — the tool did not put it there).
func TestTC_ClaimLabelAlreadyOnCardSkipsWrite(t *testing.T) {
	installFakeBin(t)
	noEnvClaimLabel(t)
	readArgv := argvLog(t)
	exp := time.Now().Unix() + kanban.DefaultClaimTTLSeconds
	backend := newClaimBackend()
	backend.orders["task-ok"] = []string{
		`{"task":{"id":"task-ok","title":"T","status":"ready","assignee":"codex-orchestrator"}}`,
		claimedRow("task-ok", "codex-orchestrator", exp),
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketClaim(context.Background(), TicketClaimInput{
		ID: "task-ok", Board: testBoard, Worker: "codex-orchestrator",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	argv := readArgv()
	if len(argv) != 1 || !strings.Contains(argv[0], "claim --ttl") {
		t.Fatalf("argv = %v, want only the claim (no redundant assign)", argv)
	}
	if n := backend.getCount("task-ok"); n != 2 {
		t.Errorf("GET count = %d, want 2 (preflight + post-claim; nothing to read back)", n)
	}
}

// TestTC_ClaimTTLFromKnob is the card's core case: the knob reaches the CLI's
// argv and comes back in the response, with its source named. The argv half is
// read from the fake CLI's capture file, so this test fails if the tool reports
// a TTL it did not actually pass.
func TestTC_ClaimTTLFromKnob(t *testing.T) {
	installFakeBin(t)
	t.Setenv(kanban.ClaimTTLEnv, "7200")
	noEnvClaimLabel(t)
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("HERMES_FAKE_ARGV_LOG", argvLog)
	exp := time.Now().Unix() + 7200
	backend := newClaimBackend()
	backend.orders["task-ok"] = claimSeq("task-ok", defaultClaimWorkerLabel, exp)
	s := newClaimToolServer(t, backend)

	res := s.TicketClaim(context.Background(), TicketClaimInput{ID: "task-ok", Board: testBoard})
	out := decodeOut[TicketClaimOut](t, res)
	if out.ClaimTTLSeconds != 7200 {
		t.Errorf("claim_ttl_seconds = %d, want 7200 (the knob's value)", out.ClaimTTLSeconds)
	}
	if out.ClaimTTLSource != "MCP_CLAIM_TTL_SECONDS" {
		t.Errorf("claim_ttl_source = %q, want %q", out.ClaimTTLSource, "MCP_CLAIM_TTL_SECONDS")
	}
	if !strings.Contains(out.Note, "7200s") || !strings.Contains(out.Note, "MCP_CLAIM_TTL_SECONDS") {
		t.Errorf("note = %q, want it to state the applied TTL and its source", out.Note)
	}
	if strings.Contains(out.Note, "WARNING") {
		t.Errorf("note = %q, want no mismatch warning when the kernel agrees with the argv", out.Note)
	}
	logged, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("fake CLI captured no argv (%v): the claim path did not reach the CLI", err)
	}
	wantArgv := "kanban --board " + testBoard + " claim --ttl 7200 task-ok"
	if !strings.Contains(string(logged), wantArgv) {
		t.Errorf("CLI argv = %q, want it to contain %q", strings.TrimSpace(string(logged)), wantArgv)
	}
}

// TestTC_ClaimTTLDisagreementWarns is the runtime half of the "do the two
// agree?" check. A CLI that ignores --ttl is otherwise invisible: the claim
// succeeds, and the card quietly expires 15 minutes later.
func TestTC_ClaimTTLDisagreementWarns(t *testing.T) {
	installFakeBin(t)
	t.Setenv(kanban.ClaimTTLEnv, "")
	noEnvClaimLabel(t)
	exp := time.Now().Unix() + 900 // the old CLI default, i.e. the flag was ignored
	backend := newClaimBackend()
	backend.orders["task-ok"] = claimSeq("task-ok", defaultClaimWorkerLabel, exp)
	s := newClaimToolServer(t, backend)

	res := s.TicketClaim(context.Background(), TicketClaimInput{ID: "task-ok", Board: testBoard})
	out := decodeOut[TicketClaimOut](t, res)
	if !strings.Contains(out.Note, "WARNING") || !strings.Contains(out.Note, "may have ignored --ttl") {
		t.Fatalf("note = %q, want a mismatch warning naming --ttl", out.Note)
	}
	// The warning must not cost the caller the fact it still needs.
	if !strings.Contains(out.Note, "re-claim via ticket_claim") {
		t.Errorf("note = %q, want the re-claim fallback kept alongside the warning", out.Note)
	}
	if out.ClaimTTLSeconds != kanban.DefaultClaimTTLSeconds {
		t.Errorf("claim_ttl_seconds = %d, want the requested %d even though the kernel reported 900", out.ClaimTTLSeconds, kanban.DefaultClaimTTLSeconds)
	}
}

// TestTC_ClaimTTLInvalidKnob says what happened: an unusable knob falls back to
// the documented default and the response admits it rather than looking like the
// operator's value was applied.
func TestTC_ClaimTTLInvalidKnob(t *testing.T) {
	installFakeBin(t)
	t.Setenv(kanban.ClaimTTLEnv, "15m")
	noEnvClaimLabel(t)
	exp := time.Now().Unix() + kanban.DefaultClaimTTLSeconds
	backend := newClaimBackend()
	backend.orders["task-ok"] = claimSeq("task-ok", defaultClaimWorkerLabel, exp)
	s := newClaimToolServer(t, backend)

	res := s.TicketClaim(context.Background(), TicketClaimInput{ID: "task-ok", Board: testBoard})
	out := decodeOut[TicketClaimOut](t, res)
	if out.ClaimTTLSeconds != kanban.DefaultClaimTTLSeconds {
		t.Errorf("claim_ttl_seconds = %d, want the default %d", out.ClaimTTLSeconds, kanban.DefaultClaimTTLSeconds)
	}
	if !strings.Contains(out.ClaimTTLSource, "not a positive integer") {
		t.Errorf("claim_ttl_source = %q, want it to say the knob was unusable", out.ClaimTTLSource)
	}
}

// TestTC_ClaimAssignFailure is the write failing on its own: nothing was
// claimed, so the error must not tell the caller to undo anything.
func TestTC_ClaimAssignFailure(t *testing.T) {
	installFakeBin(t)
	noEnvClaimLabel(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["task-assignfail"] = []string{`{"task":{"id":"task-assignfail","title":"T","status":"ready"}}`}
	s := newClaimToolServer(t, backend)

	res := s.TicketClaim(context.Background(), TicketClaimInput{ID: "task-assignfail", Board: testBoard})
	if !res.IsError {
		t.Fatalf("expected IsError, got success: %s", res.Content[0].Text)
	}
	for _, want := range []string{"could not assign the lane label", "error: cannot assign: task is already running", "NOT claimed"} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("error = %q, want it to mention %q", res.Content[0].Text, want)
		}
	}
	// The claim must not run after a failed label write.
	argv := readArgv()
	if len(argv) != 1 || !strings.Contains(argv[0], "assign task-assignfail") {
		t.Fatalf("argv = %v, want only the failed assign", argv)
	}
}

// TestTC_ClaimFailureAfterAssignUnassigns is AC 5's first branch (t_7f1caa0a):
// the label landed and the claim then lost the race, so the tool puts the card
// back to unassigned+ready and says so.
func TestTC_ClaimFailureAfterAssignUnassigns(t *testing.T) {
	installFakeBin(t)
	t.Setenv(kanban.ClaimTTLEnv, "")
	noEnvClaimLabel(t)
	readArgv := argvLog(t)
	label := "codex-orchestrator"
	backend := newClaimBackend()
	backend.orders["task-claimfail"] = []string{
		`{"task":{"id":"task-claimfail","title":"T","status":"ready"}}`,
		fmt.Sprintf(`{"task":{"id":"task-claimfail","title":"T","status":"ready","assignee":%q}}`, label),
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketClaim(context.Background(), TicketClaimInput{
		ID: "task-claimfail", Board: testBoard, Worker: label,
	})
	if !res.IsError {
		t.Fatalf("expected IsError, got success: %s", res.Content[0].Text)
	}
	for _, want := range []string{
		"error: ticket is no longer ready (lost the CAS race)",
		"back to ready and unassigned",
		"Re-read it (ticket_get)",
	} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("error = %q, want it to mention %q", res.Content[0].Text, want)
		}
	}
	argv := readArgv()
	want := []string{
		"kanban --board " + testBoard + " assign task-claimfail " + label,
		fmt.Sprintf("kanban --board %s claim --ttl %d task-claimfail", testBoard, kanban.DefaultClaimTTLSeconds),
		"kanban --board " + testBoard + " assign task-claimfail none",
	}
	if len(argv) != len(want) {
		t.Fatalf("argv = %q, want %q", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, argv[i], want[i])
		}
	}
}

// TestTC_ClaimFailureUnassignFails is AC 5's second branch: the label cannot be
// removed either, so the caller is told the card is ready and still assigned to
// the lane label, and that an operator has to unassign it.
func TestTC_ClaimFailureUnassignFails(t *testing.T) {
	installFakeBin(t)
	t.Setenv(kanban.ClaimTTLEnv, "")
	noEnvClaimLabel(t)
	label := "codex-orchestrator"
	backend := newClaimBackend()
	backend.orders["task-claimfail-stuck"] = []string{
		`{"task":{"id":"task-claimfail-stuck","title":"T","status":"ready"}}`,
		fmt.Sprintf(`{"task":{"id":"task-claimfail-stuck","title":"T","status":"ready","assignee":%q}}`, label),
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketClaim(context.Background(), TicketClaimInput{
		ID: "task-claimfail-stuck", Board: testBoard, Worker: label,
	})
	if !res.IsError {
		t.Fatalf("expected IsError, got success: %s", res.Content[0].Text)
	}
	for _, want := range []string{
		"automatic unassign ALSO failed",
		"cannot assign: task is locked by a live claim",
		"ready and assigned to \"codex-orchestrator\"",
		"operator unassign",
	} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("error = %q, want it to mention %q", res.Content[0].Text, want)
		}
	}
}

// TestTC_ClaimThenReviewNeedsNoWorker is AC 7: identity is supplied ONCE, at
// claim. A card claimed through ticket_claim carries the label, so the review
// handoff resolves the implementer off the card with no worker argument, makes
// NO assign/reassign call (the stamp is a no-op), and still carries the
// explicit --reviewer plus --force for the live claim.
func TestTC_ClaimThenReviewNeedsNoWorker(t *testing.T) {
	installFakeBin(t)
	t.Setenv(kanban.ClaimTTLEnv, "")
	noEnvClaimLabel(t)
	label := "codex-orchestrator"
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["task-review-now"] = []string{
		// request-review's preflight: running under the claim ticket_claim took.
		fmt.Sprintf(`{"task":{"id":"task-review-now","title":"T","status":"running","assignee":%q,"claim_lock":"holder-1","claim_expires":9999999999}}`, label),
		// the authoritative post-transition read: the review row + the
		// implementer the kernel recorded.
		fmt.Sprintf(`{"task":{"id":"task-review-now","title":"T","status":"review","assignee":"alice"},`+
			`"events":[{"id":9,"kind":"review_requested","payload":{"implementer":%q,"reviewer":"alice"}}]}`, label),
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "task-review-now", Board: testBoard, Summary: "shipped it", Reviewer: "alice", Force: true,
	})
	if res.IsError {
		t.Fatalf("expected the handoff to inherit the claim's label, got IsError: %s", res.Content[0].Text)
	}
	out := decodeOut[TicketRequestReviewOut](t, res)
	if out.Implementer != label {
		t.Errorf("implementer = %q, want %q (the label ticket_claim wrote)", out.Implementer, label)
	}
	argv := readArgv()
	if len(argv) != 1 {
		t.Fatalf("argv = %v, want exactly one invocation (the handoff; the stamp is a no-op)", argv)
	}
	if strings.Contains(argv[0], "assign") || strings.Contains(argv[0], "reassign") {
		t.Errorf("argv[0] = %q, must not write the assignee when it already matches", argv[0])
	}
	for _, want := range []string{"request-review task-review-now", "--reviewer=alice", "--force"} {
		if !strings.Contains(argv[0], want) {
			t.Errorf("argv[0] = %q, want it to contain %q", argv[0], want)
		}
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
