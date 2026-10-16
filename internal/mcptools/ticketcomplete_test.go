package mcptools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// NOTE on file naming: the canonical write-tools test suite lives in
// write_tools_test.go (ticket t_b1bf0fc8). This file is the
// ticket_complete focused suite, kept under a distinct name and
// TestTComplete_* prefix so the merge never collides (same convention
// as ticketcreate_test.go / TestTC_*, ticketcomment_test.go /
// TestTCM_*, ticketblock_test.go / TestTB_*). The recorder helpers
// (newRecServer, newTestServer, bodyKeys, renderedSize) are shared from
// ticketcreate_test.go; decodeCompleteOut is local and uniquely named
// so it cannot clash with decodeOut in ticketblock_test.go.

// decodeCompleteOut unmarshals a ticket_complete success payload.
func decodeCompleteOut(t *testing.T, res *ToolResult) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, res.Content[0].Text)
	}
	return out
}

// runnableRecServer answers every request 200 with a running ticket, so
// GetTask passes the claim guard and comment/PATCH succeed. The single
// canned body doubles as the GET /tasks/{id} response.
func runnableRecServer(t *testing.T) (*httptest.Server, *[]recReq) {
	t.Helper()
	return newRecServer(t, http.StatusOK, `{"task":{"id":"t_x1","title":"T","status":"running"}}`)
}

// reviewLaneRecServer is the review-path fixture for ticket_complete: it
// serves the profile roster the reviewer check verifies against, the
// pre-review status for the claim guard and request-review's own
// preflight, then the review row that handoff's post-condition requires.
// Requests are recorded in the same shape newRecServer uses, so
// assertions keep their index habits.
//
// MCP_REVIEWER_PROFILE is pinned to "default" because the handoff refuses
// to hand a card to a reviewer it cannot resolve: a card that lands in
// 'review' with no assignee the dispatcher can spawn is exactly the
// silent strand this path exists to prevent.
func reviewLaneRecServer(t *testing.T, id, preReviewStatus string) (*httptest.Server, *[]recReq) {
	t.Helper()
	t.Setenv("MCP_REVIEWER_PROFILE", "default")
	var rec []recReq
	gets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec = append(rec, recReq{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(b)})
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/profiles":
			io.WriteString(w, `{"profiles":[{"name":"default"}]}`)
		case r.Method == http.MethodGet:
			gets++
			status := preReviewStatus
			if gets > 2 {
				status = "review"
			}
			io.WriteString(w, `{"task":{"id":"`+id+`","title":"T","status":"`+status+`","assignee":"default"}}`)
		default:
			io.WriteString(w, `{}`)
		}
	}))
	return srv, &rec
}

func TestTComplete_ReviewDefaultMode(t *testing.T) {
	os.Unsetenv("MCP_COMPLETE_MODE")
	os.Unsetenv("MCP_ALLOW_SKIP_CLAIM")
	readArgv := argvLog(t)
	srv, rec := reviewLaneRecServer(t, "t_x1", "running")
	defer srv.Close()
	s := newTestServer(srv) // server default board testBoard (board passed explicitly below)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board:    testBoard,
		ID:       "t_x1",
		Summary:  "Shipped the widget",
		Result:   "3 tests passed",
		Metadata: `{"changed_files":["a.go"]}`,
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	// Order: the claim-guard read, then the comment, then the handoff.
	if (*rec)[0].method != http.MethodGet || (*rec)[0].path != "/tasks/t_x1" {
		t.Errorf("request 0 = %s %s, want GET /tasks/t_x1", (*rec)[0].method, (*rec)[0].path)
	}
	comment := (*rec)[1]
	if comment.method != http.MethodPost || comment.path != "/tasks/t_x1/comments" || comment.query != "board=hermes-agent" {
		t.Errorf("request 1 = %s %s?%s, want POST /tasks/t_x1/comments?board=hermes-agent", comment.method, comment.path, comment.query)
	}
	if keys := bodyKeys(t, comment.body); strings.Join(keys, ",") != "author,body" {
		t.Errorf("comment body keys = %v, want exactly [author body] (body=%s)", keys, comment.body)
	}
	var sent commentBody
	if err := json.Unmarshal([]byte(comment.body), &sent); err != nil {
		t.Fatalf("comment body not a commentBody: %v (%s)", err, comment.body)
	}
	wantComment := "Shipped the widget\nresult: 3 tests passed\nmetadata: {\"changed_files\":[\"a.go\"]}"
	if sent.Body != wantComment {
		t.Errorf("comment body = %q, want %q", sent.Body, wantComment)
	}
	// The transition is the CLI handoff, so nothing PATCHes a status. In
	// particular the retired review-required block must not reappear: its
	// only consumer was the review-sweeper cron, retired 2026-09-14.
	for _, rr := range *rec {
		if rr.method == http.MethodPatch {
			t.Errorf("review path PATCHed the ticket: %s %s body=%s", rr.method, rr.path, rr.body)
		}
	}
	argv := readArgv()
	if len(argv) != 1 || !strings.Contains(argv[0], "request-review t_x1") {
		t.Errorf("argv = %v, want exactly one request-review handoff", argv)
	}

	out := decodeCompleteOut(t, res)
	if out["id"] != "t_x1" || out["final_status"] != "review" || out["review_required"] != true {
		t.Errorf("output = %v, want {id:t_x1 final_status:review review_required:true}", out)
	}
	if rendered := renderedSize(t, res); rendered > MaxToolResultBytes {
		t.Errorf("rendered result %d bytes > %d", rendered, MaxToolResultBytes)
	}
}

