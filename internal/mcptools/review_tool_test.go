package mcptools

// review_tool_test.go exercises TicketRequestReview against the scripted
// kanban REST backend (which also serves GET /profiles) and the fake
// hermes CLI.
//
// The load-bearing cases, in the order they matter:
//   - TestTRR_StrandGuard and TestTRR_NonProfileReviewerRefused: an
//     unassigned OR non-profile review row is skipped by the dispatcher
//     every tick, forever (t_44d19d72 / t_371b7d27, 5 days silent,
//     t_124ab31f). Both must REFUSE, asserted as negatives (no CLI
//     invocation, no status write).
//   - TestTRR_ImplementerStampedThenRecorded: the handoff's implementer
//     is the OTHER half of the round trip. The kernel records it once, at
//     request time, from the assignee — so the tool stamps the lane label
//     first and then READS BACK what the kernel recorded. measured live
//     2026-10-18 (t_a6e3ba60): a profile-less claim plus no assignee wrote
//     implementer=null and the reviewer's only exit was a block.
//   - TestTRR_ImplementerMustNotBeProfile: a label that collides with an
//     installed profile makes the dispatcher spawn a worker when changes
//     come back — the inverse strand, and the reason the label is checked
//     in the opposite direction from the reviewer.
//   - TestTRR_NoReviewerReusesPriorRoundReviewer / TestTRR_NoReviewer
//     NoProvenanceRefusedBeforeStamp: with no reviewer argument and no
//     MCP_REVIEWER_PROFILE the tool resolves the reviewer from the
//     ticket's last changes-requested round and refuses (before the stamp)
//     when there is none. The kernel's own fallback is the row's existing
//     assignee, which the stamp has just moved to the lane label, so
//     "verify the existing assignee as the reviewer" verified a value the
//     handoff itself destroyed — the F1 defect in the round-2 review of
//     t_b917c3c6.
//   - TestTRR_RunningRequiresForce / TestTRR_ForceRejectedOnReady: the
//     bridge cannot verify claim ownership, so releasing a live claim is
//     an explicit caller decision and never automatic.
//   - TestTRR_Postcondition*: never report "reviewer dispatched" for a
//     ticket that did not reach a dispatchable review row, and never
//     report a handoff whose implementer cannot route back.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// argvLog installs the fake CLI and returns a function that reads back
// the argv lines the fake recorded. Empty when the CLI was never run.
func argvLog(t *testing.T) func() []string {
	t.Helper()
	installFakeBin(t)
	path := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("HERMES_FAKE_ARGV_LOG", path)
	return func() []string {
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			t.Fatalf("read argv log: %v", err)
		}
		return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	}
}

// noEnvClaimWorker makes sure the implementer knob is absent so tests
// observe the argument/knob resolution, not ambient config.
func noEnvClaimWorker(t *testing.T) {
	t.Helper()
	t.Setenv("MCP_CLAIM_WORKER", "")
}

// noEnvReviewer makes sure the server-default knob is absent so tests
// observe the argument/default resolution, not ambient config.
func noEnvReviewer(t *testing.T) {
	t.Helper()
	t.Setenv("MCP_REVIEWER_PROFILE", "")
}

// withClaimWorker sets the server-side implementer label (this install's
// value) and returns it.
func withClaimWorker(t *testing.T) string {
	t.Helper()
	t.Setenv("MCP_CLAIM_WORKER", "opencode-remote")
	return "opencode-remote"
}

// reviewRow renders the post-transition GET body: the landed review row
// plus the event log the tool reads the recorded implementer from.
// implementer is a raw JSON value so a test can express the null the
// kernel writes when it has no provenance.
func reviewRow(id, assignee, implementer string) string {
	return fmt.Sprintf(
		`{"task":{"id":%q,"title":"T","status":"review","assignee":%q},`+
			`"events":[{"id":9,"kind":"review_requested","payload":{"implementer":%s,"reviewer":%q}}]}`,
		id, assignee, implementer, assignee)
}

// stampedRow renders the post-stamp GET body: the ticket after the
// implementer label landed but before the handoff.
func stampedRow(id, status, label string) string {
	return fmt.Sprintf(`{"task":{"id":%q,"title":"T","status":%q,"assignee":%q}}`, id, status, label)
}

// assertNoCLI asserts the CLI was never invoked (the refusal paths must
// all refuse BEFORE exec).
func assertNoCLI(t *testing.T, argv []string) {
	t.Helper()
	if len(argv) != 0 {
		t.Errorf("argv = %v, want no CLI invocation", argv)
	}
}

