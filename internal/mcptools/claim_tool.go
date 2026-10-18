package mcptools

// This file implements the ticket_claim and ticket_block MCP tools.
//
// ticket_claim exists because the kanban REST API has no claim endpoint
// (PATCH tasks/{id} {"status":"running"} is rejected by the kernel); the
// atomic ready->running transition is CLI-native. The tool preflights the
// ticket over REST, shells out to `hermes kanban claim`, then re-reads
// the ticket and returns the authoritative post-claim state — never the
// CLI's stdout.
//
// The claim TTL is passed explicitly (MCP_CLAIM_TTL_SECONDS, default four
// hours — see internal/kanban/claim.go) because this lane has no heartbeat
// verb: at the CLI's own 900s default every claim dies mid-run and the
// dispatcher books a FAILED ATTEMPT against the ticket when it reclaims.
// The response reports the TTL and its source, and warns when the kernel's
// expiry disagrees with what was asked — the one runtime signal that the CLI
// stopped honouring the flag.
//
// The preflight is also where the refusal has to be truthful. The old
// blanket refusal boiled every non-ready status down to "not ready, so
// not claimable": it said nothing about WHICH lane the ticket was in and
// read like a missing step the caller could take, which on t_a6e3ba60
// (2026-10-18) sent an agent hunting for a promote verb that has never
// existed in this bridge. Each non-claimable status now gets the reason
// for that status, and only a status with a real agent action names one.
// Triage in particular names no verb: it is the kernel's internal intake
// lane, a specifier rewrites the card, and the only step for an agent is
// to surface it to a human.
//
// ticket_block routes typed kinds (dependency|needs_input|capability|
// transient) through the hardened CLI path and keeps T5's REST fallback
// engaged: when the CLI is unavailable the untyped REST PATCH still runs,
// and the result reports kind_applied=false with the fallback note.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/RKelln/hermes-kanban-mcp/internal/kanban"
)

// maxCLIErrorRunes caps the stderr line surfaced in a claim/block tool
// error, per the T6 spec (first non-empty stderr line, <=200 chars).
const maxCLIErrorRunes = 200

// TicketClaimInput is the ticket_claim tool input. ID is required; board
// required; board is required; worker is accepted
// for forward compatibility but never passed to the CLI: as of 2026-08
// `hermes kanban claim` defines no assignee flag, and inventing one is
// forbidden.
type TicketClaimInput struct {
	ID     string `json:"id"`
	Board  string `json:"board"`
	Worker string `json:"worker"`
}

// TicketClaimOut is the authoritative post-claim projection. Every field
// but the TTL pair is re-read from the backend after the claim (GetTask), so
// the caller sees the kernel's state, not the CLI's output.
//
// ClaimTTLSeconds is the TTL the bridge ASKED the CLI for (MCP_CLAIM_TTL_
// SECONDS, else the 4h default) — not the remaining lifetime; ClaimExpires
// is the kernel's authoritative expiry, so remaining is ClaimExpires minus
// now. ClaimTTLSource says which of those supplied the number, and Note
// carries a warning when the two disagree, which is the signature of a CLI
// that ignored --ttl.
type TicketClaimOut struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	Assignee        string `json:"assignee"`
	ClaimExpires    int64  `json:"claim_expires"`
	ClaimTTLSeconds int64  `json:"claim_ttl_seconds"`
	ClaimTTLSource  string `json:"claim_ttl_source"`
	Note            string `json:"note"`
}

// claimTTLGraceSeconds is how far the kernel's recorded expiry may fall
// short of the requested TTL before claimTTLWarning calls it a mismatch.
// The bridge re-reads the ticket within a second of the claim, so a healthy
// claim is within one or two seconds of what was asked; a minute is slack
// for clock skew between the bridge and the dashboard, not for a wrong TTL.
const claimTTLGraceSeconds int64 = 60