func TestTComplete_ReviewExplicitEnv(t *testing.T) {
	t.Setenv("MCP_COMPLETE_MODE", "review")
	argvLog(t)
	srv, rec := reviewLaneRecServer(t, "t_x1", "running")
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{Board: testBoard, ID: "t_x1", Summary: "s"})
	if res.IsError {
		t.Fatalf("expected success, got: %s", res.Content[0].Text)
	}
	for _, rr := range *rec {
		if rr.method == http.MethodPatch {
			t.Errorf("explicit review mode PATCHed the ticket: %s", rr.body)
		}
	}
	out := decodeCompleteOut(t, res)
	if out["final_status"] != "review" || out["review_required"] != true {
		t.Errorf("output = %v, want the review path", out)
	}
}

func TestTComplete_ReviewOmitsEmptyResultMetadata(t *testing.T) {
	os.Unsetenv("MCP_COMPLETE_MODE")
	argvLog(t)
	srv, rec := reviewLaneRecServer(t, "t_x1", "running")
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{Board: testBoard, ID: "t_x1", Summary: "just summary"})
	if res.IsError {
		t.Fatalf("expected success, got: %s", res.Content[0].Text)
	}
	var sent commentBody
	if err := json.Unmarshal([]byte((*rec)[1].body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Body != "just summary" {
		t.Errorf("comment body = %q, want summary only when result/metadata empty", sent.Body)
	}
}

func TestTComplete_DoneMode(t *testing.T) {
	t.Setenv("MCP_COMPLETE_MODE", "done")
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board:        testBoard,
		ID:           "t_x1",
		Summary:      "Done and dusted",
		Result:       "all green",
		Metadata:     `{"tests_run":42}`,
		CreatedCards: []string{"t_child01", "t_child02"},
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	if len(*rec) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(*rec))
	}
	var sent commentBody
	if err := json.Unmarshal([]byte((*rec)[1].body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Body != "Done and dusted" {
		t.Errorf("done-mode comment = %q, want summary only", sent.Body)
	}
	patch := (*rec)[2]
	if patch.method != http.MethodPatch || patch.path != "/tasks/t_x1" {
		t.Errorf("request 2 = %s %s, want PATCH /tasks/t_x1", patch.method, patch.path)
	}
	if keys := bodyKeys(t, patch.body); strings.Join(keys, ",") != "created_cards,metadata,result,status,summary" {
		t.Errorf("PATCH body keys = %v, want exactly [created_cards metadata result status summary] (body=%s)", keys, patch.body)
	}
	var pb map[string]any
	if err := json.Unmarshal([]byte(patch.body), &pb); err != nil {
		t.Fatal(err)
	}
	if pb["status"] != "done" || pb["summary"] != "Done and dusted" ||
		pb["result"] != "all green" || pb["metadata"] != `{"tests_run":42}` {
		t.Errorf("PATCH body = %v, want status=done summary/result/metadata present", pb)
	}
	cc, ok := pb["created_cards"].([]any)
	if !ok || len(cc) != 2 || cc[0] != "t_child01" || cc[1] != "t_child02" {
		t.Errorf("PATCH created_cards = %v, want [t_child01 t_child02]", pb["created_cards"])
	}

	out := decodeCompleteOut(t, res)
	if out["id"] != "t_x1" || out["final_status"] != "done" || out["review_required"] != false {
		t.Errorf("output = %v, want {id:t_x1 final_status:done review_required:false}", out)
	}
	if _, hasNote := out["note"]; hasNote {
		t.Errorf("output still carries the dropped note warning: %v", out)
	}
}

func TestTComplete_DoneOmitsEmptyFields(t *testing.T) {
	t.Setenv("MCP_COMPLETE_MODE", "done")
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{Board: testBoard, ID: "t_x1", Summary: "s"})
	if res.IsError {
		t.Fatalf("expected success, got: %s", res.Content[0].Text)
	}
	if keys := bodyKeys(t, (*rec)[2].body); strings.Join(keys, ",") != "status,summary" {
		t.Errorf("PATCH body keys = %v, want only [status summary] when result/metadata/created_cards empty (body=%s)", keys, (*rec)[2].body)
	}
}

func TestTComplete_ReviewIgnoresCreatedCards(t *testing.T) {
	os.Unsetenv("MCP_COMPLETE_MODE")
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board:        testBoard,
		ID:           "t_x1",
		Summary:      "s",
		CreatedCards: []string{"t_child01"},
	})
	// created_cards is done-mode only: on the review path it is rejected
	// loudly, never silently dropped (a dropped manifest would bypass the
	// kernel's audit gate via caller error).
	if !res.IsError {
		t.Fatalf("expected IsError when created_cards is passed on the review path, got: %+v", res)
	}
	if res.Content[0].Text != "invalid_input: created_cards requires done mode (review_tier LOW or MCP_COMPLETE_MODE=done)" {
		t.Errorf("error = %q", res.Content[0].Text)
	}
	if len(*rec) != 0 {
		t.Errorf("no request must be issued when created_cards is rejected on the review path, got %d", len(*rec))
	}
}

