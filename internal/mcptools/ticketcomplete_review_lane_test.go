package mcptools

// ticketcomplete_review_lane_test.go is the ticket_complete review-lane
// suite: the regression guard for the dead `review-required:` BLOCK
// convention (t_affa85ca on board `hermes-kanban-mcp`).
//
// Why this file exists. ticket_complete's review-gated path used to PATCH
// status=blocked with reason "review-required: …". That block's only
// consumer was the review-sweeper cron, retired 2026-09-14; its
// successor lane_return.py deliberately ignores unmarked worker-phase
// `review-required:` blocks ("belong to whatever workflow wrote them").
// Net effect: every bridge-driven completion landed on a card nothing
// would ever pick up. t_ac950fce (board `togather`) sat that way with no
// verdict and no reviewer.
//
// The fix routes the completion through the native review lane instead:
// post the comment, then shell out to `hermes kanban request-review`,
// which sets status='review' and is what the dispatcher actually claims
// as a REVIEW run.
//
// Load-bearing cases, in the order they matter:
//   - TestTCRL_ReviewNeverBlocks and TestTCRL_ReviewHandsOffToNativeLane:
//     the block is gone and the CLI handoff happens, asserted as
//     negatives too (no `review-required` anywhere in the wire traffic or
//     argv) so a future refactor cannot quietly restore the dead path.
//   - TestTCRL_ReviewHandoffSurfacesRefusal: when the handoff refuses,
//     the caller sees the refusal and no false success is reported.
//
// Prefix TestTCRL_ is unique to this file (the suite-per-file convention
// documented in ticketcomplete_test.go) so parallel edits never collide.

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/RKelln/hermes-kanban-mcp/internal/kanban"
)

// runningThenReview scripts the GET bodies the completion handoff needs.
// Four reads happen in order: the claim guard, request-review's own
// preflight, the post-stamp read (t_b917c3c6: the lane label lands on the
// card before the handoff, and a running card is reclaimed to do it), and
// the post-transition read. The preflight requires ready|running, so the
// second body must still be running; the third must carry the label the
// stamp wrote; the fourth must be the review row the post-condition
// asserts, with the review_requested event the tool reads the recorded
// implementer from.
func runningThenReview(id, reviewer, label string) []string {
	return []string{
		`{"task":{"id":"` + id + `","title":"T","status":"running"}}`,
		`{"task":{"id":"` + id + `","title":"T","status":"running"}}`,
		`{"task":{"id":"` + id + `","title":"T","status":"ready","assignee":"` + label + `"}}`,
		`{"task":{"id":"` + id + `","title":"T","status":"review","assignee":"` + reviewer + `"},` +
			`"events":[{"id":9,"kind":"review_requested","payload":{"implementer":"` + label + `","reviewer":"` + reviewer + `"}}]}`,
	}
}

// hasReviewRequiredBlock reports whether any recorded backend request or
// CLI invocation carries the retired convention's marker.
func hasReviewRequiredBlock(backend *claimBackend, argv []string) bool {
	for _, rr := range backend.snapshot() {
		if strings.Contains(rr.body, "review-required") || strings.Contains(rr.body, "blocked") {
			return true
		}
	}
	for _, line := range argv {
		if strings.Contains(line, "review-required") || strings.Contains(line, "block") {
			return true
		}
	}
	return false
}

// TestTCRL_ReviewNeverBlocks is the regression guard: the default review
// path must not write the retired `review-required:` block. A card left
// blocked that way has no consumer at all, which is how t_ac950fce sat
// with no verdict and no reviewer.
func TestTCRL_ReviewNeverBlocks(t *testing.T) {
	os.Unsetenv("MCP_COMPLETE_MODE")
	os.Unsetenv("MCP_ALLOW_SKIP_CLAIM")
	t.Setenv("MCP_REVIEWER_PROFILE", "default")
	label := withClaimWorker(t)
	readArgv := argvLog(t)

	backend := newClaimBackend()
	backend.orders["t_x1"] = runningThenReview("t_x1", "default", label)
	s := newClaimToolServer(t, backend)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board: testBoard, ID: "t_x1", Summary: "shipped the widget",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	if hasReviewRequiredBlock(backend, readArgv()) {
		t.Errorf("the review path wrote the retired review-required BLOCK: reqs=%v argv=%v",
			backend.snapshot(), readArgv())
	}
}