// TestTRR_ImplementerStampedThenRecorded is the whole point of the card:
// an unassigned ready ticket gets the lane label BEFORE the handoff, the
// handoff carries --reviewer, and the tool reports the implementer the
// kernel actually recorded.
func TestTRR_ImplementerStampedThenRecorded(t *testing.T) {
	noEnvReviewer(t)
	label := withClaimWorker(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"ready"}}`,
		stampedRow("t_x1", "ready", label),
		reviewRow("t_x1", "alice", fmt.Sprintf("%q", label)),
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "shipped it", Reviewer: "alice",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	if rendered := renderedSize(t, res); rendered > MaxToolResultBytes {
		t.Errorf("rendered result %d bytes > %d", rendered, MaxToolResultBytes)
	}
	out := decodeOut[TicketRequestReviewOut](t, res)
	if out.ID != "t_x1" || out.Status != "review" || out.Assignee != "alice" {
		t.Errorf("out = %+v, want id t_x1 status review assignee alice", out)
	}
	if out.Implementer != label {
		t.Errorf("implementer = %q, want %q (the value the kernel recorded)", out.Implementer, label)
	}
	for _, want := range []string{reviewNote, "implementer=" + label} {
		if !strings.Contains(out.Note, want) {
			t.Errorf("note = %q, want it to contain %q", out.Note, want)
		}
	}
	// preflight + post-stamp re-read + post-transition read. The event log
	// rides the last one, so verification costs no extra fetch.
	if n := backend.getCount("t_x1"); n != 3 {
		t.Errorf("GET count = %d, want 3 (preflight, post-stamp, post-transition)", n)
	}
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0", n)
	}
	// reviewer verification, implementer verification, landed-assignee
	// verification.
	if n := backend.profileProbes(); n != 3 {
		t.Errorf("profile probes = %d, want 3", n)
	}
	argv := readArgv()
	if len(argv) != 2 {
		t.Fatalf("argv lines = %v, want the stamp then the handoff", argv)
	}
	if !strings.Contains(argv[0], "assign t_x1 "+label) {
		t.Errorf("argv[0] = %q, want the implementer stamp first", argv[0])
	}
	if strings.Contains(argv[0], "reassign") || strings.Contains(argv[0], "--reclaim") {
		t.Errorf("argv[0] = %q, a READY ticket must not be reclaimed", argv[0])
	}
	if !strings.Contains(argv[1], "request-review t_x1") || !strings.Contains(argv[1], "--summary=shipped it") {
		t.Errorf("argv[1] = %q, want request-review with the summary", argv[1])
	}
	if !strings.Contains(argv[1], "--reviewer=alice") {
		t.Errorf("argv[1] = %q, want --reviewer=alice", argv[1])
	}
	if strings.Contains(argv[1], "--force") {
		t.Errorf("argv[1] = %q, must NOT force a ready ticket", argv[1])
	}
}

// The knob is the only implicit source, and its absence is a refusal that
// names it. A silently invented default would become every lane's
// identity, and an unnamed handoff is the defect this card exists to end.
func TestTRR_ImplementerLabelRequired(t *testing.T) {
	noEnvReviewer(t)
	noEnvClaimWorker(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{`{"task":{"id":"t_x1","title":"T","status":"ready"}}`}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s", Reviewer: "alice",
	})
	if !res.IsError {
		t.Fatalf("expected IsError with no implementer source, got success: %s", res.Content[0].Text)
	}
	for _, want := range []string{"no implementer label", "MCP_CLAIM_WORKER", "request-changes"} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("error = %q, want it to mention %q", res.Content[0].Text, want)
		}
	}
	assertNoCLI(t, readArgv())
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0", n)
	}
}

// The inverse strand: a label that IS a profile makes the dispatcher spawn
// a worker on the card when changes come back. Refuse in the opposite
// direction from the reviewer check.
func TestTRR_ImplementerLabelMustNotBeProfile(t *testing.T) {
	noEnvReviewer(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{`{"task":{"id":"t_x1","title":"T","status":"ready"}}`}
	s := newClaimToolServer(t, backend)

	// "alice" is in the scripted roster. Case-insensitive, because the
	// kernel canonicalises the assignee before writing it.
	for _, label := range []string{"alice", "ALICE"} {
		res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
			ID: "t_x1", Board: testBoard, Summary: "s", Reviewer: "bob", Worker: label,
		})
		if !res.IsError {
			t.Fatalf("label %q: expected IsError for a profile label, got success: %s", label, res.Content[0].Text)
		}
		for _, want := range []string{"IS an installed profile", "MCP_CLAIM_WORKER", "spawns a worker"} {
			if !strings.Contains(res.Content[0].Text, want) {
				t.Errorf("label %q: error = %q, want it to mention %q", label, res.Content[0].Text, want)
			}
		}
	}
	assertNoCLI(t, readArgv())
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0", n)
	}
}

// The kernel only falls back to the assignee when it DIFFERS from the
// reviewer, so a label equal to the reviewer still records null.
func TestTRR_ImplementerLabelMustDifferFromReviewer(t *testing.T) {
	noEnvReviewer(t)
	noEnvClaimWorker(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{`{"task":{"id":"t_x1","title":"T","status":"ready"}}`}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s", Reviewer: "bob", Worker: "bob",
	})
	if !res.IsError {
		t.Fatalf("expected IsError when the label is the reviewer, got success: %s", res.Content[0].Text)
	}
	for _, want := range []string{"also the reviewer", "implementer=null"} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("error = %q, want it to mention %q", res.Content[0].Text, want)
		}
	}
	assertNoCLI(t, readArgv())
}

// An explicit worker argument is the first source and beats the knob.
func TestTRR_WorkerArgumentBeatsKnob(t *testing.T) {
	noEnvReviewer(t)
	withClaimWorker(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"ready"}}`,
		stampedRow("t_x1", "ready", "framework-lane"),
		reviewRow("t_x1", "alice", `"framework-lane"`),
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s", Reviewer: "alice", Worker: "framework-lane",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	out := decodeOut[TicketRequestReviewOut](t, res)
	if out.Implementer != "framework-lane" {
		t.Errorf("implementer = %q, want framework-lane (the argument, not the knob)", out.Implementer)
	}
	// argv[0] is the stamp carrying the ARGUMENT's label, argv[1] the
	// handoff. A READY card, so no --reclaim.
	argv := readArgv()
	if len(argv) != 2 || !strings.Contains(argv[0], "assign t_x1 framework-lane") {
		t.Fatalf("argv = %v, want the stamp (assign t_x1 framework-lane) then the handoff", argv)
	}
	if strings.Contains(argv[0], "--reclaim") {
		t.Errorf("argv[0] = %q, a READY ticket must not be reclaimed", argv[0])
	}
}

