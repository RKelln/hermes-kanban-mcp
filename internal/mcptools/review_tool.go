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
// request-changes verdict returns it to the implementer.
//
// The dispatcher only spawns a review row whose assignee is an INSTALLED
// PROFILE. Anything else parks the card in 'review' forever with no
// reviewer, no runs and no comments — the same class that left
// t_44d19d72 and t_371b7d27 invisible for 5 days (t_124ab31f). The
// in-process kanban_request_review tool guards this with profile_exists()
// (tools/kanban_tools.py, "would park the card in `review` on an assignee
// the dispatcher can never spawn"); this tool reaches the same roster
// over HTTP (GET <base>/profiles, the endpoint the dashboard's assignee
// picker uses, backed by hermes_cli.profiles.list_profiles) because it
// has no access to the kernel.

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/RKelln/hermes-kanban-mcp/internal/kanban"
)

// TicketRequestReviewInput is the ticket_request_review tool input.
//
// ID and board are required; summary is required (a reviewer with no
// handoff re-derives everything the implementer already knew); reviewer
// is optional and falls back to MCP_REVIEWER_PROFILE.
//
// force is required to move a RUNNING ticket, and must be omitted for a
// ready one. It exists because the CLI exposes no --expected-run-id, so
// clearing a live claim is only possible with --force — and this bridge
// has no caller identity (one shared bearer token), so it cannot verify
// that the caller owns the claim it is about to release. Requiring the
// caller to assert it keeps that capability deliberate instead of
// automatic.
type TicketRequestReviewInput struct {
	Board    string `json:"board"`
	ID       string `json:"id"`
	Summary  string `json:"summary"`
	Reviewer string `json:"reviewer"`
	Force    bool   `json:"force"`
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
const reviewNote = "a verdict of done authorises your merge, request-changes returns the ticket to you — do NOT merge before the verdict"

// reviewUnverifiedNote is appended when the installed-profile roster
// could not be read, so the spawnability check could not run. Announced,
// never silent: an unverifiable reviewer is exactly the case that strands
// a card, and the caller should know the check did not happen.
const reviewUnverifiedNote = " reviewer profile could NOT be verified against the installed roster (the profiles endpoint did not answer); if this profile is not installed the card will sit in 'review' unspawned"

// reviewProvenanceNote is used when no reviewer was supplied: the kernel
// then resolves its own re-review provenance (the reviewer recorded by the
// previous request-changes round) instead of this tool overriding it.
const reviewProvenanceNote = " no reviewer supplied, so the kernel reused the reviewer from the previous review round"

// profileRoster is the GET /profiles response shape (the dashboard's
// assignee picker reads the same endpoint).
type profileRoster struct {
	Profiles []struct {
		Name      string `json:"name"`
		IsDefault bool   `json:"is_default"`
	} `json:"profiles"`
}

// profileSpawnable reports whether reviewer is an installed profile the
// dispatcher can spawn, plus the roster it checked against. checked is
// false when the roster could not be read at all — the caller proceeds
// but must say so (see reviewUnverifiedNote). Comparison is
// case-insensitive because the kernel canonicalises the assignee with
// normalize_profile_name before writing it.
//
// The oracle matters and is easy to get wrong: GET /profiles is backed by
// hermes_cli.profiles.list_profiles(), which returns only profiles that
// EXIST ON DISK — exactly what the dispatcher's profile_exists() check
// uses. `hermes kanban assignees` is a DIFFERENT, wider list (profiles
// seen on tickets plus on-disk ones): on this host it reports default,
// parent-worker and ryan, and the latter two are not spawnable. Do not
// "simplify" this to the wider list; a permission slip for a
// non-spawnable assignee re-creates the exact strand this guard prevents.
func (s *Server) profileSpawnable(ctx context.Context, reviewer string) (ok, checked bool, names []string) {
	var roster profileRoster
	if err := s.doJSON(ctx, http.MethodGet, "/profiles", nil, nil, &roster); err != nil {
		return false, false, nil
	}
	for _, p := range roster.Profiles {
		if p.Name != "" {
			names = append(names, p.Name)
		}
		if strings.EqualFold(p.Name, reviewer) {
			return true, true, names
		}
	}
	return false, true, names
}

// TicketRequestReview implements the ticket_request_review MCP tool:
// validate, fail fast when HERMES_BIN is missing, preflight the ticket
// over REST (ready or running only — the kernel refuses anything else),
// resolve and verify the reviewer, shell out to the hermes CLI, then
// re-read the ticket, confirm the transition actually happened, and
// return the authoritative state.
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
	// Fail fast on a missing binary before any network call, matching
	// TicketClaim's ordering.
	if err := kanban.CLIBinUnavailable("request-review"); err != nil {
		return ErrorResult("%s", err)
	}

	ts, err := s.GetTask(ctx, board, in.ID)
	if err != nil {
		return ErrorResult("%s", RestErrorMessage(err))
	}

	// --force is only ever meaningful for a live claim, and a ready
	// ticket has none. Passing it there signals a misunderstanding of the
	// lifecycle, so say so rather than silently ignoring it.
	if ts.Status == "ready" && in.Force {
		return ErrorResult("invalid_input: force is only for a RUNNING ticket (this one is ready and holds no claim); drop force. force clears a live claim, so a ready ticket never needs it.")
	}

	var force bool
	switch ts.Status {
	case "ready":
		// Nothing holds it; the kernel transition needs no override.
	case "running":
		if !in.Force {
			return ErrorResult("cannot request review: ticket %s is running under a live claim held by %q. The bridge cannot verify that you own it (every MCP client shares one bearer token), so this is your call to make explicitly: if YOU claimed it with ticket_claim and are requesting review of your OWN finished work, pass force: true. If a worker or the dispatcher holds it mid-implementation, do not — force clears the live claim and hands unfinished work to review.", in.ID, ts.Assignee)
		}
		// The CLI exposes no --expected-run-id, so --force is the only
		// way to release a live claim.
		force = true
	default:
		return ErrorResult("cannot request review: ticket is %s, request-review requires ready or running (a blocked ticket cannot enter the review lane; unblock it first)", ts.Status)
	}

	// Reviewer resolution: explicit argument, then the server default.
	// Deliberately NOT the ticket's assignee — after a request-changes
	// verdict the kernel sets assignee = the implementer, so falling back
	// to it would assign the next round to the author of the change, i.e.
	// self-review. When neither source supplies a reviewer we omit the
	// flag entirely and let the kernel resolve its own re-review
	// provenance, which is the only durable source for it.
	reviewer := strings.TrimSpace(in.Reviewer)
	if reviewer == "" {
		reviewer = strings.TrimSpace(os.Getenv("MCP_REVIEWER_PROFILE"))
	}
	note := reviewNote
	if reviewer == "" {
		if strings.TrimSpace(ts.Assignee) == "" {
			return ErrorResult("invalid_input: no reviewer resolved and ticket %s is unassigned. The dispatcher never spawns an unassigned review ticket — it would sit in 'review' silently and no reviewer would ever run. Pass reviewer, or set MCP_REVIEWER_PROFILE (e.g. \"default\") on the server.", in.ID)
		}
		// Keep the kernel's own provenance path: assignee is preserved
		// and _prior_reviewer is consulted.
		note = reviewNote + ";" + reviewProvenanceNote
	} else {
		ok, checked, names := s.profileSpawnable(ctx, reviewer)
		switch {
		case checked && !ok:
			return ErrorResult("invalid_input: reviewer %q is not an installed profile, so the dispatcher could never spawn it and the card would sit in 'review' unclaimed. Installed profiles: %s.", reviewer, strings.Join(names, ", "))
		case !checked:
			note = reviewNote + ";" + reviewUnverifiedNote
		}
	}

	if _, stderr, err := kanban.RequestReview(ctx, in.ID, board, strings.TrimSpace(in.Summary), reviewer, force); err != nil {
		return ErrorResult("%s", cliFailureText(stderr, err))
	}

	after, err := s.GetTask(ctx, board, in.ID)
	if err != nil {
		return ErrorResult("%s", RestErrorMessage(err))
	}
	// Post-condition. The CLI's own refusal paths all exit non-zero, so a
	// false success would mean a silent no-op or a concurrent change
	// between the exec and this read. Never report "reviewer dispatched"
	// for a ticket that did not actually reach a dispatchable review row.
	if after.Status != "review" {
		return ErrorResult("request-review did not take effect: ticket %s is still %s (expected review). Nothing was dispatched; re-check the ticket before retrying.", in.ID, after.Status)
	}
	if strings.TrimSpace(after.Assignee) == "" {
		return ErrorResult("request-review left ticket %s in 'review' with NO assignee, so the dispatcher will never spawn a reviewer. Set MCP_REVIEWER_PROFILE (e.g. \"default\") on the server, or pass reviewer explicitly, then request again.", in.ID)
	}
	return SuccessResult(TicketRequestReviewOut{
		ID:       after.ID,
		Status:   after.Status,
		Assignee: after.Assignee,
		Note:     note,
	})
}
