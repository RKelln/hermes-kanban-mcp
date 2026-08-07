package mcptools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// reviewQueueFixture serves the two endpoints ReviewQueue depends on: GET
// /boards (board list) and GET /board?board=<slug> (per-board columns).
// reviewTasks is keyed by slug; every slug in it also appears in the
// board list.
func reviewQueueFixture(t *testing.T, boards []string, reviewTasks map[string]string) *httptest.Server {
	t.Helper()
	var boardList strings.Builder
	boardList.WriteString(`{"boards":[`)
	for i, slug := range boards {
		if i > 0 {
			boardList.WriteString(",")
		}
		boardList.WriteString(`{"slug":"` + slug + `","name":"` + slug + `","counts":{}}`)
	}
	boardList.WriteString(`]}`)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/boards"):
			io_WriteString(w, boardList.String())
		case strings.HasSuffix(r.URL.Path, "/board"):
			slug := r.URL.Query().Get("board")
			body, ok := reviewTasks[slug]
			if !ok {
				http.Error(w, `{"detail":"unknown board"}`, http.StatusNotFound)
				return
			}
			io_WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// reviewBoardBody builds a board JSON body with the given tasks under a
// single column named by status. Tasks are hand-specified so each test
// controls exactly what ReviewQueue must filter.
func reviewBoardBody(tasks ...string) string {
	var b strings.Builder
	b.WriteString(`{"columns":[{"name":"x","tasks":[`)
	for i, task := range tasks {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(task)
	}
	b.WriteString(`]}]}`)
	return b.String()
}

func TestReviewQueueScansAllBoards(t *testing.T) {
	reviewBlocked := `{"id":"t_review1","title":"Shipped the widget","status":"blocked","priority":2,"block_reason":"review-required: Shipped the widget"}`
	otherBlocked := `{"id":"t_other","title":"Waiting on infra","status":"blocked","priority":1,"block_reason":"Waiting on the infra team"}`
	reviewNotBlocked := `{"id":"t_ready","title":"Review me","status":"ready","priority":0,"block_reason":"review-required: premature"}`
	secondBoard := `{"id":"t_review2","title":"Second board review","status":"blocked","priority":3,"block_reason":"review-required: second board"}`

	boards := []string{"hermes-agent", "default"}
	reviewTasks := map[string]string{
		"hermes-agent": reviewBoardBody(reviewBlocked, otherBlocked, reviewNotBlocked),
		"default":      reviewBoardBody(secondBoard),
	}
	fake := reviewQueueFixture(t, boards, reviewTasks)
	s := NewServer(fake.URL, "hermes-agent")

	res := s.ReviewQueue(context.Background(), ReviewQueueInput{})
	if res == nil || res.IsError {
		t.Fatalf("ReviewQueue returned error result: %+v", res)
	}
	var out ReviewQueueOut
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if out.Total != 2 || out.Returned != 2 {
		t.Errorf("Total/Returned = %d/%d, want 2/2", out.Total, out.Returned)
	}
	if len(out.Tickets) != 2 {
		t.Fatalf("len(Tickets) = %d, want 2", len(out.Tickets))
	}
	want := []struct {
		board string
		id    string
	}{
		{"default", "t_review2"},
		{"hermes-agent", "t_review1"},
	}
	for i, w := range want {
		if out.Tickets[i].Board != w.board || out.Tickets[i].ID != w.id {
			t.Errorf("ticket %d = %s/%s, want %s/%s", i, out.Tickets[i].Board, out.Tickets[i].ID, w.board, w.id)
		}
	}
}

func TestReviewQueueSkipsBlockedNonReviewAndNonBlocked(t *testing.T) {
	// No review-required tickets anywhere: result must be empty, not an
	// error, even though other blocked/ready tickets exist.
	reviewTasks := map[string]string{
		"hermes-agent": reviewBoardBody(
			`{"id":"t_b1","title":"B1","status":"blocked","block_reason":"needs input"}`,
			`{"id":"t_r1","title":"R1","status":"ready"}`,
			`{"id":"t_d1","title":"D1","status":"done"}`,
		),
	}
	fake := reviewQueueFixture(t, []string{"hermes-agent"}, reviewTasks)
	s := NewServer(fake.URL, "hermes-agent")

	res := s.ReviewQueue(context.Background(), ReviewQueueInput{})
	if res == nil || res.IsError {
		t.Fatalf("ReviewQueue returned error result: %+v", res)
	}
	var out ReviewQueueOut
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if out.Total != 0 || len(out.Tickets) != 0 {
		t.Errorf("Total/len = %d/%d, want 0/0 (only review-required blocked tickets match)", out.Total, len(out.Tickets))
	}
}

func TestReviewQueueMatchesLatestSummaryFallback(t *testing.T) {
	// The live kanban API omits block_reason on the task dict (t_828c3b69);
	// the review-required marker lives in latest_summary/last_run_summary.
	// A blocked ticket whose summary carries "review-required: <summary>"
	// must match even with no block_reason present.
	reviewTasks := map[string]string{
		"hermes-agent": reviewBoardBody(
			`{"id":"t_summary","title":"S","status":"blocked","latest_summary":"review-required: shipped the widget"}`,
			`{"id":"t_run","title":"R","status":"blocked","last_run_summary":"review-required: from the run"}`,
			`{"id":"t_plain","title":"P","status":"blocked","latest_summary":"Waiting on the infra team"}`,
			`{"id":"t_urgent","title":"U","status":"blocked","latest_summary":"review-required-urgent: still blocked"}`,
			`{"id":"t_case","title":"C","status":"blocked","latest_summary":"  Review-required: mixed case"}`,
		),
	}
	fake := reviewQueueFixture(t, []string{"hermes-agent"}, reviewTasks)
	s := NewServer(fake.URL, "hermes-agent")

	res := s.ReviewQueue(context.Background(), ReviewQueueInput{})
	if res == nil || res.IsError {
		t.Fatalf("ReviewQueue returned error result: %+v", res)
	}
	var out ReviewQueueOut
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if out.Total != 3 || len(out.Tickets) != 3 {
		t.Fatalf("Total/len = %d/%d, want 3/3 (latest_summary, last_run_summary, and case-insensitive matches; not plain/urgent)", out.Total, len(out.Tickets))
	}
	got := map[string]bool{}
	for _, it := range out.Tickets {
		got[it.ID] = true
	}
	for _, want := range []string{"t_summary", "t_run", "t_case"} {
		if !got[want] {
			t.Errorf("missing match %q (have %v)", want, got)
		}
	}
	for _, not := range []string{"t_plain", "t_urgent"} {
		if got[not] {
			t.Errorf("must NOT match %q (have %v)", not, got)
		}
	}
}

func TestReviewQueueBackendError(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/boards") {
			io_WriteString(w, `{"boards":[{"slug":"hermes-agent","name":"Hermes Agent","counts":{}}]}`)
			return
		}
		http.Error(w, `{"detail":"boom"}`, http.StatusInternalServerError)
	}))
	defer fake.Close()

	s := NewServer(fake.URL, "hermes-agent")
	res := s.ReviewQueue(context.Background(), ReviewQueueInput{})
	if res == nil || !res.IsError {
		t.Fatalf("ReviewQueue on backend 500: expected error result, got %+v", res)
	}
	if !strings.Contains(res.Content[0].Text, "unavailable") {
		t.Errorf("error text = %q, want it to mention unavailability", res.Content[0].Text)
	}
}

