package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/RKelln/hermes-kanban-mcp/internal/kanban"
)

// readTestdata loads a fixture from the package testdata directory.
func readTestdata(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", name))
}

func TestValidateBoardSlug(t *testing.T) {
	max64 := strings.Repeat("a", 64)
	tests := []struct {
		name string
		slug string
		want bool
	}{
		{"single letter", "a", true},
		{"single digit", "1", true},
		{"hyphenated", "hermes-agent", true},
		{"numbers and hyphens", "board-2026", true},
		{"max length", max64, true},
		{"empty", "", false},
		{"uppercase", "Hermes-agent", false},
		{"underscore", "hermes_agent", false},
		{"dot", "hermes.agent", false},
		{"space", "hermes agent", false},
		{"slash", "hermes/agent", false},
		{"too long", max64 + "x", false},
		{"unicode", "hérnès", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateBoardSlug(tt.slug)
			if (err == nil) != tt.want {
				t.Errorf("ValidateBoardSlug(%q) error = %v, want error=%v", tt.slug, err, tt.want)
			}
		})
	}
}

func TestValidateTicketID(t *testing.T) {
	max64 := strings.Repeat("t", 64)
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{"kanban id", "t_bc1ea8dd", true},
		{"letters and digits", "Task123", true},
		{"dots", "t.1.2", true},
		{"underscores", "t_a_b", true},
		{"hyphens", "t-a-b", true},
		{"max length", max64, true},
		{"empty", "", false},
		{"space", "t a", false},
		{"slash", "t/a", false},
		{"hash", "t#a", false},
		{"colon", "t:a", false},
		{"too long", max64 + "x", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTicketID(tt.id)
			if (err == nil) != tt.want {
				t.Errorf("ValidateTicketID(%q) error = %v, want error=%v", tt.id, err, tt.want)
			}
		})
	}
}

func TestIsValidStatus(t *testing.T) {
	// Every value in ValidStatuses must be valid.
	for _, s := range ValidStatuses {
		if !IsValidStatus(s) {
			t.Errorf("IsValidStatus(%q) = false, want true (it is in ValidStatuses)", s)
		}
	}
	invalid := []string{"", "READY", "Ready", "ready ", " pending", "archived2", "bogus"}
	for _, s := range invalid {
		if IsValidStatus(s) {
			t.Errorf("IsValidStatus(%q) = true, want false", s)
		}
	}
}

// fakeBoardLister implements BoardLister against an httptest server
// serving the board_list fixture, counting requests.
type fakeBoardLister struct {
	server *httptest.Server
	hits   atomic.Int64
}

