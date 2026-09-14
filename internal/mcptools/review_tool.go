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
	"encoding/json"
	"fmt"
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

// The provenance note has TWO branches, because the kernel does two
// different things when no reviewer is supplied and saying otherwise was
// simply untrue (hermes_cli/kanban_db.py:3046-3054, :3060-3069, :3095-3108):
//
//   - FIRST review — the ticket has no `changes_requested` run, so
//     _prior_reviewer returns None, `reviewer` stays None, `assignee_sql`
//     is empty and the UPDATE PRESERVES the ticket's existing assignee.
//     Nothing is reused from any previous round: there is no previous
//     round.
//   - RE-review — a `changes_requested` run exists, and the reviewer that
//     round recorded is what request_review lands, OVERRIDING the existing
//     assignee.
//
// Which one applies is decided from what actually landed (see the note
// assembly in TicketRequestReview), not from what this tool predicted.

// reviewProvenanceFirstNote is the FIRST-review branch: the kernel had no
// prior round to draw on, so it left the assignee alone, and the assignee is
// the value this tool verified against the roster.
const reviewProvenanceFirstNote = " no reviewer supplied, so the kernel kept the ticket's assignee (verified as spawnable)"

// reviewProvenanceFirstUnverifiedNote is the same branch when the roster
// could not be read: the claim "(verified as spawnable)" would then be a
// statement about a check that never ran — the exact kind of false assurance
// this tool exists to avoid (the unverified-roster warning says the probe did
// not answer; a note asserting it did contradicts it).
const reviewProvenanceFirstUnverifiedNote = " no reviewer supplied, so the kernel kept the ticket's assignee (spawnability could NOT be verified against the installed roster)"

// reviewProvenanceReReviewNote is the RE-review branch: the kernel reused
// the reviewer recorded by the previous request-changes round.
const reviewProvenanceReReviewNote = " no reviewer supplied, so the kernel reused the reviewer recorded by the previous request-changes round"

// reviewStrandFix names the recovery that actually works for a review row
// the dispatcher will never spawn. Both call sites are POST-execution: the
// ticket is already in 'review', and this tool's own status preflight
// accepts only ready/running (pinned by TestTRR_StatusPreflight), so "then
// request again" — the advice this message used to end with — is refused by
// the very tool that gives it. A retry the caller cannot perform is worse
// than no advice: it burns a call to learn the row cannot be fixed from the
// MCP surface. Name the two recoveries that do work instead.
func reviewStrandFix(board, id string) string {
	return fmt.Sprintf("Requesting again from this tool would be refused (it accepts ready/running only), so fix the row from outside the MCP surface: reassign it to an installed profile (dashboard or REST), or run `hermes kanban --board %s reopen-review %s` to send it back to ready, then request review again with an explicit reviewer.", board, id)
}

// containsNote reports whether notes already carries note, so a warning
// cannot be appended twice when two probes both fail to answer.
func containsNote(notes []string, note string) bool {
	for _, n := range notes {
		if n == note {
			return true
		}
	}
	return false
}

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

// rawRun is the slice of a task-detail run entry the kernel's re-review
// provenance rule reads: request_review looks for the NEWEST run whose
// outcome is `changes_requested` (kanban_db.py:3095-3103).
type rawRun struct {
	ID      int64  `json:"id"`
	Outcome string `json:"outcome"`
}

// reviewPreflightEnvelope is the task-detail slice the preflight needs in
// ONE call: the task itself (status/assignee) plus the runs and events —
// the two tables the kernel's provenance rule reads. The same envelope
// backs ticket_events, so the pre-check costs no extra round trip.
type reviewPreflightEnvelope struct {
	Task   kanban.TaskSummary `json:"task"`
	Runs   []rawRun           `json:"runs"`
	Events []rawEvent         `json:"events"`
}

// preflightReview fetches the ticket detail the request-review preflight
// needs: the task, its runs and its events. It replaces a bare GetTask
// because the reviewer that LANDS can come from the run history rather
// than from the assignee, and the pre-check has to see that history.
func (s *Server) preflightReview(ctx context.Context, board, id string) (*kanban.TaskSummary, []rawRun, []rawEvent, error) {
	var env reviewPreflightEnvelope
	if err := s.doJSON(ctx, http.MethodGet, "/tasks/"+url.PathEscape(id), url.Values{"board": []string{board}}, nil, &env); err != nil {
		return nil, nil, nil, err
	}
	return &env.Task, env.Runs, env.Events, nil
}

