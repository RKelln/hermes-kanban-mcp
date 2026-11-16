package mcptools

// The backend read-cap suite. The regression (2026-11-16): the togather
// board stopped being listable over MCP with
//
//	unavailable: decode kanban backend response: unexpected end of JSON input
//
// while board_list kept working. Nothing was malformed. GET /board carries
// every card's full body, so the board view grows with a board's history:
// the payload was 1,143,909 bytes, the bridge read at most 1 MiB, and
// io.LimitReader cut the document mid-JSON. json.Unmarshal reports a
// truncated document as "unexpected end of JSON input", so a SIZE limit
// presented itself as a backend fault — an error that blames the wrong
// component reads as unactionable and stays unfixed.
//
// Two properties are pinned here, and they pull in opposite directions on
// purpose:
//   - a large-but-realistic board view must DECODE (TestRest_BoardView*);
//   - a response past the cap must be REFUSED BY NAME, never clipped
//     (TestRest_OversizedResponseRefusedByName), because a silent clip is
//     how this failed in the first place — the house rule in AGENTS.md.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// boardViewFixture builds a GET /board-shaped response of at least
// target bytes and returns it with the number of cards it carries. The
// bulk rides in each card's body: that is where a real board view's
// bytes come from, and it is exactly what the list projection discards
// after decoding.
func boardViewFixture(t *testing.T, target int) ([]byte, int) {
	t.Helper()
	const cardPad = 4096
	pad := strings.Repeat("b", cardPad)
	cards := 0
	for {
		cards++
		var cols []map[string]any
		tasks := make([]map[string]any, 0, cards)
		for i := 0; i < cards; i++ {
			tasks = append(tasks, map[string]any{
				"id":     fmt.Sprintf("t_%08x", i),
				"title":  fmt.Sprintf("card %d", i),
				"status": "ready",
				"body":   pad,
			})
		}
		cols = append(cols, map[string]any{"name": "ready", "tasks": tasks})
		payload, err := json.Marshal(map[string]any{"columns": cols})
		if err != nil {
			t.Fatalf("marshal fixture: %v", err)
		}
		if len(payload) >= target {
			return payload, cards
		}
	}
}

// serveBody answers every request with body, as 200 JSON.
func serveBody(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestRest_BoardViewOverOneMiBDecodes is the regression test for the
// togather board. The fixture is deliberately larger than the 1 MiB cap
// this change replaced and no larger than a real board view (1,143,909
// bytes measured), so the test fails against the old limit with the exact
// message the lane agents saw.
func TestRest_BoardViewOverOneMiBDecodes(t *testing.T) {
	payload, wantCards := boardViewFixture(t, 1200*1024) // b > 1 MiB
	if len(payload) <= 1<<20 {
		t.Fatalf("fixture is %d bytes; it must exceed the old 1 MiB cap for this test to mean anything", len(payload))
	}
	s := NewServer(serveBody(t, payload).URL, "togather")

	var resp boardResponse
	if err := s.doJSON(context.Background(), http.MethodGet, "/board",
		url.Values{"board": {"togather"}}, nil, &resp); err != nil {
		t.Fatalf("a %d-byte board view must decode, got: %v", len(payload), err)
	}
	var got int
	for _, col := range resp.Columns {
		got += len(col.Tasks)
	}
	if got != wantCards {
		t.Errorf("decoded %d cards, want %d", got, wantCards)
	}
}

// TestRest_OversizedResponseRefusedByName pins the failure MODE. The
// response is valid JSON, so the only thing wrong with it is its size;
// the error must say so, and must not be the decode error that sent
// everyone looking at the backend.
func TestRest_OversizedResponseRefusedByName(t *testing.T) {
	payload, _ := boardViewFixture(t, 8192)
	if !json.Valid(payload) {
		t.Fatal("fixture must be valid JSON, or the test proves nothing about size")
	}
	s := NewServer(serveBody(t, payload).URL, "togather")
	s.maxBodyBytes = 4096 // shrink rather than allocate a 16 MiB fixture

	var resp boardResponse
	err := s.doJSON(context.Background(), http.MethodGet, "/board",
		url.Values{"board": {"togather"}}, nil, &resp)
	if err == nil {
		t.Fatal("an over-cap response must be refused")
	}
	msg := RestErrorMessage(err)
	if strings.Contains(msg, "unexpected end of JSON input") {
		t.Errorf("an over-cap response still reports as a decode error: %s", msg)
	}
	for _, want := range []string{"read cap", "4096", "GET /board?board=togather"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not name %q", msg, want)
		}
	}
	if !strings.HasPrefix(msg, "unavailable: ") {
		t.Errorf("a read-cap refusal must map to unavailable, got %q", msg)
	}
}

// TestRest_PayloadExactlyAtCapDecodes pins the limit+1 read: a body that
// fills the cap exactly is legal, and only a byte more is overflow. JSON
// tolerates trailing whitespace, which is how the boundary is reached
// without changing the document.
func TestRest_PayloadExactlyAtCapDecodes(t *testing.T) {
	const limit = 512
	base := `{"columns":[]}`
	atCap := base + strings.Repeat(" ", limit-len(base))
	if len(atCap) != limit {
		t.Fatalf("fixture is %d bytes, want exactly %d", len(atCap), limit)
	}

	s := NewServer(serveBody(t, []byte(atCap)).URL, "togather")
	s.maxBodyBytes = limit
	var resp boardResponse
	if err := s.doJSON(context.Background(), http.MethodGet, "/board", nil, nil, &resp); err != nil {
		t.Fatalf("a response of exactly %d bytes must decode: %v", limit, err)
	}

	over := NewServer(serveBody(t, []byte(atCap+" ")).URL, "togather")
	over.maxBodyBytes = limit
	if err := over.doJSON(context.Background(), http.MethodGet, "/board", nil, nil, &resp); err == nil {
		t.Fatalf("a response of %d bytes must be refused", limit+1)
	}
}

// TestTicketList_BoardViewOverOneMiB walks the whole tool the lane agents
// call, not just doJSON: a board whose view is past 1 MiB lists normally,
// and the rows still respect the projection's own limit.
func TestTicketList_BoardViewOverOneMiB(t *testing.T) {
	payload, wantCards := boardViewFixture(t, 1200*1024)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/boards") {
			_, _ = io.WriteString(w, `{"boards":[{"slug":"`+testBoard+`","name":"Togather","counts":{}}]}`)
			return
		}
		_, _ = w.Write(payload)
	}))
	t.Cleanup(fake.Close)
	s := NewServer(fake.URL, testBoard)
	SetBoardLister(s)

	res := s.TicketList(context.Background(), TicketListInput{Board: testBoard})
	if res == nil {
		t.Fatal("TicketList returned nil")
	}
	if res.IsError {
		t.Fatalf("ticket_list failed on a %d-byte board view: %s", len(payload), res.Content[0].Text)
	}
	var out TicketListOut
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("ticket_list payload is not valid JSON: %v", err)
	}
	if out.TotalMatched != wantCards {
		t.Errorf("total_matched = %d, want %d (every card in the view)", out.TotalMatched, wantCards)
	}
	// No limit requested, so the default window applies (not the max).
	wantReturned := wantCards
	if wantReturned > DefaultTicketListLimit {
		wantReturned = DefaultTicketListLimit
	}
	if out.Returned != wantReturned {
		t.Errorf("returned = %d, want %d", out.Returned, wantReturned)
	}
}