func (f *fakeBoardLister) ListBoards(ctx context.Context, includeArchived bool) ([]kanban.Board, error) {
	f.hits.Add(1)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.server.URL+"/boards", nil)
	if err != nil {
		return nil, err
	}
	if includeArchived {
		q := req.URL.Query()
		q.Set("include_archived", "true")
		req.URL.RawQuery = q.Encode()
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, kanban.MapError(resp.StatusCode, body)
	}
	var envelope struct {
		Boards []kanban.Board `json:"boards"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	return envelope.Boards, nil
}

// newFixtureServer serves testdata/board_list.json and returns the
// server plus a lister pointing at it.
func newFixtureServer(t *testing.T) (*httptest.Server, *fakeBoardLister) {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/boards" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(readFixture(t, "board_list.json"))
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, &fakeBoardLister{server: srv}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := readTestdata(name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return body
}

func TestBoardSlugCacheKnown(t *testing.T) {
	_, lister := newFixtureServer(t)
	c := newBoardSlugCache(lister, time.Minute)
	ctx := context.Background()

	slugs, err := c.known(ctx)
	if err != nil {
		t.Fatalf("known(): %v", err)
	}
	for _, want := range []string{"hermes-agent", "default", "bard"} {
		if _, ok := slugs[want]; !ok {
			t.Errorf("known() missing board %q; got %v", want, slugs)
		}
	}
}

func TestBoardSlugCacheTTL(t *testing.T) {
	_, lister := newFixtureServer(t)
	c := newBoardSlugCache(lister, 80*time.Millisecond)
	ctx := context.Background()

	// First call populates the cache (one backend hit).
	if _, err := c.known(ctx); err != nil {
		t.Fatalf("first known(): %v", err)
	}
	if got := lister.hits.Load(); got != 1 {
		t.Fatalf("after first call: backend hits = %d, want 1", got)
	}

	// Second call within TTL must not hit the backend.
	if _, err := c.known(ctx); err != nil {
		t.Fatalf("cached known(): %v", err)
	}
	if got := lister.hits.Load(); got != 1 {
		t.Fatalf("within TTL: backend hits = %d, want 1 (cached)", got)
	}

	// Past TTL the cache refreshes (second backend hit).
	time.Sleep(120 * time.Millisecond)
	if _, err := c.known(ctx); err != nil {
		t.Fatalf("refreshed known(): %v", err)
	}
	if got := lister.hits.Load(); got != 2 {
		t.Fatalf("after TTL expiry: backend hits = %d, want 2", got)
	}
}

func TestBoardSlugCacheMissRefreshes(t *testing.T) {
	// A cache that never populated must fetch on first use even if the
	// lister was installed late (miss => fetch).
	_, lister := newFixtureServer(t)
	c := newBoardSlugCache(nil, time.Minute)
	ctx := context.Background()

	// No lister yet: hard error, no panic.
	if _, err := c.known(ctx); err == nil {
		t.Fatal("known() with nil lister: expected error")
	}
	// Install lister: cache is a miss and must fetch.
	c.mu.Lock()
	c.lister = lister
	c.mu.Unlock()
	if _, err := c.known(ctx); err != nil {
		t.Fatalf("known() after lister install: %v", err)
	}
	if got := lister.hits.Load(); got != 1 {
		t.Fatalf("backend hits = %d, want 1 (miss fetch)", got)
	}
}

func TestEnsureKnownBoard(t *testing.T) {
	_, lister := newFixtureServer(t)
	c := newBoardSlugCache(lister, time.Minute)
	ctx := context.Background()

	if err := c.ensure(ctx, "hermes-agent"); err != nil {
		t.Errorf("ensure(hermes-agent) = %v, want nil", err)
	}
	err := c.ensure(ctx, "no-such-board")
	if err == nil {
		t.Fatal("ensure(no-such-board) = nil, want error")
	}
	want := `unknown board "no-such-board"; call board_list`
	if err.Error() != want {
		t.Errorf("ensure unknown error = %q, want %q", err.Error(), want)
	}
}

func TestMapTicketError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "404 becomes not found",
			err:  kanban.MapError(http.StatusNotFound, []byte(`{"detail":"not found"}`)),
			want: "not found: ticket t_abc123 on board hermes-agent",
		},
		{
			name: "500 becomes unavailable with status",
			err:  kanban.MapError(http.StatusInternalServerError, []byte(`{"detail":"boom"}`)),
			want: "kanban backend unavailable: status 500",
		},
		{
			name: "unexpected 3xx becomes unavailable with status",
			err:  kanban.MapError(http.StatusMovedPermanently, []byte(`{"detail":"moved"}`)),
			want: "kanban backend unavailable: status 301",
		},
		{
			name: "transport error becomes unavailable with message",
			err:  errors.New("connection refused"),
			want: "kanban backend unavailable: connection refused",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mapTicketError(tt.err, "t_abc123", "hermes-agent"); got != tt.want {
				t.Errorf("mapTicketError() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTicketReadErrorIsToolError(t *testing.T) {
	res := ticketReadError(errors.New("connection refused"), "t_abc123", "hermes-agent")
	if res == nil {
		t.Fatal("ticketReadError() = nil")
	}
	if !res.IsError {
		t.Error("ticketReadError() IsError = false, want true")
	}
	if len(res.Content) != 1 {
		t.Fatalf("ticketReadError() content len = %d, want 1", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type = %T, want text content", res.Content[0])
	}
	want := "kanban backend unavailable: connection refused"
	if text.Text != want {
		t.Errorf("content text = %q, want %q", text.Text, want)
	}
}

func TestReadToolErrorFormatting(t *testing.T) {
	res := readToolError("unknown board %q; call board_list", "bogus")
	if res == nil || !res.IsError {
		t.Fatal("readToolError() must return IsError result")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type = %T", res.Content[0])
	}
	if want := `unknown board "bogus"; call board_list`; text.Text != want {
		t.Errorf("text = %q, want %q", text.Text, want)
	}
}

func TestTicketListOmittedBoardRejected(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no backend request must be issued for an omitted board, got %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	defer fake.Close()

	s := NewServerWithClient(fake.Client(), fake.URL, testBoard)
	res := s.TicketList(context.Background(), TicketListInput{})
	if res == nil || !res.IsError {
		t.Fatalf("TicketList with omitted board: expected error result, got %+v", res)
	}
	if res.Content[0].Text != "invalid_input: board required; pass board" {
		t.Errorf("error = %q", res.Content[0].Text)
	}
}

func TestTicketListPopulatesBlockReasonFromSummary(t *testing.T) {
	// The live API omits block_reason (t_828c3b69); the reason lives in
	// latest_summary. List items must surface it so list-based tooling can
	// filter, per the ticket's fix option (a).
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/boards"):
			io_WriteString(w, `{"boards":[{"slug":"hermes-agent","name":"Hermes Agent","counts":{}}]}`)
		case strings.HasSuffix(r.URL.Path, "/board"):
			io_WriteString(w, `{"columns":[{"name":"blocked","tasks":[`+
				`{"id":"t_sum","title":"S","status":"blocked","latest_summary":"review-required: shipped it"},`+
				`{"id":"t_reason","title":"R","status":"blocked","block_reason":"review-required: via reason"}`+
				`]}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()

	s := NewServerWithClient(fake.Client(), fake.URL, testBoard)
	SetBoardLister(s)
	res := s.TicketList(context.Background(), TicketListInput{Board: testBoard, Status: []string{"blocked"}})
	if res == nil || res.IsError {
		t.Fatalf("TicketList returned error result: %+v", res)
	}
	var out TicketListOut
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	byID := map[string]TicketListItem{}
	for _, it := range out.Tickets {
		byID[it.ID] = it
	}
	if got := byID["t_sum"].BlockReason; !strings.Contains(got, "review-required") {
		t.Errorf("t_sum block_reason = %q, want the summary fallback (review-required...)", got)
	}
	if got := byID["t_reason"].BlockReason; !strings.Contains(got, "via reason") {
		t.Errorf("t_reason block_reason = %q, want the explicit reason", got)
	}
}

func TestTicketGetOmittedBoardRejected(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no backend request must be issued for an omitted board, got %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	defer fake.Close()

	s := NewServerWithClient(fake.Client(), fake.URL, testBoard)
	res := s.TicketGet(context.Background(), TicketGetInput{ID: "t_x1"})
	if res == nil || !res.IsError {
		t.Fatalf("TicketGet with omitted board: expected error result, got %+v", res)
	}
	if res.Content[0].Text != "invalid_input: board required; pass board" {
		t.Errorf("error = %q", res.Content[0].Text)
	}
}
