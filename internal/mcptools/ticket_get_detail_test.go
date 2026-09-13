package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RKelln/hermes-kanban-mcp/internal/kanban"
)

// The ticket_get detail-mode suite. The regression it pins (2026-09-13):
// a 2,445-rune revision comment reached a remote lane agent cut to 488
// runes — "…(1957 more)" — so the steer was unreadable and the agent had
// to re-derive the design. Review threads are the round-2 implementer's
// brief; the read path must never destroy them silently.

// getBackend is a fake kanban backend serving one ticket detail envelope,
// counting the detail requests it receives (so fail-fast validation can
// be proven not to touch the backend).
type getBackend struct {
	server    *Server
	taskHits  atomic.Int32
	boardHits atomic.Int32
}

func (b *getBackend) taskRequests() int { return int(b.taskHits.Load()) }

// comments builds a comment slice whose bodies are distinguishable and
// of a known size.
func comments(specs ...[2]string) []kanban.Comment {
	out := make([]kanban.Comment, 0, len(specs))
	for i, s := range specs {
		out = append(out, kanban.Comment{Author: s[0], CreatedAt: int64(i + 1), Body: s[1]})
	}
	return out
}

// repeatBody returns a body that starts with tag and is exactly n runes
// long (rune- and byte-aware: tags like "REVISION — " are longer in bytes
// than in runes, and an off-by-two here would quietly change the size the
// test claims to reproduce).
func repeatBody(tag string, n int) string {
	runes := []rune(tag)
	if len(runes) >= n {
		return string(runes[:n])
	}
	return tag + strings.Repeat("z", n-len(runes))
}

// newGetBackend serves fx at the ticket detail endpoint.
func newGetBackend(t *testing.T, id, body string, cmts []kanban.Comment) *getBackend {
	t.Helper()
	be := &getBackend{}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/boards") {
			be.boardHits.Add(1)
			io_WriteString(w, `{"boards":[{"slug":"`+testBoard+`","name":"Hermes Agent","counts":{}}]}`)
			return
		}
		be.taskHits.Add(1)
		env := map[string]any{
			"task": map[string]any{
				"id": id, "title": "T", "status": "running", "body": body,
			},
			"comments": cmts,
			"events":   []any{},
			"runs":     []any{},
		}
		b, err := json.Marshal(env)
		if err != nil {
			t.Errorf("marshal fixture: %v", err)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(fake.Close)
	s := NewServer(fake.URL, testBoard)
	SetBoardLister(s)
	be.server = s
	return be
}

// callGet runs ticket_get, asserting a non-error result with a payload
// that parses as JSON and fits the budget the mode advertises.
func callGet(t *testing.T, be *getBackend, in TicketGetInput) TicketGetOut {
	t.Helper()
	res := be.server.TicketGet(context.Background(), in)
	if res == nil {
		t.Fatal("TicketGet returned nil")
	}
	if res.IsError {
		t.Fatalf("TicketGet error result: %s", res.Content[0].Text)
	}
	text := res.Content[0].Text
	var out TicketGetOut
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("ticket_get payload is not valid JSON: %v\npayload: %.200s", err, text)
	}
	if n := len(text); n > MaxTicketGetFullOutputBytes {
		t.Errorf("payload is %d bytes, over every mode budget", n)
	}
	return out
}

// omitsMarker parses an inline "…(N more)" marker out of body.
func omitsMarker(t *testing.T, body string) int {
	t.Helper()
	i := strings.LastIndex(body, "…(")
	if i < 0 {
		t.Fatalf("body has no omission marker: %.80s", body)
	}
	rest := body[i+len("…("):]
	sp := strings.Index(rest, " more)")
	if sp < 0 {
		t.Fatalf("malformed omission marker near %.60s", body[i:])
	}
	n, err := strconv.Atoi(rest[:sp])
	if err != nil {
		t.Fatalf("omission marker count %q does not parse: %v", rest[:sp], err)
	}
	return n
}

// TestTicketGetPartialIsDefaultAndUnchanged pins the default contract:
// omitting detail keeps the bounded caps, so existing callers (the review
// sweeper, the lane) stay cheap and see identical behavior.
func TestTicketGetPartialIsDefaultAndUnchanged(t *testing.T) {
	long := repeatBody("REVISION — ", 2445) // the live regression's length
	be := newGetBackend(t, "t_x1", "", comments([2]string{"hermes-agent", long}))

	out := callGet(t, be, TicketGetInput{ID: "t_x1", Board: testBoard})
	if out.Detail != DetailPartial {
		t.Errorf("Detail = %q, want %q", out.Detail, DetailPartial)
	}
	got := out.Comments[0].Body
	if n := len([]rune(got)); n != MaxCommentBodyChars {
		t.Errorf("partial comment is %d runes, want %d", n, MaxCommentBodyChars)
	}
	if !strings.Contains(got, "…(1957 more)") {
		t.Errorf("partial comment marker = %.40s…, want the live 1957-rune marker", got[len(got)-40:])
	}
	if !out.Comments[0].Truncated {
		t.Error("clipped comment did not set its per-comment truncated flag")
	}
	if !out.Truncated.Comments {
		t.Error("clipped comments did not set the result's truncated.comments flag")
	}
	if out.CommentsTotal != 1 || out.CommentsReturned != 1 || out.CommentsDropped != 0 {
		t.Errorf("counts = total:%d returned:%d dropped:%d, want 1/1/0",
			out.CommentsTotal, out.CommentsReturned, out.CommentsDropped)
	}
}