func TestTComplete_Patch400CreatedCardsRejected(t *testing.T) {
	t.Setenv("MCP_COMPLETE_MODE", "done")
	var rec []recReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec = append(rec, recReq{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(b)})
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"detail":"created_cards rejected: t_bogus1, t_bogus2 do not exist or were not created by this worker"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"task":{"id":"t_x1","title":"T","status":"running"}}`)
	}))
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board:        testBoard,
		ID:           "t_x1",
		Summary:      "s",
		CreatedCards: []string{"t_bogus1", "t_bogus2"},
	})
	if !res.IsError {
		t.Fatalf("expected IsError for 400 created_cards rejection")
	}
	want := "created_cards rejected: t_bogus1, t_bogus2 do not exist or were not created by this worker"
	if res.Content[0].Text != want {
		t.Errorf("error = %q, want %q", res.Content[0].Text, want)
	}
	if len(rec) != 3 {
		t.Fatalf("expected 3 requests (GET, comment POST, PATCH), got %d", len(rec))
	}
	if rec[1].method != http.MethodPost || rec[2].method != http.MethodPatch {
		t.Errorf("request order = %s,%s,%s, want GET,POST,PATCH", rec[0].method, rec[1].method, rec[2].method)
	}
	// The failing PATCH must have carried the created_cards manifest so
	// the backend could audit it in the first place.
	if keys := bodyKeys(t, rec[2].body); strings.Join(keys, ",") != "created_cards,status,summary" {
		t.Errorf("PATCH body keys = %v, want [created_cards status summary] (body=%s)", keys, rec[2].body)
	}
}

func TestTComplete_Patch400GenericFallthrough(t *testing.T) {
	t.Setenv("MCP_COMPLETE_MODE", "done")
	var rec []recReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec = append(rec, recReq{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(b)})
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"detail":"bad state for this transition"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"task":{"id":"t_x1","title":"T","status":"running"}}`)
	}))
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{Board: testBoard, ID: "t_x1", Summary: "s"})
	if !res.IsError {
		t.Fatalf("expected IsError for generic 400 on the PATCH")
	}
	// Non-created_cards 400s must keep the standard kind-prefixed
	// mapping — CompleteErrorMessage only special-cases the phantom-id
	// rejection.
	if res.Content[0].Text != "invalid_request: bad state for this transition" {
		t.Errorf("error = %q, want %q", res.Content[0].Text, "invalid_request: bad state for this transition")
	}
}

func TestTComplete_ClaimGuard(t *testing.T) {
	os.Unsetenv("MCP_ALLOW_SKIP_CLAIM")
	for _, status := range []string{"triage", "todo", "scheduled", "ready", "review", "blocked", "done", "archived", ""} {
		srv, rec := newRecServer(t, http.StatusOK, `{"task":{"id":"t_x1","title":"T","status":"`+status+`"}}`)
		s := newTestServer(srv)
		res := s.TicketComplete(context.Background(), TicketCompleteInput{Board: testBoard, ID: "t_x1", Summary: "s"})
		srv.Close()
		if !res.IsError {
			t.Errorf("status %q: expected IsError from claim guard", status)
			continue
		}
		want := "ticket is " + status + " and unclaimed; call ticket_claim first (or set MCP_ALLOW_SKIP_CLAIM=true)"
		if res.Content[0].Text != want {
			t.Errorf("status %q: error = %q, want %q", status, res.Content[0].Text, want)
		}
		if len(*rec) != 1 || (*rec)[0].method != http.MethodGet {
			t.Errorf("status %q: guard failure must issue only the GetTask GET, no mutations (got %d requests)", status, len(*rec))
		}
	}
}

func TestTComplete_SkipClaimReview(t *testing.T) {
	t.Setenv("MCP_ALLOW_SKIP_CLAIM", "true")
	readArgv := argvLog(t)
	// A ready ticket: the handoff must not force, because force releases a
	// live claim that a ready ticket does not hold.
	srv, rec := reviewLaneRecServer(t, "t_x1", "ready")
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{Board: testBoard, ID: "t_x1", Summary: "s"})
	if res.IsError {
		t.Fatalf("expected success with skip-claim, got IsError: %s", res.Content[0].Text)
	}
	for _, rr := range *rec {
		if rr.method == http.MethodPatch {
			t.Errorf("review path PATCHed the ticket: %s", rr.body)
		}
	}
	out := decodeCompleteOut(t, res)
	if out["final_status"] != "review" || out["review_required"] != true {
		t.Errorf("output = %v, want the review path despite a ready status", out)
	}
	argv := readArgv()
	if len(argv) != 1 {
		t.Fatalf("argv = %v, want exactly one handoff", argv)
	}
	if strings.Contains(argv[0], "--force") {
		t.Errorf("argv = %q, must NOT force a ready ticket", argv[0])
	}
}

