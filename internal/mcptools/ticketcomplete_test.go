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

const wantCompleteNote = "REST completion does not record created_cards; create follow-up tickets with ticket_create"

func TestTComplete_ReviewDefaultMode(t *testing.T) {
	os.Unsetenv("MCP_COMPLETE_MODE")
	os.Unsetenv("MCP_ALLOW_SKIP_CLAIM")
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv) // default board hermes-agent

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		ID:       "t_x1",
		Summary:  "Shipped the widget",
		Result:   "3 tests passed",
		Metadata: `{"changed_files":["a.go"]}`,
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	if len(*rec) != 3 {
		t.Fatalf("expected 3 backend requests (GET, comment, PATCH), got %d", len(*rec))
	}
	// Order: GetTask first, then comment POST, then PATCH.
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
	patch := (*rec)[2]
	if patch.method != http.MethodPatch || patch.path != "/tasks/t_x1" || patch.query != "board=hermes-agent" {
		t.Errorf("request 2 = %s %s?%s, want PATCH /tasks/t_x1?board=hermes-agent", patch.method, patch.path, patch.query)
	}
	if keys := bodyKeys(t, patch.body); strings.Join(keys, ",") != "block_reason,status" {
		t.Errorf("PATCH body keys = %v, want exactly [block_reason status] (body=%s)", keys, patch.body)
	}
	var pb map[string]any
	if err := json.Unmarshal([]byte(patch.body), &pb); err != nil {
		t.Fatal(err)
	}
	if pb["status"] != "blocked" || pb["block_reason"] != "review-required: Shipped the widget" {
		t.Errorf("PATCH body = %v, want {status:blocked block_reason:review-required: Shipped the widget}", pb)
	}

	out := decodeCompleteOut(t, res)
	if out["id"] != "t_x1" || out["final_status"] != "blocked" || out["review_required"] != true || out["note"] != wantCompleteNote {
		t.Errorf("output = %v, want {id:t_x1 final_status:blocked review_required:true note:%q}", out, wantCompleteNote)
	}
	if rendered := renderedSize(t, res); rendered > MaxToolResultBytes {
		t.Errorf("rendered result %d bytes > %d", rendered, MaxToolResultBytes)
	}
}

func TestTComplete_ReviewExplicitEnv(t *testing.T) {
	t.Setenv("MCP_COMPLETE_MODE", "review")
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{ID: "t_x1", Summary: "s"})
	if res.IsError {
		t.Fatalf("expected success, got: %s", res.Content[0].Text)
	}
	if len(*rec) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(*rec))
	}
	out := decodeCompleteOut(t, res)
	if out["final_status"] != "blocked" || out["review_required"] != true {
		t.Errorf("output = %v, want review path", out)
	}
}

func TestTComplete_ReviewOmitsEmptyResultMetadata(t *testing.T) {
	os.Unsetenv("MCP_COMPLETE_MODE")
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{ID: "t_x1", Summary: "just summary"})
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
		ID:       "t_x1",
		Summary:  "Done and dusted",
		Result:   "all green",
		Metadata: `{"tests_run":42}`,
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
	if keys := bodyKeys(t, patch.body); strings.Join(keys, ",") != "metadata,result,status,summary" {
		t.Errorf("PATCH body keys = %v, want exactly [metadata result status summary] (body=%s)", keys, patch.body)
	}
	var pb map[string]any
	if err := json.Unmarshal([]byte(patch.body), &pb); err != nil {
		t.Fatal(err)
	}
	if pb["status"] != "done" || pb["summary"] != "Done and dusted" ||
		pb["result"] != "all green" || pb["metadata"] != `{"tests_run":42}` {
		t.Errorf("PATCH body = %v, want status=done summary/result/metadata present", pb)
	}

	out := decodeCompleteOut(t, res)
	if out["id"] != "t_x1" || out["final_status"] != "done" || out["review_required"] != false || out["note"] != wantCompleteNote {
		t.Errorf("output = %v, want {id:t_x1 final_status:done review_required:false note:%q}", out, wantCompleteNote)
	}
}

func TestTComplete_DoneOmitsEmptyFields(t *testing.T) {
	t.Setenv("MCP_COMPLETE_MODE", "done")
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{ID: "t_x1", Summary: "s"})
	if res.IsError {
		t.Fatalf("expected success, got: %s", res.Content[0].Text)
	}
	if keys := bodyKeys(t, (*rec)[2].body); strings.Join(keys, ",") != "status,summary" {
		t.Errorf("PATCH body keys = %v, want only [status summary] when result/metadata empty (body=%s)", keys, (*rec)[2].body)
	}
}

