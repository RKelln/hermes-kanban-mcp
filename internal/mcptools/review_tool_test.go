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
//   - TestTRR_AssigneeIsNotUsedAsReviewer: after a request-changes
//     verdict the kernel sets assignee = the implementer, so falling back
//     to the assignee would make round 2 a self-review.
//   - TestTRR_RunningRequiresForce / TestTRR_ForceRejectedOnReady: the
//     bridge cannot verify claim ownership, so releasing a live claim is
//     an explicit caller decision and never automatic.
//   - TestTRR_Postcondition*: never report "reviewer dispatched" for a
//     ticket that did not reach a dispatchable review row.

import (
	"context"
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

// noEnvReviewer makes sure the server-default knob is absent so tests
// observe the argument/default resolution, not ambient config.
func noEnvReviewer(t *testing.T) {
	t.Helper()
	t.Setenv("MCP_REVIEWER_PROFILE", "")
}

// assertNoCLI asserts the CLI was never invoked (the refusal paths must
// all refuse BEFORE exec).
func assertNoCLI(t *testing.T, argv []string) {
	t.Helper()
	if len(argv) != 0 {
		t.Errorf("argv = %v, want no CLI invocation", argv)
	}
}

func TestTRR_SuccessReady(t *testing.T) {
	noEnvReviewer(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"ready"}}`,
		`{"task":{"id":"t_x1","title":"T","status":"review","assignee":"alice"}}`,
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
	if out.Note != reviewNote {
		t.Errorf("note = %q, want %q", out.Note, reviewNote)
	}
	// Authoritative state: preflight + re-read, never a PATCH.
	if n := backend.getCount("t_x1"); n != 2 {
		t.Errorf("GET count = %d, want 2 (preflight + authoritative re-read)", n)
	}
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0", n)
	}
	if n := backend.profileProbes(); n != 2 {
		t.Errorf("profile probes = %d, want 2 (verify the requested reviewer, then re-verify the assignee that actually landed)", n)
	}
	argv := readArgv()
	if len(argv) != 1 {
		t.Fatalf("argv lines = %v, want exactly 1 CLI invocation", argv)
	}
	if !strings.Contains(argv[0], "request-review t_x1") || !strings.Contains(argv[0], "--summary=shipped it") {
		t.Errorf("argv = %q, want request-review with the summary", argv[0])
	}
	if !strings.Contains(argv[0], "--reviewer=alice") {
		t.Errorf("argv = %q, want --reviewer=alice", argv[0])
	}
	if strings.Contains(argv[0], "--force") {
		t.Errorf("argv = %q, must NOT force a ready ticket", argv[0])
	}
}

// F2 regression: a running ticket holds a live claim the bridge cannot
// attribute, so the caller must assert force explicitly.
func TestTRR_RunningRequiresForce(t *testing.T) {
	noEnvReviewer(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"running","assignee":"other-profile","claim_lock":"holder-1","claim_expires":9999999999}}`,
		`{"task":{"id":"t_x1","title":"T","status":"review","assignee":"alice"}}`,
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

	// With the explicit assertion the transition proceeds, carrying --force.
	// Fresh backend: the scripted orders repeat their last body, so reusing
	// this one would serve "review" to the second preflight.
	backend2 := newClaimBackend()
	backend2.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"running","assignee":"other-profile","claim_lock":"holder-1","claim_expires":9999999999}}`,
		`{"task":{"id":"t_x1","title":"T","status":"review","assignee":"alice"}}`,
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
	if len(argv) != 1 || !strings.Contains(argv[0], "--force") {
		t.Fatalf("argv = %v, want exactly one invocation carrying --force", argv)
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

// When the roster cannot be read the check cannot run. Proceed, but SAY so:
// a silently degraded guard is the failure mode this tool exists to prevent.
func TestTRR_UnverifiedRosterAnnounced(t *testing.T) {
	noEnvReviewer(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.profiles = nil // /profiles 404s
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"ready"}}`,
		`{"task":{"id":"t_x1","title":"T","status":"review","assignee":"alice"}}`,
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s", Reviewer: "alice",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	out := decodeOut[TicketRequestReviewOut](t, res)
	if !strings.Contains(out.Note, "could NOT be verified") {
		t.Errorf("note = %q, want an announced unverified-roster warning", out.Note)
	}
	if len(readArgv()) != 1 {
		t.Errorf("argv = %v, want the transition to proceed", readArgv())
	}
}

