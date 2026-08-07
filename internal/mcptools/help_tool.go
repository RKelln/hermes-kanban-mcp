package mcptools

import "context"

// MaxHelpOutputBytes is the hard cap on the rendered kanban_help result.
// Help is called rarely and on demand, so it may exceed the 2 KB
// write-tool budget — but it stays well under the read-tool ceiling.
const MaxHelpOutputBytes = 8 * 1024

// helpDoc is the full usage documentation returned by kanban_help. It is
// the MCP-native replacement for a docs page: agents self-serve the
// complete workflow without any of it living in their context until
// they ask.
const helpDoc = `# hermes-kanban MCP — usage

MCP tools for a Hermes kanban board:
- board_list      orient: boards + per-status task counts
- ticket_list     summary view; filters: status[], assignee, limit (max 50)
- ticket_get      full detail (truncated); call when list summaries are thin
- ticket_events   tail a ticket's events (verdicts/block/unblock); returns events
    newer than since_event_id or empty on timeout (default 120s, max 15m).
    If the ticket has already left 'blocked' (verdict landed), returns
    IMMEDIATELY with its ticket_status instead of waiting the timeout.
- review_queue    single-call scan for tickets awaiting human review across ALL
    boards (blocked + review-required block_reason); replaces per-board scans
- ticket_claim    ready->running BEFORE editing (TTL ~15m; re-claim if expired)
- ticket_comment  log context/decisions as you work
- ticket_complete finish; REVIEW-GATED by default (comment + review-required block).
    review_tier: LOW completes direct to done; MEDIUM/HIGH stay review-gated
    (default MEDIUM when omitted). MCP_COMPLETE_MODE=done also forces done
    for MEDIUM/HIGH. repo/branch/sha are optional structured refs folded
    into the review-required block_reason (trimmed; single-line).
- ticket_block    blockers; typed kinds: dependency|needs_input|capability|transient
- ticket_create   new ticket; title required; parents supported
- kanban_help     this doc

The per-ticket tools (ticket_events/claim/comment/get/complete/block)
require both board and id; the strict schema rejects calls missing
either. ticket_list and ticket_create also require board — an omitted
board is rejected, never silently defaulted, so multi-board setups can't
land tickets in the wrong queue.

Workflow: board_list -> ticket_list/ticket_get -> ticket_claim -> work ->
ticket_comment -> ticket_complete.

Lifecycle facts:
- Claims are kernel-enforced: a ticket with an open parent stays 'todo';
  gate tickets are wired as parents and complete when the human completes
  them (children promote immediately).
- ticket_complete refuses an unclaimed ticket (must be 'running') unless
  MCP_ALLOW_SKIP_CLAIM=true; MCP_COMPLETE_MODE=done completes to 'done'
  instead of the review path. review_tier=LOW completes direct to done;
  MEDIUM/HIGH default to review-gated (MCP_COMPLETE_MODE=done overrides).
- Results are hard-capped (write tools 2 KB, ticket_list 6 KB, ticket_get
  8 KB, ticket_events 6 KB, review_queue 8 KB) — they are summaries; use
  ticket_get for depth.
- REST completion does not record created_cards; create follow-ups
  explicitly with ticket_create.
- Push the commit BEFORE ticket_complete and pass repo/branch/sha so the
  review block carries the refs; a ticket reaching review with no sha is
  a workflow violation, not a code-review finding.
- ticket_events is stateless long-polling: pass the last seen event id as
  since_event_id to wait for the next verdict; empty + timed_out means
  nothing new arrived. When truncated is set, some events were dropped to
  the size cap — fall back to ticket_get rather than advancing the cursor
  past unseen events.

Rules:
- Never touch the live Hermes install tree (~/.hermes).
- Ask before deploy/publish: ticket_block("approval-required: ...").
- Don't create tickets for yourself; assign follow-ups to the right lane.`

// HelpInput is the kanban_help tool input. It takes no arguments.
type HelpInput struct{}

// HelpOut is the kanban_help result: the full usage documentation.
type HelpOut struct {
	Text string `json:"text"`
}

// Help implements the kanban_help MCP tool.
func (s *Server) Help(ctx context.Context, _ HelpInput) *ToolResult {
	return renderResult(MaxHelpOutputBytes, false, HelpOut{Text: helpDoc})
}