// kernelPriorReviewer mirrors hermes_cli.kanban_db._prior_reviewer over the
// runs and events the ticket detail already carries, so the pre-check can
// verify the value the kernel will LAND rather than the one it was handed.
//
// The kernel's rule (:3046-3054, :3095-3108): with no reviewer argument it
// takes the newest run whose outcome is `changes_requested` (:3095-3103),
// then the NEWEST `changes_requested` event for that run (:3106 ->
// _latest_event, "ORDER BY id DESC LIMIT 1"), reads payload["reviewer"] and
// reassigns the task to it — so the assignee the caller can see is NOT what
// lands on a re-review. No such run means a FIRST review: reviewer stays
// None and the UPDATE preserves the assignee (:3054, :3060-3069).
//
// Three details are load-bearing, because the naive reading of each one
// diverges from the kernel:
//
//   - Only the newest event for the selected run is read. An older
//     `changes_requested` event with a perfectly good reviewer does NOT
//     rescue a newest event whose provenance is missing or blank: the
//     kernel returns False there and request_review refuses ("re-review has
//     no durable reviewer provenance"). Scanning for any usable event would
//     bless a row the kernel is about to refuse.
//   - A non-string reviewer (JSON number, object) and a payload that is not
//     a JSON object are both unusable (isinstance check + _json_dict).
//   - The value that LANDS is canonicalised by request_review
//     (:3048 -> _canonical_assignee -> normalize_profile_name: strip, then
//     lowercase), so what is returned here is that landing form — the raw
//     payload string is not what the dispatcher would spawn on. Profile
//     directories are lowercase on disk, so this is the exact name, not an
//     approximation ("default" included, which the kernel matches
//     case-insensitively).
//
// found is false whenever this tool cannot read the prior reviewer the kernel
// would land (first review, or a run whose newest event is missing/blank/
// malformed — where the kernel refuses outright). The caller then falls back
// to verifying the assignee, which is the value the kernel preserves when it
// does not refuse. Deriving this from the ticket's own history (rather than
// trusting a scripted answer) is the point: a test can now express "the
// kernel will land a name nobody requested" instead of asserting only the
// values this tool chooses.
func kernelPriorReviewer(runs []rawRun, events []rawEvent) (string, bool) {
	var (
		runID   int64
		haveRun bool
	)
	for _, r := range runs {
		if r.Outcome == "changes_requested" && (!haveRun || r.ID > runID) {
			runID, haveRun = r.ID, true
		}
	}
	if !haveRun {
		return "", false
	}
	var newest *rawEvent
	for i := range events {
		if e := &events[i]; e.Kind == "changes_requested" && e.RunID != nil && *e.RunID == runID {
			if newest == nil || e.ID > newest.ID {
				newest = e
			}
		}
	}
	if newest == nil || newest.Payload == nil {
		return "", false
	}
	var p struct {
		Reviewer string `json:"reviewer"`
	}
	if json.Unmarshal(newest.Payload, &p) != nil {
		return "", false
	}
	// normalize_profile_name: strip, then lowercase ("default" is matched
	// case-insensitively and profile directories are lowercase on disk). The
	// value returned IS the value that lands, not the raw payload string.
	name := strings.ToLower(strings.TrimSpace(p.Reviewer))
	if name == "" {
		return "", false
	}
	return name, true
}