// A card already carrying the label must not be written again: on a
// running card that write would reclaim a live claim for nothing.
func TestTRR_ImplementerAlreadyStamped(t *testing.T) {
	noEnvReviewer(t)
	label := withClaimWorker(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		fmt.Sprintf(`{"task":{"id":"t_x1","title":"T","status":"ready","assignee":%q}}`, label),
		reviewRow("t_x1", "alice", fmt.Sprintf("%q", label)),
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s", Reviewer: "alice",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	if n := backend.getCount("t_x1"); n != 2 {
		t.Errorf("GET count = %d, want 2 (no post-stamp re-read when nothing was written)", n)
	}
	argv := readArgv()
	if len(argv) != 1 || !strings.Contains(argv[0], "request-review t_x1") {
		t.Fatalf("argv = %v, want exactly one invocation (the handoff)", argv)
	}
}

// F2 regression: a running ticket holds a live claim the bridge cannot
// attribute, so the caller must assert force explicitly.
func TestTRR_RunningRequiresForce(t *testing.T) {
	noEnvReviewer(t)
	label := withClaimWorker(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"running","assignee":"other-profile","claim_lock":"holder-1","claim_expires":9999999999}}`,
		reviewRow("t_x1", "alice", fmt.Sprintf("%q", label)),
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "shipped it", Reviewer: "alice",
	})
	if !res.IsError {
		t.Fatalf("expected an IsError requiring explicit force, got success: %s", res.Content[0].Text)
	}
	for _, want := range []string{"force: true", "live claim", "other-profile"} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("error = %q, want it to mention %q", res.Content[0].Text, want)
		}
	}
	assertNoCLI(t, readArgv())

	// With the explicit assertion the stamp reclaims the claim and the
	// handoff then needs no --force: the claim is already gone, and
	// passing --force would assert an override nobody needs.
	// Fresh backend: the scripted orders repeat their last body, so reusing
	// this one would serve "review" to the second preflight.
	backend2 := newClaimBackend()
	backend2.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"running","assignee":"other-profile","claim_lock":"holder-1","claim_expires":9999999999}}`,
		stampedRow("t_x1", "ready", label),
		reviewRow("t_x1", "alice", fmt.Sprintf("%q", label)),
	}
	readArgv2 := argvLog(t)
	s2 := newClaimToolServer(t, backend2)
	res = s2.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "shipped it", Reviewer: "alice", Force: true,
	})
	if res.IsError {
		t.Fatalf("expected success with force, got IsError: %s", res.Content[0].Text)
	}
	argv := readArgv2()
	if len(argv) != 2 {
		t.Fatalf("argv = %v, want the reclaim stamp then the handoff", argv)
	}
	if !strings.Contains(argv[0], "reassign t_x1 "+label) || !strings.Contains(argv[0], "--reclaim") {
		t.Errorf("argv[0] = %q, want reassign --reclaim on a running ticket", argv[0])
	}
	if strings.Contains(argv[1], "--force") {
		t.Errorf("argv[1] = %q, must not force once the stamp reclaimed the claim", argv[1])
	}
}

