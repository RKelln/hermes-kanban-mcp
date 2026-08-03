package mcptools

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Constants sanity
// ---------------------------------------------------------------------------

func TestConstantsSane(t *testing.T) {
	if DefaultTicketListLimit > MaxTicketListLimit {
		t.Errorf("DefaultTicketListLimit (%d) must not exceed MaxTicketListLimit (%d)", DefaultTicketListLimit, MaxTicketListLimit)
	}
	if DefaultTicketListLimit != 25 {
		t.Errorf("DefaultTicketListLimit = %d, want 25", DefaultTicketListLimit)
	}
	if MaxTicketListLimit != 50 {
		t.Errorf("MaxTicketListLimit = %d, want 50", MaxTicketListLimit)
	}
	if MaxTicketListOutputBytes != 6*1024 {
		t.Errorf("MaxTicketListOutputBytes = %d, want %d", MaxTicketListOutputBytes, 6*1024)
	}
	if MaxTicketGetOutputBytes != 8*1024 {
		t.Errorf("MaxTicketGetOutputBytes = %d, want %d", MaxTicketGetOutputBytes, 8*1024)
	}
	if MaxBlockedReasonChars > MaxTicketBodyChars {
		t.Errorf("blocked-reason cap (%d) must not exceed body cap (%d)", MaxBlockedReasonChars, MaxTicketBodyChars)
	}
	if MaxCommentBodyChars > MaxTicketBodyChars {
		t.Errorf("comment-body cap (%d) must not exceed body cap (%d)", MaxCommentBodyChars, MaxTicketBodyChars)
	}
	if MaxCommentsReturned <= 0 || MaxEventsReturned <= 0 ||
		MaxAttachmentsNames <= 0 || MaxLinksNames <= 0 || MaxWarningsReturned <= 0 {
		t.Error("collection caps must all be positive")
	}
	if !strings.Contains(OmittedMarkerFmt, "%d") {
		t.Errorf("OmittedMarkerFmt %q must contain a %%d verb", OmittedMarkerFmt)
	}

	// ValidStatuses is the canonical 9-status set, in the documented order.
	wantStatuses := []string{"triage", "todo", "scheduled", "ready", "running", "blocked", "review", "done", "archived"}
	if !reflect.DeepEqual(ValidStatuses, wantStatuses) {
		t.Errorf("ValidStatuses = %v, want %v", ValidStatuses, wantStatuses)
	}

	// ticketStatusOrder is a permutation of ValidStatuses.
	if len(ticketStatusOrder) != len(ValidStatuses) {
		t.Fatalf("ticketStatusOrder has %d entries, ValidStatuses has %d", len(ticketStatusOrder), len(ValidStatuses))
	}
	seen := make(map[string]bool, len(ticketStatusOrder))
	for _, s := range ticketStatusOrder {
		if !validStatusSet(ValidStatuses)[s] {
			t.Errorf("ticketStatusOrder contains %q, which is not a valid status", s)
		}
		if seen[s] {
			t.Errorf("ticketStatusOrder contains duplicate %q", s)
		}
		seen[s] = true
	}
}

func validStatusSet(statuses []string) map[string]bool {
	m := make(map[string]bool, len(statuses))
	for _, s := range statuses {
		m[s] = true
	}
	return m
}

// ---------------------------------------------------------------------------
// truncateToRunes
// ---------------------------------------------------------------------------

