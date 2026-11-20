package mcptools

// LaneInstructions is the server-level MCP `instructions` string: the text
// the SDK serves in the initialize result (ServerOptions.Instructions) and
// that an instructions-honouring client holds before it builds its first
// tool call. It is deliberately NOT a second helpDoc: kanban_help is the
// full reference an agent fetches on demand, while this is the per-session
// overhead every client pays, so it carries only the rules a lane must
// know to avoid a refusal on its first handoff.
//
// Budget: <= 2000 chars. instructions_test.go enforces the cap, asserts
// every `ticket_*` token here is a registered tool name, and checks that
// each tool↔parameter pair below is taught on the same line it is pinned
// against — a sentence can name a real tool and the wrong parameter, which
// is the round-1 defect this text shipped with.
//
// Kept out of helpDoc's territory on purpose: no workflow narration, no
// consequence essays. One rule per line, in the order a lane meets it.
//
// Identity is written at the CLAIM (t_7f1caa0a): ticket_claim assigns the
// lane label to the card BEFORE claiming it, so the kernel copies that
// label into the claim run's profile and ticket_request_review inherits it
// from the card's assignee. Nothing needs to be passed at the handoff.
//
// Force is the precondition of that handoff: a card this lane claimed is
// RUNNING, and ticket_request_review refuses to move a running ticket
// without an explicit force: true (the bridge cannot attribute one shared
// bearer token's claim to a single client). A ready ticket refuses force.
const LaneInstructions = `Lane contract for external agents.

Start — ticket_claim the card (ready→running) before you edit.

Identity — ticket_claim stamps worker: <lane-label> on the card before claiming (worker > MCP_CLAIM_WORKER > remote-mcp). One STABLE label per lane, never an installed profile name. ticket_request_review inherits it: no worker argument on the handoff.

Review — omit reviewer on ticket_request_review; the server resolves a profile. A reviewer is a PROFILE: sdlc-review is the skill a review run loads, not a name to pass.

Handoff — summary is required on ticket_request_review and IS the reviewer's handoff: what changed, the refs (repo/branch/SHA) to verify, and what you already proved. A card you claimed is RUNNING: pass force: true so the handoff can move it (it releases your claim).

Order — push your branch BEFORE the handoff. A done verdict authorises the merge; the reviewer never merges.

Wait — poll ticket_events with since_event_id (the last event id you saw). A request-changes verdict returns the card to your implementer label: re-claim it by id before fixing.
`
