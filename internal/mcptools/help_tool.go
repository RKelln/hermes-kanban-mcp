package mcptools

import "context"

// MaxHelpOutputBytes is the hard cap on the rendered kanban_help result.
// Help is called rarely and on demand, so it may exceed the 2 KB
// write-tool budget — but it stays inside the read-tool range (the
// largest read budget is ticket_get detail=full at 32 KB).
//
// An over-cap help result is a HARD ERROR, not a degradation:
// renderResult refuses an over-budget result rather than clipping it, so
// a helpDoc that outgrew the cap would break kanban_help for every MCP
// client at once. At 23406ae the doc measured 7859 B served, i.e. 333 B
// under the former 8 KiB cap — too small to be a budget and too easy to
// spend by accident. The cap was raised to 12 KiB (4429 B of headroom)
// on 2026-10-16 rather than trimming served contract text to buy bytes:
// the help IS the contract agents self-serve, and a doc loss is silent
// where an over-budget error is at least loud. help_tool_test.go holds
// the floor, so the next edit that eats the margin fails the suite
// instead of every client.
const MaxHelpOutputBytes = 12 * 1024

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
    The wait ends on new events, on the ticket's status CHANGING from the
    status it had when you called, or on the timeout — whichever comes first.
    ticket_status is reported on every return, so "no events + a different
    ticket_status" is the "something happened" signal. A ticket that already
    finished, polled with your cursor caught up, waits out the timeout: pass
    the last seen event id (a verdict writes an event) for an instant answer.
- review_queue    single-call scan for tickets awaiting human review across ALL
    boards (blocked + review-required marker via block_reason or latest_summary);
    replaces per-board scans
- ticket_claim    ready->running BEFORE editing (TTL ~15m; re-claim if expired).
    A non-ready ticket is refused with the reason for THAT status, never a
    generic one — see the claim-refusal facts below before hunting for another
    verb: there is none.
- ticket_comment  log context/decisions as you work
- ticket_request_review  request the FINAL REVIEW of finished work: ready/running ->
    'review' (not a block). The dispatcher claims the row as a REVIEW run and
    spawns the sdlc-review reviewer; APPROVE completes the ticket to done,
    request-changes returns it to you. summary is REQUIRED — it is the
    reviewer's handoff (what changed, the refs to verify, what you proved).
    reviewer resolves as explicit reviewer > MCP_REVIEWER_PROFILE. Whichever
    value will END UP on the review row is verified against the installed
    profile roster and REFUSED if the dispatcher could never spawn it, because
    a review row on a non-profile assignee parks in 'review' forever. When
    neither source supplies a reviewer the flag is omitted and the kernel
    keeps its own choice (prior-reviewer provenance, else the existing
    assignee) — that value is verified the same way. worker is the IMPLEMENTER
    label stamped on the card before the handoff (explicit worker >
    MCP_CLAIM_WORKER; REFUSED when neither supplies one). The kernel records it
    as the handoff's implementer, which is what lets a request-changes verdict
    return the card to you — without it the reviewer's only exit is a block. It
    must NOT be an installed profile, or the dispatcher spawns a worker for the
    card when changes come back. Moving a RUNNING ticket
    additionally requires force: true (the bridge cannot verify claim
    ownership behind one shared bearer token; force also authorises the stamp,
    which reclaims that claim one step earlier).
    Push your branch FIRST. Do NOT merge before the verdict: done means merge.
- ticket_complete finish; review-gated by default, and review-gated now means
    the NATIVE lane: the tool posts the completion comment, then hands the
    card to 'hermes kanban request-review', so it lands in 'review' — the
    state the dispatcher claims as a REVIEW run. It does NOT write the older
    'review-required' block: nothing has consumed that shape since the
    review-sweeper retired (lane_return.py ignores worker-phase blocks by
    design), so a card blocked that way strands with no verdict.
    review_tier: LOW completes direct to done; MEDIUM/HIGH take the handoff
    (default MEDIUM when omitted). MCP_COMPLETE_MODE=done also forces done
    for MEDIUM/HIGH. repo/branch/sha are optional structured refs carried
    into the comment and the reviewer's summary. done mode accepts
    created_cards (child ticket ids; kernel-verified, phantom ids get a 400
    naming the offenders). The handoff needs a reviewer the dispatcher can
    spawn (MCP_REVIEWER_PROFILE, verified against the installed roster) AND an
    implementer label it must never spawn (MCP_CLAIM_WORKER); the completion is
    REFUSED when either resolves to nothing usable, because a review row on a
    non-profile assignee parks in 'review' forever and a handoff with no
    implementer can only ever be blocked by a request-changes verdict.
- ticket_block    blockers; typed kinds: dependency|needs_input|capability|transient
- ticket_create   new ticket; title required; parents supported. There is NO
    triage flag: triage is the kernel's internal intake lane for
    under-specified input (a specifier REWRITES the card's title and body
    before it becomes todo), the bridge never writes there and refuses
    triage: true, and there is no agent "later" lane either — if a card is
    not yours to start, comment why, or block it with kind=needs_input.
- kanban_help     this doc

The per-ticket tools (ticket_events/claim/comment/get/complete/block)
require both board and id; the strict schema rejects calls missing
either. ticket_list and ticket_create also require board — an omitted
board is rejected, never silently defaulted, so multi-board setups can't
land tickets in the wrong queue.

Workflow: board_list -> ticket_list/ticket_get -> ticket_claim -> work ->
ticket_comment -> ticket_request_review -> wait (ticket_events) -> verdict.
'done' means MERGE your branch; request-changes means fix and re-request.
ticket_complete is the same gate in one call: review_tier LOW completes
straight to done, MEDIUM/HIGH hand the card to the review lane. Never merge
before the verdict.

Lifecycle facts:
- Claims are kernel-enforced: a ticket with an open parent stays 'todo';
  gate tickets are wired as parents and complete when the human completes
  them (children promote immediately).
- Claim refusals are status-truthful, and there is no promote tool here: a
  card moves only by claim, block, request-review and complete. triage: an
  internal intake lane — a specifier rewrites the card, no agent action
  exists, surface it to a human. todo: waiting on parent dependencies: <ids>
  — the kernel promotes it to ready when they complete; todo with NO parents
  is a kernel fault, surface it. scheduled: waiting on time, not
  dispatchable. blocked: someone must unblock it (ticket_get shows
  block_reason). running/review: a claim or a reviewer holds it — wait on
  ticket_events, do not re-claim.
- ticket_complete refuses an unclaimed ticket (must be 'running') unless
  MCP_ALLOW_SKIP_CLAIM=true; MCP_COMPLETE_MODE=done completes to 'done'
  instead of the review path. review_tier=LOW completes direct to done;
  MEDIUM/HIGH hand the card to the native review lane (MCP_COMPLETE_MODE=done
  overrides), and the handoff summary is capped at 16 KiB.
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
  transition nobody will act on. The handoff needs BOTH values, and they pull
  opposite ways: a reviewer the dispatcher CAN spawn, and an implementer label
  it must NEVER spawn. The kernel writes the implementer once, at request
  time, from the ticket's assignee — so the tool stamps the label first (on a
  RUNNING card that is reassign --reclaim, which is what force authorises)
  and then reads the value back off the review_requested event; a handoff that
  recorded null is reported as a broken round trip, not a success. A ticket in
  'review' has no branch
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