// claimNote is the note on every successful claim: the TTL the bridge asked
// for, where that number came from, and the re-claim fallback. It replaces a
// fixed fifteen-minute line that described the CLI's built-in default — that
// is, the defect, stated as the contract (t_cb0d23c0).
func claimNote(ttl kanban.ClaimTTLSettings) string {
	return fmt.Sprintf("claim TTL %ds (%s); a reclaim books a failed attempt, so re-claim via ticket_claim if yours expired",
		ttl.Seconds, ttl.Source)
}

// claimTTLWarning reports a disagreement between the TTL the bridge passed
// and the expiry the kernel recorded, or "" when they agree. It is the
// runtime half of the "do the two agree?" check: a CLI that ignores --ttl
// would otherwise keep the old 900s silently, which is exactly how this
// defect survived two fixes applied on the deployment side.
func claimTTLWarning(ttl kanban.ClaimTTLSettings, claimExpires int64) string {
	if claimExpires <= 0 {
		return ""
	}
	rem := claimExpires - time.Now().Unix()
	if rem+claimTTLGraceSeconds >= ttl.Seconds {
		return ""
	}
	return fmt.Sprintf("WARNING: kernel expiry is %ds out but %ds was requested; the CLI may have ignored --ttl",
		rem, ttl.Seconds)
}

// TicketClaim implements the ticket_claim MCP tool: validate, fail fast
// when HERMES_BIN is missing, preflight the ticket status over REST
// (ready is claimable; a running ticket with a held lock is reported as
// already claimed; anything else is rejected without exec), shell out to
// the hermes CLI, then re-read the ticket and return the authoritative
// state. Every expected failure is a one-line IsError result.
func (s *Server) TicketClaim(ctx context.Context, in TicketClaimInput) *ToolResult {
	board := in.Board
	if board == "" {
		return ErrorResult("invalid_input: board required; pass board")
	}
	if err := ValidateBoardSlug(board); err != nil {
		return ErrorResult("invalid_input: %v", err)
	}
	if err := ValidateTicketID(in.ID); err != nil {
		return ErrorResult("invalid_input: %v", err)
	}
	if err := kanban.CLIBinUnavailable("claim"); err != nil {
		return ErrorResult("%s", err)
	}
	// Resolve the TTL once, here: the same value goes into argv and into the
	// response, so a caller can never be told a TTL that was not passed.
	ttl := kanban.ResolveClaimTTL()

	ts, parents, err := s.claimPreflight(ctx, board, in.ID)
	if err != nil {
		return ErrorResult("%s", RestErrorMessage(err))
	}
	switch {
	case ts.Status == "ready":
		// Claimable; fall through to the CLI.
	case ts.Status == "running" && ts.ClaimLock != "" && ts.ClaimExpires > time.Now().Unix():
		return ErrorResult("already claimed (expires %d)", ts.ClaimExpires)
	default:
		return ErrorResult("%s", claimRefusal(ts.Status, parents))
	}

	if _, stderr, err := kanban.ClaimWithTTL(ctx, in.ID, board, in.Worker, ttl); err != nil {
		return ErrorResult("%s", cliFailureText(stderr, err))
	}

	after, err := s.GetTask(ctx, board, in.ID)
	if err != nil {
		return ErrorResult("%s", RestErrorMessage(err))
	}
	// The TTL pair comes from what was ASKED (resolved once, above, and
	// passed to the CLI), not from a fresh read of the environment, so the
	// argv and the response cannot disagree. ClaimExpires is the kernel's
	// own answer; a warning when the two are far apart is the only runtime
	// way to see a CLI that stopped honouring --ttl.
	note := claimNote(ttl)
	if warn := claimTTLWarning(ttl, after.ClaimExpires); warn != "" {
		note = warn + "; " + note
	}
	return SuccessResult(TicketClaimOut{
		ID:              after.ID,
		Status:          after.Status,
		Assignee:        after.Assignee,
		ClaimExpires:    after.ClaimExpires,
		ClaimTTLSeconds: ttl.Seconds,
		ClaimTTLSource:  ttl.Source,
		Note:            note,
	})
}

