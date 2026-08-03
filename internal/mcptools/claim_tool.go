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
// ticket_block routes typed kinds (dependency|needs_input|capability|
// transient) through the hardened CLI path and keeps T5's REST fallback
// engaged: when the CLI is unavailable the untyped REST PATCH still runs,
// and the result reports kind_applied=false with the fallback note.

import (
	"context"
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
// defaults to the configured KANBAN_DEFAULT_BOARD. Worker is accepted
// for forward compatibility but never passed to the CLI: as of 2026-08
// `hermes kanban claim` defines no assignee flag, and inventing one is
// forbidden.
type TicketClaimInput struct {
	ID     string `json:"id"`
	Board  string `json:"board"`
	Worker string `json:"worker"`
}

// TicketClaimOut is the authoritative post-claim projection. Every field
// is re-read from the backend after the claim (GetTask), so the caller
// sees the kernel's state, not the CLI's output.
type TicketClaimOut struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	Assignee        string `json:"assignee"`
	ClaimExpires    int64  `json:"claim_expires"`
	ClaimTTLSeconds int64  `json:"claim_ttl_seconds"`
	Note            string `json:"note"`
}

// claimNote is the stable note on every successful claim: the claim TTL
// is ~15 minutes, and once it expires the dispatcher re-queues the
// ticket to ready, at which point ticket_claim works again.
const claimNote = "claim TTL ~15m; re-claim via ticket_claim if it expires"

// TicketClaim implements the ticket_claim MCP tool: validate, fail fast
// when HERMES_BIN is missing, preflight the ticket status over REST
// (ready is claimable; a running ticket with a held lock is reported as
// already claimed; anything else is rejected without exec), shell out to
// the hermes CLI, then re-read the ticket and return the authoritative
// state. Every expected failure is a one-line IsError result.
func (s *Server) TicketClaim(ctx context.Context, in TicketClaimInput) *ToolResult {
	board := in.Board
	if board == "" {
		board = s.defaultBoard
	}
	if board == "" {
		return ErrorResult("invalid_input: no board specified; pass board or set KANBAN_DEFAULT_BOARD")
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

	ts, err := s.GetTask(ctx, board, in.ID)
	if err != nil {
		return ErrorResult("%s", RestErrorMessage(err))
	}
	switch {
	case ts.Status == "ready":
		// Claimable; fall through to the CLI.
	case ts.Status == "running" && ts.ClaimLock != "" && ts.ClaimExpires > time.Now().Unix():
		return ErrorResult("already claimed (expires %d)", ts.ClaimExpires)
	default:
		return ErrorResult("cannot claim: ticket is %s, claim requires ready", ts.Status)
	}

	if _, stderr, err := kanban.Claim(ctx, in.ID, board, in.Worker); err != nil {
		return ErrorResult("%s", cliFailureText(stderr, err))
	}

	after, err := s.GetTask(ctx, board, in.ID)
	if err != nil {
		return ErrorResult("%s", RestErrorMessage(err))
	}
	return SuccessResult(TicketClaimOut{
		ID:              after.ID,
		Status:          after.Status,
		Assignee:        after.Assignee,
		ClaimExpires:    after.ClaimExpires,
		ClaimTTLSeconds: ttlSeconds(after.ClaimExpires),
		Note:            claimNote,
	})
}

// TicketBlockInput is the ticket_block tool input. ID and reason are
// required; board defaults to the configured KANBAN_DEFAULT_BOARD; kind
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
		board = s.defaultBoard
	}
	if board == "" {
		return ErrorResult("invalid_input: no board specified; pass board or set KANBAN_DEFAULT_BOARD")
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

// ttlSeconds is the remaining claim lifetime in whole seconds, floored
// at zero when the lock already expired or the backend reported none.
func ttlSeconds(claimExpires int64) int64 {
	if claimExpires <= 0 {
		return 0
	}
	if rem := claimExpires - time.Now().Unix(); rem > 0 {
		return rem
	}
	return 0
}