// F2 regression, other direction: force on a ready ticket is a
// misunderstanding of the lifecycle and must not be silently ignored.
func TestTRR_ForceRejectedOnReady(t *testing.T) {
	noEnvReviewer(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{`{"task":{"id":"t_x1","title":"T","status":"ready"}}`}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s", Reviewer: "alice", Force: true,
	})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "only for a RUNNING ticket") {
		t.Errorf("got %q, want an IsError about force-on-ready", res.Content[0].Text)
	}
	assertNoCLI(t, readArgv())
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0", n)
	}
}

// F1 regression (HIGH): a reviewer that is not an installed profile parks
// the card in 'review' forever. The tool must verify against the roster
// and refuse.
func TestTRR_NonProfileReviewerRefused(t *testing.T) {
	noEnvReviewer(t)
	withClaimWorker(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{`{"task":{"id":"t_x1","title":"T","status":"ready"}}`}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s", Reviewer: "not-a-profile",
	})
	if !res.IsError {
		t.Fatalf("expected IsError for a non-profile reviewer, got success: %s", res.Content[0].Text)
	}
	for _, want := range []string{"not an installed profile", "not-a-profile", "alice"} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("error = %q, want it to mention %q", res.Content[0].Text, want)
		}
	}
	assertNoCLI(t, readArgv())
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0", n)
	}

	// Same refusal via the env default: a stale/typo'd MCP_REVIEWER_PROFILE
	// is the realistic production trigger.
	t.Setenv("MCP_REVIEWER_PROFILE", "ghost")
	res = s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s",
	})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "not an installed profile") {
		t.Errorf("got %q, want the same refusal for a stale MCP_REVIEWER_PROFILE", res.Content[0].Text)
	}
	assertNoCLI(t, readArgv())
}

// The roster is the oracle for BOTH directions: spawnable for the
// reviewer, not-spawnable for the implementer label. When it cannot be
// read, neither value can be checked, and a handoff that cannot be
// checked is refused rather than risked — the implementer half fails to a
// spawned worker, which is silent, so there is no "announce and proceed"
// version of this branch.
func TestTRR_UnverifiedRosterRefused(t *testing.T) {
	noEnvReviewer(t)
	withClaimWorker(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.profiles = nil // /profiles 404s
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"ready"}}`,
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s", Reviewer: "alice",
	})
	if !res.IsError {
		t.Fatalf("expected IsError when the roster is unreadable, got success: %s", res.Content[0].Text)
	}
	for _, want := range []string{"could not read the installed profile roster", "implementer label"} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("error = %q, want it to mention %q", res.Content[0].Text, want)
		}
	}
	assertNoCLI(t, readArgv())
}

// F1 (review, round 2) — the omit-reviewer path, rewritten against what
// the kernel actually does.
//
// The old version of this test asserted that the tool omits --reviewer and
// leaves "the kernel's own choice" intact, then scripted the post row with
// the ticket's PRE-STAMP assignee. With no --reviewer the kernel resolves an
// actual re-review from the ticket's latest changes-requested round, but on
// a first review it keeps whatever assignee the row already carries — and
// the implementer stamp has moved that to the lane label by then. So the
// fixture described a row the kernel can only produce when nothing was
// stamped, and the case it existed to protect (round 2, a card that came
// back from a verdict) was the one that could not run.
//
// The reusable case: a card whose last round was changes-requested carries
// assignee = the lane label and the reviewer on its changes_requested
// event. The tool reuses THAT reviewer, verifies it, and passes it
// explicitly — never the assignee, which is the label.
func TestTRR_NoReviewerReusesPriorRoundReviewer(t *testing.T) {
	noEnvReviewer(t)
	label := withClaimWorker(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	priorRound := `{"task":{"id":"t_x1","title":"T","status":"ready","assignee":"opencode-remote"},` +
		`"events":[{"id":4,"kind":"review_requested","payload":{"implementer":"opencode-remote","reviewer":"bob"}},` +
		`{"id":5,"kind":"changes_requested","payload":{"reason":"fix it","implementer":"opencode-remote","reviewer":"bob","status":"ready"}}]}`
	backend.orders["t_x1"] = []string{
		priorRound, // preflight
		priorRound, // event log for the provenance lookup
		reviewRow("t_x1", "bob", fmt.Sprintf("%q", label)), // post-transition
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	out := decodeOut[TicketRequestReviewOut](t, res)
	if out.Assignee != "bob" {
		t.Errorf("assignee = %q, want bob (the prior round's reviewer)", out.Assignee)
	}
	if out.Implementer != label {
		t.Errorf("implementer = %q, want %q", out.Implementer, label)
	}
	if !strings.Contains(out.Note, "changes-requested round") {
		t.Errorf("note = %q, want it to name where the reviewer came from", out.Note)
	}
	argv := readArgv()
	// The label is already on the card, so there is nothing to stamp.
	if len(argv) != 1 {
		t.Fatalf("argv = %v, want only the handoff", argv)
	}
	if !strings.Contains(argv[0], "request-review t_x1") || !strings.Contains(argv[0], "--reviewer=bob") {
		t.Errorf("argv[0] = %q, want the handoff with --reviewer=bob", argv[0])
	}
	if strings.Contains(argv[0], "--reviewer="+label) {
		t.Errorf("argv[0] = %q, substituted the lane label as the reviewer", argv[0])
	}
}

// F1's core, first round: no reviewer argument, no server default, and no
// earlier verdict to reuse one from. The old code fell back to the ticket's
// assignee, verified it, stamped OVER it, and then handed off with no
// --reviewer — at which point the kernel kept the stamped assignee, so the
// review row landed on the lane label and stranded. Nothing may be written
// before this refusal: no stamp, no PATCH, no CLI.
func TestTRR_NoReviewerNoProvenanceRefusedBeforeStamp(t *testing.T) {
	noEnvReviewer(t)
	withClaimWorker(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"running","assignee":"default"},"events":[]}`,
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s", Force: true,
	})
	if !res.IsError {
		t.Fatalf("expected IsError with no reviewer source, got success: %s", res.Content[0].Text)
	}
	for _, want := range []string{"no reviewer resolved", "MCP_REVIEWER_PROFILE", "keeps whatever assignee"} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("error = %q, want it to mention %q", res.Content[0].Text, want)
		}
	}
	assertNoCLI(t, readArgv())
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0", n)
	}
}

