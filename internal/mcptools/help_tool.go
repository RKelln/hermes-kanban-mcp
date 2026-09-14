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
- ticket_get      full detail; detail=partial (default) applies the bounded
    sizes below, detail=full returns COMPLETE comment bodies and ticket
    body. Use full whenever you must read a long-form comment verbatim —
    a steer, a revision comment, a review verdict — because partial clips
    every comment body and the clipped tail is usually the payload.
- ticket_events   tail a ticket's events (verdicts/block/unblock); returns events
    newer than since_event_id or empty on timeout (default 120s, max 15m).
    If the ticket has already left 'blocked' (verdict landed), returns
    IMMEDIATELY with its ticket_status instead of waiting the timeout.
- review_queue    single-call scan for tickets awaiting human review across ALL
    boards (blocked + review-required marker via block_reason or latest_summary);
    replaces per-board scans
- ticket_claim    ready->running BEFORE editing (TTL ~15m; re-claim if expired)
- ticket_comment  log context/decisions as you work
- ticket_request_review  request the FINAL REVIEW of finished work: ready/running ->
    'review' (not a block). The dispatcher claims the row as a REVIEW run and
    spawns the sdlc-review reviewer; APPROVE completes the ticket to done,
    request-changes returns it to you. summary is REQUIRED — it is the
    reviewer's handoff (what changed, the refs to verify, what you proved).
    reviewer resolves: explicit reviewer > MCP_REVIEWER_PROFILE > the ticket's
    assignee; if none resolves the call is REFUSED, because the dispatcher
    never spawns an unassigned review ticket and it would strand silently.
    Push your branch FIRST. Do NOT merge before the verdict: done means merge.
- ticket_complete finish; REVIEW-GATED by default (comment + review-required block).
    review_tier: LOW completes direct to done; MEDIUM/HIGH stay review-gated
    (default MEDIUM when omitted). MCP_COMPLETE_MODE=done also forces done
    for MEDIUM/HIGH. repo/branch/sha are optional structured refs folded
    into the review-required block_reason (trimmed; single-line). done mode
    accepts created_cards (child ticket ids; kernel-verified, phantom ids
    get a 400 naming the offenders). This is the OLDER review convention
    (a block a sweeper consumes); prefer ticket_request_review for a final
    review when the server has review dispatch enabled.
- ticket_block    blockers; typed kinds: dependency|needs_input|capability|transient
- ticket_create   new ticket; title required; parents supported
- kanban_help     this doc

The per-ticket tools (ticket_events/claim/comment/get/complete/block)
require both board and id; the strict schema rejects calls missing
either. ticket_list and ticket_create also require board — an omitted
board is rejected, never silently defaulted, so multi-board setups can't
land tickets in the wrong queue.

Workflow: board_list -> ticket_list/ticket_get -> ticket_claim -> work ->
ticket_comment -> ticket_request_review -> wait (ticket_events) -> verdict.
'done' means MERGE your branch; request-changes means fix and re-request.
ticket_complete is the older path (review-required block); ticket_request_review
is the review lane. Both are live — use whichever the server's review dispatch
supports, and never merge before the verdict.

Lifecycle facts:
- Claims are kernel-enforced: a ticket with an open parent stays 'todo';
  gate tickets are wired as parents and complete when the human completes
  them (children promote immediately).
- ticket_complete refuses an unclaimed ticket (must be 'running') unless
  MCP_ALLOW_SKIP_CLAIM=true; MCP_COMPLETE_MODE=done completes to 'done'
  instead of the review path. review_tier=LOW completes direct to done;
  MEDIUM/HIGH default to review-gated (MCP_COMPLETE_MODE=done overrides).
- Results are hard-capped (write tools 2 KB, ticket_list 6 KB, ticket_get
  8 KB partial / 32 KB full, ticket_events 6 KB, review_queue 8 KB) — they
  are summaries; use ticket_get for depth.
- Truncation is ALWAYS reported: a clipped field carries an inline
  "…(N more)" marker plus its flag (truncated.body / truncated.comments),
  clipped comments carry a per-comment truncated flag, and
  comments_total/comments_returned/comments_dropped say exactly how much
  of the thread you received. When the budget is tight the OLDEST comments
  are dropped first, so the newest — the live review thread — survive.
  Never treat a partial result as complete, and never re-clip a payload
  yourself on the assumption that the bridge already did.
- REST completion in done mode carries created_cards (kernel-verified;
  phantom ids get a 400 naming the offenders).
- Push the commit BEFORE ticket_complete and pass repo/branch/sha so the
  review block carries the refs; a ticket reaching review with no sha is
  a workflow violation, not a code-review finding.
- Review-lane facts (ticket_request_review): the dispatcher only spawns a
  review row whose assignee is an installed profile, so an UNASSIGNED
  review ticket is skipped EVERY tick with no reviewer, no runs and no
  comments — that is why the tool refuses rather than performing a
  transition nobody will act on. A ticket in 'review' has no branch
  requirement of its own, but the reviewer must be able to see the work:
  push first, and put the refs in summary. If your branch was already
  merged and deleted, say so in summary and name the merge commit and the
  range to diff — a reviewer that goes looking for a deleted branch burns
  its run.
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