func TestTComplete_SkipClaimDone(t *testing.T) {
	t.Setenv("MCP_ALLOW_SKIP_CLAIM", "true")
	t.Setenv("MCP_COMPLETE_MODE", "done")
	srv, rec := newRecServer(t, http.StatusOK, `{"task":{"id":"t_x1","title":"T","status":"todo"}}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{Board: testBoard, ID: "t_x1", Summary: "s"})
	if res.IsError {
		t.Fatalf("expected success with skip-claim, got IsError: %s", res.Content[0].Text)
	}
	if len(*rec) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(*rec))
	}
	out := decodeCompleteOut(t, res)
	if out["final_status"] != "done" || out["review_required"] != false {
		t.Errorf("output = %v, want done path despite todo status", out)
	}
}

func TestTComplete_CommentAuthorFromEnv(t *testing.T) {
	t.Setenv("MCP_COMMENT_AUTHOR", "robot")
	argvLog(t)
	srv, rec := reviewLaneRecServer(t, "t_x1", "running")
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{Board: testBoard, ID: "t_x1", Summary: "s"})
	if res.IsError {
		t.Fatalf("expected success, got: %s", res.Content[0].Text)
	}
	var sent commentBody
	if err := json.Unmarshal([]byte((*rec)[1].body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Author != "robot" {
		t.Errorf("comment author = %q, want MCP_COMMENT_AUTHOR value robot", sent.Author)
	}
}

func TestTComplete_SummaryRequired(t *testing.T) {
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	for _, summary := range []string{"", "   ", "\n\t "} {
		res := s.TicketComplete(context.Background(), TicketCompleteInput{Board: testBoard, ID: "t_x1", Summary: summary})
		if !res.IsError {
			t.Errorf("summary %q: expected IsError", summary)
			continue
		}
		if res.Content[0].Text != "invalid_input: summary required" {
			t.Errorf("summary %q: error = %q, want %q", summary, res.Content[0].Text, "invalid_input: summary required")
		}
	}
	if len(*rec) != 0 {
		t.Errorf("no request must be issued for an empty summary, got %d", len(*rec))
	}
}

func TestTComplete_InvalidID(t *testing.T) {
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	for _, id := range []string{"", "t id", "t_ab@c", "t_ab/c", strings.Repeat("a", 65)} {
		res := s.TicketComplete(context.Background(), TicketCompleteInput{Board: testBoard, ID: id, Summary: "s"})
		if !res.IsError {
			t.Errorf("id %q: expected IsError", id)
			continue
		}
		want := "invalid_input: invalid ticket id \"" + id + "\""
		if res.Content[0].Text != want {
			t.Errorf("id %q: error = %q, want %q", id, res.Content[0].Text, want)
		}
	}
	if len(*rec) != 0 {
		t.Errorf("no request must be issued for an invalid id, got %d", len(*rec))
	}
}

func TestTComplete_InvalidBoard(t *testing.T) {
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	for _, board := range []string{"HERMES", "a b", "a/b", "agent@x", strings.Repeat("a", 65)} {
		res := s.TicketComplete(context.Background(), TicketCompleteInput{Board: board, ID: "t_x1", Summary: "s"})
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

func TestTComplete_OmittedBoardRejected(t *testing.T) {
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := NewServerWithClient(srv.Client(), srv.URL, "")

	res := s.TicketComplete(context.Background(), TicketCompleteInput{ID: "t_x1", Summary: "s"})
	if !res.IsError {
		t.Fatalf("expected IsError when board is omitted")
	}
	if res.Content[0].Text != "invalid_input: board required; pass board" {
		t.Errorf("error = %q", res.Content[0].Text)
	}
	if len(*rec) != 0 {
		t.Errorf("no request must be issued without a board, got %d", len(*rec))
	}
}

func TestTComplete_GetTask404(t *testing.T) {
	srv, rec := newRecServer(t, http.StatusNotFound, `{"detail":"not found"}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{Board: testBoard, ID: "t_missing", Summary: "s"})
	if !res.IsError {
		t.Fatalf("expected IsError for 404 GetTask")
	}
	if res.Content[0].Text != "not_found: not found" {
		t.Errorf("error = %q, want %q", res.Content[0].Text, "not_found: not found")
	}
	if len(*rec) != 1 || (*rec)[0].method != http.MethodGet {
		t.Errorf("only the GetTask GET may occur, got %d requests", len(*rec))
	}
}

func TestTComplete_CommentBackend422(t *testing.T) {
	var rec []recReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec = append(rec, recReq{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(b)})
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusUnprocessableEntity)
			io.WriteString(w, `{"detail":[{"msg":"field required"},{"msg":"should not be empty"}]}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"task":{"id":"t_x1","title":"T","status":"running"}}`)
	}))
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{Board: testBoard, ID: "t_x1", Summary: "s"})
	if !res.IsError {
		t.Fatalf("expected IsError for 422 on the comment POST")
	}
	if res.Content[0].Text != "schema error: field required; should not be empty" {
		t.Errorf("error = %q, want %q", res.Content[0].Text, "schema error: field required; should not be empty")
	}
	// GET + failed comment POST; the PATCH must never follow a failed comment.
	if len(rec) != 2 {
		t.Fatalf("expected 2 requests (GET, comment POST), got %d", len(rec))
	}
	if rec[0].method != http.MethodGet || rec[1].method != http.MethodPost {
		t.Errorf("request order = %s,%s, want GET,POST", rec[0].method, rec[1].method)
	}
}