// The reused reviewer must still be a profile the dispatcher can spawn: a
// reviewer profile deleted since the last round would park the card in
// 'review' with nobody to run it. Refuse before the stamp, naming the
// provenance so the operator knows which value is stale.
func TestTRR_NoReviewerProvenanceNotSpawnableRefused(t *testing.T) {
	noEnvReviewer(t)
	withClaimWorker(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"ready","assignee":"opencode-remote"},` +
			`"events":[{"id":5,"kind":"changes_requested","payload":{"reason":"r","implementer":"opencode-remote","reviewer":"ryan","status":"ready"}}]}`,
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s",
	})
	if !res.IsError {
		t.Fatalf("expected IsError for a stale prior reviewer, got success: %s", res.Content[0].Text)
	}
	for _, want := range []string{"ryan", "not an installed profile", "MCP_REVIEWER_PROFILE"} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("error = %q, want it to mention %q", res.Content[0].Text, want)
		}
	}
	assertNoCLI(t, readArgv())
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0", n)
	}
}

// The invariant F1 broke, asserted directly: every successful handoff
// carries an explicit --reviewer, so the review row's assignee is always a
// value this tool verified. A handoff that omits the flag hands the choice
// to the kernel, and then nothing here can promise what the row carries —
// which is how a review row ended up assigned to the lane label.
func TestTRR_EverySuccessPathPassesReviewer(t *testing.T) {
	cases := []struct {
		name    string
		review  string
		env     string
		bodies  []string
		wantRev string
	}{
		{
			name:   "from the argument",
			review: "alice",
			bodies: []string{
				`{"task":{"id":"t_x1","title":"T","status":"ready","assignee":"opencode-remote"}}`,
				reviewRow("t_x1", "alice", `"opencode-remote"`),
			},
			wantRev: "alice",
		},
		{
			name: "from MCP_REVIEWER_PROFILE",
			env:  "bob",
			bodies: []string{
				`{"task":{"id":"t_x1","title":"T","status":"ready","assignee":"opencode-remote"}}`,
				reviewRow("t_x1", "bob", `"opencode-remote"`),
			},
			wantRev: "bob",
		},
		{
			name: "from the prior changes-requested round",
			bodies: []string{
				`{"task":{"id":"t_x1","title":"T","status":"ready","assignee":"opencode-remote"},` +
					`"events":[{"id":5,"kind":"changes_requested","payload":{"reviewer":"carol","implementer":"opencode-remote"}}]}`,
				`{"task":{"id":"t_x1","title":"T","status":"ready","assignee":"opencode-remote"},` +
					`"events":[{"id":5,"kind":"changes_requested","payload":{"reviewer":"carol","implementer":"opencode-remote"}}]}`,
				reviewRow("t_x1", "carol", `"opencode-remote"`),
			},
			wantRev: "carol",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			noEnvReviewer(t)
			withClaimWorker(t)
			if tc.env != "" {
				t.Setenv("MCP_REVIEWER_PROFILE", tc.env)
			}
			readArgv := argvLog(t)
			backend := newClaimBackend()
			backend.orders["t_x1"] = tc.bodies
			s := newClaimToolServer(t, backend)

			res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
				ID: "t_x1", Board: testBoard, Summary: "s", Reviewer: tc.review,
			})
			if res.IsError {
				t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
			}
			var handoffs int
			for _, line := range readArgv() {
				if !strings.Contains(line, "request-review") {
					continue
				}
				handoffs++
				if !strings.Contains(line, "--reviewer="+tc.wantRev) {
					t.Errorf("handoff = %q, want --reviewer=%s", line, tc.wantRev)
				}
			}
			if handoffs != 1 {
				t.Fatalf("handoffs = %d, want exactly 1", handoffs)
			}
			if out := decodeOut[TicketRequestReviewOut](t, res); out.Assignee != tc.wantRev {
				t.Errorf("landed assignee = %q, want %q", out.Assignee, tc.wantRev)
			}
		})
	}
}

// F2 (review, round 2): on a RUNNING card the stamp is `reassign
// --reclaim`, so a failed handoff leaves the caller's claim released and
// the card ready under the lane label. The stamp's own failure path says
// so; the handoff's failure path must not disagree by staying silent.
func TestTRR_HandoffFailureAfterReclaimTellsCaller(t *testing.T) {
	noEnvReviewer(t)
	label := withClaimWorker(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["task-reviewfail"] = []string{
		`{"task":{"id":"task-reviewfail","title":"T","status":"running"}}`,
		stampedRow("task-reviewfail", "ready", label),
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "task-reviewfail", Board: testBoard, Summary: "s", Reviewer: "alice", Force: true,
	})
	if !res.IsError {
		t.Fatalf("expected IsError, got success: %s", res.Content[0].Text)
	}
	for _, want := range []string{"re-review has no durable reviewer provenance", "may now be ready", "your claim is gone"} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("error = %q, want it to contain %q", res.Content[0].Text, want)
		}
	}
	argv := readArgv()
	if len(argv) != 2 {
		t.Fatalf("argv = %v, want the reclaiming stamp then the handoff", argv)
	}
	if !strings.Contains(argv[0], "reassign task-reviewfail "+label) || !strings.Contains(argv[0], "--reclaim") {
		t.Errorf("argv[0] = %q, want reassign --reclaim on the running ticket", argv[0])
	}
}