// TestTicketGetFullReturnsTheWholeSteer is the core regression: in full
// mode the revision comment arrives complete, tail included, and nothing
// is flagged as truncated. The partial call on the same ticket is run
// alongside to show the contrast the fix removes.
func TestTicketGetFullReturnsTheWholeSteer(t *testing.T) {
	const tail = "TAIL: a DST-boundary test pins the behaviour."
	body := repeatBody("REVISION — supersedes the proposed fix. ", 1000) + tail
	be := newGetBackend(t, "t_x1", "", comments([2]string{"hermes-agent", body}))

	partial := callGet(t, be, TicketGetInput{ID: "t_x1", Board: testBoard, Detail: DetailPartial})
	if strings.Contains(partial.Comments[0].Body, tail) {
		t.Fatal("test premise broken: partial mode kept the tail, so it proves nothing")
	}

	full := callGet(t, be, TicketGetInput{ID: "t_x1", Board: testBoard, Detail: DetailFull})
	if full.Detail != DetailFull {
		t.Errorf("Detail = %q, want %q", full.Detail, DetailFull)
	}
	if full.Comments[0].Body != body {
		t.Errorf("full mode did not return the comment verbatim (%d runes, want %d)",
			len([]rune(full.Comments[0].Body)), len([]rune(body)))
	}
	if !strings.Contains(full.Comments[0].Body, tail) {
		t.Error("full mode lost the tail of the steer")
	}
	if full.Comments[0].Truncated || full.Truncated.Comments {
		t.Error("full mode flagged truncation for a payload that fits")
	}
	if full.CommentsDropped != 0 {
		t.Errorf("CommentsDropped = %d, want 0", full.CommentsDropped)
	}
}

// TestTicketGetFullReturnsCompleteBody covers the body half of the same
// contract: partial caps it, full returns it whole.
func TestTicketGetFullReturnsCompleteBody(t *testing.T) {
	const tail = "TAIL: end of body."
	body := repeatBody("ticket body ", 9000) + tail
	be := newGetBackend(t, "t_x1", body, nil)

	partial := callGet(t, be, TicketGetInput{ID: "t_x1", Board: testBoard, Detail: DetailPartial})
	if len([]rune(partial.Body)) != MaxTicketBodyChars {
		t.Errorf("partial body is %d runes, want %d", len([]rune(partial.Body)), MaxTicketBodyChars)
	}
	if !partial.Truncated.Body {
		t.Error("partial body clip did not set truncated.body")
	}

	full := callGet(t, be, TicketGetInput{ID: "t_x1", Board: testBoard, Detail: DetailFull})
	if full.Body != body {
		t.Errorf("full body is %d runes, want %d verbatim", len([]rune(full.Body)), len([]rune(body)))
	}
	if full.Truncated.Body {
		t.Error("full body flagged truncation for a payload that fits")
	}
}

// TestTicketGetRejectsUnknownDetail proves a typo fails fast and loudly
// instead of silently degrading to partial and costing the caller
// content, and that validation happens before any backend request.
func TestTicketGetRejectsUnknownDetail(t *testing.T) {
	be := newGetBackend(t, "t_x1", "", nil)
	res := be.server.TicketGet(context.Background(), TicketGetInput{ID: "t_x1", Board: testBoard, Detail: "everything"})
	if res == nil || !res.IsError {
		t.Fatalf("unknown detail returned %+v, want an error result", res)
	}
	msg := res.Content[0].Text
	for _, want := range []string{"partial", "full"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not name the valid value %q", msg, want)
		}
	}
	if n := be.taskRequests(); n != 0 {
		t.Errorf("invalid detail made %d backend request(s), want 0 (fail fast)", n)
	}
}

