package mcptools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// eventsBackend is a fake kanban REST backend that serves task detail
// responses for ticket_events tests. The events array is swapped via
// atomic.Pointer so long-poll tests can change responses across ticks
// without locks.
type eventsBackend struct {
	events atomic.Pointer[[]json.RawMessage]
	// tickCount tracks how many task GETs have been served.
	tickCount atomic.Int64
	// server is the httptest server wrapping this backend.
	server *httptest.Server
}

func newEventsBackend() *eventsBackend {
	b := &eventsBackend{}
	b.events.Store(&[]json.RawMessage{})
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/boards") {
			io_WriteString(w, `{"boards":[{"slug":"hermes-agent","name":"Hermes Agent","counts":{}}]}`)
			return
		}
		b.tickCount.Add(1)
		events := b.events.Load()
		if events == nil {
			events = &[]json.RawMessage{}
		}
		payload, _ := json.Marshal(map[string]any{
			"task":     map[string]any{"id": "t_1", "title": "T", "status": "running"},
			"comments": []json.RawMessage{},
			"events":   *events,
			"runs":     []json.RawMessage{},
		})
		io_WriteString(w, string(payload))
	})
	b.server = httptest.NewServer(mux)
	return b
}

func (b *eventsBackend) setEvents(events []json.RawMessage) {
	b.events.Store(&events)
}

func (b *eventsBackend) close() {
	b.server.Close()
}

// mkEvent builds a single rawEvent as a json.RawMessage with the given
// id, kind, created_at, and optional note.
func mkEvent(id int64, kind string, createdAt int64, note string) json.RawMessage {
	ev := rawEvent{
		ID:        id,
		Kind:      kind,
		CreatedAt: createdAt,
	}
	if note != "" {
		b, _ := json.Marshal(map[string]any{"note": note})
		ev.Payload = b
	}
	raw, _ := json.Marshal(ev)
	return raw
}

func TestTE_ReturnsEventsAfterCursor(t *testing.T) {
	defer restorePollInterval(t)
	eventsPollInterval = 50 * time.Millisecond

	b := newEventsBackend()
	defer b.close()

	s := NewServer(b.server.URL, "hermes-agent")
	SetBoardLister(s)

	base := []json.RawMessage{
		mkEvent(1, "created", 1000, "start"),
		mkEvent(2, "claimed", 2000, "claimed by agent"),
		mkEvent(3, "blocked", 3000, "review-required"),
	}
	b.setEvents(base)

	input := TicketEventsInput{ID: "t_1", Board: "hermes-agent", SinceEventID: 0, TimeoutSeconds: 2}
	res := s.TicketEvents(context.Background(), input)
	if res == nil || res.IsError {
		t.Fatalf("TicketEvents returned error: %+v", res)
	}
	var out TicketEventsOut
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(out.Events) != 3 {
		t.Fatalf("got %d events, want 3", len(out.Events))
	}
	if out.Events[0].Note != "start" {
		t.Errorf("event 0 note = %q, want start", out.Events[0].Note)
	}
	if out.Events[1].Note != "claimed by agent" {
		t.Errorf("event 1 note = %q, want claimed by agent", out.Events[1].Note)
	}
	if out.Events[2].Note != "review-required" {
		t.Errorf("event 2 note = %q, want review-required", out.Events[2].Note)
	}

	// since_event_id=2 should return only event 3
	input2 := TicketEventsInput{ID: "t_1", Board: "hermes-agent", SinceEventID: 2, TimeoutSeconds: 2}
	res2 := s.TicketEvents(context.Background(), input2)
	if res2 == nil || res2.IsError {
		t.Fatalf("TicketEvents returned error: %+v", res2)
	}
	var out2 TicketEventsOut
	if err := json.Unmarshal([]byte(res2.Content[0].Text), &out2); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(out2.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(out2.Events))
	}
	if out2.Events[0].ID != 3 {
		t.Errorf("event ID = %d, want 3", out2.Events[0].ID)
	}
}

func TestTE_EmptyTimeout(t *testing.T) {
	defer restorePollInterval(t)
	eventsPollInterval = 100 * time.Millisecond

	b := newEventsBackend()
	defer b.close()

	s := NewServer(b.server.URL, "hermes-agent")
	SetBoardLister(s)

	// No events ever.
	b.setEvents([]json.RawMessage{})

	input := TicketEventsInput{ID: "t_1", Board: "hermes-agent", SinceEventID: 0, TimeoutSeconds: 2}
	start := time.Now()
	res := s.TicketEvents(context.Background(), input)
	elapsed := time.Since(start)

	if res == nil || res.IsError {
		t.Fatalf("TicketEvents returned error: %+v", res)
	}
	var out TicketEventsOut
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !out.TimedOut {
		t.Errorf("expected TimedOut true, got false")
	}
	if len(out.Events) != 0 {
		t.Errorf("expected no events, got %d", len(out.Events))
	}
	// Must actually wait ~timeout_seconds (generous bound; regression
	// that returns immediately would trip this).
	if elapsed < 1500*time.Millisecond {
		t.Errorf("timeout returned after %v, want >= ~2s (immediate return regression)", elapsed)
	}
}

