package mcptools

// This file implements the ticket_request_review MCP tool: the external
// lane's entry into the native review lane (status='review').
//
// Why it exists: the bridge's only other review entry is ticket_complete
// with review_tier MEDIUM/HIGH, which writes a `review-required:` BLOCK.
// A block needs a consumer, and that consumer is the review-sweeper cron.
// The native lane needs no block at all — request-review sets
// status='review', the dispatcher claims the row as a REVIEW run
// (claimed {source_status: review}), force-loads the sdlc-review skill
// and spawns the reviewer. An APPROVE completes the ticket to done; a
// request-changes verdict returns it to the implementer. Retiring the
// sweeper depends on this tool existing.
//
// The strand guard is the load-bearing part. The kernel's request_review
// accepts status IN ('running','ready') and keeps the task's assignee
// unless a reviewer is supplied. The dispatcher only spawns a review row
// whose assignee matches an installed profile, so an UNASSIGNED review
// ticket is skipped every tick, forever, with no reviewer, no runs and no
// comments. That is not hypothetical: it happened to t_44d19d72 and
// t_371b7d27 on 2026-08-20 (5 days silent, diagnosed in t_124ab31f).
// This tool therefore refuses rather than flipping a status it cannot
// make anyone act on.

import (
	"context"
	"os"
	"strings"

	"github.com/RKelln/hermes-kanban-mcp/internal/kanban"
)

// TicketRequestReviewInput is the ticket_request_review tool input.
// ID and board are required; summary is required (a reviewer with no
// handoff re-derives everything the implementer already knew); reviewer
// is optional and falls back to MCP_REVIEWER_PROFILE, then to the
// ticket's existing assignee.
type TicketRequestReviewInput struct {
	Board    string `json:"board"`
	ID       string `json:"id"`
	Summary  string `json:"summary"`
	Reviewer string `json:"reviewer"`
}

// TicketRequestReviewOut is the authoritative post-transition projection.
// Every field is re-read from the backend after the request (GetTask), so
// the caller sees the kernel's state, not the CLI's stdout. Assignee is
// the reviewer: request_review reassigns the task when a reviewer is
// resolved, and that assignment is the durable provenance reopen-review
// routes changes back to.
type TicketRequestReviewOut struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Assignee string `json:"assignee"`
	Note     string `json:"note,omitempty"`
}

// reviewNote is the stable note on every successful request: the verdict
// arrives asynchronously, and "done" is what authorises the merge.
const reviewNote = "reviewer dispatched on the next tick; a verdict of done authorises your merge, request-changes returns the ticket to you — do NOT merge before the verdict"

// TicketRequestReview implements the ticket_request_review MCP tool:
// validate, resolve the reviewer, fail fast when HERMES_BIN is missing,
// preflight the ticket over REST (ready or running only — the kernel
// refuses anything else), shell out to the hermes CLI, then re-read the
// ticket and return the authoritative state.
//
// force is passed only for a RUNNING ticket: the CLI has no
// --expected-run-id flag, so releasing a live claim requires --force, and
// a running ticket in this bridge was necessarily claimed through
// ticket_claim. A ready ticket gets no force, so this tool can never yank
// a ticket out from under a worker's claim.
func (s *Server) TicketRequestReview(ctx context.Context, in TicketRequestReviewInput) *ToolResult {
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
	if strings.TrimSpace(in.Summary) == "" {
		return ErrorResult("invalid_input: summary required; the reviewer reads it as the handoff (what changed, the refs to verify, what was already proven). Use ticket_comment first if you need to stage it.")
	}

	ts, err := s.GetTask(ctx, board, in.ID)
	if err != nil {
		return ErrorResult("%s", RestErrorMessage(err))
	}

	// Reviewer resolution order: explicit argument, then the server
	// default, then whatever the ticket already carries.
	reviewer := strings.TrimSpace(in.Reviewer)
	if reviewer == "" {
		reviewer = strings.TrimSpace(os.Getenv("MCP_REVIEWER_PROFILE"))
	}
	if reviewer == "" {
		reviewer = strings.TrimSpace(ts.Assignee)
	}
	if reviewer == "" {
		return ErrorResult("invalid_input: no reviewer resolved and ticket %s is unassigned. The dispatcher never spawns an unassigned review ticket — it would sit in 'review' silently and no reviewer would ever run. Pass reviewer, or set MCP_REVIEWER_PROFILE (e.g. \"default\") on the server.", in.ID)
	}
	var force bool
	switch ts.Status {
	case "ready":
		// Nothing holds it; the kernel transition needs no override.
	case "running":
		// Our own claim is live. The CLI has no --expected-run-id, so
		// --force is the only way to release it.
		force = true
	default:
		return ErrorResult("cannot request review: ticket is %s, request-review requires ready or running (a blocked ticket cannot enter the review lane; unblock it first)", ts.Status)
	}

	if err := kanban.CLIBinUnavailable("request-review"); err != nil {
		return ErrorResult("%s", err)
	}
	if _, stderr, err := kanban.RequestReview(ctx, in.ID, board, strings.TrimSpace(in.Summary), reviewer, force); err != nil {
		return ErrorResult("%s", cliFailureText(stderr, err))
	}

	after, err := s.GetTask(ctx, board, in.ID)
	if err != nil {
		return ErrorResult("%s", RestErrorMessage(err))
	}
	return SuccessResult(TicketRequestReviewOut{
		ID:       after.ID,
		Status:   after.Status,
		Assignee: after.Assignee,
		Note:     reviewNote,
	})
}