// F3 regression (MEDIUM): after request-changes the kernel sets
// assignee = the implementer, so SUBSTITUTING the assignee as the reviewer
// would make round 2 a self-review. With no reviewer configured the tool
// must omit --reviewer and leave the kernel's own choice intact.
//
// Note what this test does NOT claim: it is not a licence to skip
// verification. The assignee here is a roster member, so the review row is
// dispatchable; TestTRR_PreservedNonSpawnableAssigneeRefused covers the
// non-spawnable case, which an earlier version of this test blessed.
func TestTRR_AssigneeIsNotSubstitutedAsReviewer(t *testing.T) {
	noEnvReviewer(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"ready","assignee":"bob"}}`,
		`{"task":{"id":"t_x1","title":"T","status":"review","assignee":"bob"}}`,
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	argv := readArgv()
	if len(argv) != 1 {
		t.Fatalf("argv = %v, want one invocation", argv)
	}
	if strings.Contains(argv[0], "--reviewer=") {
		t.Errorf("argv = %q, must omit --reviewer so the kernel's own choice applies", argv[0])
	}
	// FIRST review: no `changes_requested` run exists, so the kernel's
	// _prior_reviewer returns None, `reviewer` stays None and the UPDATE
	// PRESERVES the assignee (kanban_db.py:3054, :3060-3069). The note must
	// say that, not that a prior reviewer was reused — there is no prior
	// round to reuse anything from.
	out := decodeOut[TicketRequestReviewOut](t, res)
	if !strings.Contains(out.Note, "kept the ticket's assignee") {
		t.Errorf("note = %q, want the first-review branch: the kernel kept the assignee", out.Note)
	}
	if strings.Contains(out.Note, "previous request-changes round") {
		t.Errorf("note = %q, must NOT claim a prior request-changes round on a first review", out.Note)
	}
	// The preserved assignee is still VERIFIED for spawnability — that is
	// the difference between checking a value and substituting it.
	if n := backend.profileProbes(); n != 2 {
		t.Errorf("profile probes = %d, want 2 (the preserved assignee must still be verified)", n)
	}
}

// --- re-review provenance: the value the KERNEL lands ---
//
// kernelPriorReviewer mirrors hermes_cli.kanban_db._prior_reviewer over the
// ticket's OWN runs and events, so these fixtures state the kernel's rule as
// a predicate over served data — "newest run whose outcome is
// changes_requested, reviewer read off that run's changes_requested event" —
// instead of a scripted answer. That is what lets a test express "the kernel
// will land a name nobody requested".

// reReviewBody is a preflight body for a ticket that already has a
// request-changes round behind it: run 13 carries outcome
// changes_requested, and event 162 (recorded against run 13) names the
// reviewer that round used.
func reReviewBody(status, assignee, reviewer string) string {
	event := `{"id":162,"run_id":13,"kind":"changes_requested","payload":{"reviewer":"` + reviewer + `","reason":"fix it"}}`
	return `{"task":{"id":"t_x1","title":"T","status":"` + status + `","assignee":"` + assignee + `"},` +
		`"runs":[{"id":11,"outcome":"review_requested"},{"id":13,"outcome":"changes_requested"}],` +
		`"events":[{"id":158,"run_id":13,"kind":"claimed"},` + event + `]}`
}

// Round-2 leftover F4 (the strand the post-condition could only announce):
// the kernel PREFERS the prior request-changes round's reviewer over the
// assignee, so a re-review whose prior reviewer is not an on-disk profile
// lands a name nobody requested — after the row is already in 'review'. The
// pre-check must verify the value that will land, and refuse before creating
// that row.
func TestTRR_PriorReviewerRefusedWhenNotSpawnable(t *testing.T) {
	noEnvReviewer(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	// The assignee (alice) IS a spawnable profile — today's pre-check would
	// pass on it and strand the row on "ghost".
	backend.orders["t_x1"] = []string{reReviewBody("ready", "alice", "ghost")}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s",
	})
	if !res.IsError {
		t.Fatalf("expected IsError for a prior reviewer that is not an installed profile, got success: %s", res.Content[0].Text)
	}
	for _, want := range []string{"ghost", "previous request-changes round", "not an installed profile", "alice"} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("error = %q, want it to mention %q", res.Content[0].Text, want)
		}
	}
	assertNoCLI(t, readArgv())
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0 (must not create the stranded row)", n)
	}
}

// The same rule, other direction: when the kernel will OVERRIDE the assignee
// with a spawnable prior reviewer, the request is safe even though the
// current assignee is not a profile — the tool verifies the value that
// lands, and reports the re-review branch of the note.
func TestTRR_PriorReviewerOverridesUnspawnableAssignee(t *testing.T) {
	noEnvReviewer(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	// Preflight: assignee is "ryan" (real non-spawnable assignee on this
	// host), prior round recorded "bob". The kernel reassigns to bob.
	backend.orders["t_x1"] = []string{
		reReviewBody("ready", "ryan", "bob"),
		`{"task":{"id":"t_x1","title":"T","status":"review","assignee":"bob"}}`,
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s",
	})
	if res.IsError {
		t.Fatalf("expected success (the prior reviewer overrides the assignee), got IsError: %s", res.Content[0].Text)
	}
	out := decodeOut[TicketRequestReviewOut](t, res)
	if out.Assignee != "bob" {
		t.Errorf("assignee = %q, want bob (the prior reviewer the kernel lands)", out.Assignee)
	}
	if !strings.Contains(out.Note, "reused the reviewer recorded by the previous request-changes round") {
		t.Errorf("note = %q, want the re-review branch", out.Note)
	}
	argv := readArgv()
	if len(argv) != 1 {
		t.Fatalf("argv = %v, want one invocation", argv)
	}
	if strings.Contains(argv[0], "--reviewer=") {
		t.Errorf("argv = %q, must omit --reviewer: the kernel's provenance is what lands", argv[0])
	}
	// Verified twice: the prior reviewer pre-exec, the landed assignee
	// post-exec.
	if n := backend.profileProbes(); n != 2 {
		t.Errorf("profile probes = %d, want 2 (prior reviewer, then the value that landed)", n)
	}
}

// A request-changes run whose event carries no readable reviewer is exactly
// the case the kernel REFUSES ("re-review has no durable reviewer
// provenance"); this tool cannot mirror that refusal from the detail alone,
// so it falls back to the value the kernel would otherwise preserve — the
// assignee — and the CLI's own error surfaces if the kernel refuses. What it
// must not do is claim a prior reviewer it could not read.
func TestTRR_PriorRoundWithoutReviewerFallsBackToAssignee(t *testing.T) {
	noEnvReviewer(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"ready","assignee":"ryan"},` +
			`"runs":[{"id":13,"outcome":"changes_requested"}],` +
			`"events":[{"id":162,"run_id":13,"kind":"changes_requested","payload":{"reason":"fix it"}}]}`,
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s",
	})
	if !res.IsError {
		t.Fatalf("expected IsError (the assignee is not a profile), got success: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "ryan") || !strings.Contains(res.Content[0].Text, "not an installed profile") {
		t.Errorf("error = %q, want the preserved-assignee refusal naming ryan", res.Content[0].Text)
	}
	if strings.Contains(res.Content[0].Text, "previous request-changes round") {
		t.Errorf("error = %q, must not claim a prior reviewer it could not read", res.Content[0].Text)
	}
	assertNoCLI(t, readArgv())
}