// TestTicketGetFullDropsOldestFirst proves the survival order under
// budget pressure: the newest comments (the review thread) are kept
// complete and the oldest are the ones dropped, with the loss counted.
func TestTicketGetFullDropsOldestFirst(t *testing.T) {
	const n = 30
	specs := make([][2]string, 0, n)
	for i := 0; i < n; i++ {
		specs = append(specs, [2]string{
			fmt.Sprintf("author-%02d", i),
			repeatBody(fmt.Sprintf("comment-%02d ", i), 2000),
		})
	}
	be := newGetBackend(t, "t_x1", "", comments(specs...))

	out := callGet(t, be, TicketGetInput{ID: "t_x1", Board: testBoard, Detail: DetailFull})
	if out.CommentsDropped == 0 {
		t.Fatalf("expected the %d×2000-rune thread to exceed the full budget", n)
	}
	if !out.Truncated.Comments {
		t.Error("dropped comments did not set truncated.comments")
	}
	if out.CommentsTotal != n {
		t.Errorf("CommentsTotal = %d, want %d", out.CommentsTotal, n)
	}
	if out.CommentsReturned != len(out.Comments) {
		t.Errorf("CommentsReturned = %d, want %d", out.CommentsReturned, len(out.Comments))
	}
	if out.CommentsReturned+out.CommentsDropped != n {
		t.Errorf("returned %d + dropped %d != %d total", out.CommentsReturned, out.CommentsDropped, n)
	}
	// Newest survives intact...
	newest := out.Comments[len(out.Comments)-1]
	if newest.Truncated || !strings.HasPrefix(newest.Body, fmt.Sprintf("comment-%02d", n-1)) {
		t.Errorf("newest comment was not kept complete: truncated=%v body=%.40s", newest.Truncated, newest.Body)
	}
	// ...and the drops are the OLDEST, in order, so the kept slice is a
	// contiguous suffix of the source.
	firstKept := out.Comments[0].Body[:10]
	wantFirstKept := fmt.Sprintf("comment-%02d", out.CommentsDropped)
	if firstKept != wantFirstKept {
		t.Errorf("oldest surviving comment = %q, want %q (drops must come off the head)", firstKept, wantFirstKept)
	}
	if strings.Contains(strings.Join(bodies(out), " "), "comment-00") && out.CommentsDropped > 0 {
		t.Error("dropped the newest comments instead of the oldest")
	}
}

func bodies(out TicketGetOut) []string {
	b := make([]string, 0, len(out.Comments))
	for _, c := range out.Comments {
		b = append(b, c.Body)
	}
	return b
}

// TestTicketGetFullClipsSingleHugeCommentWithMarker covers the case the
// budget cannot solve by dropping: one comment larger than the whole
// budget. It must be clipped WITH a truthful marker, flagged, and the
// marker must count runes against the original — never layer.
func TestTicketGetFullClipsSingleHugeCommentWithMarker(t *testing.T) {
	const origRunes = 100000
	be := newGetBackend(t, "t_x1", "", comments([2]string{"hermes-agent", repeatBody("huge ", origRunes)}))

	out := callGet(t, be, TicketGetInput{ID: "t_x1", Board: testBoard, Detail: DetailFull})
	if !out.Comments[0].Truncated || !out.Truncated.Comments {
		t.Error("clipped comment was not flagged")
	}
	got := out.Comments[0].Body
	if c := strings.Count(got, "…("); c != 1 {
		t.Errorf("found %d omission markers, want exactly 1 (markers must never layer)", c)
	}
	if kept, omitted := len([]rune(got))-len([]rune(fmt.Sprintf("…(%d more)", omitsMarker(t, got)))), omitsMarker(t, got); kept+omitted != origRunes {
		t.Errorf("marker arithmetic: kept %d + omitted %d = %d, want the original %d", kept, omitted, kept+omitted, origRunes)
	}
	if n := len(got); n < 1024 {
		t.Errorf("clipped comment kept only %d bytes; the budget allows far more", n)
	}
}

// TestTicketGetWindowsWidenInFullMode pins the other axis of
// completeness: partial keeps the last 10 comments, full keeps the last
// 50 so a long review thread is readable rather than merely less clipped.
func TestTicketGetWindowsWidenInFullMode(t *testing.T) {
	const n = 15
	specs := make([][2]string, 0, n)
	for i := 0; i < n; i++ {
		specs = append(specs, [2]string{"author", repeatBody(fmt.Sprintf("c%02d ", i), 60)})
	}
	be := newGetBackend(t, "t_x1", "", comments(specs...))

	partial := callGet(t, be, TicketGetInput{ID: "t_x1", Board: testBoard, Detail: DetailPartial})
	if partial.CommentsReturned != MaxCommentsReturned {
		t.Errorf("partial returned %d comments, want %d", partial.CommentsReturned, MaxCommentsReturned)
	}
	if partial.CommentsTotal != n || partial.CommentsDropped != n-MaxCommentsReturned {
		t.Errorf("partial counts = total:%d dropped:%d, want %d/%d",
			partial.CommentsTotal, partial.CommentsDropped, n, n-MaxCommentsReturned)
	}
	if partial.Comments[0].Truncated {
		t.Error("60-rune comments must not be clipped at the partial cap")
	}
	if !partial.Truncated.Comments {
		t.Error("a windowed comment set must set truncated.comments even when no body was clipped")
	}

	full := callGet(t, be, TicketGetInput{ID: "t_x1", Board: testBoard, Detail: DetailFull})
	if full.CommentsReturned != n || full.CommentsDropped != 0 {
		t.Errorf("full counts = returned:%d dropped:%d, want %d/0", full.CommentsReturned, full.CommentsDropped, n)
	}
	if full.Truncated.Comments {
		t.Error("full mode flagged truncation with nothing dropped or clipped")
	}
}

