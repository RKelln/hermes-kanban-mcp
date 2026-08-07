package mcptools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestHelpReturnsUsageDoc(t *testing.T) {
	s := NewServer("http://127.0.0.1:9/api/plugins/kanban", "hermes-agent")
	res := s.Help(context.Background(), HelpInput{})
	if res == nil || res.IsError {
		t.Fatalf("Help returned an error result: %+v", res)
	}
	if len(res.Content) == 0 {
		t.Fatal("Help result has no content")
	}
	var out HelpOut
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("Help result is not decodable JSON: %v", err)
	}
	lower := strings.ToLower(out.Text)
	for _, want := range []string{"ticket_events", "ticket_claim", "review-gated", "ticket_complete", "mcp_complete_mode", "ticket_create", "review_queue"} {
		if !strings.Contains(lower, want) {
			t.Errorf("help doc is missing %q", want)
		}
	}
	if len(res.Content[0].Text) > MaxHelpOutputBytes {
		t.Errorf("help result is %d bytes, budget is %d", len(res.Content[0].Text), MaxHelpOutputBytes)
	}
}