func TestTE_ReturnsEarlyWhenEventArrives(t *testing.T) {
	defer restorePollInterval(t)
	eventsPollInterval = 100 * time.Millisecond

	b := newEventsBackend()
	defer b.close()

	s := NewServer(b.server.URL, "hermes-agent")
	SetBoardLister(s)

	// Empty at first, then add an event after the second tick.
	b.setEvents([]json.RawMessage{})

	go func() {
		// Wait for at least 2 polls before adding an event.
		for b.tickCount.Load() < 2 {
			time.Sleep(50 * time.Millisecond)
		}
		b.setEvents([]json.RawMessage{
			mkEvent(99, "blocked", 9999, "verdict arrived"),
		})
	}()

	input := TicketEventsInput{ID: "t_1", Board: "hermes-agent", SinceEventID: 0, TimeoutSeconds: 10}
	start := time.Now()
	res := s.TicketEvents(context.Background(), input)
	elapsed := time.Since(start)

	if res == nil || res.IsError {
		t.Fatalf("TicketEvents returned error: %+v", res)
	}
	var out TicketEventsOut
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if out.TimedOut {
		t.Error("expected TimedOut false (event arrived), got true")
	}
	if len(out.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(out.Events))
	}
	if out.Events[0].ID != 99 {
		t.Errorf("event ID = %d, want 99", out.Events[0].ID)
	}
	if out.Events[0].Kind != "blocked" {
		t.Errorf("event kind = %q, want blocked", out.Events[0].Kind)
	}
	if out.Events[0].Note != "verdict arrived" {
		t.Errorf("event note = %q, want verdict arrived", out.Events[0].Note)
	}
	t.Logf("returned early in %v", elapsed)
}

func TestTE_Validation(t *testing.T) {
	b := newEventsBackend()
	defer b.close()

	s := NewServer(b.server.URL, "hermes-agent")
	SetBoardLister(s)

	tests := []struct {
		name  string
		input TicketEventsInput
	}{
		{"bad board", TicketEventsInput{ID: "t_1", Board: "Bad Board!", TimeoutSeconds: 1}},
		{"bad id", TicketEventsInput{ID: "bad id!", Board: "hermes-agent", TimeoutSeconds: 1}},
		{"empty id", TicketEventsInput{ID: "", Board: "hermes-agent", TimeoutSeconds: 1}},
		{"empty board and no default", TicketEventsInput{ID: "t_1", Board: "", TimeoutSeconds: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use a fresh Server with no default board for the "empty board" case.
			sv := s
			if tt.name == "empty board and no default" {
				sv = NewServerWithClient(nil, b.server.URL, "")
			}
			res := sv.TicketEvents(context.Background(), tt.input)
			if res == nil || !res.IsError {
				t.Fatalf("expected error result, got: %+v", res)
			}
		})
	}
	// Validation fails early: no backend requests for any of these.
	if n := b.tickCount.Load(); n != 0 {
		t.Errorf("backend tick count = %d, want 0 (validation should reject before HTTP)", n)
	}
}

func TestTE_Budget(t *testing.T) {
	defer restorePollInterval(t)
	eventsPollInterval = 50 * time.Millisecond

	b := newEventsBackend()
	defer b.close()

	s := NewServer(b.server.URL, "hermes-agent")
	SetBoardLister(s)

	// 50 events — more than maxReturnedEvents (25).
	events := make([]json.RawMessage, 50)
	for i := 0; i < 50; i++ {
		events[i] = mkEvent(int64(i+1), "created", int64(1000+i), "event note "+strings.Repeat("x", 256))
	}
	b.setEvents(events)

	input := TicketEventsInput{ID: "t_1", Board: "hermes-agent", SinceEventID: 0, TimeoutSeconds: 2}
	res := s.TicketEvents(context.Background(), input)
	if res == nil || res.IsError {
		t.Fatalf("TicketEvents returned error: %+v", res)
	}
	// Assert the result fits in the tool result budget.
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if len(raw) > MaxTicketEventsOutputBytes {
		t.Errorf("result size %d exceeds MaxTicketEventsOutputBytes %d", len(raw), MaxTicketEventsOutputBytes)
	}
	// The result text must be parseable JSON (M1 regression).
	var out TicketEventsOut
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("result text is not valid JSON: %v (text=%q)", err, res.Content[0].Text)
	}
	if !out.Truncated {
		t.Errorf("expected Truncated true when > maxReturnedEvents matched, got false")
	}
	if len(out.Events) > maxReturnedEvents {
		t.Errorf("returned %d events, want <= %d", len(out.Events), maxReturnedEvents)
	}
}

