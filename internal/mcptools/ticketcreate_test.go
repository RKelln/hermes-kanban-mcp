package mcptools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

// NOTE on file naming: the canonical write-tools test suite lives in
// write_tools_test.go (ticket t_b1bf0fc8). This file is the Task-0
// focused suite for ticket_create + shared helpers, kept under a
// distinct name and TestTC_* prefix so the two merge without symbol
// collisions.

// recReq is one captured backend request.
type recReq struct {
	method string
	path   string
	query  string
	body   string
}

// newRecServer spins up an httptest server that records every request
// and answers with the given status/body (default 200 created ticket).
func newRecServer(t *testing.T, status int, body string) (*httptest.Server, *[]recReq) {
	t.Helper()
	var rec []recReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec = append(rec, recReq{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(b)})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	return srv, &rec
}

// newTestServer builds a mcptools.Server pointed at an httptest server.
func newTestServer(srv *httptest.Server) *Server {
	return NewServerWithClient(srv.Client(), srv.URL, "hermes-agent")
}

// bodyKeys returns the sorted JSON keys of a captured request body.
func bodyKeys(t *testing.T, raw string) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("request body is not a JSON object: %v (body=%q)", err, raw)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestTC_CreateSuccess(t *testing.T) {
	srv, rec := newRecServer(t, http.StatusOK,
		`{"task":{"id":"t_abc123","title":"Created title","status":"triage"}}`)
	defer srv.Close()
	s := newTestServer(srv)

	in := TicketCreateInput{
		Board:          "hermes-agent",
		Title:          "Fix the widget",
		Body:           "details",
		Assignee:       "ryan",
		Priority:       3,
		WorkspaceKind:  "worktree",
		Parents:        []string{"t_parent01"},
		Skills:         []string{"go"},
		Triage:         true,
		IdempotencyKey: "k123",
	}
	res := s.TicketCreate(context.Background(), in)
	if res.IsError {
		t.Fatalf("expected success, got IsError result: %s", res.Content[0].Text)
	}
	if len(*rec) != 1 {
		t.Fatalf("expected exactly 1 backend request, got %d", len(*rec))
	}
	got := (*rec)[0]
	if got.method != http.MethodPost {
		t.Errorf("method = %s, want POST", got.method)
	}
	if got.path != "/tasks" {
		t.Errorf("path = %s, want /tasks", got.path)
	}
	if got.query != "board=hermes-agent" {
		t.Errorf("query = %s, want board=hermes-agent", got.query)
	}
	wantKeys := []string{"assignee", "body", "idempotency_key", "parents", "priority", "skills", "title", "triage", "workspace_kind"}
	if keys := bodyKeys(t, got.body); strings.Join(keys, ",") != strings.Join(wantKeys, ",") {
		t.Errorf("body keys = %v, want %v (body=%s)", keys, wantKeys, got.body)
	}

	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, res.Content[0].Text)
	}
	if out["id"] != "t_abc123" || out["status"] != "triage" || out["title"] != "Created title" || out["board"] != "hermes-agent" {
		t.Errorf("output = %v, want {id:t_abc123 status:triage title:Created title board:hermes-agent}", out)
	}
	if rendered := renderedSize(t, res); rendered > MaxToolResultBytes {
		t.Errorf("rendered result %d bytes > %d", rendered, MaxToolResultBytes)
	}
}