// claimEnvelope is the claim preflight projection: the GET the tool
// already makes, decoded for just the fields the decision needs. Parents
// are the one extra fact (the envelope carries them twice — the task
// dict's own `parents` array and the sibling `links.parents`), and they
// are what turns "ticket is todo" into an actionable sentence. Parent
// STATUSES are not in this projection and are not fetched: naming which
// parent is still open would cost one request per parent, and the card
// promotes itself when the last one completes.
type claimEnvelope struct {
	Task struct {
		kanban.TaskSummary
		Parents []string `json:"parents,omitempty"`
	} `json:"task"`
	Links *kanban.Links `json:"links"`
}

// claimPreflight reads the ticket once and returns the summary the claim
// decision needs plus its parent ids. The preflight and the post-claim
// re-read are separate GETs on purpose: the second one is the
// authoritative state, and this one must not be cached.
func (s *Server) claimPreflight(ctx context.Context, board, id string) (*kanban.TaskSummary, []string, error) {
	var env claimEnvelope
	if err := s.doJSON(ctx, http.MethodGet, "/tasks/"+url.PathEscape(id), url.Values{"board": []string{board}}, nil, &env); err != nil {
		return nil, nil, err
	}
	parents := env.Task.Parents
	if env.Links != nil && len(env.Links.Parents) > 0 {
		parents = env.Links.Parents
	}
	return &env.Task.TaskSummary, parents, nil
}

// claimRefusal is the one-line reason a non-ready ticket cannot be
// claimed, told per status. The rule: say what the lane MEANS and who
// moves the card, and name an agent action only when one exists. Triage
// gets no verb at all (no agent can spec it and no bridge tool leaves
// it); todo names its parents because the kernel's promotion is what ends
// that state; review and running name the holder to wait on. The
// fallback still refuses, but admits it does not know the lane.
func claimRefusal(status string, parents []string) string {
	switch status {
	case "triage":
		return "cannot claim: ticket is triage, an internal intake lane where a specifier rewrites the card; no agent action exists, so surface it to a human"
	case "todo":
		if len(parents) > 0 {
			return fmt.Sprintf("cannot claim: ticket is todo, waiting on parent dependencies: %s (the kernel promotes it to ready when they complete)", strings.Join(parents, ", "))
		}
		return "cannot claim: ticket is todo with no parent dependencies; the kernel should have promoted it to ready, so surface it to a human"
	case "scheduled":
		return "cannot claim: ticket is scheduled, waiting on its scheduled time and not dispatchable now"
	case "blocked":
		return "cannot claim: ticket is blocked and must be unblocked first; ticket_get shows the block_reason, and this bridge has no unblock tool"
	case "running":
		return "cannot claim: ticket is running without a live claim lock; the dispatcher owns it, so wait for ticket_events or surface it to a human"
	case "review":
		return "cannot claim: ticket is in review, the reviewer holds it; wait for the verdict with ticket_events"
	case "done":
		return "cannot claim: ticket is done, there is nothing left to claim"
	case "archived":
		return "cannot claim: ticket is archived, it is out of the workflow"
	case "":
		return "cannot claim: the ticket projection carries no status; surface it to a human"
	default:
		return fmt.Sprintf("cannot claim: ticket is %s, which is not a claimable state; surface it to a human", status)
	}
}

// TicketBlockInput is the ticket_block tool input. ID and reason are
// required; board required; kind
// is one of dependency|needs_input|capability|transient.
type TicketBlockInput struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
	Board  string `json:"board"`
	Kind   string `json:"kind"`
}