// The exact-mirror case the pre-gate review produced: the kernel reads ONLY
// the newest changes_requested event for the selected run (kanban_db.py:3106
// -> _latest_event, "ORDER BY id DESC LIMIT 1"). An older event with a usable
// reviewer does NOT rescue a newest one whose provenance is blank — the kernel
// returns False there and request_review refuses. An implementation that
// scanned for any usable event would bless the row the kernel is about to
// refuse, having verified a name nobody will ever see.
func TestTRR_PriorRoundNewerBlankEventShadowsOlder(t *testing.T) {
	noEnvReviewer(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"ready","assignee":"ryan"},` +
			`"runs":[{"id":13,"outcome":"changes_requested"}],` +
			`"events":[{"id":161,"run_id":13,"kind":"changes_requested","payload":{"reviewer":"carol"}},` +
			`{"id":162,"run_id":13,"kind":"changes_requested","payload":{"reason":"no reviewer recorded"}}]}`,
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s",
	})
	if !res.IsError {
		t.Fatalf("expected IsError (the assignee is not a profile), got success: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "ryan") || !strings.Contains(res.Content[0].Text, "not an installed profile") {
		t.Errorf("error = %q, want the preserved-assignee refusal naming ryan", res.Content[0].Text)
	}
	if strings.Contains(res.Content[0].Text, "previous request-changes round") {
		t.Errorf("error = %q, must NOT report the shadowed older event's reviewer (the kernel returns False here)", res.Content[0].Text)
	}
	assertNoCLI(t, readArgv())
}

// _prior_reviewer returns the RAW payload string, but the value request_review
// LANDS is canonicalised (kanban_db.py:3048 -> _canonical_assignee ->
// normalize_profile_name: strip, then lowercase). A whitespace-padded reviewer
// must therefore be verified as the name that will actually be spawned rather
// than refused for failing to match the roster verbatim — and the row lands
// the canonical form.
func TestTRR_PriorReviewerPaddedIsVerified(t *testing.T) {
	noEnvReviewer(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		reReviewBody("ready", "ryan", "  bob  "),
		`{"task":{"id":"t_x1","title":"T","status":"review","assignee":"bob"}}`,
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s",
	})
	if res.IsError {
		t.Fatalf("expected success (the kernel lands the canonicalised reviewer), got IsError: %s", res.Content[0].Text)
	}
	out := decodeOut[TicketRequestReviewOut](t, res)
	if out.Assignee != "bob" {
		t.Errorf("assignee = %q, want bob (normalize_profile_name strips the padding)", out.Assignee)
	}
	if !strings.Contains(out.Note, "reused the reviewer recorded by the previous request-changes round") {
		t.Errorf("note = %q, want the re-review branch", out.Note)
	}
	if len(readArgv()) != 1 {
		t.Errorf("argv = %v, want the transition to proceed", readArgv())
	}
}

// The mirror returns the name the kernel LANDS, not the raw payload string:
// request_review canonicalises it (normalize_profile_name: strip + lowercase,
// kanban_db.py:3048) and profile directories are lowercase on disk. Naming the
// raw value in a refusal would send the caller looking for a profile that
// cannot exist ("GHOST" vs "ghost") — and the roster check itself is
// case-insensitive, so only the reported name pins this.
func TestTRR_PriorReviewerReportedCanonicalised(t *testing.T) {
	noEnvReviewer(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{reReviewBody("ready", "bob", "GHOST")}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s",
	})
	if !res.IsError {
		t.Fatalf("expected IsError for the non-profile prior reviewer, got success: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, `"ghost"`) {
		t.Errorf("error = %q, want the canonicalised name %q", res.Content[0].Text, "ghost")
	}
	if strings.Contains(res.Content[0].Text, "GHOST") {
		t.Errorf("error = %q, must not report the raw payload case", res.Content[0].Text)
	}
	assertNoCLI(t, readArgv())
}

// The FIRST-review note used to end with "(verified as spawnable)" even when
// /profiles never answered — an assurance about a check that never ran,
// printed next to the warning saying it never ran (pre-gate finding 1). The
// parenthetical is now gated on the probe that covers the value that landed.
func TestTRR_UnverifiedRosterNoVerificationClaim(t *testing.T) {
	noEnvReviewer(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.profiles = nil // /profiles 404s, before and after the transition
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"ready","assignee":"alice"}}`,
		`{"task":{"id":"t_x1","title":"T","status":"review","assignee":"alice"}}`,
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	out := decodeOut[TicketRequestReviewOut](t, res)
	if !strings.Contains(out.Note, "the profiles endpoint did not answer") {
		t.Errorf("note = %q, want the announced unverified-roster warning", out.Note)
	}
	if !strings.Contains(out.Note, "spawnability could NOT be verified") {
		t.Errorf("note = %q, want the unverified first-review branch", out.Note)
	}
	if strings.Contains(out.Note, "(verified as spawnable)") {
		t.Errorf("note = %q, must NOT claim a verification that did not run", out.Note)
	}
	if len(readArgv()) != 1 {
		t.Errorf("argv = %v, want the transition to proceed", readArgv())
	}
}