// reviewRoundReviewer is the provenance reader the omit path now depends
// on: the NEWEST changes_requested event wins, a null reviewer is reported
// as an empty (not missing) one, and an undecodable payload is its own
// case so the refusal can say which it was.
func TestReviewRoundReviewer(t *testing.T) {
	ev := func(s string) json.RawMessage { return json.RawMessage(s) }
	cases := []struct {
		name    string
		events  []json.RawMessage
		want    string
		found   bool
		decoded bool
	}{
		{name: "no events", events: nil},
		{
			name:   "first review has no verdict yet",
			events: []json.RawMessage{ev(`{"kind":"review_requested","payload":{"implementer":"lane","reviewer":"bob"}}`)},
		},
		{
			name: "newest verdict wins",
			events: []json.RawMessage{
				ev(`{"kind":"changes_requested","payload":{"reviewer":"alice"}}`),
				ev(`{"kind":"claimed","payload":{}}`),
				ev(`{"kind":"changes_requested","payload":{"reviewer":"bob"}}`),
			},
			want: "bob", found: true, decoded: true,
		},
		{
			name:    "null reviewer is present but empty",
			events:  []json.RawMessage{ev(`{"kind":"changes_requested","payload":{"reviewer":null}}`)},
			found:   true,
			decoded: true,
		},
		{
			name:    "undecodable payload",
			events:  []json.RawMessage{ev(`{"kind":"changes_requested","payload":"not-an-object"}`)},
			found:   true,
			decoded: false,
		},
		{
			name:    "unreadable event is skipped",
			events:  []json.RawMessage{ev(`{`), ev(`{"kind":"changes_requested","payload":{"reviewer":"carol"}}`)},
			want:    "carol",
			found:   true,
			decoded: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found, decoded := reviewRoundReviewer(tc.events)
			if got != tc.want || found != tc.found || decoded != tc.decoded {
				t.Errorf("reviewRoundReviewer = (%q, %v, %v), want (%q, %v, %v)", got, found, decoded, tc.want, tc.found, tc.decoded)
			}
		})
	}
}

