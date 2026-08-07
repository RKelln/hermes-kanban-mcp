package mcptools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// NOTE on file naming: the canonical write-tools test suite lives in
// write_tools_test.go (ticket t_b1bf0fc8). This file is the comment
// tool's focused suite, kept under a distinct name and TestTCM_* prefix
// so the two merge without symbol collisions (same convention as the
// Task-0 ticketcreate_test.go / TestTC_* suite).

func TestTCM_CommentSuccess(t *testing.T) {
	srv, rec := newRecServer(t, http.StatusOK, `{"author":"alice","body":"hi","created_at":123}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComment(context.Background(), TicketCommentInput{
		Board:  testBoard,
		ID:     "t_abc123",
		Body:   "Ship it",
		Author: "alice",
	})
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
	if got.path != "/tasks/t_abc123/comments" {
		t.Errorf("path = %s, want /tasks/t_abc123/comments", got.path)
	}
	if got.query != "board=hermes-agent" {
		t.Errorf("query = %s, want board=hermes-agent", got.query)
	}
	if keys := bodyKeys(t, got.body); strings.Join(keys, ",") != "author,body" {
		t.Errorf("body keys = %v, want exactly [author body] (body=%s)", keys, got.body)
	}
	var sent commentBody
	if err := json.Unmarshal([]byte(got.body), &sent); err != nil {
		t.Fatalf("body is not a commentBody: %v (body=%s)", err, got.body)
	}
	if sent.Body != "Ship it" || sent.Author != "alice" {
		t.Errorf("sent = %+v, want {Body:Ship it Author:alice}", sent)
	}

	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, res.Content[0].Text)
	}
	if out["id"] != "t_abc123" || out["ok"] != true {
		t.Errorf("output = %v, want {id:t_abc123 ok:true}", out)
	}
	if rendered := renderedSize(t, res); rendered > MaxToolResultBytes {
		t.Errorf("rendered result %d bytes > %d", rendered, MaxToolResultBytes)
	}
}

func TestTCM_AuthorDefaultsToEnv(t *testing.T) {
	t.Setenv("MCP_COMMENT_AUTHOR", "robot")
	srv, rec := newRecServer(t, http.StatusOK, `{}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComment(context.Background(), TicketCommentInput{
		Board: testBoard,
		ID:    "t_abc123",
		Body:  "hi",
	})
	if res.IsError {
		t.Fatalf("expected success, got: %s", res.Content[0].Text)
	}
	var sent commentBody
	if err := json.Unmarshal([]byte((*rec)[0].body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Author != "robot" {
		t.Errorf("author = %q, want env value robot", sent.Author)
	}
}

func TestTCM_AuthorEnvUnsetSendsEmpty(t *testing.T) {
	os.Unsetenv("MCP_COMMENT_AUTHOR")
	srv, rec := newRecServer(t, http.StatusOK, `{}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComment(context.Background(), TicketCommentInput{
		Board: testBoard,
		ID:    "t_abc123",
		Body:  "hi",
	})
	if res.IsError {
		t.Fatalf("expected success, got: %s", res.Content[0].Text)
	}
	var sent commentBody
	if err := json.Unmarshal([]byte((*rec)[0].body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Author != "" {
		t.Errorf("author = %q, want empty when env unset", sent.Author)
	}
}

func TestTCM_ExplicitAuthorBeatsEnv(t *testing.T) {
	t.Setenv("MCP_COMMENT_AUTHOR", "robot")
	srv, rec := newRecServer(t, http.StatusOK, `{}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComment(context.Background(), TicketCommentInput{
		Board:  testBoard,
		ID:     "t_abc123",
		Body:   "hi",
		Author: "alice",
	})
	if res.IsError {
		t.Fatalf("expected success, got: %s", res.Content[0].Text)
	}
	var sent commentBody
	if err := json.Unmarshal([]byte((*rec)[0].body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Author != "alice" {
		t.Errorf("author = %q, want explicit alice", sent.Author)
	}
}

func TestTCM_OmittedBoardRejected(t *testing.T) {
	srv, rec := newRecServer(t, http.StatusOK, `{}`)
	defer srv.Close()
	s := NewServerWithClient(srv.Client(), srv.URL, "hermes-agent")

	res := s.TicketComment(context.Background(), TicketCommentInput{ID: "t_abc123", Body: "hi"})
	if !res.IsError {
		t.Fatalf("expected IsError when board is omitted, got success")
	}
	if res.Content[0].Text != "invalid_input: board required; pass board" {
		t.Errorf("error = %q", res.Content[0].Text)
	}
	if len(*rec) != 0 {
		t.Errorf("no request must be issued without a board, got %d", len(*rec))
	}
}

func TestTCM_EmptyBodyRejected(t *testing.T) {
	srv, rec := newRecServer(t, http.StatusOK, `{}`)
	defer srv.Close()
	s := newTestServer(srv)

	for _, body := range []string{"", "   ", "\n\t "} {
		res := s.TicketComment(context.Background(), TicketCommentInput{Board: testBoard, ID: "t_abc123", Body: body})
		if !res.IsError {
			t.Errorf("body %q: expected IsError", body)
			continue
		}
		if res.Content[0].Text != "invalid_input: body required" {
			t.Errorf("body %q: error = %q, want %q", body, res.Content[0].Text, "invalid_input: body required")
		}
	}
	if len(*rec) != 0 {
		t.Errorf("no request must be issued for an empty body, got %d", len(*rec))
	}
}

func TestTCM_InvalidID(t *testing.T) {
	srv, rec := newRecServer(t, http.StatusOK, `{}`)
	defer srv.Close()
	s := newTestServer(srv)

	for _, id := range []string{"", "t id", "t_ab@c", "t_ab/c", strings.Repeat("a", 65)} {
		res := s.TicketComment(context.Background(), TicketCommentInput{Board: testBoard, ID: id, Body: "hi"})
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

func TestTCM_InvalidBoard(t *testing.T) {
	srv, rec := newRecServer(t, http.StatusOK, `{}`)
	defer srv.Close()
	s := newTestServer(srv)

	for _, board := range []string{"HERMES", "a b", "a/b", "agent@x", strings.Repeat("a", 65)} {
		res := s.TicketComment(context.Background(), TicketCommentInput{Board: board, ID: "t_abc123", Body: "hi"})
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

func TestTCM_OmittedBoardRejectedNoDefault(t *testing.T) {
	srv, rec := newRecServer(t, http.StatusOK, `{}`)
	defer srv.Close()
	s := NewServerWithClient(srv.Client(), srv.URL, "") // no default board

	res := s.TicketComment(context.Background(), TicketCommentInput{ID: "t_abc123", Body: "hi"})
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

func TestTCM_Backend422(t *testing.T) {
	srv, _ := newRecServer(t, http.StatusUnprocessableEntity,
		`{"detail":[{"msg":"field required"},{"msg":"should not be empty"}]}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComment(context.Background(), TicketCommentInput{Board: testBoard, ID: "t_abc123", Body: "hi"})
	if !res.IsError {
		t.Fatalf("expected IsError for 422, got success: %s", res.Content[0].Text)
	}
	if res.Content[0].Text != "schema error: field required; should not be empty" {
		t.Errorf("error = %q, want %q", res.Content[0].Text, "schema error: field required; should not be empty")
	}
}

func TestTCM_Backend404(t *testing.T) {
	srv, _ := newRecServer(t, http.StatusNotFound, `{"detail":"not found"}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComment(context.Background(), TicketCommentInput{Board: testBoard, ID: "t_missing", Body: "hi"})
	if !res.IsError {
		t.Fatalf("expected IsError for 404")
	}
	if res.Content[0].Text != "not_found: not found" {
		t.Errorf("error = %q, want %q", res.Content[0].Text, "not_found: not found")
	}
}

func TestTCM_Backend500(t *testing.T) {
	srv, _ := newRecServer(t, http.StatusInternalServerError, `{"detail":"boom"}`)
	defer srv.Close()
	s := newTestServer(srv)

	res := s.TicketComment(context.Background(), TicketCommentInput{Board: testBoard, ID: "t_abc123", Body: "hi"})
	if !res.IsError {
		t.Fatalf("expected IsError for 500")
	}
	if res.Content[0].Text != "unavailable: boom" {
		t.Errorf("error = %q, want %q", res.Content[0].Text, "unavailable: boom")
	}
}

func TestTCM_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close() // refuse connections
	client := &http.Client{Timeout: 2 * time.Second}
	s := NewServerWithClient(client, deadURL, "hermes-agent")

	res := s.TicketComment(context.Background(), TicketCommentInput{Board: testBoard, ID: "t_abc123", Body: "hi"})
	if !res.IsError {
		t.Fatalf("expected IsError on transport failure")
	}
	if !strings.HasPrefix(res.Content[0].Text, "unavailable: ") {
		t.Errorf("error = %q, want unavailable: prefix", res.Content[0].Text)
	}
}

func TestTCM_PostCommentReusable(t *testing.T) {
	// ticket_complete reuses postComment for the exact outbound request;
	// assert the raw helper issues POST /tasks/{id}/comments with the
	// exact body and no extra fields, and surfaces backend errors as-is.
	srv, rec := newRecServer(t, http.StatusOK, `{}`)
	defer srv.Close()
	s := newTestServer(srv)

	if err := s.postComment(context.Background(), "hermes-agent", "t_xyz", "summary text", "robot"); err != nil {
		t.Fatalf("postComment: %v", err)
	}
	if len(*rec) != 1 {
		t.Fatalf("expected 1 request, got %d", len(*rec))
	}
	got := (*rec)[0]
	if got.method != http.MethodPost || got.path != "/tasks/t_xyz/comments" || got.query != "board=hermes-agent" {
		t.Errorf("request = %s %s?%s, want POST /tasks/t_xyz/comments?board=hermes-agent", got.method, got.path, got.query)
	}
	if keys := bodyKeys(t, got.body); strings.Join(keys, ",") != "author,body" {
		t.Errorf("body keys = %v, want exactly [author body] (body=%s)", keys, got.body)
	}
}

func TestTCM_PostCommentErrorPropagates(t *testing.T) {
	srv, _ := newRecServer(t, http.StatusConflict, `{"detail":"already commented"}`)
	defer srv.Close()
	s := newTestServer(srv)

	err := s.postComment(context.Background(), "hermes-agent", "t_xyz", "summary text", "robot")
	if err == nil {
		t.Fatal("expected error for 409")
	}
	if msg := RestErrorMessage(err); msg != "conflict: already commented" {
		t.Errorf("RestErrorMessage = %q, want %q", msg, "conflict: already commented")
	}
}