// TestTCRL_ReviewHandsOffToNativeLane asserts the positive: the handoff
// is the native lane's request-review, with the reviewer resolved from
// the server default, and the reported final status is 'review'.
func TestTCRL_ReviewHandsOffToNativeLane(t *testing.T) {
	os.Unsetenv("MCP_COMPLETE_MODE")
	os.Unsetenv("MCP_ALLOW_SKIP_CLAIM")
	t.Setenv("MCP_REVIEWER_PROFILE", "default")
	label := withClaimWorker(t)
	readArgv := argvLog(t)

	backend := newClaimBackend()
	backend.orders["t_x1"] = runningThenReview("t_x1", "default", label)
	s := newClaimToolServer(t, backend)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board: testBoard, ID: "t_x1", Summary: "shipped the widget",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}

	// The reviewer handoff is the CLI call; a PATCH to any status would
	// mean the old block path survived.
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0 (the review transition is CLI-only): %v", n, backend.patches())
	}
	// Two CLI writes: the implementer stamp (which is also what releases
	// the running ticket's claim, via reassign --reclaim) and the handoff.
	argv := readArgv()
	if len(argv) != 2 {
		t.Fatalf("argv lines = %v, want the implementer stamp then the handoff", argv)
	}
	if !strings.Contains(argv[0], "reassign t_x1 "+label) || !strings.Contains(argv[0], "--reclaim") {
		t.Errorf("argv[0] = %q, want the implementer stamp reclaiming the live claim", argv[0])
	}
	h := handoffArgv(t, argv)
	if !strings.Contains(h, "request-review t_x1") {
		t.Errorf("argv = %q, want the native request-review handoff", h)
	}
	if !strings.Contains(h, "--reviewer=default") {
		t.Errorf("argv = %q, want --reviewer=default from MCP_REVIEWER_PROFILE", h)
	}
	// The claim-release authority is exercised ONCE, by the stamp. The
	// handoff must not also force: the claim is already gone, and a second
	// override would assert ownership nobody needs.
	if strings.Contains(h, "--force") {
		t.Errorf("argv = %q, must not force after the stamp reclaimed the claim", h)
	}

	out := decodeCompleteOut(t, res)
	if out["final_status"] != "review" {
		t.Errorf("final_status = %v, want review", out["final_status"])
	}
	if out["review_required"] != true {
		t.Errorf("review_required = %v, want true", out["review_required"])
	}

	// The comment must land before the handoff: the reviewer reads the
	// card, and a handoff with no handoff-comment is a reviewer with
	// nothing to review against.
	var commentIdx, firstReq int = -1, -1
	for i, rr := range backend.snapshot() {
		if commentIdx < 0 && rr.method == http.MethodPost && strings.HasSuffix(rr.path, "/comments") {
			commentIdx = i
		}
		if firstReq < 0 {
			firstReq = i
		}
	}
	if commentIdx < 0 {
		t.Fatalf("no comment was posted before the handoff: %v", backend.snapshot())
	}
	if commentIdx <= firstReq {
		t.Errorf("comment index %d not after the guard read %d", commentIdx, firstReq)
	}
}

// TestTCRL_ReviewHandoffSurfacesRefusal: when the handoff cannot be made
// (the reviewer is not an installed profile), the tool must report the
// refusal rather than a success. A silent success here is the original
// bug in a new coat: the caller believes the card moved.
func TestTCRL_ReviewHandoffSurfacesRefusal(t *testing.T) {
	os.Unsetenv("MCP_COMPLETE_MODE")
	os.Unsetenv("MCP_ALLOW_SKIP_CLAIM")
	t.Setenv("MCP_REVIEWER_PROFILE", "not-a-profile")
	readArgv := argvLog(t)

	backend := newClaimBackend()
	backend.orders["t_x1"] = runningThenReview("t_x1", "default", "opencode-remote")
	s := newClaimToolServer(t, backend)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board: testBoard, ID: "t_x1", Summary: "shipped the widget",
	})
	if !res.IsError {
		t.Fatalf("expected a refusal, got success: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "not an installed profile") {
		t.Errorf("refusal = %q, want it to name the uninstalled reviewer profile", res.Content[0].Text)
	}
	assertNoCLI(t, readArgv())
}

// TestTCRL_OverLongSummaryRefusesBeforeWriting pins the ORDER of the refusal
// (reviewer finding F3 on t_affa85ca): a summary past the CLI's handoff bound
// must fail before the completion comment is posted. Posting first leaves a
// RUNNING card holding a completion comment for a transition that never
// happened, which a human then has to unpick.
func TestTCRL_OverLongSummaryRefusesBeforeWriting(t *testing.T) {
	os.Unsetenv("MCP_COMPLETE_MODE")
	os.Unsetenv("MCP_ALLOW_SKIP_CLAIM")
	t.Setenv("MCP_REVIEWER_PROFILE", "default")
	readArgv := argvLog(t)

	backend := newClaimBackend()
	backend.orders["t_x1"] = runningThenReview("t_x1", "default", "opencode-remote")
	s := newClaimToolServer(t, backend)

	res := s.TicketComplete(context.Background(), TicketCompleteInput{
		Board:   testBoard,
		ID:      "t_x1",
		Summary: strings.Repeat("s", kanban.MaxReviewSummaryBytes+1),
	})
	if !res.IsError {
		t.Fatalf("expected a refusal past the handoff bound, got: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "too long") {
		t.Errorf("refusal = %q, want it to name the summary bound", res.Content[0].Text)
	}
	for _, rr := range backend.snapshot() {
		if rr.method != http.MethodGet {
			t.Errorf("the refusal still wrote to the backend: %s %s body=%s", rr.method, rr.path, rr.body)
		}
	}
	assertNoCLI(t, readArgv())
}