func TestTComplete_ClaimGuard(t *testing.T) {
	os.Unsetenv("MCP_ALLOW_SKIP_CLAIM")
	for _, status := range []string{"triage", "todo", "scheduled", "ready", "review", "blocked", "done", "archived", ""} {
		srv, rec := newRecServer(t, http.StatusOK, `{"task":{"id":"t_x1","title":"T","status":"`+status+`"}}`)
		s := newTestServer(srv)
		res := s.TicketComplete(context.Background(), TicketCompleteInput{ID: "t_x1", Summary: "s"})
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
	os.Unsetenv("MCP_COMPLETE_MODE")
	srv, rec := newRecServer(t, http.StatusOK, `{"task":{"id":"t_x1","title":"T","status":"ready"}}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{ID: "t_x1", Summary: "s"})
	if res.IsError {
		t.Fatalf("expected success with skip-claim, got IsError: %s", res.Content[0].Text)
	}
	if len(*rec) != 3 {
		t.Fatalf("expected 3 requests (GET, comment, PATCH), got %d", len(*rec))
	}
	out := decodeCompleteOut(t, res)
	if out["final_status"] != "blocked" || out["review_required"] != true {
		t.Errorf("output = %v, want review path despite ready status", out)
	}
}

func TestTComplete_SkipClaimDone(t *testing.T) {
	t.Setenv("MCP_ALLOW_SKIP_CLAIM", "true")
	t.Setenv("MCP_COMPLETE_MODE", "done")
	srv, rec := newRecServer(t, http.StatusOK, `{"task":{"id":"t_x1","title":"T","status":"todo"}}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{ID: "t_x1", Summary: "s"})
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
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{ID: "t_x1", Summary: "s"})
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
		res := s.TicketComplete(context.Background(), TicketCompleteInput{ID: "t_x1", Summary: summary})
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
		res := s.TicketComplete(context.Background(), TicketCompleteInput{ID: id, Summary: "s"})
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

func TestTComplete_NoBoardConfigured(t *testing.T) {
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := NewServerWithClient(srv.Client(), srv.URL, "")

	res := s.TicketComplete(context.Background(), TicketCompleteInput{ID: "t_x1", Summary: "s"})
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

func TestTComplete_GetTask404(t *testing.T) {
	srv, rec := newRecServer(t, http.StatusNotFound, `{"detail":"not found"}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{ID: "t_missing", Summary: "s"})
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

	res := s.TicketComplete(context.Background(), TicketCompleteInput{ID: "t_x1", Summary: "s"})
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

func TestTComplete_PatchBackend500(t *testing.T) {
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

	res := s.TicketComplete(context.Background(), TicketCompleteInput{ID: "t_x1", Summary: "s"})
	if !res.IsError {
		t.Fatalf("expected IsError for 500 on the PATCH")
	}
	if res.Content[0].Text != "unavailable: boom" {
		t.Errorf("error = %q, want %q", res.Content[0].Text, "unavailable: boom")
	}
	if len(rec) != 3 {
		t.Fatalf("expected 3 requests (GET, comment POST, PATCH), got %d", len(rec))
	}
	if rec[1].method != http.MethodPost || rec[2].method != http.MethodPatch {
		t.Errorf("request order = %s,%s,%s, want GET,POST,PATCH", rec[0].method, rec[1].method, rec[2].method)
	}
}

func TestTComplete_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close() // refuse connections
	client := &http.Client{Timeout: 2 * time.Second}
	s := NewServerWithClient(client, deadURL, "hermes-agent")

	res := s.TicketComplete(context.Background(), TicketCompleteInput{ID: "t_x1", Summary: "s"})
	if !res.IsError {
		t.Fatalf("expected IsError on transport failure")
	}
	if !strings.HasPrefix(res.Content[0].Text, "unavailable: ") {
		t.Errorf("error = %q, want unavailable: prefix", res.Content[0].Text)
	}
}

func TestTComplete_SummaryTruncated100Runes(t *testing.T) {
	os.Unsetenv("MCP_COMPLETE_MODE")
	// 150 em-dashes (450 bytes): rune-safe truncation must keep exactly
	// 100 runes without splitting a character.
	summary := strings.Repeat("—", 150)
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{ID: "t_x1", Summary: summary})
	if res.IsError {
		t.Fatalf("expected success, got: %s", res.Content[0].Text)
	}
	var pb map[string]any
	if err := json.Unmarshal([]byte((*rec)[2].body), &pb); err != nil {
		t.Fatal(err)
	}
	want := "review-required: " + strings.Repeat("—", 100)
	if pb["block_reason"] != want {
		t.Errorf("block_reason length/prefix wrong: got %q", pb["block_reason"])
	}
	if len([]rune(pb["block_reason"].(string))) != len("review-required: ")+100 {
		t.Errorf("block_reason runes = %d, want %d", len([]rune(pb["block_reason"].(string))), len("review-required: ")+100)
	}
}

func TestTComplete_ErrorsOneLine(t *testing.T) {
	os.Unsetenv("MCP_ALLOW_SKIP_CLAIM")
	srv, _ := newRecServer(t, http.StatusNotFound, `{"detail":"multi\nline\nerror"}`)
	defer srv.Close()
	s := newTestServer(srv)

	results := []*ToolResult{
		s.TicketComplete(context.Background(), TicketCompleteInput{Board: "BAD BOARD", ID: "t_x1", Summary: "s"}),
		s.TicketComplete(context.Background(), TicketCompleteInput{ID: "bad id!", Summary: "s"}),
		s.TicketComplete(context.Background(), TicketCompleteInput{ID: "t_x1", Summary: "  "}),
		s.TicketComplete(context.Background(), TicketCompleteInput{ID: "t_missing", Summary: "s"}),
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
	// A large summary (review mode truncates only the block_reason; the
	// comment carries the full text) must still render <= 2 KB.
	srv, _ := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
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
	if out["id"] != "t_x1" || out["final_status"] != "done" || out["review_required"] != false || out["review_tier"] != "LOW" || out["note"] != wantCompleteNote {
		t.Errorf("output = %v, want {id:t_x1 final_status:done review_required:false review_tier:LOW note:%q}", out, wantCompleteNote)
	}
}

func TestTComplete_ReviewTierLowOverridesReviewEnv(t *testing.T) {
	t.Setenv("MCP_COMPLETE_MODE", "review")
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
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
	os.Unsetenv("MCP_COMPLETE_MODE")
	os.Unsetenv("MCP_ALLOW_SKIP_CLAIM")
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		ID:      "t_x1",
		Summary: "Medium tier test",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	patch := (*rec)[2]
	var pb map[string]any
	if err := json.Unmarshal([]byte(patch.body), &pb); err != nil {
		t.Fatal(err)
	}
	if pb["status"] != "blocked" {
		t.Errorf("PATCH status = %v, want blocked", pb["status"])
	}
	reason, ok := pb["block_reason"].(string)
	if !ok || !strings.HasPrefix(reason, "review-required: ") {
		t.Errorf("block_reason = %v, want prefix review-required:", pb["block_reason"])
	}
	out := decodeCompleteOut(t, res)
	if out["final_status"] != "blocked" || out["review_required"] != true || out["review_tier"] != "MEDIUM" {
		t.Errorf("output = %v, want {final_status:blocked review_required:true review_tier:MEDIUM}", out)
	}
}

func TestTComplete_ReviewTierHigh(t *testing.T) {
	os.Unsetenv("MCP_COMPLETE_MODE")
	os.Unsetenv("MCP_ALLOW_SKIP_CLAIM")
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		ID:         "t_x1",
		Summary:    "High risk change",
		ReviewTier: "HIGH",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	var pb map[string]any
	if err := json.Unmarshal([]byte((*rec)[2].body), &pb); err != nil {
		t.Fatal(err)
	}
	if pb["status"] != "blocked" {
		t.Errorf("PATCH status = %v, want blocked", pb["status"])
	}
	out := decodeCompleteOut(t, res)
	if out["final_status"] != "blocked" || out["review_required"] != true || out["review_tier"] != "HIGH" {
		t.Errorf("output = %v, want {final_status:blocked review_required:true review_tier:HIGH}", out)
	}
}

func TestTComplete_InvalidReviewTier(t *testing.T) {
	srv, rec := runnableRecServer(t)
	defer srv.Close()
	s := newTestServer(srv)

	for _, tier := range []string{"EXTREME", " EXTREME "} {
		res := s.TicketComplete(context.Background(), TicketCompleteInput{
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