// validBlockKinds are the typed block reasons the CLI's --kind accepts.
// Anything else is rejected before any exec or HTTP call.
var validBlockKinds = map[string]bool{
	"dependency":  true,
	"needs_input": true,
	"capability":  true,
	"transient":   true,
}

// TicketBlockOut is the ticket_block success projection, matching T5's
// wire shape. Note is present only when a typed kind was requested but
// the CLI was unavailable, so the caller sees the downgrade explicitly.
type TicketBlockOut struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	KindApplied bool   `json:"kind_applied"`
	Note        string `json:"note,omitempty"`
}

// fallbackBlockNote is T5's exact note for a typed block that had to be
// recorded as untyped.
const fallbackBlockNote = "typed kind unavailable; recorded as untyped block"

// TicketBlock implements the ticket_block MCP tool. An untyped block
// (no kind) goes straight to the REST PATCH. A typed block prefers the
// hardened CLI path (hermes kanban block --kind); when the CLI is
// unavailable — HERMES_BIN missing, or BlockTyped reporting the
// "block unavailable" error — the tool keeps T5's REST fallback engaged
// and records the block as untyped with kind_applied=false.
func (s *Server) TicketBlock(ctx context.Context, in TicketBlockInput) *ToolResult {
	board := in.Board
	if board == "" {
		return ErrorResult("invalid_input: board required; pass board")
	}
	if err := ValidateBoardSlug(board); err != nil {
		return ErrorResult("invalid_input: %v", err)
	}
	if err := ValidateTicketID(in.ID); err != nil {
		return ErrorResult("invalid_input: %v", err)
	}
	if strings.TrimSpace(in.Reason) == "" {
		return ErrorResult("invalid_input: reason required")
	}
	if in.Kind == "" {
		return s.blockUntyped(ctx, board, in.ID, in.Reason, "")
	}
	if !validBlockKinds[in.Kind] {
		return ErrorResult("invalid block kind: %s", in.Kind)
	}
	if err := kanban.CLIBinUnavailable("block"); err != nil {
		return s.blockUntyped(ctx, board, in.ID, in.Reason, fallbackBlockNote)
	}
	if _, stderr, err := kanban.BlockTyped(ctx, in.ID, board, in.Kind, in.Reason); err != nil {
		if strings.HasPrefix(err.Error(), "block unavailable:") {
			return s.blockUntyped(ctx, board, in.ID, in.Reason, fallbackBlockNote)
		}
		return ErrorResult("%s", cliFailureText(stderr, err))
	}
	return SuccessResult(TicketBlockOut{ID: in.ID, Status: "blocked", KindApplied: true})
}

// blockTaskBody is the PATCH /tasks/{id} body for an untyped block.
type blockTaskBody struct {
	Status      string `json:"status"`
	BlockReason string `json:"block_reason"`
}

// blockUntyped sends the REST PATCH that records a block. note, when
// non-empty, is echoed into the result to explain why a requested typed
// kind was not applied.
func (s *Server) blockUntyped(ctx context.Context, board, id, reason, note string) *ToolResult {
	body := blockTaskBody{Status: "blocked", BlockReason: reason}
	err := s.doJSON(ctx, http.MethodPatch, "/tasks/"+url.PathEscape(id), url.Values{"board": []string{board}}, body, nil)
	if err != nil {
		return ErrorResult("%s", RestErrorMessage(err))
	}
	out := TicketBlockOut{ID: id, Status: "blocked", KindApplied: false}
	if note != "" {
		out.Note = note
	}
	return SuccessResult(out)
}

// cliFailureText renders a Claim/BlockTyped failure for a tool error:
// the first non-empty stderr line (trimmed, capped at 200 runes). When
// the CLI produced no stderr — e.g. it was killed by a deadline — it
// falls back to the wrapped error text, which carries the context error.
func cliFailureText(stderr string, err error) string {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return clampRunes(line, maxCLIErrorRunes)
		}
	}
	return clampRunes(err.Error(), maxCLIErrorRunes)
}