// TestRenderResultNeverClipsPayload is the direct regression for the
// silent raw-chop: an oversized payload must FAIL LOUDLY, because the old
// loop emitted invalid JSON and destroyed the last-encoded content (the
// newest comments) with no flag set.
func TestRenderResultNeverClipsPayload(t *testing.T) {
	oversized := struct {
		S string `json:"s"`
	}{S: strings.Repeat("x", 9000)}

	res := renderResult(MaxTicketGetOutputBytes, false, oversized)
	if res == nil || !res.IsError {
		t.Fatalf("oversized payload returned %+v, want an error result", res)
	}
	msg := res.Content[0].Text
	if !strings.Contains(msg, "budget") {
		t.Errorf("error %q does not name the budget", msg)
	}
	// The payload itself must never be silently clipped into something
	// that parses: no success path may return less than the whole value.
	if strings.Contains(msg, "xxxx") {
		t.Error("oversized payload was echoed/clipped instead of reported")
	}

	// A value the fitter sized as fitting must be accepted, and the
	// fitter's measure must equal the rendered envelope — if these drift,
	// the fitter can shape a payload the guard then rejects.
	small := TicketGetOut{ID: "t_x1", Title: "T", Detail: DetailPartial, Truncated: TruncationFlags{}}
	ok := renderResult(MaxTicketGetOutputBytes, false, small)
	if ok.IsError {
		t.Fatalf("renderResult rejected a value that resultBytes sized as fitting: %s", ok.Content[0].Text)
	}
	raw, err := json.Marshal(ok)
	if err != nil {
		t.Fatalf("marshal rendered result: %v", err)
	}
	if got := resultBytes(small); got != len(raw) {
		t.Errorf("resultBytes = %d but the rendered envelope is %d bytes; the fitter and the guard disagree", got, len(raw))
	}
}

// TestTicketListDropsTailToFitBudget proves ticket_list also stopped
// relying on the silent chop: an oversized list drops its lowest-priority
// tail, reports the drop, and still parses.
func TestTicketListDropsTailToFitBudget(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/boards") {
			io_WriteString(w, `{"boards":[{"slug":"`+testBoard+`","name":"Hermes Agent","counts":{}}]}`)
			return
		}
		tasks := make([]map[string]any, 0, MaxTicketListLimit)
		for i := 0; i < MaxTicketListLimit; i++ {
			tasks = append(tasks, map[string]any{
				"id":             fmt.Sprintf("t_%04d", i),
				"title":          strings.Repeat("t", 200),
				"status":         "ready",
				"priority":       i,
				"latest_summary": "review-required: " + strings.Repeat("r", 300),
			})
		}
		env := map[string]any{"columns": []any{map[string]any{"name": "ready", "tasks": tasks}}}
		b, err := json.Marshal(env)
		if err != nil {
			t.Errorf("marshal board fixture: %v", err)
			return
		}
		_, _ = w.Write(b)
	}))
	defer fake.Close()

	s := NewServer(fake.URL, testBoard)
	SetBoardLister(s)
	res := s.TicketList(context.Background(), TicketListInput{Board: testBoard})
	if res == nil || res.IsError {
		t.Fatalf("TicketList error result: %+v", res)
	}
	text := res.Content[0].Text
	var out TicketListOut
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("ticket_list payload is not valid JSON: %v", err)
	}
	if n := len(text); n > MaxTicketListOutputBytes {
		t.Errorf("list payload is %d bytes, over the %d-byte budget", n, MaxTicketListOutputBytes)
	}
	if !out.Truncated {
		t.Error("an over-budget list must set truncated")
	}
	if out.Returned >= out.TotalMatched {
		t.Errorf("Returned = %d, TotalMatched = %d; want the tail dropped", out.Returned, out.TotalMatched)
	}
	if out.Returned != len(out.Tickets) {
		t.Errorf("Returned = %d but %d tickets were carried", out.Returned, len(out.Tickets))
	}
}