func TestReviewQueueBoardsListError(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/boards") {
			http.Error(w, `{"detail":"boards down"}`, http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
	}))
	defer fake.Close()

	s := NewServer(fake.URL, "hermes-agent")
	res := s.ReviewQueue(context.Background(), ReviewQueueInput{})
	if res == nil || !res.IsError {
		t.Fatalf("ReviewQueue on /boards 503: expected error result, got %+v", res)
	}
	if !strings.Contains(res.Content[0].Text, "unavailable") {
		t.Errorf("error text = %q, want it to mention unavailability", res.Content[0].Text)
	}
}

func TestReviewQueueProjectionAndTruncation(t *testing.T) {
	longTitle := strings.Repeat("T", 200)
	longReason := "review-required: " + strings.Repeat("s", 200)
	reviewTasks := map[string]string{
		"hermes-agent": reviewBoardBody(
			`{"id":"t_trunc","title":"` + longTitle + `","status":"blocked","assignee":"reviewer","priority":5,"block_reason":"` + longReason + `"}`,
		),
	}
	fake := reviewQueueFixture(t, []string{"hermes-agent"}, reviewTasks)
	s := NewServer(fake.URL, "hermes-agent")

	res := s.ReviewQueue(context.Background(), ReviewQueueInput{})
	if res == nil || res.IsError {
		t.Fatalf("ReviewQueue returned error result: %+v", res)
	}
	var out ReviewQueueOut
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(out.Tickets) != 1 {
		t.Fatalf("len(Tickets) = %d, want 1", len(out.Tickets))
	}
	item := out.Tickets[0]
	if item.ID != "t_trunc" || item.Board != "hermes-agent" || item.Status != "blocked" || item.Assignee != "reviewer" || item.Priority != 5 {
		t.Errorf("projection = %+v, want id/board/status/assignee/priority all populated", item)
	}
	if len([]rune(item.Title)) != 120 {
		t.Errorf("title runes = %d, want 120 (truncated)", len([]rune(item.Title)))
	}
	if len([]rune(item.BlockReason)) != MaxBlockedReasonChars {
		t.Errorf("block_reason runes = %d, want %d (truncated)", len([]rune(item.BlockReason)), MaxBlockedReasonChars)
	}
}