// TicketRequestReview implements the ticket_request_review MCP tool:
// validate, fail fast when HERMES_BIN is missing, preflight the ticket
// over REST (ready or running only — the kernel refuses anything else),
// resolve and verify the reviewer that will actually LAND on the row (the
// kernel's prior-reviewer provenance when the ticket has a
// request-changes round behind it, else the current assignee), shell out
// to the hermes CLI, then re-read the ticket, confirm the transition
// actually happened, and return the authoritative state.
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

	ts, runs, events, err := s.preflightReview(ctx, board, in.ID)
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
	// verdict the kernel sets assignee = the implementer, so *substituting*
	// it as the reviewer would make the next round a self-review.
	//
	// Three values matter and they are NOT the same one:
	//   flagReviewer   — what we pass to the CLI. Empty means "omit
	//                   --reviewer" and let the kernel make its own choice.
	//                   Passing the assignee here would substitute it and
	//                   defeat the kernel's provenance.
	//   priorReviewer  — the reviewer the kernel's OWN provenance would
	//                   land, read from the ticket's run/event history when
	//                   no reviewer is supplied and a previous
	//                   request-changes round exists. On a re-review this
	//                   OVERRIDES the assignee (:3046-3054), so it — not
	//                   the assignee — is what will be spawned.
	//   verifyValue   — what will END UP on the review row. Verified in
	//                   every tier: an unspawnable assignee is the strand no
	//                   matter who chose it. Verifying a value is not
	//                   substituting it.
	flagReviewer := strings.TrimSpace(in.Reviewer)
	if flagReviewer == "" {
		flagReviewer = strings.TrimSpace(os.Getenv("MCP_REVIEWER_PROFILE"))
	}
	useProvenance := flagReviewer == ""
	verifyValue := flagReviewer
	priorReviewer := ""
	if useProvenance {
		prior, ok := kernelPriorReviewer(runs, events)
		if ok {
			// The kernel lands this value on a re-review, and it OVERRIDES
			// the assignee — verify the prior reviewer, not the assignee.
			priorReviewer = prior
			verifyValue = prior
		} else {
			verifyValue = strings.TrimSpace(ts.Assignee)
			if verifyValue == "" {
				return ErrorResult("invalid_input: no reviewer resolved and ticket %s is unassigned. The dispatcher never spawns an unassigned review ticket — it would sit in 'review' silently and no reviewer would ever run. Pass reviewer, or set MCP_REVIEWER_PROFILE (e.g. \"default\") on the server.", in.ID)
			}
		}
	}
	notes := []string{reviewNote}
	ok, checked, names := s.profileSpawnable(ctx, verifyValue)
	switch {
	case checked && !ok:
		switch {
		case priorReviewer != "":
			return ErrorResult("invalid_input: no reviewer was supplied, and ticket %s's previous request-changes round recorded reviewer %q — the value the kernel lands on the review row, overriding the current assignee. %q is not an installed profile, so the dispatcher would never spawn a reviewer and the card would sit in 'review' unspawned. Installed profiles: %s. Pass reviewer, or set MCP_REVIEWER_PROFILE (e.g. \"default\") on the server.", in.ID, priorReviewer, priorReviewer, strings.Join(names, ", "))
		case useProvenance:
			return ErrorResult("invalid_input: no reviewer was supplied and ticket %s carries assignee %q, which is not an installed profile. The kernel would preserve that assignee, so the card would sit in 'review' with no reviewer the dispatcher can ever spawn. Installed profiles: %s. Pass reviewer, or set MCP_REVIEWER_PROFILE (e.g. \"default\") on the server.", in.ID, verifyValue, strings.Join(names, ", "))
		default:
			return ErrorResult("invalid_input: reviewer %q is not an installed profile, so the dispatcher could never spawn it and the card would sit in 'review' unclaimed. Installed profiles: %s.", verifyValue, strings.Join(names, ", "))
		}
	case !checked:
		notes = append(notes, reviewUnverifiedNote)
	}

	if _, stderr, err := kanban.RequestReview(ctx, in.ID, board, strings.TrimSpace(in.Summary), flagReviewer, force); err != nil {
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
		return ErrorResult("request-review left ticket %s in 'review' with NO assignee, so the dispatcher will never spawn a reviewer. %s", in.ID, reviewStrandFix(board, in.ID))
	}
	// The row's assignee can differ from what we asked for: with no reviewer
	// supplied the kernel prefers its own prior-reviewer provenance over the
	// existing assignee. Verify what actually LANDED, not what was requested
	// — the dispatcher spawns on this value and nothing else.
	landed := strings.TrimSpace(after.Assignee)
	aok, achecked, anames := s.profileSpawnable(ctx, landed)
	if achecked && !aok {
		return ErrorResult("request-review left ticket %s in 'review' assigned to %q, which is not an installed profile, so the dispatcher will never spawn a reviewer. Installed profiles: %s. %s", in.ID, landed, strings.Join(anames, ", "), reviewStrandFix(board, in.ID))
	}
	// landedVerified: was the value the dispatcher will spawn on actually
	// checked against the roster? The pre-exec probe covers the value this
	// tool predicted; the post-exec probe covers whatever the kernel wrote,
	// which is the only one that counts if they differ. Never claim a
	// verification that did not run — an unverifiable reviewer is exactly the
	// case that strands a card.
	landedVerified := achecked || (checked && strings.EqualFold(landed, verifyValue))
	if !landedVerified && !containsNote(notes, reviewUnverifiedNote) {
		notes = append(notes, reviewUnverifiedNote)
	}
	// The provenance note describes what the kernel DID, decided from the
	// value that actually landed: on a re-review the prior reviewer is what
	// request_review writes (and it can equal a preserved assignee by
	// coincidence), otherwise the assignee was preserved. The old
	// single-branch wording asserted "reused the reviewer from the previous
	// review round" on a FIRST review, where no previous round exists and
	// nothing is reused.
	if useProvenance {
		switch {
		case priorReviewer != "" && strings.EqualFold(landed, priorReviewer):
			notes = append(notes, reviewProvenanceReReviewNote)
		case landedVerified:
			notes = append(notes, reviewProvenanceFirstNote)
		default:
			notes = append(notes, reviewProvenanceFirstUnverifiedNote)
		}
	}
	return SuccessResult(TicketRequestReviewOut{
		ID:       after.ID,
		Status:   after.Status,
		Assignee: after.Assignee,
		Note:     strings.Join(notes, ";"),
	})
}
