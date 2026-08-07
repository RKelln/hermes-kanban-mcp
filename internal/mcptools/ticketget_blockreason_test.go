package mcptools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTicketGetSurfacesBlockReason is the regression for the 2026-08-03
// wobble: the REST task dict carries NO block_reason — block reasons live
// in latest_summary and the run summaries. ticket_get must surface them
// so a blocked ticket is never reason-less through the MCP surface.
func TestTicketGetSurfacesBlockReason(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/boards") {
			// board-lister path: the known-board slug cache needs this
			io_WriteString(w, `{"boards":[{"slug":"hermes-agent","name":"Hermes Agent","counts":{}}]}`)
			return
		}
		io_WriteString(w, `{
			"task": {"id": "t_x1", "title": "T", "status": "blocked", "latest_summary": "review-required: shipped the widget"},
			"comments": [],
			"events": [],
			"runs": [{"id": 43, "status": "blocked", "started_at": 1, "summary": "review-required: shipped the widget"}]
		}`)
	}))
	defer fake.Close()

	s := NewServer(fake.URL, "hermes-agent")
	SetBoardLister(s) // production wiring installs the Server itself via Register
	res := s.TicketGet(context.Background(), TicketGetInput{ID: "t_x1", Board: "hermes-agent"})
	if res == nil || res.IsError {
		t.Fatalf("TicketGet returned error result: %+v", res)
	}
	var out TicketGetOut
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !strings.Contains(out.LatestSummary, "review-required") {
		t.Errorf("LatestSummary = %q, want the review-required reason", out.LatestSummary)
	}
	if !strings.Contains(out.LastRunSummary, "review-required") {
		t.Errorf("LastRunSummary = %q, want the review-required reason from the run", out.LastRunSummary)
	}
}

// TestTicketGetPreservesReviewRefs is the M1 regression: the
// review-required block_reason carries the structured repo/branch/sha
// suffix, which the kernel records in the run summary. ticket_get's
// read-back budget (MaxRunSummaryChars) must be large enough that the
// sha tail survives, so a reviewer on a host without the checkout can
// still resolve the commit. Uses the maximum ref lengths the length-cap
// validation allows, so the budget is proven at the worst case.
func TestTicketGetPreservesReviewRefs(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	reason := "review-required: " + strings.Repeat("s", 100) +
		" | repo: " + strings.Repeat("r", 256) +
		"; branch: " + strings.Repeat("b", 256) +
		"; sha: " + sha
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/boards") {
			io_WriteString(w, `{"boards":[{"slug":"hermes-agent","name":"Hermes Agent","counts":{}}]}`)
			return
		}
		io_WriteString(w, `{
			"task": {"id": "t_x1", "title": "T", "status": "blocked", "latest_summary": "`+reason+`"},
			"comments": [],
			"events": [],
			"runs": [{"id": 43, "status": "blocked", "started_at": 1, "summary": "`+reason+`"}]
		}`)
	}))
	defer fake.Close()

	s := NewServer(fake.URL, "hermes-agent")
	SetBoardLister(s)
	res := s.TicketGet(context.Background(), TicketGetInput{ID: "t_x1", Board: "hermes-agent"})
	if res == nil || res.IsError {
		t.Fatalf("TicketGet returned error result: %+v", res)
	}
	var out TicketGetOut
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	shaTail := "sha: " + sha
	if !strings.Contains(out.LatestSummary, shaTail) {
		t.Errorf("LatestSummary truncated the sha tail: %q", out.LatestSummary)
	}
	if !strings.Contains(out.LastRunSummary, shaTail) {
		t.Errorf("LastRunSummary truncated the sha tail: %q", out.LastRunSummary)
	}
}

func TestTGet_BranchNameSurfaced(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/boards") {
			io_WriteString(w, `{"boards":[{"slug":"hermes-agent","name":"Hermes Agent","counts":{}}]}`)
			return
		}
		io_WriteString(w, `{
			"task": {"id": "t_x1", "title": "T", "status": "running", "branch_name": "feat/x"},
			"comments": [],
			"events": [],
			"runs": []
		}`)
	}))
	defer fake.Close()

	s := NewServer(fake.URL, "hermes-agent")
	SetBoardLister(s)
	res := s.TicketGet(context.Background(), TicketGetInput{ID: "t_x1", Board: "hermes-agent"})
	if res == nil || res.IsError {
		t.Fatalf("TicketGet returned error result: %+v", res)
	}
	var out TicketGetOut
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if out.BranchName != "feat/x" {
		t.Errorf("BranchName = %q, want feat/x", out.BranchName)
	}
}

func io_WriteString(w http.ResponseWriter, s string) {
	_, _ = w.Write([]byte(s))
}