// An UNASSIGNED ticket is the original strand — unless the kernel's own
// provenance supplies the assignee. The unassigned refusal must not fire
// when a prior request-changes round will land a reviewer.
func TestTRR_UnassignedWithPriorReviewerProceeds(t *testing.T) {
	noEnvReviewer(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		reReviewBody("ready", "", "bob"),
		`{"task":{"id":"t_x1","title":"T","status":"review","assignee":"bob"}}`,
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s",
	})
	if res.IsError {
		t.Fatalf("expected success (the prior reviewer is the assignee), got IsError: %s", res.Content[0].Text)
	}
	if out := decodeOut[TicketRequestReviewOut](t, res); out.Assignee != "bob" {
		t.Errorf("assignee = %q, want bob", out.Assignee)
	}
	if len(readArgv()) != 1 {
		t.Errorf("argv = %v, want the transition to proceed", readArgv())
	}
}

// Round-2 F1 regression (HIGH): with no reviewer supplied, the kernel
// PRESERVES the existing assignee. If that assignee is not a spawnable
// profile the card sits in 'review' forever — the exact strand — so the
// tool must refuse before creating it.
func TestTRR_PreservedNonSpawnableAssigneeRefused(t *testing.T) {
	noEnvReviewer(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	// "ryan" and "parent-worker" are real non-spawnable assignees on this
	// host (`hermes kanban assignees`: default yes, ryan no).
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"ready","assignee":"ryan"}}`,
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s",
	})
	if !res.IsError {
		t.Fatalf("expected IsError for a preserved non-spawnable assignee, got success: %s", res.Content[0].Text)
	}
	for _, want := range []string{"ryan", "not an installed profile", "MCP_REVIEWER_PROFILE", "alice"} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("error = %q, want it to mention %q", res.Content[0].Text, want)
		}
	}
	assertNoCLI(t, readArgv())
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0 (must not create the stranded row)", n)
	}

	// Same card, but with a configured reviewer: now it proceeds, and the
	// configured reviewer is used rather than the preserved assignee.
	// Fresh backend: scripted orders repeat their last body, and the first
	// half already consumed a GET.
	t.Setenv("MCP_REVIEWER_PROFILE", "bob")
	backend2 := newClaimBackend()
	backend2.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"ready","assignee":"ryan"}}`,
		`{"task":{"id":"t_x1","title":"T","status":"review","assignee":"bob"}}`,
	}
	readArgv2 := argvLog(t)
	s2 := newClaimToolServer(t, backend2)
	res = s2.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s",
	})
	if res.IsError {
		t.Fatalf("expected success with a configured reviewer, got IsError: %s", res.Content[0].Text)
	}
	argv := readArgv2()
	if len(argv) != 1 || !strings.Contains(argv[0], "--reviewer=bob") {
		t.Fatalf("argv = %v, want exactly one invocation with --reviewer=bob", argv)
	}
}

