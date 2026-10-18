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
//
// Two fields on the handoff decide whether the lane can ever get the card
// back, and they pull in OPPOSITE directions:
//
//	reviewer    must be an installed profile the dispatcher can spawn;
//	implementer must NOT be one, because the kernel lands the card back at
//	            `ready` with assignee = implementer and the dispatcher
//	            claims any ready card whose assignee is on-disk.
//
// The kernel writes the implementer once, at request time, from the
// current run's profile, falling back to the ticket's assignee only when
// that assignee is not the reviewer (kanban_db.request_review). A bridge
// claim writes no run profile — `hermes kanban claim` has no profile flag
// — and the external lane never assigned its cards, so both sources were
// empty and every handoff recorded implementer=null. request_changes then
// REFUSED (\"review handoff has no valid implementer provenance\") and the
// reviewer's only remaining exit was a block: measured live 2026-10-18 on
// board `togather`, ticket t_a6e3ba60 (t_b917c3c6). This tool therefore
// stamps a durable lane label on the card BEFORE the handoff, which is
// the kernel's own designed fallback, and then verifies the value the
// kernel actually recorded.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
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
// worker is the implementer label stamped on the card before the handoff
// (the same name ticket_claim accepts). It falls back to
// MCP_CLAIM_WORKER, and the handoff is REFUSED when neither supplies one:
// an unnamed implementer is the exact defect this parameter exists to
// end. Do not read it as a profile — it must NOT be one (see the file
// comment).
//
// force is required to move a RUNNING ticket, and must be omitted for a
// ready one. It exists because the CLI exposes no --expected-run-id, so
// clearing a live claim is only possible with --force — and this bridge
// has no caller identity (one shared bearer token), so it cannot verify
// that the caller owns the claim it is about to release. Requiring the
// caller to assert it keeps that capability deliberate instead of
// automatic. On a running ticket the same assertion also authorises the
// implementer stamp, which releases that claim one step earlier.
type TicketRequestReviewInput struct {
	Board    string `json:"board"`
	ID       string `json:"id"`
	Summary  string `json:"summary"`
	Reviewer string `json:"reviewer"`
	Worker   string `json:"worker"`
	Force    bool   `json:"force"`
}

// TicketRequestReviewOut is the authoritative post-transition projection.
// Every field is re-read from the backend after the request (GetTask), so
// the caller sees the kernel's state, not the CLI's stdout. Assignee is
// the reviewer: request_review reassigns the task when a reviewer is
// resolved, and that assignment is the durable provenance reopen-review
// routes changes back to. Implementer is the value the kernel recorded on
// the review_requested event, read back from the event log — the field a
// request-changes verdict routes on.
type TicketRequestReviewOut struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Assignee    string `json:"assignee"`
	Implementer string `json:"implementer,omitempty"`
	Note        string `json:"note,omitempty"`
}

// reviewNote is the stable note on every successful request: the verdict
// arrives asynchronously, and "done" is what authorises the merge.
const reviewNote = "a verdict of done authorises your merge, request-changes returns the ticket to you — do NOT merge before the verdict"

// reviewProvenanceNote is used when no reviewer was supplied: the kernel
// then resolves its own re-review provenance (the reviewer recorded by the
// previous request-changes round) instead of this tool overriding it.
const reviewProvenanceNote = " no reviewer supplied, so the kernel reused the reviewer from the previous review round"

// implementerKnob is the server knob every implementer refusal names, so
// the operator does not have to guess which setting the handoff needs.
const implementerKnob = "MCP_CLAIM_WORKER"

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