// The row that LANDS is what matters: the post-condition must verify what
// landed, not what was asked for (a concurrent write or a kernel rule can
// differ), because the dispatcher spawns on the assignee alone.
func TestTRR_PostconditionNonSpawnableAssignee(t *testing.T) {
	noEnvReviewer(t)
	label := withClaimWorker(t)
	argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"ready"}}`,
		stampedRow("t_x1", "ready", label),
		reviewRow("t_x1", "parent-worker", fmt.Sprintf("%q", label)),
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s", Reviewer: "alice",
	})
	if !res.IsError {
		t.Fatalf("expected IsError for a review row assigned to a non-profile, got success: %s", res.Content[0].Text)
	}
	for _, want := range []string{"parent-worker", "not an installed profile"} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("error = %q, want it to mention %q", res.Content[0].Text, want)
		}
	}
}

// The original strand guard, now reached through reviewer resolution: an
// unassigned, claimable card with no reviewer anywhere. The dispatcher
// never spawns an unassigned review row (t_44d19d72 / t_371b7d27), so the
// handoff must be refused before anything is written.
func TestTRR_StrandGuard(t *testing.T) {
	noEnvReviewer(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{`{"task":{"id":"t_x1","title":"T","status":"running"}}`}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "shipped it", Force: true,
	})
	if !res.IsError {
		t.Fatalf("expected IsError for an unassigned review request, got success: %s", res.Content[0].Text)
	}
	for _, want := range []string{"no reviewer resolved", "dispatcher", "MCP_REVIEWER_PROFILE"} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("error = %q, want it to mention %q", res.Content[0].Text, want)
		}
	}
	assertNoCLI(t, readArgv())
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0 (must not flip the status)", n)
	}
	// preflight, then the event-log read the provenance lookup needs. No
	// re-read after the refusal, and no write of any kind.
	if n := backend.getCount("t_x1"); n != 2 {
		t.Errorf("GET count = %d, want 2 (preflight + event log, nothing after)", n)
	}
}

// F7 regression: the CLI exiting 0 without transitioning must not be
// reported as a dispatched review.
func TestTRR_PostconditionNotReview(t *testing.T) {
	noEnvReviewer(t)
	label := withClaimWorker(t)
	argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"ready"}}`,
		stampedRow("t_x1", "ready", label),
		`{"task":{"id":"t_x1","title":"T","status":"ready","assignee":"alice"}}`,
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s", Reviewer: "alice",
	})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "did not take effect") {
		t.Errorf("got %q, want an IsError about the transition not taking effect", res.Content[0].Text)
	}
}

// F7/F3 regression: a review row with no assignee is exactly the stranded
// state, and the tool must not report success for it.
func TestTRR_PostconditionEmptyAssignee(t *testing.T) {
	noEnvReviewer(t)
	label := withClaimWorker(t)
	argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"ready"}}`,
		stampedRow("t_x1", "ready", label),
		reviewRow("t_x1", "", fmt.Sprintf("%q", label)),
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s", Reviewer: "alice",
	})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "NO assignee") {
		t.Errorf("got %q, want an IsError about the stranded review row", res.Content[0].Text)
	}
}

// The read-back is the whole reason this tool exists: a handoff the kernel
// recorded with implementer=null still APPROVES, but a request-changes
// verdict cannot return it, and the caller must hear that at handoff time
// rather than discover it when the verdict blocks.
func TestTRR_ImplementerRecordedNullRefused(t *testing.T) {
	noEnvReviewer(t)
	withClaimWorker(t)
	argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"ready","assignee":"default"}}`,
		stampedRow("t_x1", "ready", "opencode-remote"),
		reviewRow("t_x1", "alice", `null`),
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s", Reviewer: "alice",
	})
	if !res.IsError {
		t.Fatalf("expected IsError when the kernel recorded implementer=null, got success: %s", res.Content[0].Text)
	}
	for _, want := range []string{"implementer=null", "request-changes", "block", "MCP_CLAIM_WORKER"} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("error = %q, want it to mention %q", res.Content[0].Text, want)
		}
	}
}

// A handoff that recorded someone else is not a failure of the transition,
// but it is not the destination the caller asked for — say which it is.
func TestTRR_ImplementerRecordedDifferentRefused(t *testing.T) {
	noEnvReviewer(t)
	withClaimWorker(t)
	argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"ready"}}`,
		stampedRow("t_x1", "ready", "opencode-remote"),
		reviewRow("t_x1", "alice", `"someone-else"`),
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s", Reviewer: "alice",
	})
	if !res.IsError {
		t.Fatalf("expected IsError when the recorded implementer differs, got success: %s", res.Content[0].Text)
	}
	for _, want := range []string{"someone-else", "opencode-remote"} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("error = %q, want it to mention %q", res.Content[0].Text, want)
		}
	}
}

// An event log without a review_requested event cannot answer the
// question; the tool says so instead of implying the handoff is sound.
func TestTRR_ImplementerUnreadableFromEvents(t *testing.T) {
	noEnvReviewer(t)
	label := withClaimWorker(t)
	argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"ready"}}`,
		stampedRow("t_x1", "ready", label),
		`{"task":{"id":"t_x1","title":"T","status":"review","assignee":"alice"},"events":[]}`,
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s", Reviewer: "alice",
	})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "cannot be verified") {
		t.Errorf("got %q, want an IsError about an unverifiable implementer", res.Content[0].Text)
	}
}