func TestTE_MidPollTransientRetry(t *testing.T) {
	defer restorePollInterval(t)
	eventsPollInterval = 50 * time.Millisecond

	// Backend that serves one 500 mid-poll then recovers with a new event.
	var calls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/boards") {
			io_WriteString(w, `{"boards":[{"slug":"hermes-agent","name":"Hermes Agent","counts":{}}]}`)
			return
		}
		n := calls.Add(1)
		if n == 2 { // first poll (after initial fetch) fails transiently
			w.WriteHeader(http.StatusInternalServerError)
			io_WriteString(w, `{"detail":"boom"}`)
			return
		}
		payload, _ := json.Marshal(map[string]any{
			"task":     map[string]any{"id": "t_1", "title": "T", "status": "running"},
			"comments": []json.RawMessage{},
			"events":   []json.RawMessage{mkEvent(99, "blocked", 9999, "verdict")},
			"runs":     []json.RawMessage{},
		})
		io_WriteString(w, string(payload))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := NewServer(srv.URL, "hermes-agent")
	SetBoardLister(s)

	res := s.TicketEvents(context.Background(), TicketEventsInput{ID: "t_1", Board: "hermes-agent", SinceEventID: 0, TimeoutSeconds: 5})
	if res == nil || res.IsError {
		t.Fatalf("expected success despite transient mid-poll error, got: %+v", res)
	}
	var out TicketEventsOut
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(out.Events) != 1 || out.Events[0].ID != 99 {
		t.Errorf("events = %+v, want the recovered verdict event", out.Events)
	}
}

func TestTE_MidPollDefinitiveErrorAborts(t *testing.T) {
	defer restorePollInterval(t)
	eventsPollInterval = 50 * time.Millisecond

	// Backend that serves a 404 on the second request: definitive error
	// must abort immediately, not keep polling.
	var calls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/boards") {
			io_WriteString(w, `{"boards":[{"slug":"hermes-agent","name":"Hermes Agent","counts":{}}]}`)
			return
		}
		if calls.Add(1) == 2 {
			w.WriteHeader(http.StatusNotFound)
			io_WriteString(w, `{"detail":"not found"}`)
			return
		}
		payload, _ := json.Marshal(map[string]any{
			"task":     map[string]any{"id": "t_1", "title": "T", "status": "running"},
			"comments": []json.RawMessage{},
			"events":   []json.RawMessage{},
			"runs":     []json.RawMessage{},
		})
		io_WriteString(w, string(payload))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := NewServer(srv.URL, "hermes-agent")
	SetBoardLister(s)

	res := s.TicketEvents(context.Background(), TicketEventsInput{ID: "t_1", Board: "hermes-agent", SinceEventID: 0, TimeoutSeconds: 5})
	if res == nil || !res.IsError {
		t.Fatalf("expected IsError on definitive mid-poll 404, got: %+v", res)
	}
	if !strings.Contains(res.Content[0].Text, "not_found") {
		t.Errorf("error = %q, want a not_found error", res.Content[0].Text)
	}
}

func TestTE_ContextCancelled(t *testing.T) {
	defer restorePollInterval(t)
	eventsPollInterval = 100 * time.Millisecond

	b := newEventsBackend()
	defer b.close()

	s := NewServer(b.server.URL, "hermes-agent")
	SetBoardLister(s)
	b.setEvents([]json.RawMessage{}) // never delivers

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for b.tickCount.Load() < 2 {
			time.Sleep(50 * time.Millisecond)
		}
		cancel()
	}()

	res := s.TicketEvents(ctx, TicketEventsInput{ID: "t_1", Board: "hermes-agent", SinceEventID: 0, TimeoutSeconds: 30})
	if res == nil || !res.IsError {
		t.Fatalf("expected IsError on context cancellation, got: %+v", res)
	}
	if !strings.Contains(res.Content[0].Text, "context canceled") {
		t.Errorf("error = %q, want a context canceled error", res.Content[0].Text)
	}
}

func restorePollInterval(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { eventsPollInterval = 1 * time.Second })
}
