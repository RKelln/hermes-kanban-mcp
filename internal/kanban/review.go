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
	// reviewerReStr is anchored so a reviewer value can never smuggle
	// extra argv into the CLI. Reviewer values are profile names.
	reviewerReStr = `^[A-Za-z0-9._-]{1,64}$`

	// maxSummaryBytes bounds the handoff text. It is generous (a real
	// handoff carries refs, scope and verification claims) but finite,
	// so one caller cannot blow past the kernel's argv limits.
	maxSummaryBytes = 16 << 10
)

var reviewerRe = regexp.MustCompile(reviewerReStr)

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
	if reviewer != "" && !reviewerRe.MatchString(reviewer) {
		return "", "", fmt.Errorf("request-review: invalid reviewer %q (must match %s)", reviewer, reviewerReStr)
	}
	if strings.ContainsRune(summary, 0) || strings.ContainsRune(reviewer, 0) {
		return "", "", fmt.Errorf("request-review: value contains a NUL byte")
	}
	if len(summary) > maxSummaryBytes {
		return "", "", fmt.Errorf("request-review: summary too long (%d bytes, max %d)", len(summary), maxSummaryBytes)
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