func TestTC_CreateOmitsZeroFields(t *testing.T) {
	srv, rec := newRecServer(t, http.StatusOK, `{"task":{"id":"t_abc123","title":"Minimal","status":"todo"}}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketCreate(context.Background(), TicketCreateInput{Board: "hermes-agent", Title: "Minimal"})
	if res.IsError {
		t.Fatalf("expected success, got: %s", res.Content[0].Text)
	}
	keys := bodyKeys(t, (*rec)[0].body)
	want := []string{"idempotency_key", "title"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("body keys = %v, want only %v (body=%s)", keys, want, (*rec)[0].body)
	}
}

func TestTC_CreateDefaultBoard(t *testing.T) {
	srv, rec := newRecServer(t, http.StatusOK, `{"task":{"id":"t_abc123","title":"T","status":"ready"}}`)
	defer srv.Close()
	s := NewServerWithClient(srv.Client(), srv.URL, "hermes-agent")

	res := s.TicketCreate(context.Background(), TicketCreateInput{Title: "T"})
	if res.IsError {
		t.Fatalf("expected success, got: %s", res.Content[0].Text)
	}
	if (*rec)[0].query != "board=hermes-agent" {
		t.Errorf("query = %s, want board=hermes-agent (default board applied)", (*rec)[0].query)
	}
}

func TestTC_CreateSynthesizesIdempotencyKey(t *testing.T) {
	body := strings.Repeat("x", 250) // > 200 chars: prefix must be truncated
	srv, rec := newRecServer(t, http.StatusOK, `{"task":{"id":"t_abc123","title":"T","status":"todo"}}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketCreate(context.Background(), TicketCreateInput{Board: "hermes-agent", Title: "Some title", Body: body})
	if res.IsError {
		t.Fatalf("expected success, got: %s", res.Content[0].Text)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte((*rec)[0].body), &sent); err != nil {
		t.Fatal(err)
	}
	key, _ := sent["idempotency_key"].(string)
	want := sha256Hex("hermes-agent|Some title|" + strings.Repeat("x", 200))[:16]
	if key != want {
		t.Errorf("synthesized idempotency_key = %q, want %q", key, want)
	}
	if len(key) != 16 {
		t.Errorf("idempotency_key length = %d, want 16", len(key))
	}
}

func TestTC_CreateSynthesizesKeyMultibyteBody(t *testing.T) {
	// 300 three-byte runes: rune-safe truncation must take the first 200
	// runes without splitting a character.
	body := strings.Repeat("—", 300)
	srv, rec := newRecServer(t, http.StatusOK, `{"task":{"id":"t_abc123","title":"T","status":"todo"}}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketCreate(context.Background(), TicketCreateInput{Board: "b", Title: "t", Body: body})
	if res.IsError {
		t.Fatalf("expected success, got: %s", res.Content[0].Text)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte((*rec)[0].body), &sent); err != nil {
		t.Fatal(err)
	}
	key, _ := sent["idempotency_key"].(string)
	want := sha256Hex("b|t|" + strings.Repeat("—", 200))[:16]
	if key != want {
		t.Errorf("synthesized idempotency_key = %q, want %q", key, want)
	}
}

func TestTC_CreateKeepsSuppliedIdempotencyKey(t *testing.T) {
	srv, rec := newRecServer(t, http.StatusOK, `{"task":{"id":"t_abc123","title":"T","status":"todo"}}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketCreate(context.Background(), TicketCreateInput{Board: "hermes-agent", Title: "T", IdempotencyKey: "custom-key-01"})
	if res.IsError {
		t.Fatalf("expected success, got: %s", res.Content[0].Text)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte((*rec)[0].body), &sent); err != nil {
		t.Fatal(err)
	}
	if key, _ := sent["idempotency_key"].(string); key != "custom-key-01" {
		t.Errorf("idempotency_key = %q, want supplied value custom-key-01", key)
	}
}

func TestTC_Create422SchemaError(t *testing.T) {
	srv, rec := newRecServer(t, http.StatusUnprocessableEntity,
		`{"detail":[{"msg":"field required"},{"msg":"should have at least 3 characters"},{"msg":"third issue ignored"}]}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketCreate(context.Background(), TicketCreateInput{Board: "hermes-agent", Title: "T"})
	if !res.IsError {
		t.Fatalf("expected IsError for 422, got success: %s", res.Content[0].Text)
	}
	want := "schema error: field required; should have at least 3 characters"
	if res.Content[0].Text != want {
		t.Errorf("error text = %q, want %q", res.Content[0].Text, want)
	}
	if len(*rec) != 1 {
		t.Errorf("expected 1 backend request, got %d", len(*rec))
	}
}

func TestTC_Create422StringDetail(t *testing.T) {
	srv, _ := newRecServer(t, http.StatusUnprocessableEntity, `{"detail":"boom"}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketCreate(context.Background(), TicketCreateInput{Board: "hermes-agent", Title: "T"})
	if !res.IsError || res.Content[0].Text != "schema error: boom" {
		t.Errorf("got %q, want %q", res.Content[0].Text, "schema error: boom")
	}
}

func TestTC_Create422NewlineCollapsed(t *testing.T) {
	srv, _ := newRecServer(t, http.StatusUnprocessableEntity, `{"detail":[{"msg":"line1\nline2"}]}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketCreate(context.Background(), TicketCreateInput{Board: "hermes-agent", Title: "T"})
	if !res.IsError {
		t.Fatalf("expected IsError, got success")
	}
	if strings.ContainsAny(res.Content[0].Text, "\r\n") {
		t.Errorf("error text contains a newline: %q", res.Content[0].Text)
	}
}

func TestTC_CreateMissingTitle(t *testing.T) {
	srv, rec := newRecServer(t, http.StatusOK, `{}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketCreate(context.Background(), TicketCreateInput{Board: "hermes-agent", Title: "   "})
	if !res.IsError {
		t.Fatalf("expected IsError for whitespace title")
	}
	if res.Content[0].Text != "invalid_input: title required" {
		t.Errorf("error text = %q, want %q", res.Content[0].Text, "invalid_input: title required")
	}
	if len(*rec) != 0 {
		t.Errorf("no request must be issued for a missing title, got %d", len(*rec))
	}
}

func TestTC_CreateInvalidBoard(t *testing.T) {
	srv, rec := newRecServer(t, http.StatusOK, `{}`)
	defer srv.Close()
	s := newTestServer(srv)

	for _, board := range []string{"HERMES", "a b", "a/b", "agent@x", strings.Repeat("a", 65)} {
		res := s.TicketCreate(context.Background(), TicketCreateInput{Board: board, Title: "T"})
		if !res.IsError {
			t.Errorf("board %q: expected IsError", board)
			continue
		}
		want := "invalid_input: invalid board \"" + board + "\""
		if res.Content[0].Text != want {
			t.Errorf("board %q: error = %q, want %q", board, res.Content[0].Text, want)
		}
	}
	if len(*rec) != 0 {
		t.Errorf("no request must be issued for an invalid board, got %d", len(*rec))
	}
}

func TestTC_CreateNoBoardConfigured(t *testing.T) {
	srv, rec := newRecServer(t, http.StatusOK, `{}`)
	defer srv.Close()
	s := NewServerWithClient(srv.Client(), srv.URL, "") // no default board

	res := s.TicketCreate(context.Background(), TicketCreateInput{Title: "T"})
	if !res.IsError {
		t.Fatalf("expected IsError when no board is resolvable")
	}
	if res.Content[0].Text != "invalid_input: no board specified; pass board or set KANBAN_DEFAULT_BOARD" {
		t.Errorf("error = %q", res.Content[0].Text)
	}
	if len(*rec) != 0 {
		t.Errorf("no request must be issued without a board, got %d", len(*rec))
	}
}

func TestTC_CreateInvalidParent(t *testing.T) {
	srv, rec := newRecServer(t, http.StatusOK, `{}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketCreate(context.Background(), TicketCreateInput{
		Board:   "hermes-agent",
		Title:   "T",
		Parents: []string{"t_good", "bad id!"},
	})
	if !res.IsError {
		t.Fatalf("expected IsError for an invalid parent id")
	}
	if res.Content[0].Text != `invalid_input: invalid parent id "bad id!"` {
		t.Errorf("error = %q", res.Content[0].Text)
	}
	if len(*rec) != 0 {
		t.Errorf("no request must be issued for an invalid parent, got %d", len(*rec))
	}
}

func TestTC_CreateBackend500(t *testing.T) {
	srv, _ := newRecServer(t, http.StatusInternalServerError, `{"detail":"boom"}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketCreate(context.Background(), TicketCreateInput{Board: "hermes-agent", Title: "T"})
	if !res.IsError {
		t.Fatalf("expected IsError for 500")
	}
	if res.Content[0].Text != "unavailable: boom" {
		t.Errorf("error = %q, want %q", res.Content[0].Text, "unavailable: boom")
	}
}

func TestTC_CreateTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close() // refuse connections
	client := &http.Client{Timeout: 2 * time.Second}
	s := NewServerWithClient(client, deadURL, "hermes-agent")

	res := s.TicketCreate(context.Background(), TicketCreateInput{Board: "hermes-agent", Title: "T"})
	if !res.IsError {
		t.Fatalf("expected IsError on transport failure")
	}
	if !strings.HasPrefix(res.Content[0].Text, "unavailable: ") {
		t.Errorf("error = %q, want unavailable: prefix", res.Content[0].Text)
	}
}

func TestTC_ResultSizeInvariant(t *testing.T) {
	// Pathological JSON-escaping blowup (quotes and ampersands) must not
	// push a rendered result over the 2 KB budget.
	msg := strings.Repeat(`&"<x>`, 4000)
	res := ErrorResult("%s", msg)
	if rendered := renderedSize(t, res); rendered > MaxToolResultBytes {
		t.Errorf("ErrorResult rendered %d bytes, budget %d", rendered, MaxToolResultBytes)
	}
	big := strings.Repeat(`{"field":"value"}`, 500)
	ok := SuccessResult(big)
	if rendered := renderedSize(t, ok); rendered > MaxToolResultBytes {
		t.Errorf("SuccessResult rendered %d bytes, budget %d", rendered, MaxToolResultBytes)
	}
}

func TestTC_GetTask(t *testing.T) {
	srv, rec := newRecServer(t, http.StatusOK, `{"task":{"id":"t_x1","title":"T","status":"running","assignee":"me"}}`)
	defer srv.Close()
	s := newTestServer(srv)

	ts, err := s.GetTask(context.Background(), "hermes-agent", "t_x1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if ts.Status != "running" || ts.ID != "t_x1" {
		t.Errorf("GetTask summary = %+v", ts)
	}
	got := (*rec)[0]
	if got.method != http.MethodGet || got.path != "/tasks/t_x1" || got.query != "board=hermes-agent" {
		t.Errorf("request = %s %s?%s, want GET /tasks/t_x1?board=hermes-agent", got.method, got.path, got.query)
	}
}

func TestTC_GetTask404(t *testing.T) {
	srv, _ := newRecServer(t, http.StatusNotFound, `{"detail":"not found"}`)
	defer srv.Close()
	s := newTestServer(srv)

	_, err := s.GetTask(context.Background(), "hermes-agent", "t_missing")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if msg := RestErrorMessage(err); msg != "not_found: not found" {
		t.Errorf("RestErrorMessage = %q, want %q", msg, "not_found: not found")
	}
}

func TestTC_Validators(t *testing.T) {
	validSlugs := []string{"hermes-agent", "a", "0", "a-b-c", strings.Repeat("a", 64)}
	for _, slug := range validSlugs {
		if err := ValidateBoardSlug(slug); err != nil {
			t.Errorf("ValidateBoardSlug(%q) = %v, want nil", slug, err)
		}
	}
	invalidSlugs := []string{"", "HERMES", "a b", "a/b", "agent@x", "a_under", strings.Repeat("a", 65)}
	for _, slug := range invalidSlugs {
		if err := ValidateBoardSlug(slug); err == nil {
			t.Errorf("ValidateBoardSlug(%q) = nil, want error", slug)
		}
	}

	validIDs := []string{"t_abc123", "t_bc1ea8dd", "ABC.def-1_2", "x"}
	for _, id := range validIDs {
		if err := ValidateTicketID(id); err != nil {
			t.Errorf("ValidateTicketID(%q) = %v, want nil", id, err)
		}
	}
	invalidIDs := []string{"", "t id", "t_ab@c", "t_ab/c", strings.Repeat("a", 65)}
	for _, id := range invalidIDs {
		if err := ValidateTicketID(id); err == nil {
			t.Errorf("ValidateTicketID(%q) = nil, want error", id)
		}
	}
}

func TestTC_ErrorMessagesOneLine(t *testing.T) {
	// Every ErrorResult must render as a single line.
	for _, msg := range []string{"simple", "line1\nline2", "a\r\nb", "multi\nline\nerror\n\n\n"} {
		res := ErrorResult("%s", msg)
		if strings.ContainsAny(res.Content[0].Text, "\r\n") {
			t.Errorf("ErrorResult(%q) is not one line: %q", msg, res.Content[0].Text)
		}
	}
}

// renderedSize marshals a ToolResult the way the wire will see it.
func renderedSize(t *testing.T, res *ToolResult) int {
	t.Helper()
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal ToolResult: %v", err)
	}
	return len(b)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