func TestTruncateToRunes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"empty string", "", 10, ""},
		{"zero max", "hello", 0, ""},
		{"negative max", "hello", -3, ""},
		{"shorter than max unchanged", "hi", 5, "hi"},
		{"exact max unchanged", "hello", 5, "hello"},
		{"ascii truncation", "hello world", 5, "hello"},
		{"unicode boundary", "héllo wörld", 7, "héllo w"},
		{"multibyte truncation", "日本語のテキスト", 4, "日本語の"},
		{"em dash and ellipsis", "a—b…c", 3, "a—b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateToRunes(tt.in, tt.max)
			if got != tt.want {
				t.Errorf("truncateToRunes(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
			if n := len([]rune(got)); n > tt.max && tt.max >= 0 {
				t.Errorf("truncateToRunes(%q, %d) returned %d runes, exceeds max", tt.in, tt.max, n)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// truncateWithMarker
// ---------------------------------------------------------------------------

// markerRE matches a complete OmittedMarkerFmt marker at the end of a
// truncated string: "…(N more)" with N the omitted rune count.
var markerRE = regexp.MustCompile(`…\((\d+) more\)$`)

func TestTruncateWithMarker(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		// wantFull: a complete "…(N more)" marker is expected.
		// wantDegrade: the budget is too small for a full marker.
		wantFull, wantDegrade bool
	}{
		{"short fits, no marker", "hello", 10, false, false},
		{"exact fit, no marker", "hello", 5, false, false},
		{"ascii over budget", "hello world truncate me", 14, true, false},
		{"unicode over budget", "héllo wörld truncate me", 12, true, false},
		{"multibyte over budget", "日本語のテキストを切り詰める", 10, true, false},
		{"marker only, no prefix room", "hello world trunc", 10, true, false},
		{"tiny budget degrades", "abcdefghij", 4, false, true},
		{"empty string", "", 10, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateWithMarker(tt.in, tt.max)
			if n := len([]rune(got)); n > tt.max {
				t.Errorf("truncateWithMarker(%q, %d) = %q (%d runes), exceeds max", tt.in, tt.max, got, n)
			}
			switch {
			case tt.wantFull:
				m := markerRE.FindStringSubmatchIndex(got)
				if m == nil {
					t.Fatalf("truncateWithMarker(%q, %d) = %q, want a complete %q marker",
						tt.in, tt.max, got, fmt.Sprintf(OmittedMarkerFmt, 0))
				}
				var n int
				fmt.Sscanf(got[m[2]:m[3]], "%d", &n)
				prefixRunes := len([]rune(got[:m[0]]))
				wantOmitted := len([]rune(tt.in)) - prefixRunes
				if n != wantOmitted {
					t.Errorf("marker reports %d omitted, want %d (prefix %d runes kept)", n, wantOmitted, prefixRunes)
				}
			case tt.wantDegrade:
				if !strings.Contains(got, "…") {
					t.Errorf("degraded result %q must still signal omission", got)
				}
			default:
				if got != tt.in {
					t.Errorf("truncateWithMarker(%q, %d) = %q, want unchanged %q", tt.in, tt.max, got, tt.in)
				}
				if strings.Contains(got, "…") {
					t.Errorf("truncateWithMarker(%q, %d) = %q, unexpected marker for in-budget input", tt.in, tt.max, got)
				}
			}
		})
	}
}

func TestTruncateWithMarkerMarkerOnly(t *testing.T) {
	// max=10 fits the 2-digit marker "…(17 more)" (10 runes) exactly, so
	// the whole budget goes to the marker and no prefix rune is kept.
	got := truncateWithMarker("hello world trunc", 10)
	if n := len([]rune(got)); n > 10 {
		t.Fatalf("result %q has %d runes, exceeds budget", got, n)
	}
	if !markerRE.MatchString(got) {
		t.Errorf("result %q must be a complete marker when no prefix fits", got)
	}
}

// ---------------------------------------------------------------------------
// statusRank
// ---------------------------------------------------------------------------

func TestStatusRankOrder(t *testing.T) {
	// The required client-side sort order from the ticket_list spec.
	wantOrder := []string{"running", "ready", "todo", "review", "blocked", "scheduled", "triage", "done", "archived"}
	if !reflect.DeepEqual(ticketStatusOrder, wantOrder) {
		t.Fatalf("ticketStatusOrder = %v, want %v", ticketStatusOrder, wantOrder)
	}
	for i, s := range wantOrder {
		if got := statusRank(s); got != i {
			t.Errorf("statusRank(%q) = %d, want %d", s, got, i)
		}
	}
}

func TestStatusRankUnknownIsLarge(t *testing.T) {
	got := statusRank("bogus")
	if got <= len(ticketStatusOrder)-1 {
		t.Errorf("statusRank(unknown) = %d, want a value larger than every known rank (max %d)", got, len(ticketStatusOrder)-1)
	}
	// Every known status must rank strictly below an unknown one.
	for _, s := range ticketStatusOrder {
		if statusRank(s) >= got {
			t.Errorf("known status %q ranks %d, must be below unknown rank %d", s, statusRank(s), got)
		}
	}
}

// ---------------------------------------------------------------------------
// Synthetic fixtures
// ---------------------------------------------------------------------------

// Wire-shape mirrors of the plugin's /board and /tasks/{id} responses,
// kept local to the test so this package's tests stay self-contained.
type fixtureBoard struct {
	Columns []struct {
		Name  string            `json:"name"`
		Tasks []json.RawMessage `json:"tasks"`
	} `json:"columns"`
}

type fixtureTaskDetail struct {
	Task struct {
		Body        string            `json:"body"`
		Warnings    []json.RawMessage `json:"warnings"`
		Diagnostics []json.RawMessage `json:"diagnostics"`
	} `json:"task"`
	Comments    []json.RawMessage `json:"comments"`
	Events      []json.RawMessage `json:"events"`
	Attachments []json.RawMessage `json:"attachments"`
	Runs        []json.RawMessage `json:"runs"`
	Links       struct {
		Parents  []string `json:"parents"`
		Children []string `json:"children"`
	} `json:"links"`
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return b
}

func TestBoardFixtureShape(t *testing.T) {
	var fb fixtureBoard
	if err := json.Unmarshal(loadFixture(t, "board_hermes.json"), &fb); err != nil {
		t.Fatalf("board_hermes.json does not decode: %v", err)
	}
	if len(fb.Columns) != 8 {
		t.Fatalf("board fixture has %d columns, want 8 (triage..done, no archived)", len(fb.Columns))
	}
	total := 0
	statuses := map[string]int{}
	for _, col := range fb.Columns {
		statuses[col.Name] = len(col.Tasks)
		total += len(col.Tasks)
	}
	if total < 20 || total > 30 {
		t.Errorf("board fixture has %d tasks, want 20-30", total)
	}
	if len(statuses) < 4 {
		t.Errorf("board fixture spans only %d statuses, want several", len(statuses))
	}
	for _, s := range statuses {
		if s == 0 {
			t.Error("board fixture has an empty column; every status should be exercised")
		}
	}
	// At least one task carries a blocked_reason longer than the 120-rune cap.
	found := false
	for _, col := range fb.Columns {
		for _, raw := range col.Tasks {
			var task struct {
				BlockReason string `json:"block_reason"`
			}
			if err := json.Unmarshal(raw, &task); err != nil {
				t.Fatalf("task in column %q does not decode: %v", col.Name, err)
			}
			if len([]rune(task.BlockReason)) > MaxBlockedReasonChars {
				found = true
			}
		}
	}
	if !found {
		t.Error("board fixture has no blocked_reason longer than MaxBlockedReasonChars")
	}
}

func TestTaskOversizedFixtureShape(t *testing.T) {
	var td fixtureTaskDetail
	if err := json.Unmarshal(loadFixture(t, "task_oversized.json"), &td); err != nil {
		t.Fatalf("task_oversized.json does not decode: %v", err)
	}
	if len(td.Comments) != 100 {
		t.Errorf("oversized fixture has %d comments, want 100", len(td.Comments))
	}
	if len(td.Runs) != 20 {
		t.Errorf("oversized fixture has %d runs, want 20", len(td.Runs))
	}
	if len(td.Attachments) != 10 {
		t.Errorf("oversized fixture has %d attachments, want 10", len(td.Attachments))
	}
	if len(td.Links.Parents)+len(td.Links.Children) != 10 {
		t.Errorf("oversized fixture has %d links, want 10", len(td.Links.Parents)+len(td.Links.Children))
	}
	if len(td.Task.Warnings) != 15 {
		t.Errorf("oversized fixture has %d warnings, want 15", len(td.Task.Warnings))
	}
	if len(td.Task.Diagnostics) != 3 {
		t.Errorf("oversized fixture has %d diagnostics, want 3", len(td.Task.Diagnostics))
	}
	if n := len(td.Task.Body); n != 50000 {
		t.Errorf("oversized fixture body is %d bytes, want exactly 50000", n)
	}
	if len(td.Task.Body) <= MaxTicketBodyChars {
		t.Error("oversized fixture body must exceed MaxTicketBodyChars to exercise truncation")
	}
	// At least one comment body must exceed the comment cap so the
	// truncateWithMarker path is exercised in get output.
	over := 0
	for _, raw := range td.Comments {
		var c struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("comment does not decode: %v", err)
		}
		if len([]rune(c.Body)) > MaxCommentBodyChars {
			over++
		}
	}
	if over == 0 {
		t.Error("oversized fixture has no comment body longer than MaxCommentBodyChars")
	}
}
