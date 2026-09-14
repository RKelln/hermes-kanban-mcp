package mcptools

// review_tool_test.go exercises TicketRequestReview against the scripted
// kanban REST backend and the fake hermes CLI.
//
// The load-bearing case is TestTRR_StrandGuard: an unassigned review
// ticket is skipped by the dispatcher every tick, forever, with no
// reviewer and no runs (t_44d19d72 / t_371b7d27, 5 days silent,
// diagnosed in t_124ab31f). The tool must REFUSE rather than perform a
// transition nothing will act on — so that test asserts the negative: no
// CLI invocation and no status change.

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
// observe the argument/assignee resolution order, not ambient config.
func noEnvReviewer(t *testing.T) {
	t.Helper()
	t.Setenv("MCP_REVIEWER_PROFILE", "")
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
	// Authoritative state: preflight + re-read, and never a PATCH.
	if n := backend.getCount("t_x1"); n != 2 {
		t.Errorf("GET count = %d, want 2 (preflight + authoritative re-read)", n)
	}
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0", n)
	}
	argv := readArgv()
	if len(argv) != 1 {
		t.Fatalf("argv lines = %v, want exactly 1 CLI invocation", argv)
	}
	if !strings.Contains(argv[0], "request-review t_x1") || !strings.Contains(argv[0], "--summary=shipped it") {
		t.Errorf("argv = %q, want request-review with the summary", argv[0])
	}
	if strings.Contains(argv[0], "--force") {
		t.Errorf("argv = %q, must NOT force a ready ticket", argv[0])
	}
}

func TestTRR_RunningPassesForce(t *testing.T) {
	noEnvReviewer(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{
		`{"task":{"id":"t_x1","title":"T","status":"running","assignee":"alice","claim_lock":"exp:1","claim_expires":9999999999}}`,
		`{"task":{"id":"t_x1","title":"T","status":"review","assignee":"alice"}}`,
	}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "shipped it",
	})
	if res.IsError {
		t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
	}
	// A running ticket holds our own live claim; the CLI has no
	// --expected-run-id, so --force is the only release path.
	argv := readArgv()
	if len(argv) != 1 || !strings.Contains(argv[0], "--force") {
		t.Fatalf("argv = %v, want exactly one invocation carrying --force", argv)
	}
}

// TestTRR_StrandGuard is the negative control for the failure this whole
// tool exists to prevent: a review row nobody will ever pick up.
func TestTRR_StrandGuard(t *testing.T) {
	noEnvReviewer(t)
	readArgv := argvLog(t)
	backend := newClaimBackend()
	backend.orders["t_x1"] = []string{`{"task":{"id":"t_x1","title":"T","status":"running"}}`}
	s := newClaimToolServer(t, backend)

	res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
		ID: "t_x1", Board: testBoard, Summary: "shipped it",
	})
	if !res.IsError {
		t.Fatalf("expected IsError for an unassigned review request, got success: %s", res.Content[0].Text)
	}
	for _, want := range []string{"unassigned", "never spawns", "MCP_REVIEWER_PROFILE"} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("error = %q, want it to mention %q", res.Content[0].Text, want)
		}
	}
	// It must refuse BEFORE acting: no CLI, no status write.
	if argv := readArgv(); len(argv) != 0 {
		t.Errorf("argv = %v, want no CLI invocation", argv)
	}
	if n := len(backend.patches()); n != 0 {
		t.Errorf("PATCH count = %d, want 0 (must not flip the status)", n)
	}
	if n := backend.getCount("t_x1"); n != 1 {
		t.Errorf("GET count = %d, want 1 (preflight only, no re-read after a refusal)", n)
	}
}

func TestTRR_ReviewerResolution(t *testing.T) {
	tests := []struct {
		name         string
		argReviewer  string
		envReviewer  string
		assignee     string
		wantReviewer string
	}{
		{name: "explicit argument wins", argReviewer: "carol", envReviewer: "bob", assignee: "dave", wantReviewer: "carol"},
		{name: "env used when argument empty", envReviewer: "bob", assignee: "dave", wantReviewer: "bob"},
		{name: "existing assignee used when both empty", assignee: "dave", wantReviewer: "dave"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MCP_REVIEWER_PROFILE", tt.envReviewer)
			readArgv := argvLog(t)
			backend := newClaimBackend()
			backend.orders["t_x1"] = []string{
				`{"task":{"id":"t_x1","title":"T","status":"ready","assignee":"` + tt.assignee + `"}}`,
				`{"task":{"id":"t_x1","title":"T","status":"review","assignee":"` + tt.wantReviewer + `"}}`,
			}
			s := newClaimToolServer(t, backend)

			res := s.TicketRequestReview(context.Background(), TicketRequestReviewInput{
				ID: "t_x1", Board: testBoard, Summary: "s", Reviewer: tt.argReviewer,
			})
			if res.IsError {
				t.Fatalf("expected success, got IsError: %s", res.Content[0].Text)
			}
			if out := decodeOut[TicketRequestReviewOut](t, res); out.Assignee != tt.wantReviewer {
				t.Errorf("assignee = %q, want %q", out.Assignee, tt.wantReviewer)
			}
			argv := readArgv()
			if len(argv) != 1 || !strings.Contains(argv[0], "--reviewer="+tt.wantReviewer) {
				t.Errorf("argv = %v, want --reviewer=%s", argv, tt.wantReviewer)
			}
		})
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
			if argv := readArgv(); len(argv) != 0 {
				t.Errorf("argv = %v, want no CLI invocation from a rejected status", argv)
			}
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
		{name: "bad board", in: TicketRequestReviewInput{ID: "t_x1", Board: "BAD BOARD", Summary: "s"}, want: "invalid_input"},
		{name: "bad id", in: TicketRequestReviewInput{ID: "a b", Board: testBoard, Summary: "s"}, want: "invalid_input"},
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