func TestTComplete_CommentBackend500(t *testing.T) {
	// The comment is the review path's first mutation. A backend failure
	// there must surface as an error and must NOT be reported as a
	// completed handoff.
	readArgv := argvLog(t)
	var rec []recReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec = append(rec, recReq{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(b)})
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"detail":"boom"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"task":{"id":"t_x1","title":"T","status":"running","assignee":"default"}}`)
	}))
	defer srv.Close()
	t.Setenv("MCP_REVIEWER_PROFILE", "default")
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{Board: testBoard, ID: "t_x1", Summary: "s"})
	if !res.IsError {
		t.Fatalf("expected IsError for a 500 on the comment POST")
	}
	if res.Content[0].Text != "unavailable: boom" {
		t.Errorf("error = %q, want %q", res.Content[0].Text, "unavailable: boom")
	}
	if len(rec) != 2 {
		t.Fatalf("expected 2 requests (guard, failed comment), got %d: %v", len(rec), rec)
	}
	if rec[1].method != http.MethodPost {
		t.Errorf("request 1 = %s, want the comment POST", rec[1].method)
	}
	assertNoCLI(t, readArgv())
}

func TestTComplete_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close() // refuse connections
	client := &http.Client{Timeout: 2 * time.Second}
	s := NewServerWithClient(client, deadURL, "hermes-agent")

	res := s.TicketComplete(context.Background(), TicketCompleteInput{Board: testBoard, ID: "t_x1", Summary: "s"})
	if !res.IsError {
		t.Fatalf("expected IsError on transport failure")
	}
	if !strings.HasPrefix(res.Content[0].Text, "unavailable: ") {
		t.Errorf("error = %q, want unavailable: prefix", res.Content[0].Text)
	}
}

func TestTComplete_SummaryReachesReviewerUncapped(t *testing.T) {
	// The old 100-rune cap existed only to keep the review-required
	// block_reason a bounded single line. That field is gone, so the
	// handoff summary is bounded only by the CLI's own 16 KiB limit and
	// must reach the reviewer whole.
	readArgv := argvLog(t)
	srv, _ := reviewLaneRecServer(t, "t_x1", "running")
	defer srv.Close()
	s := newTestServer(srv)
	summary := strings.Repeat("—", 150)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{Board: testBoard, ID: "t_x1", Summary: summary})
	if res.IsError {
		t.Fatalf("expected success, got: %s", res.Content[0].Text)
	}
	argv := readArgv()
	if len(argv) != 1 || !strings.Contains(argv[0], "--summary="+summary) {
		t.Errorf("argv = %v, want the summary passed through uncapped", argv)
	}
}

func TestTComplete_ErrorsOneLine(t *testing.T) {
	os.Unsetenv("MCP_ALLOW_SKIP_CLAIM")
	srv, _ := newRecServer(t, http.StatusNotFound, `{"detail":"multi\nline\nerror"}`)
	defer srv.Close()
	s := newTestServer(srv)

	results := []*ToolResult{
		s.TicketComplete(context.Background(), TicketCompleteInput{Board: "BAD BOARD", ID: "t_x1", Summary: "s"}),
		s.TicketComplete(context.Background(), TicketCompleteInput{Board: testBoard, ID: "bad id!", Summary: "s"}),
		s.TicketComplete(context.Background(), TicketCompleteInput{Board: testBoard, ID: "t_x1", Summary: "  "}),
		s.TicketComplete(context.Background(), TicketCompleteInput{Board: testBoard, ID: "t_missing", Summary: "s"}),
	}
	for i, res := range results {
		if !res.IsError {
			t.Errorf("case %d: expected IsError", i)
			continue
		}
		if strings.ContainsAny(res.Content[0].Text, "\r\n") {
			t.Errorf("case %d: error is not one line: %q", i, res.Content[0].Text)
		}
		if rendered := renderedSize(t, res); rendered > MaxToolResultBytes {
			t.Errorf("case %d: rendered %d bytes > %d", i, rendered, MaxToolResultBytes)
		}
	}
}

func TestTComplete_SizeInvariant(t *testing.T) {
	argvLog(t)
	srv, _ := reviewLaneRecServer(t, "t_x1", "running")
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board:   testBoard,
		ID:      "t_x1",
		Summary: strings.Repeat("s", 1500),
		Result:  strings.Repeat("r", 500),
	})
	if res.IsError {
		t.Fatalf("expected success, got: %s", res.Content[0].Text)
	}
	if rendered := renderedSize(t, res); rendered > MaxToolResultBytes {
		t.Errorf("rendered result %d bytes > %d", rendered, MaxToolResultBytes)
	}
}

func TestTComplete_ReviewTierLowCompletesDone(t *testing.T) {
	os.Unsetenv("MCP_COMPLETE_MODE")
	os.Unsetenv("MCP_ALLOW_SKIP_CLAIM")
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board:      testBoard,
		ID:         "t_x1",
		Summary:    "Low tier done",
		Result:     "all good",
		Metadata:   `{"changed_files":["a.go"]}`,
		ReviewTier: "LOW",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	if len(*rec) != 3 {
		t.Fatalf("expected 3 backend requests (GET, comment, PATCH), got %d", len(*rec))
	}
	comment := (*rec)[1]
	var sent commentBody
	if err := json.Unmarshal([]byte(comment.body), &sent); err != nil {
		t.Fatalf("comment body not a commentBody: %v (%s)", err, comment.body)
	}
	if sent.Body != "Low tier done" {
		t.Errorf("comment body = %q, want summary only", sent.Body)
	}
	patch := (*rec)[2]
	if keys := bodyKeys(t, patch.body); strings.Join(keys, ",") != "metadata,result,status,summary" {
		t.Errorf("PATCH body keys = %v, want exactly [metadata result status summary] (body=%s)", keys, patch.body)
	}
	var pb map[string]any
	if err := json.Unmarshal([]byte(patch.body), &pb); err != nil {
		t.Fatal(err)
	}
	if pb["status"] != "done" {
		t.Errorf("PATCH status = %v, want done", pb["status"])
	}
	if _, ok := pb["block_reason"]; ok {
		t.Errorf("block_reason must not be present in LOW tier PATCH (body=%s)", patch.body)
	}

	out := decodeCompleteOut(t, res)
	if out["id"] != "t_x1" || out["final_status"] != "done" || out["review_required"] != false || out["review_tier"] != "LOW" {
		t.Errorf("output = %v, want {id:t_x1 final_status:done review_required:false review_tier:LOW}", out)
	}
	if _, hasNote := out["note"]; hasNote {
		t.Errorf("output still carries the dropped note warning: %v", out)
	}
}

func TestTComplete_ReviewTierLowOverridesReviewEnv(t *testing.T) {
	t.Setenv("MCP_COMPLETE_MODE", "review")
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board:      testBoard,
		ID:         "t_x1",
		Summary:    "Override test",
		ReviewTier: "LOW",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	var pb map[string]any
	if err := json.Unmarshal([]byte((*rec)[2].body), &pb); err != nil {
		t.Fatal(err)
	}
	if pb["status"] != "done" {
		t.Errorf("PATCH status = %v, want done (LOW overrides MCP_COMPLETE_MODE=review)", pb["status"])
	}
	out := decodeCompleteOut(t, res)
	if out["final_status"] != "done" || out["review_required"] != false || out["review_tier"] != "LOW" {
		t.Errorf("output = %v, want done path with review_tier LOW", out)
	}
}

func TestTComplete_ReviewTierMediumDefaultReview(t *testing.T) {
	argvLog(t)
	srv, rec := reviewLaneRecServer(t, "t_x1", "running")
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board:   testBoard,
		ID:      "t_x1",
		Summary: "Medium tier test",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	for _, rr := range *rec {
		if rr.method == http.MethodPatch {
			t.Errorf("MEDIUM review path PATCHed the ticket: %s", rr.body)
		}
	}
	out := decodeCompleteOut(t, res)
	if out["final_status"] != "review" || out["review_required"] != true || out["review_tier"] != "MEDIUM" {
		t.Errorf("output = %v, want {final_status:review review_required:true review_tier:MEDIUM}", out)
	}
}

func TestTComplete_ReviewTierHigh(t *testing.T) {
	argvLog(t)
	srv, rec := reviewLaneRecServer(t, "t_x1", "running")
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board:      testBoard,
		ID:         "t_x1",
		Summary:    "High risk change",
		ReviewTier: "HIGH",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	for _, rr := range *rec {
		if rr.method == http.MethodPatch {
			t.Errorf("HIGH review path PATCHed the ticket: %s", rr.body)
		}
	}
	out := decodeCompleteOut(t, res)
	if out["final_status"] != "review" || out["review_required"] != true || out["review_tier"] != "HIGH" {
		t.Errorf("output = %v, want {final_status:review review_required:true review_tier:HIGH}", out)
	}
}

func TestTComplete_InvalidReviewTier(t *testing.T) {
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	for _, tier := range []string{"EXTREME", " EXTREME "} {
		res := s.TicketComplete(context.Background(), TicketCompleteInput{
			Board:      testBoard,
			ID:         "t_x1",
			Summary:    "s",
			ReviewTier: tier,
		})
		if !res.IsError {
			t.Errorf("tier %q: expected IsError", tier)
			continue
		}
		want := `invalid_input: review_tier must be one of LOW|MEDIUM|HIGH, got "EXTREME"`
		if res.Content[0].Text != want {
			t.Errorf("tier %q: error = %q, want %q", tier, res.Content[0].Text, want)
		}
	}
	if len(*rec) != 0 {
		t.Errorf("no backend request must be issued for invalid review_tier, got %d", len(*rec))
	}
}

func TestTComplete_ReviewTierWhitespaceNormalized(t *testing.T) {
	os.Unsetenv("MCP_COMPLETE_MODE")
	os.Unsetenv("MCP_ALLOW_SKIP_CLAIM")
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board:      testBoard,
		ID:         "t_x1",
		Summary:    "ws test",
		ReviewTier: "  LOW ",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	var pb map[string]any
	if err := json.Unmarshal([]byte((*rec)[2].body), &pb); err != nil {
		t.Fatal(err)
	}
	if pb["status"] != "done" {
		t.Errorf("PATCH status = %v, want done (whitespace-wrapped LOW)", pb["status"])
	}
	out := decodeCompleteOut(t, res)
	if out["review_tier"] != "LOW" {
		t.Errorf("review_tier = %v, want canonical LOW", out["review_tier"])
	}
}

func TestTComplete_ReviewTierHighWithDoneEnv(t *testing.T) {
	t.Setenv("MCP_COMPLETE_MODE", "done")
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board:      testBoard,
		ID:         "t_x1",
		Summary:    "high + env done",
		ReviewTier: "HIGH",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	var pb map[string]any
	if err := json.Unmarshal([]byte((*rec)[2].body), &pb); err != nil {
		t.Fatal(err)
	}
	if pb["status"] != "done" {
		t.Errorf("PATCH status = %v, want done (HIGH follows MCP_COMPLETE_MODE=done)", pb["status"])
	}
}

func TestTComplete_ReviewTierCaseInsensitive(t *testing.T) {
	os.Unsetenv("MCP_COMPLETE_MODE")
	os.Unsetenv("MCP_ALLOW_SKIP_CLAIM")
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board:      testBoard,
		ID:         "t_x1",
		Summary:    "case test",
		ReviewTier: "low",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	var pb map[string]any
	if err := json.Unmarshal([]byte((*rec)[2].body), &pb); err != nil {
		t.Fatal(err)
	}
	if pb["status"] != "done" {
		t.Errorf("PATCH status = %v, want done (lowercase low)", pb["status"])
	}
	out := decodeCompleteOut(t, res)
	if out["review_tier"] != "LOW" {
		t.Errorf("review_tier = %v, want canonical LOW", out["review_tier"])
	}
}

func TestTComplete_ReviewRefsVerbatim(t *testing.T) {
	readArgv := argvLog(t)
	srv, rec := reviewLaneRecServer(t, "t_x1", "running")
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board:   testBoard,
		ID:      "t_x1",
		Summary: "s",
		Repo:    "github.com/RKelln/hermes-kanban-mcp",
		Branch:  "feat/x",
		SHA:     "abc123def",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	var sent commentBody
	if err := json.Unmarshal([]byte((*rec)[1].body), &sent); err != nil {
		t.Fatal(err)
	}
	wantComment := "s\nrepo: github.com/RKelln/hermes-kanban-mcp\nbranch: feat/x\nsha: abc123def"
	if sent.Body != wantComment {
		t.Errorf("comment body = %q, want %q", sent.Body, wantComment)
	}
	// The refs also travel in the reviewer's brief, so the reviewer does
	// not have to go looking for the branch.
	wantSummary := "s | repo: github.com/RKelln/hermes-kanban-mcp; branch: feat/x; sha: abc123def"
	argv := readArgv()
	if len(argv) != 1 || !strings.Contains(argv[0], "--summary="+wantSummary) {
		t.Errorf("argv = %v, want --summary=%s", argv, wantSummary)
	}
	out := decodeCompleteOut(t, res)
	if out["repo"] != "github.com/RKelln/hermes-kanban-mcp" {
		t.Errorf("out repo = %v, want github.com/RKelln/hermes-kanban-mcp", out["repo"])
	}
	if out["branch"] != "feat/x" {
		t.Errorf("out branch = %v, want feat/x", out["branch"])
	}
	if out["sha"] != "abc123def" {
		t.Errorf("out sha = %v, want abc123def", out["sha"])
	}
}

func TestTComplete_ReviewRefsEmptyNoSuffix(t *testing.T) {
	readArgv := argvLog(t)
	srv, rec := reviewLaneRecServer(t, "t_x1", "running")
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board:   testBoard,
		ID:      "t_x1",
		Summary: "s",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	var sent commentBody
	if err := json.Unmarshal([]byte((*rec)[1].body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Body != "s" {
		t.Errorf("comment body = %q, want exact 's'", sent.Body)
	}
	argv := readArgv()
	if len(argv) != 1 || !strings.Contains(argv[0], "--summary=s ") {
		t.Errorf("argv = %v, want a bare summary with no ref suffix", argv)
	}
	if len(argv) == 1 && strings.Contains(argv[0], "--summary=s |") {
		t.Errorf("argv = %q, want no ref separator when all three refs are empty", argv[0])
	}
}

func TestTComplete_ReviewRefsPartial(t *testing.T) {
	readArgv := argvLog(t)
	srv, rec := reviewLaneRecServer(t, "t_x1", "running")
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board:   testBoard,
		ID:      "t_x1",
		Summary: "s",
		SHA:     "abc123",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	var sent commentBody
	if err := json.Unmarshal([]byte((*rec)[1].body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Body != "s\nsha: abc123" {
		t.Errorf("comment body = %q, want 's\\nsha: abc123'", sent.Body)
	}
	argv := readArgv()
	if len(argv) != 1 || !strings.Contains(argv[0], "--summary=s | sha: abc123") {
		t.Errorf("argv = %v, want the present ref only, in canonical order", argv)
	}
}

func TestTComplete_DoneModeRefsIgnored(t *testing.T) {
	t.Setenv("MCP_COMPLETE_MODE", "done")
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board:   testBoard,
		ID:      "t_x1",
		Summary: "s",
		Repo:    "github.com/RKelln/hermes-kanban-mcp",
		Branch:  "feat/x",
		SHA:     "abc123",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}

	patch := (*rec)[2]
	var pb map[string]any
	if err := json.Unmarshal([]byte(patch.body), &pb); err != nil {
		t.Fatal(err)
	}
	if pb["status"] != "done" {
		t.Errorf("PATCH status = %v, want done", pb["status"])
	}
	if _, ok := pb["block_reason"]; ok {
		t.Errorf("block_reason must not be present in done mode PATCH")
	}

	var sent commentBody
	if err := json.Unmarshal([]byte((*rec)[1].body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Body != "s" {
		t.Errorf("done-mode comment = %q, want summary only", sent.Body)
	}

	out := decodeCompleteOut(t, res)
	if out["repo"] != "github.com/RKelln/hermes-kanban-mcp" {
		t.Errorf("out repo = %v", out["repo"])
	}
	if out["branch"] != "feat/x" {
		t.Errorf("out branch = %v", out["branch"])
	}
	if out["sha"] != "abc123" {
		t.Errorf("out sha = %v", out["sha"])
	}
}

func TestTComplete_ReviewTierLowRefsIgnored(t *testing.T) {
	os.Unsetenv("MCP_COMPLETE_MODE")
	os.Unsetenv("MCP_ALLOW_SKIP_CLAIM")
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board:      testBoard,
		ID:         "t_x1",
		Summary:    "s",
		ReviewTier: "LOW",
		Repo:       "github.com/RKelln/hermes-kanban-mcp",
		Branch:     "feat/x",
		SHA:        "abc123",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	patch := (*rec)[2]
	var pb map[string]any
	if err := json.Unmarshal([]byte(patch.body), &pb); err != nil {
		t.Fatal(err)
	}
	if pb["status"] != "done" {
		t.Errorf("PATCH status = %v, want done (LOW)", pb["status"])
	}
	if _, ok := pb["block_reason"]; ok {
		t.Errorf("block_reason must not be present for review_tier LOW")
	}
	var sent commentBody
	if err := json.Unmarshal([]byte((*rec)[1].body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Body != "s" {
		t.Errorf("LOW-mode comment = %q, want summary only (refs ignored on the wire)", sent.Body)
	}
	out := decodeCompleteOut(t, res)
	if out["final_status"] != "done" || out["sha"] != "abc123" {
		t.Errorf("output = %v, want done path echoing refs", out)
	}
}

func TestTComplete_RefsNewlinesStayInTheComment(t *testing.T) {
	// The single-line rule died with the block_reason field: refs now
	// travel in the comment (multiline by design) and the reviewer's
	// brief. completeRefParts still collapses each ref's own newlines so
	// one ref cannot inject a fake extra line into the comment.
	argvLog(t)
	srv, rec := reviewLaneRecServer(t, "t_x1", "running")
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board:   testBoard,
		ID:      "t_x1",
		Summary: "s",
		Repo:    "acme/\nprod",
		Branch:  "feat/x",
		SHA:     "abc123",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	var sent commentBody
	if err := json.Unmarshal([]byte((*rec)[1].body), &sent); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent.Body, "repo: acme/ prod") {
		t.Errorf("comment = %q, want the ref newline collapsed to a space", sent.Body)
	}
}

func TestTComplete_SummaryNewlinesStayInTheComment(t *testing.T) {
	// The comment is the multiline record of the handoff: a summary with
	// newlines must survive intact there, because that is what the
	// reviewer reads the claim against.
	argvLog(t)
	srv, rec := reviewLaneRecServer(t, "t_x1", "running")
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board:   testBoard,
		ID:      "t_x1",
		Summary: "shipped\ninjected\rpayload",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	var sent commentBody
	if err := json.Unmarshal([]byte((*rec)[1].body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Body != "shipped\ninjected\rpayload" {
		t.Errorf("comment body = %q, want the summary verbatim", sent.Body)
	}
}

func TestTComplete_RefsLengthCaps(t *testing.T) {
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	cases := []struct {
		name string
		refs TicketCompleteInput
	}{
		{"repo", TicketCompleteInput{Board: testBoard, ID: "t_x1", Summary: "s", Repo: strings.Repeat("a", 257)}},
		{"branch", TicketCompleteInput{Board: testBoard, ID: "t_x1", Summary: "s", Branch: strings.Repeat("b", 257)}},
		{"sha", TicketCompleteInput{Board: testBoard, ID: "t_x1", Summary: "s", SHA: strings.Repeat("c", 65)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := s.TicketComplete(context.Background(), tc.refs)
			if !res.IsError {
				t.Fatalf("expected IsError for oversized %s", tc.name)
			}
			if !strings.HasPrefix(res.Content[0].Text, "invalid_input: "+tc.name+" exceeds ") {
				t.Errorf("error = %q, want invalid_input: %s exceeds", res.Content[0].Text, tc.name)
			}
		})
	}
	if len(*rec) != 0 {
		t.Errorf("no backend request must be issued for oversized refs, got %d", len(*rec))
	}
}

// TestTComplete_DoneModePatchBackend500 restores coverage the review-path rename
// dropped (reviewer finding F2 on t_affa85ca): the 500-on-PATCH case now belongs
// to done mode, which is the only mode that still PATCHes, and nothing covered it.
func TestTComplete_DoneModePatchBackend500(t *testing.T) {
	t.Setenv("MCP_COMPLETE_MODE", "done")
	argvLog(t)
	var rec []recReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec = append(rec, recReq{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(b)})
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"detail":"boom"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"task":{"id":"t_x1","title":"T","status":"running"}}`)
	}))
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{Board: testBoard, ID: "t_x1", Summary: "s"})
	if !res.IsError {
		t.Fatalf("expected IsError for a 500 on the done-mode PATCH")
	}
	if res.Content[0].Text != "unavailable: boom" {
		t.Errorf("error = %q, want %q", res.Content[0].Text, "unavailable: boom")
	}
	if len(rec) != 3 {
		t.Fatalf("expected 3 requests (GET, comment POST, PATCH), got %d: %v", len(rec), rec)
	}
	if rec[1].method != http.MethodPost || rec[2].method != http.MethodPatch {
		t.Errorf("request order = %s,%s,%s, want GET,POST,PATCH", rec[0].method, rec[1].method, rec[2].method)
	}
}