// Round-2 F1, second half: the assignee that ACTUALLY LANDS can differ from
// the one requested (the kernel prefers its own prior-reviewer provenance).
// The post-condition must check what landed, not what was asked for.
func TestTRR_PostconditionNonSpawnableAssignee(t *testing.T) {
	noEnvReviewer(t)
	argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"ready"}}`,
		`{"task":{"id":"t_x1","title":"T","status":"review","assignee":"parent-worker"}}`,
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
	assertNamesRecovery(t, res.Content[0].Text)
}

// The original guard, unchanged: an unassigned review row nobody will
// ever pick up.
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
	for _, want := range []string{"unassigned", "never spawns", "MCP_REVIEWER_PROFILE"} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("error = %q, want it to mention %q", res.Content[0].Text, want)
		}
	}
	assertNoCLI(t, readArgv())
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0 (must not flip the status)", n)
	}
	if n := backend.getCount("t_x1"); n != 1 {
		t.Errorf("GET count = %d, want 1 (preflight only, no re-read after a refusal)", n)
	}
}

// F7 regression: the CLI exiting 0 without transitioning must not be
// reported as a dispatched review.
func TestTRR_PostconditionNotReview(t *testing.T) {
	noEnvReviewer(t)
	argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{`{"task":{"id":"t_x1","title":"T","status":"ready"}}`}
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
	argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"ready"}}`,
		`{"task":{"id":"t_x1","title":"T","status":"review"}}`,
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "s", Reviewer: "alice",
	})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "NO assignee") {
		t.Errorf("got %q, want an IsError about the stranded review row", res.Content[0].Text)
	}
	assertNamesRecovery(t, res.Content[0].Text)
}

// assertNamesRecovery pins the wording of the two POST-execution strand
// errors. Both fire while the ticket is already in 'review', and this tool's
// own preflight accepts only ready/running (TestTRR_StatusPreflight/review),
// so a bare "…then request again" tells the caller to do something this tool
// refuses. The message must name a recovery that works from where the caller
// actually is: fix the row outside the MCP surface (reassign it), or
// reopen-review it and request again with an explicit reviewer.
func assertNamesRecovery(t *testing.T, msg string) {
	t.Helper()
	for _, want := range []string{"reopen-review", "--board " + testBoard, "t_x1", "reassign"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want it to name the recovery (%q)", msg, want)
		}
	}
	if strings.Contains(msg, "then request again") {
		t.Errorf("error = %q, offers a retry this tool refuses (the ticket is already in 'review')", msg)
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
	if want := "error: task already claimed"; res.Content[0].Text != want {
		t.Errorf("error = %q, want the fake's first stderr line %q", res.Content[0].Text, want)
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
