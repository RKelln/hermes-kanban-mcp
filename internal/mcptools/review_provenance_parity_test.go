package mcptools

// Opt-in parity harness for the pre-check's mirror of the kernel's
// re-review provenance rule (kernelPriorReviewer in review_tool.go).
//
// The scripted REST fixture in review_tool_test.go can only assert the values
// this tool chooses. The value that can STRAND a review row is the one the
// KERNEL chooses — the reviewer recorded by the ticket's newest
// `changes_requested` run, which overrides the assignee — and nothing in the
// unit suite could express "the kernel will land a name nobody requested".
//
// This harness closes that gap against the kernel itself. It is skipped
// unless KANBAN_MCP_PARITY_FIXTURE points at a corpus produced by
// scripts/kernel-provenance-parity.py, which runs the kernel's own
// hermes_cli.kanban_db._prior_reviewer over every board database (plus
// mutated copies covering the malformed-provenance paths the live data does
// not contain) and records its verdict per ticket:
//
//	python3 scripts/kernel-provenance-parity.py --out /tmp/parity.json
//	KANBAN_MCP_PARITY_FIXTURE=/tmp/parity.json \
//	    go test ./internal/mcptools -run TestKernelProvenanceParity -v
//
// Parity claim, and the only one the pre-check needs: this tool reports the
// reviewer the kernel LANDS exactly when the kernel reports a usable one — the
// same name, in the canonicalised form request_review writes
// (_canonical_assignee: strip + lowercase; the kernel's _prior_reviewer
// returns the raw payload string) — and reports no reviewer exactly when the
// kernel reports None (a first review) or False (unusable provenance). A
// mismatch means the mirror has drifted from kanban_db.py and the guard either
// refuses a request that would have worked or blesses one that strands the
// card.

import (
	"encoding/json"
	"os"
	"testing"
)

type parityCase struct {
	Board      string `json:"board"`
	ID         string `json:"id"`
	Origin     string `json:"origin"`
	KernelKind string `json:"kernel_kind"`
	Want       string `json:"want"`
	WantOK     bool   `json:"want_ok"`
	Envelope   struct {
		Task struct {
			ID       string `json:"id"`
			Status   string `json:"status"`
			Assignee string `json:"assignee"`
		} `json:"task"`
		Runs   []rawRun   `json:"runs"`
		Events []rawEvent `json:"events"`
	} `json:"envelope"`
}

func TestKernelProvenanceParity(t *testing.T) {
	fixture := os.Getenv("KANBAN_MCP_PARITY_FIXTURE")
	if fixture == "" {
		t.Skip("set KANBAN_MCP_PARITY_FIXTURE to a corpus from scripts/kernel-provenance-parity.py")
	}
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read corpus %s: %v", fixture, err)
	}
	var cases []parityCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("decode corpus %s: %v", fixture, err)
	}
	if len(cases) == 0 {
		t.Fatal("corpus is empty")
	}
	kinds := map[string]int{}
	for _, c := range cases {
		got, ok := kernelPriorReviewer(c.Envelope.Runs, c.Envelope.Events)
		if got != c.Want || ok != c.WantOK {
			t.Errorf("%s %s (%s, kernel=%s): kernelPriorReviewer = (%q, %v), want (%q, %v)",
				c.Board, c.ID, c.Origin, c.KernelKind, got, ok, c.Want, c.WantOK)
		}
		kinds[c.KernelKind]++
	}
	t.Logf("parity over %d tickets: %v", len(cases), kinds)
}
