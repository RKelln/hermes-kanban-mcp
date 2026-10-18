package kanban

// This file adds the review-request shell-out: the external lane's entry
// into the native review lane (status='review').
//
// Why this exists rather than a REST call: the kanban REST API can only
// reach status='review' through the plugin's request_review projection
// (PATCH tasks/{id} {"status":"review"}), which accepts no reviewer, no
// summary, and no force flag — so it cannot produce a review row the
// dispatcher will actually pick up, and cannot clear the requester's own
// live claim. The kernel-native transition is CLI-only, exactly like the
// ready->running claim. Same shape as Claim: validated argv, scrubbed
// environment, bounded timeout.
//
// Nothing here invokes a shell, reads Hermes state, or touches the kanban
// database directly.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

const (
	// LaneTokenPattern is anchored so a lane value can never smuggle extra
	// argv into the CLI. The same shape covers both values this package
	// passes as profile-shaped tokens: the REVIEWER (an installed profile
	// the dispatcher spawns) and the IMPLEMENTER LABEL (a control-plane
	// label that is deliberately not a profile).
	//
	// The first character may not be a dash: `assign <id> -h` prints help
	// and exits 0, and any other `-x` is swallowed as an option, so a
	// leading dash turns a write into a silent no-op that only the
	// post-write re-read catches. Review F4 on t_b917c3c6.
	//
	// Exported because the MCP layer refuses a bad label/reviewer with a
	// message that names the rule, and a second copy of the pattern in a
	// message is a copy that drifts.
	LaneTokenPattern = `^[A-Za-z0-9._][A-Za-z0-9._-]{0,63}$`

	// MaxReviewSummaryBytes bounds the handoff text. It is generous (a
	// real handoff carries refs, scope and verification claims) but
	// finite, so one caller cannot blow past the kernel's argv limits.
	// Exported because a caller that BUILDS a handoff out of other fields
	// (ticket_complete folds its refs into the summary) has to pre-check:
	// posting its own completion record first and then failing the handoff
	// leaves a card holding a record for a transition that never happened.
	MaxReviewSummaryBytes = 16 << 10
)

var laneTokenRe = regexp.MustCompile(LaneTokenPattern)

// RequestReview moves a ready or running task to 'review' by shelling out
// to `hermes kanban --board <board> request-review <id>` with the handoff
// summary, the reviewer profile, and force as needed.
//
// force exists because the CLI exposes no --expected-run-id flag, so a
// RUNNING task's live claim can only be released with --force. Ownership
// is implied by the lifecycle rather than asserted by a token: the only
// ready->running path this bridge offers is Claim, so a caller holding a
// running ticket claimed it. Callers pass force=true for a running
// ticket and false for a ready one. Do not pass force=true for a ready
// ticket: it would let a caller yank a ticket out from under a worker's
// live claim without any ownership evidence at all.
//
// summary and reviewer are passed in `--flag=value` form so a value that
// begins with a dash can never be parsed as an option. Empty values are
// omitted, which is the CLI's own default. A NUL byte anywhere in either
// is rejected before anything is exec'd (argv cannot carry one).
//
// stdout and stderr are the (4 KiB-capped) CLI streams; err is non-nil on
// argument rejection, missing binary, timeout, or non-zero exit. Callers
// must re-read the ticket for the authoritative state — never parse
// stdout.
func RequestReview(ctx context.Context, id, board, summary, reviewer string, force bool) (stdout, stderr string, err error) {
	if err := validateTarget(id, board); err != nil {
		return "", "", fmt.Errorf("request-review: %w", err)
	}
	if reviewer != "" && !laneTokenRe.MatchString(reviewer) {
		return "", "", fmt.Errorf("request-review: invalid reviewer %q (must match %s)", reviewer, LaneTokenPattern)
	}
	if strings.ContainsRune(summary, 0) || strings.ContainsRune(reviewer, 0) {
		return "", "", fmt.Errorf("request-review: value contains a NUL byte")
	}
	if len(summary) > MaxReviewSummaryBytes {
		return "", "", fmt.Errorf("request-review: summary too long (%d bytes, max %d)", len(summary), MaxReviewSummaryBytes)
	}
	bin, ok := resolveBin()
	if !ok {
		return "", "", cliNotFoundError("request-review", bin)
	}
	argv := []string{"kanban", "--board", board, "request-review", id}
	if summary != "" {
		argv = append(argv, "--summary="+summary)
	}
	if reviewer != "" {
		argv = append(argv, "--reviewer="+reviewer)
	}
	if force {
		argv = append(argv, "--force")
	}
	return run(ctx, bin, argv, "request-review")
}

