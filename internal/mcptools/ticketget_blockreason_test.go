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
		io_WriteString(w, `{
			"task": {"id": "t_x1", "title": "T", "status": "blocked", "latest_summary": "review-required: shipped the widget"},
			"comments": [],
			"events": [],
			"runs": [{"id": 43, "status": "blocked", "started_at": 1, "summary": "review-required: shipped the widget"}]
		}`)
	}))
	defer fake.Close()

	s := NewServer(fake.URL, "hermes-agent")
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

func io_WriteString(w http.ResponseWriter, s string) {
	_, _ = w.Write([]byte(s))
}