// resolveImplementerLabel decides which lane label the handoff records.
//
// Sources, in order: the explicit worker argument (the same name
// ticket_claim accepts), then MCP_CLAIM_WORKER. There is deliberately no
// third source and no default: a handoff with no provenance is the defect
// this card exists to end, so the refusal names the knob instead of
// inventing a label that would silently become every lane's identity.
//
// The label is then checked in the direction that is easy to forget: it
// must NOT be an installed profile. The kernel lands the card back at
// `ready` with assignee = implementer, and the dispatcher claims ready
// cards assigned to on-disk profiles, so a profile here puts an agent
// worker on the card when changes come back — the "reviewer writes code"
// failure with a different actor. A label that is already the reviewer is
// refused too, because the kernel only falls back to the assignee when it
// DIFFERS from the reviewer: that handoff would still record null.
//
// The roster could not be read case is a refusal, not an announcement.
// For the reviewer an unverified value strands a card (loud enough at the
// next ticket_events); for the implementer an unverified COLLISION
// spawns a worker on work the lane was supposed to resume, which is
// silent by construction. Fail closed.
func (s *Server) resolveImplementerLabel(ctx context.Context, in TicketRequestReviewInput, reviewer string) (label, source string, failure *ToolResult) {
	label = strings.TrimSpace(in.Worker)
	source = "the worker argument"
	if label == "" {
		label = strings.TrimSpace(os.Getenv(implementerKnob))
		source = implementerKnob
	}
	if label == "" {
		return "", "", ErrorResult("invalid_input: no implementer label resolved, so this handoff would record implementer=null and a request-changes verdict could not return the ticket (its only exit would be a block). Pass worker, or set %s on the server (e.g. %s=\"opencode-remote\").", implementerKnob, implementerKnob)
	}
	if !kanban.ValidLaneToken(label) {
		return "", "", ErrorResult("invalid_input: implementer label %q is not a lane token (must match %s); it is written to the ticket's assignee, so keep it to a plain control-plane name", label, "^[A-Za-z0-9._-]{1,64}$")
	}
	if strings.EqualFold(label, reviewer) {
		return "", "", ErrorResult("invalid_input: the implementer label %q is also the reviewer. The kernel records the assignee as the implementer only when it DIFFERS from the reviewer, so this handoff would still record implementer=null and a request-changes verdict could not return the ticket. Pass a distinct worker label (%s=\"opencode-remote\"), or a different reviewer.", label, implementerKnob)
	}
	ok, checked, names := s.profileSpawnable(ctx, label)
	switch {
	case !checked:
		return "", "", ErrorResult("invalid_input: could not read the installed profile roster (GET /profiles did not answer), so implementer label %q could NOT be checked. The handoff is refused rather than risked: if that label matches a profile, the dispatcher spawns a worker on the card when a request-changes verdict returns it. Retry once the dashboard API answers, or set %s to a label you know is not a profile (e.g. \"opencode-remote\").", label, implementerKnob)
	case ok:
		return "", "", ErrorResult("invalid_input: implementer label %q IS an installed profile. The kernel lands the card back at 'ready' with assignee = implementer, and the dispatcher spawns a worker for a ready card assigned to an on-disk profile — so this handoff would put an agent worker on work the lane was meant to resume. Installed profiles: %s. Set %s to the lane's control-plane label (e.g. \"opencode-remote\"), not a profile.", label, strings.Join(names, ", "), implementerKnob)
	}
	return label, source, nil
}

// stampImplementer writes label onto the ticket's assignee when it is not
// already the implementer of record.
//
// ready:  `assign <id> <label>` — ungated, and safe: the label is not a
// profile (verified above), so the dispatcher skips the card.
// running: `reassign <id> <label> --reclaim` — the only form that works
// while a claim is live, because assign_task refuses a running claimed
// task. This releases the claim, so it is gated behind the caller's
// force assertion (that is the same authority --force would exercise a
// step later) and the returned ticket is re-read, never assumed.
//
// The re-read after the write is the point: the kernel canonicalises the
// assignee (lowercase) and the CLI can no-op, so the caller's next step
// is decided from what LANDED, not from what was asked.
func (s *Server) stampImplementer(ctx context.Context, board, id, label string, ts *kanban.TaskSummary) (*kanban.TaskSummary, *ToolResult) {
	if strings.EqualFold(strings.TrimSpace(ts.Assignee), label) {
		return ts, nil
	}
	reclaim := ts.Status == "running"
	reason := ""
	if reclaim {
		reason = "review handoff: record implementer " + label
	}
	if _, stderr, err := kanban.AssignLabel(ctx, id, board, label, reason, reclaim); err != nil {
		msg := cliFailureText(stderr, err)
		if reclaim {
			return nil, ErrorResult("could not stamp the implementer label: %s. The handoff was NOT requested. This write reclaims a RUNNING ticket's live claim before reassigning, so the ticket may now be ready — re-read it (ticket_get) before retrying.", msg)
		}
		return nil, ErrorResult("could not stamp the implementer label: %s. The handoff was NOT requested; the ticket is unchanged.", msg)
	}
	after, err := s.GetTask(ctx, board, id)
	if err != nil {
		return nil, ErrorResult("%s", RestErrorMessage(err))
	}
	if !strings.EqualFold(strings.TrimSpace(after.Assignee), label) {
		return nil, ErrorResult("the implementer stamp did not take effect: ticket %s carries assignee %q, not %q. The handoff was NOT requested. Nothing was dispatched; source a different label (%s).", id, after.Assignee, label, implementerKnob)
	}
	return after, nil
}

// reviewHandoffImplementer reads the implementer recorded on the newest
// review_requested event in a task detail envelope's event log. found is
// false when no such event is readable; ok is false when one exists but
// its payload cannot be decoded. recorded is "" for an explicit null,
// which is the failure this tool exists to prevent and must be reported
// as such rather than as "no event".
func reviewHandoffImplementer(events []json.RawMessage) (recorded string, found, decoded bool) {
	for i := len(events) - 1; i >= 0; i-- {
		var e struct {
			Kind    string          `json:"kind"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(events[i], &e); err != nil {
			continue
		}
		if e.Kind != "review_requested" {
			continue
		}
		var p struct {
			Implementer *string `json:"implementer"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return "", true, false
		}
		if p.Implementer == nil {
			return "", true, true
		}
		return strings.TrimSpace(*p.Implementer), true, true
	}
	return "", false, false
}

