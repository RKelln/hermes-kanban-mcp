package mcptools

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// adapt converts a Server-method tool (returning *ToolResult) into the
// SDK's typed handler shape. The ToolResult wire shape is converted
// directly: text content parts become mcp.TextContent, IsError maps to
// the result flag.
func adapt[In, Out any](fn func(context.Context, In) *ToolResult) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		tr := fn(ctx, in)
		if tr == nil {
			return nil, *new(Out), errors.New("internal error: nil tool result")
		}
		parts := make([]mcp.Content, 0, len(tr.Content))
		for _, p := range tr.Content {
			parts = append(parts, &mcp.TextContent{Text: p.Text})
		}
		return &mcp.CallToolResult{Content: parts, IsError: tr.IsError}, *new(Out), nil
	}
}

// noOut is the shared structured-output type for Server-method tools:
// the methods return *ToolResult (text content), so the typed handler's
// Out value is always zero and unused by the SDK.
type noOut struct{}

// addTool registers one tool: name, description, JSON input schema, and
// the Server-method implementation adapted to the SDK handler shape.
func addTool[In any](srv *mcp.Server, name, desc string, schema map[string]any, fn func(context.Context, In) *ToolResult) {
	mcp.AddTool(srv, &mcp.Tool{Name: name, Description: desc, InputSchema: schema}, adapt[In, noOut](fn))
}

// --- per-ticket schemas (id and board required client-side) ---

func claimSchema() map[string]any {
	return objReq(map[string]any{"board": propStr(), "id": propStr(), "worker": propStr()}, "id", "board")
}
func commentSchema() map[string]any {
	return objReq(map[string]any{"board": propStr(), "id": propStr(), "body": propStr(), "author": propStr()}, "id", "board")
}
func getSchema() map[string]any {
	return objReq(map[string]any{"board": propStr(), "id": propStr()}, "id", "board")
}
func completeSchema() map[string]any {
	return objReq(map[string]any{
		"board":       propStr(),
		"id":          propStr(),
		"summary":     propStr(),
		"result":      propStr(),
		"metadata":    propStr(),
		"review_tier": map[string]any{"type": "string", "enum": []string{"LOW", "MEDIUM", "HIGH"}},
		"repo":        propStr(),
		"branch":      propStr(),
		"sha":         propStr(),
	}, "id", "board")
}
func blockSchema() map[string]any {
	return objReq(map[string]any{
		"board":  propStr(),
		"id":     propStr(),
		"reason": propStr(),
		"kind":   map[string]any{"type": "string", "enum": []string{"dependency", "needs_input", "capability", "transient"}},
	}, "id", "board")
}
func eventsSchema() map[string]any {
	return objReq(map[string]any{
		"id":              propStr(),
		"board":           propStr(),
		"since_event_id":  map[string]any{"type": "integer", "minimum": 0, "default": 0},
		"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": maxEventsTimeoutSeconds, "default": defaultEventsTimeoutSeconds},
	}, "id", "board")
}

// Register installs all tools on the MCP server and wires the
// known-board slug cache to the Server's backend.
func Register(srv *mcp.Server, s *Server) {
	SetBoardLister(s)

	addTool(srv, "board_list", "List kanban boards with slug, name, and per-status task counts.",
		obj(map[string]any{"include_archived": propBool()}),
		s.BoardList)

	addTool(srv, "ticket_list", "List tickets on a board with optional status/assignee filters; summary-only, never full bodies. (board required)",
		objReq(map[string]any{
			"board":    propStr(),
			"status":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"assignee": propStr(),
			"limit":    propInt(),
		}, "board"),
		s.TicketList)

	addTool(srv, "ticket_get", "Fetch one ticket in full detail with truncation; the escape hatch for ticket_list summaries. (id and board required)",
		getSchema(),
		s.TicketGet)

	addTool(srv, "ticket_claim", "Atomically claim a ready ticket (ready -> running) via the hermes CLI. (id and board required)",
		claimSchema(),
		s.TicketClaim)

	addTool(srv, "ticket_comment", "Append a comment to a ticket. (id and board required)",
		commentSchema(),
		s.TicketComment)

	addTool(srv, "ticket_complete", "Complete a ticket: review_tier LOW completes to done; MEDIUM/HIGH review-gated (default MEDIUM). MCP_COMPLETE_MODE=done overrides MEDIUM/HIGH. repo/branch/sha are optional structured refs folded into the review block. (id and board required)",
		completeSchema(),
		s.TicketComplete)

	addTool(srv, "ticket_block", "Block a ticket; typed kinds (dependency|needs_input|capability|transient) via the CLI with untyped REST fallback. (id and board required)",
		blockSchema(),
		s.TicketBlock)

	addTool(srv, "ticket_events", "Tail a ticket's events (created/claimed/blocked/unblocked/completed...); returns events newer than since_event_id or empty on timeout. Long-polls up to timeout_seconds (default 30, max 120). (id and board required)",
		eventsSchema(),
		s.TicketEvents)

	addTool(srv, "review_queue", "Single-call scan for tickets awaiting human review across ALL boards: blocked tickets whose block_reason marks a review-required completion. One call replaces per-board ticket_list scans (the sweeper's rate-limit pressure).",
		obj(map[string]any{}),
		s.ReviewQueue)

	addTool(srv, "ticket_create", "Create a ticket on a board. Title required; board required; all other fields optional.",
		objReq(map[string]any{
			"board":           propStr(),
			"title":           propStr(),
			"body":            propStr(),
			"assignee":        propStr(),
			"priority":        propInt(),
			"workspace_kind":  propStr(),
			"parents":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"skills":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"triage":          propBool(),
			"idempotency_key": propStr(),
		}, "board"),
		s.TicketCreate)

	addTool(srv, "kanban_help", "Full usage docs for the kanban MCP tools; returns the complete workflow and lifecycle facts.",
		obj(map[string]any{}),
		s.Help)
}

func obj(props map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": props}
}
func objReq(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}
func propStr() map[string]any  { return map[string]any{"type": "string"} }
func propBool() map[string]any { return map[string]any{"type": "boolean", "default": false} }
func propInt() map[string]any  { return map[string]any{"type": "integer"} }