func TestReviewQueuePrefixBoundary(t *testing.T) {
	// The machine-stamped form is "review-required: <summary>". A reason
	// that merely starts with the word but lacks the colon-separated
	// summary must not match.
	reviewTasks := map[string]string{
		"hermes-agent": reviewBoardBody(
			`{"id":"t_exact","title":"Exact","status":"blocked","block_reason":"review-required"}`,
			`{"id":"t_no_space","title":"No space","status":"blocked","block_reason":"review-required:done"}`,
			`{"id":"t_word","title":"Word","status":"blocked","block_reason":"review-required-urgent"}`,
			`{"id":"t_prefix","title":"Prefix","status":"blocked","block_reason":"review-requiredextra"}`,
		),
	}
	fake := reviewQueueFixture(t, []string{"hermes-agent"}, reviewTasks)
	s := NewServer(fake.URL, "hermes-agent")

	res := s.ReviewQueue(context.Background(), ReviewQueueInput{})
	if res == nil || res.IsError {
		t.Fatalf("ReviewQueue returned error result: %+v", res)
	}
	var out ReviewQueueOut
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if out.Total != 2 {
		t.Fatalf("Total = %d, want 2 (exact + no-space colon forms)", out.Total)
	}
	got := map[string]bool{}
	for _, it := range out.Tickets {
		got[it.ID] = true
	}
	if !got["t_exact"] || !got["t_no_space"] {
		t.Errorf("matched ids = %v, want t_exact and t_no_space", got)
	}
	if got["t_word"] || got["t_prefix"] {
		t.Errorf("matched ids = %v, must NOT match word/prefix-only reasons", got)
	}
}

func TestReviewQueueRendersValidJSONWithinBudget(t *testing.T) {
	// Many review-required tickets: the result must stay valid JSON and
	// fit the size budget even when the item cap or budget shrink kicks
	// in, and Total/Returned/Truncated must be truthful.
	var tasks []string
	for i := 0; i < 60; i++ {
		tasks = append(tasks, `{"id":"t_q`+strconv.Itoa(i)+`","title":"`+strings.Repeat("x", 120)+`","status":"blocked","priority":`+strconv.Itoa(i)+`,"block_reason":"review-required: `+strings.Repeat("y", 120)+`"}`)
	}
	reviewTasks := map[string]string{"hermes-agent": reviewBoardBody(tasks...)}
	fake := reviewQueueFixture(t, []string{"hermes-agent"}, reviewTasks)
	s := NewServer(fake.URL, "hermes-agent")

	res := s.ReviewQueue(context.Background(), ReviewQueueInput{})
	if res == nil || res.IsError {
		t.Fatalf("ReviewQueue returned error result: %+v", res)
	}
	if len(res.Content[0].Text) > MaxReviewQueueOutputBytes {
		t.Errorf("result is %d bytes, budget is %d", len(res.Content[0].Text), MaxReviewQueueOutputBytes)
	}
	var out ReviewQueueOut
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if out.Total != 60 {
		t.Errorf("Total = %d, want 60 (all found, before caps)", out.Total)
	}
	if out.Returned != len(out.Tickets) {
		t.Errorf("Returned = %d but len(Tickets) = %d, must match", out.Returned, len(out.Tickets))
	}
	if out.Returned > MaxReviewQueueItems {
		t.Errorf("Returned = %d, want <= MaxReviewQueueItems %d", out.Returned, MaxReviewQueueItems)
	}
	// 60 items at ~120+120 runes each cannot fit in 8 KB: the drop path
	// must have run, so Truncated is mandatory and Returned < 60.
	if !out.Truncated {
		t.Error("Truncated must be set: 60 oversized tickets cannot fit the budget")
	}
	if out.Returned >= 60 {
		t.Errorf("Returned = %d, want < 60 (items must have been dropped to fit the budget)", out.Returned)
	}
}