func TestTRR_SummaryRequired(t *testing.T) {
	installFakeBin(t)
	backend := newClaimBackend()
	s := newClaimToolServer(t, backend)
	for _, summary := range []string{"", "   ", "\n\t"} {
		res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
			ID: "t_x1", Board: testBoard, Summary: summary, Reviewer: "alice",
		})
		if !res.IsError || !strings.Contains(res.Content[0].Text, "summary required") {
			t.Errorf("summary %q: got %q, want a summary-required IsError", summary, res.Content[0].Text)
		}
	}
	// Validation precedes any backend call.
	if n := backend.getCount("t_x1"); n != 0 {
		t.Errorf("GET count = %d, want 0 (validation runs first)", n)
	}
	if n := backend.profileProbes(); n != 0 {
		t.Errorf("profile probes = %d, want 0", n)
	}
}

func TestTRR_StatusPreflight(t *testing.T) {
	installFakeBin(t)
	tests := []struct {
		status  string
		wantSub string
	}{
		{status: "blocked", wantSub: "requires ready or running"},
		{status: "todo", wantSub: "requires ready or running"},
		{status: "done", wantSub: "requires ready or running"},
		{status: "review", wantSub: "requires ready or running"},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			readArgv := argvLog(t)
			backend := newClaimBackend()
			backend.orders["t_x1"] = []string{`{"task":{"id":"t_x1","title":"T","status":"` + tt.status + `"}}`}
			s := newClaimToolServer(t, backend)

			res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
				ID: "t_x1", Board: testBoard, Summary: "s", Reviewer: "alice",
			})
			if !res.IsError || !strings.Contains(res.Content[0].Text, tt.wantSub) {
				t.Errorf("got %q, want an IsError mentioning %q", res.Content[0].Text, tt.wantSub)
			}
			assertNoCLI(t, readArgv())
		})
	}
}

func TestTRR_Validation(t *testing.T) {
	installFakeBin(t)
	backend := newClaimBackend()
	s := newClaimToolServer(t, backend)
	cases := []struct {
		name string
		in   TicketRequestReviewInput
		want string
	}{
		{name: "board missing", in: TicketRequestReviewInput{ID: "t_x1", Summary: "s"}, want: "board required"},
		{name: "bad board", in: TicketRequestReviewInput{ID: "t_x1", Board: "BAD BOARD", Summary: "s"}, want: "invalid board"},
		{name: "bad id", in: TicketRequestReviewInput{ID: "a b", Board: testBoard, Summary: "s"}, want: "invalid ticket id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := s.TicketRequestReview(context.Background(), tc.in)
			if !res.IsError || !strings.Contains(res.Content[0].Text, tc.want) {
				t.Errorf("got %q, want an IsError mentioning %q", res.Content[0].Text, tc.want)
			}
		})
	}
	if n := backend.getCount("t_x1"); n != 0 {
		t.Errorf("GET count = %d, want 0 (validation runs first)", n)
	}
}

func TestTRR_CLIFailureSurfacesStderr(t *testing.T) {
	noEnvReviewer(t)
	withClaimWorker(t)
	installFakeBin(t)
	backend := newClaimBackend()
	backend.orders["task-fail"] = []string{`{"task":{"id":"task-fail","title":"T","status":"ready"}}`}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "task-fail", Board: testBoard, Summary: "s", Reviewer: "alice",
	})
	if !res.IsError {
		t.Fatalf("expected IsError, got success: %s", res.Content[0].Text)
	}
	// The CLI's own first stderr line is surfaced verbatim, and the tool
	// says what did NOT happen (the handoff was never requested).
	for _, want := range []string{"error: task already claimed", "handoff was NOT requested"} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("error = %q, want it to contain %q", res.Content[0].Text, want)
		}
	}
}

// The binary check runs before any network call (TicketClaim's ordering).
func TestTRR_BinaryMissing(t *testing.T) {
	noEnvReviewer(t)
	breakBin(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{`{"task":{"id":"t_x1","title":"T","status":"ready"}}`}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s", Reviewer: "alice",
	})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "request-review unavailable") {
		t.Errorf("got %q, want a request-review-unavailable IsError", res.Content[0].Text)
	}
	// Fail fast: no REST preflight, no roster probe.
	if n := backend.getCount("t_x1"); n != 0 {
		t.Errorf("GET count = %d, want 0 (binary check must precede the preflight)", n)
	}
	if n := backend.profileProbes(); n != 0 {
		t.Errorf("profile probes = %d, want 0", n)
	}
}

func TestTRR_NotFound(t *testing.T) {
	noEnvReviewer(t)
	installFakeBin(t)
	backend := newClaimBackend()
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_missing", Board: testBoard, Summary: "s", Reviewer: "alice",
	})
	if !res.IsError {
		t.Fatalf("expected IsError for an unknown ticket, got success: %s", res.Content[0].Text)
	}
}