// getTaskDetail reads the ticket AND its event log in one GET
// (GET /tasks/{id} returns comments/events/runs alongside the task). The
// handoff verification uses one fetch for both facts so "the transition
// landed" and "the implementer was recorded" can never be answered from
// two different reads of a moving ticket.
func (s *Server) getTaskDetail(ctx context.Context, board, id string) (*kanban.TaskSummary, []json.RawMessage, error) {
	var env taskDetailEnvelope
	if err := s.doJSON(ctx, http.MethodGet, "/tasks/"+url.PathEscape(id), url.Values{"board": []string{board}}, nil, &env); err != nil {
		return nil, nil, err
	}
	return &env.Task.TaskSummary, env.Events, nil
}

// TicketRequestReview implements the ticket_request_review MCP tool:
// validate, fail fast when HERMES_BIN is missing, preflight the ticket
// over REST (ready or running only — the kernel refuses anything else),
// resolve and verify the reviewer, resolve and verify the implementer
// label, stamp that label on the card, shell out to the hermes CLI, then
// re-read the ticket and the event log, confirm the transition actually
// happened AND that the kernel recorded the implementer, and return the
// authoritative state.
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

	switch ts.Status {
	case "ready":
		// Nothing holds it; the kernel transition needs no override.
	case "running":
		if !in.Force {
			return ErrorResult("cannot request review: ticket %s is running under a live claim held by %q. The bridge cannot verify that you own it (every MCP client shares one bearer token), so this is your call to make explicitly: if YOU claimed it with ticket_claim and are requesting review of your OWN finished work, pass force: true. If a worker or the dispatcher holds it mid-implementation, do not — force clears the live claim and hands unfinished work to review.", in.ID, ts.Assignee)
		}
	default:
		return ErrorResult("cannot request review: ticket is %s, request-review requires ready or running (a blocked ticket cannot enter the review lane; unblock it first)", ts.Status)
	}

	// Reviewer resolution: explicit argument, then the server default.
	// Deliberately NOT the ticket's assignee — after a request-changes
	// verdict the kernel sets assignee = the implementer, so *substituting*
	// it as the reviewer would make the next round a self-review.
	//
	// Two values matter and they are NOT the same one:
	//   flagReviewer  — what we pass to the CLI. Empty means "omit
	//                   --reviewer" and let the kernel make its own choice
	//                   (prior-reviewer provenance, else preserve the
	//                   existing assignee). Passing the assignee here would
	//                   substitute it and defeat that provenance.
	//   verifyValue   — what will END UP on the review row, which is what
	//                   the dispatcher spawns on. Verified in every tier:
	//                   an unspawnable assignee is the strand no matter who
	//                   chose it. Verifying a value is not substituting it.
	flagReviewer := strings.TrimSpace(in.Reviewer)
	if flagReviewer == "" {
		flagReviewer = strings.TrimSpace(os.Getenv("MCP_REVIEWER_PROFILE"))
	}
	useProvenance := flagReviewer == ""
	verifyValue := flagReviewer
	if useProvenance {
		verifyValue = strings.TrimSpace(ts.Assignee)
		if verifyValue == "" {
			return ErrorResult("invalid_input: no reviewer resolved and ticket %s is unassigned. The dispatcher never spawns an unassigned review ticket — it would sit in 'review' silently and no reviewer would ever run. Pass reviewer, or set MCP_REVIEWER_PROFILE (e.g. \"default\") on the server.", in.ID)
		}
	}
	notes := []string{reviewNote}
	ok, checked, names := s.profileSpawnable(ctx, verifyValue)
	switch {
	case checked && !ok:
		if useProvenance {
			return ErrorResult("invalid_input: no reviewer was supplied and ticket %s carries assignee %q, which is not an installed profile. The kernel would preserve that assignee, so the card would sit in 'review' with no reviewer the dispatcher can ever spawn. Installed profiles: %s. Pass reviewer, or set MCP_REVIEWER_PROFILE (e.g. \"default\") on the server.", in.ID, verifyValue, strings.Join(names, ", "))
		}
		return ErrorResult("invalid_input: reviewer %q is not an installed profile, so the dispatcher could never spawn it and the card would sit in 'review' unclaimed. Installed profiles: %s.", verifyValue, strings.Join(names, ", "))
	case !checked:
		// The roster is the oracle for both directions (spawnable
		// reviewer, NOT-spawnable implementer), so an unreadable roster
		// makes the whole handoff unverifiable. This used to proceed with
		// an announced warning for the reviewer alone; the implementer
		// half cannot be announced safely (a collision spawns a worker
		// silently), so the handoff now fails closed in one sentence.
		return ErrorResult("invalid_input: could not read the installed profile roster (GET /profiles did not answer), so neither the reviewer %q nor the implementer label can be verified. The handoff is refused rather than risked: a non-profile reviewer strands the card, and an implementer label that collides with a profile makes the dispatcher spawn a worker when changes come back. Retry once the dashboard API answers.", verifyValue)
	}
	if useProvenance {
		notes = append(notes, reviewProvenanceNote)
	}

	// Implementer provenance. Resolved and verified BEFORE the stamp,
	// because a refused label must leave the ticket untouched.
	label, labelSource, failure := s.resolveImplementerLabel(ctx, in, verifyValue)
	if failure != nil {
		return failure
	}
	stamped, failure := s.stampImplementer(ctx, board, in.ID, label, ts)
	if failure != nil {
		return failure
	}
	if strings.EqualFold(strings.TrimSpace(ts.Assignee), label) {
		notes = append(notes, "implementer="+label+" (already on the card), so a request-changes verdict lands back here assigned to it — claimable by id, and never spawned because it is not an installed profile")
	} else {
		notes = append(notes, "implementer="+label+" (from "+labelSource+") stamped on the card, so a request-changes verdict lands back here assigned to it — claimable by id, and never spawned because it is not an installed profile")
	}
	// A live claim is gone once the stamp reclaimed it; the kernel's guard
	// then has nothing to clear, and passing --force would claim an
	// override this call no longer needs.
	cliForce := stamped.Status == "running"
	note := strings.Join(notes, ";")

	if _, stderr, err := kanban.RequestReview(ctx, in.ID, board, strings.TrimSpace(in.Summary), flagReviewer, cliForce); err != nil {
		return ErrorResult("%s", cliFailureText(stderr, err))
	}

	after, events, err := s.getTaskDetail(ctx, board, in.ID)
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
	// The row's assignee can differ from what we asked for: with no reviewer
	// supplied the kernel prefers its own prior-reviewer provenance over the
	// existing assignee. Verify what actually LANDED, not what was requested
	// — the dispatcher spawns on this value and nothing else.
	if aok, achecked, anames := s.profileSpawnable(ctx, after.Assignee); achecked && !aok {
		return ErrorResult("request-review left ticket %s in 'review' assigned to %q, which is not an installed profile, so the dispatcher will never spawn a reviewer. Installed profiles: %s. Set MCP_REVIEWER_PROFILE (e.g. \"default\") or pass reviewer explicitly, then request again.", in.ID, after.Assignee, strings.Join(anames, ", "))
	}

	// The implementer is the OTHER half of the round trip, and the kernel
	// writes it once, from the ticket row it read inside its own txn. Read
	// it back: a handoff that records null (or someone else) still APPROVES
	// correctly, but a request-changes verdict cannot return the ticket and
	// the reviewer is left with a block. This is the check whose absence
	// produced t_b917c3c6, so it is a hard failure, not a note.
	recorded, found, decoded := reviewHandoffImplementer(events)
	switch {
	case !found:
		return ErrorResult("request-review moved ticket %s to 'review' and assigned %q, but the ticket's event log carries no readable review_requested event, so the recorded implementer cannot be verified. An APPROVE will still complete the card; a request-changes verdict may have nowhere to route. Check ticket_get before relying on the round trip.", in.ID, after.Assignee)
	case !decoded:
		return ErrorResult("request-review moved ticket %s to 'review' and assigned %q, but its newest review_requested payload could not be decoded, so the recorded implementer cannot be verified. An APPROVE will still complete the card; a request-changes verdict may have nowhere to route.", in.ID, after.Assignee)
	case recorded == "":
		return ErrorResult("request-review moved ticket %s to 'review' but the kernel recorded implementer=null on the handoff, so a request-changes verdict cannot return it (request_changes refuses a handoff with no implementer provenance) and the reviewer would be left with a block. The stamp did not reach the row the kernel read: check that the ticket's assignee was %q when the transition ran. Assign the lane label on a ready ticket and request again.", in.ID, label)
	case !strings.EqualFold(recorded, label):
		return ErrorResult("request-review moved ticket %s to 'review' but the kernel recorded implementer=%q, not %q. A request-changes verdict would route to %q. Nothing was undone; re-read the ticket and decide whether that destination is the lane you meant to hand back to.", in.ID, recorded, label, recorded)
	}
	return SuccessResult(TicketRequestReviewOut{
		ID:          after.ID,
		Status:      after.Status,
		Assignee:    after.Assignee,
		Implementer: recorded,
		Note:        note,
	})
}