// ValidLaneToken reports whether value is a lane token this package will
// put in argv (a reviewer profile or an implementer label). Exported so
// the MCP layer can reject a bad label with a message that names the
// rule before any network or CLI call.
func ValidLaneToken(value string) bool {
	return laneTokenRe.MatchString(value)
}

// AssignLabel stamps label on the ticket's assignee field, by shelling out
// to `hermes kanban --board <board> assign <id> <label>` — or
// `reassign <id> <label> --reclaim --reason=<reason>` when reclaim is set,
// which is the only form that works on a RUNNING card (assign_task refuses
// to reassign a running, claimed task).
//
// Why this exists: request_review records the handoff's implementer from
// the CURRENT RUN's profile, falling back to the ticket's assignee only
// when that assignee differs from the reviewer it resolved
// (kanban_db.request_review: `if implementer is None and trow["assignee"]
// != reviewer: implementer = trow["assignee"]`). An external-lane claim
// writes no run profile — `hermes kanban claim` has no profile flag — and
// the lane does not assign its cards, so both sources were empty, the
// handoff recorded implementer=null, and request_changes REFUSED with
// "review handoff has no valid implementer provenance". The reviewer's
// only remaining exit is a block. Stamping a durable label before the
// handoff is the kernel's own designed fallback: it is the documented
// provisioning rule, not a workaround.
//
// Two consequences a caller should know and neither is optional:
//
//   - The stamp MOVES the assignee, so it must run AFTER the reviewer is
//     settled. request_review resolves a missing reviewer from prior
//     re-review provenance (never from the assignee), so a reviewer that
//     depended on the old assignee cannot survive this write.
//   - `assign` resets consecutive_failures and last_failure_error when the
//     assignee changes (kanban_db.assign_task), so every review round
//     clears the card's failure streak. That is the kernel's behaviour,
//     not this package's, and it is harmless for a card that completes —
//     do not read the streak as "this card never failed" after a round.
//
// The label must NOT be an installed profile. The kernel lands the card
// back at `ready` with assignee = implementer, and the dispatcher claims
// ready cards whose assignee is an on-disk profile, so a profile here
// would put an agent worker on the card when changes come back. Callers
// verify that against the roster; this package only enforces the token
// shape.
//
// reclaim releases the live claim as part of the write. That is the same
// authority `request-review --force` exercises one step earlier, so it
// belongs to a caller that has already established claim ownership.
func AssignLabel(ctx context.Context, id, board, label, reason string, reclaim bool) (stdout, stderr string, err error) {
	if err := validateTarget(id, board); err != nil {
		return "", "", fmt.Errorf("assign: %w", err)
	}
	if !ValidLaneToken(label) {
		return "", "", fmt.Errorf("assign: invalid label %q (must match %s)", label, LaneTokenPattern)
	}
	if strings.ContainsRune(label, 0) || strings.ContainsRune(reason, 0) {
		return "", "", fmt.Errorf("assign: value contains a NUL byte")
	}
	if len(reason) > MaxReviewSummaryBytes {
		return "", "", fmt.Errorf("assign: reason too long (%d bytes, max %d)", len(reason), MaxReviewSummaryBytes)
	}
	bin, ok := resolveBin()
	if !ok {
		return "", "", cliNotFoundError("assign", bin)
	}
	var argv []string
	if reclaim {
		argv = []string{"kanban", "--board", board, "reassign", id, label, "--reclaim"}
		if reason != "" {
			argv = append(argv, "--reason="+reason)
		}
	} else {
		argv = []string{"kanban", "--board", board, "assign", id, label}
	}
	return run(ctx, bin, argv, "assign")
}
