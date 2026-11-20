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
// The implementer label belongs to the HANDOFF, not the claim:
// ticket_claim accepts `worker` and discards it (ClaimWithTTL never passes
// it to the CLI), while ticket_request_review resolves it (explicit worker
// > MCP_CLAIM_WORKER) and refuses a handoff that supplies neither.
const LaneInstructions = `Lane contract for external agents.

Start — ticket_claim the card (ready→running) before you edit.

Identity — pass worker: <lane-label> on ticket_request_review (explicit worker > MCP_CLAIM_WORKER; refused when neither supplies one). ticket_claim records no label: its worker is a no-op. One STABLE label per lane, never an installed profile name.

Review — omit reviewer on ticket_request_review; the server resolves a profile. A reviewer is a PROFILE: sdlc-review is the skill a review run loads, not a name to pass.

Handoff — summary is required on ticket_request_review and IS the reviewer's handoff: what changed, the refs (repo/branch/SHA) to verify, and what you already proved.

Order — push your branch BEFORE the handoff. A done verdict authorises the merge; the reviewer never merges.

Wait — poll ticket_events with since_event_id (the last event id you saw). A request-changes verdict returns the card to your claim label: re-claim it by id before fixing.
`
