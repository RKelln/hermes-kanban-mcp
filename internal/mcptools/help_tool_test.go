package mcptools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// minHelpHeadroom is the deliberate margin kept between the served help and
// MaxHelpOutputBytes. renderResult turns an over-budget result into an ERROR
// (it never clips to fit), so a helpDoc that grows into the last few hundred
// bytes of the cap stops being a doc and becomes an outage for every MCP
// client. This floor makes the next such edit fail a test instead — and the
// measure is the SERVED envelope, not the payload text: the content wrapper
// and the JSON escaping add ~184 B that a payload-only check cannot see
// (which is exactly the blind spot that let 333 B of headroom look safe).
const minHelpHeadroom = 1024

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
	for _, want := range []string{"ticket_events", "ticket_claim", "review-gated", "ticket_complete", "mcp_complete_mode", "ticket_create", "review_queue", "ticket_request_review", "mcp_reviewer_profile", "verified against the installed", "parks in 'review' forever", "force: true"} {
		if !strings.Contains(lower, want) {
			t.Errorf("help doc is missing %q", want)
		}
	}
	served := resultBytes(HelpOut{Text: helpDoc})
	if served > MaxHelpOutputBytes {
		t.Errorf("the served help result is %d bytes, over the %d-byte cap: renderResult returns an ERROR for that, so kanban_help fails for every MCP client", served, MaxHelpOutputBytes)
	}
	if headroom := MaxHelpOutputBytes - served; headroom < minHelpHeadroom {
		t.Errorf("the served help result is %d bytes, %d under the %d-byte cap (want at least %d of headroom): the next addition to helpDoc would make kanban_help fail for every MCP client instead of degrading. Raise MaxHelpOutputBytes deliberately (state the new headroom) or trim helpDoc.", served, headroom, MaxHelpOutputBytes, minHelpHeadroom)
	}
}
