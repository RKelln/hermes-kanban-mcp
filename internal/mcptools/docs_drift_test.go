package mcptools

// docs_drift_test.go is the grep-gate the t_affa85ca review asked for.
//
// Twice in one change the drift sweep was declared complete while the
// highest-leverage surface (helpDoc, the text the running bridge serves to every
// agent that calls kanban_help) still contradicted the code. The root cause the
// reviewer named is structural: four separate documents describe what a
// completion does and nothing tied them to the behaviour they describe. This
// file is that tie.
//
// It pins exact phrasings, NOT keywords. The docs legitimately still NAME the
// retired shapes in order to warn against them ("does NOT write the older
// 'review-required' block"), so a bare grep for "blocked" or "review-required"
// would fail on correct text. Every phrase below is a prescription that only
// made sense before its fix.
//
// Two retired contracts are covered: the review-required completion block
// (t_affa85ca, 2026-10-16) and the ticket_events wait rule that short-circuited
// on `blocked` (predicate fixed in t_4bc509aa, its four prose surfaces in
// t_ff1915b0). The second family is here because that same class of fossil cost
// one card 769 hours of being blocked before anyone read the card again.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// retiredPhrases are substrings that were live instructions and are now false.
var retiredPhrases = []string{
	// The review-required completion block (t_affa85ca).
	"(review-required block)",                            // helpDoc: "ticket_complete is the older path (review-required block)"
	"review-gated (MCP_COMPLETE_MODE=done overrides)",    // helpDoc lifecycle facts
	"block with `review-required:`",                      // deploy AGENTS template + repo AGENTS.md
	"the ticket lands `blocked`",                         // repo AGENTS.md
	"or leaves `blocked`, on the `ticket_complete` path", // deploy AGENTS template wait line
	"comment + `review-required` block",                  // README knob text
	"block for human review",                             // config.go knob comment
	"review-required block_reason",                       // write_tools.go + shape.go prose

	// The ticket_events wait rule that short-circuited on `blocked`
	// (t_4bc509aa fixed the predicate; t_ff1915b0 the four prose surfaces).
	"left 'blocked' (verdict landed)",      // helpDoc: the removed short-circuit
	"IMMEDIATELY with its",                 // helpDoc, same sentence
	"it returns instantly with its",        // AGENTS.md wait bullet
	"short-circuit once the ticket leaves", // events_tool.go GetTaskEvents doc
}

// proseSurfaces are the files that describe behaviour without executing it. A
// doc comment in a .go file is in this set on purpose: the events_tool.go fossil
// WAS a doc comment, and it survived a review and two sweeps.
var proseSurfaces = []string{
	"../../AGENTS.md",
	"../../README.md",
	"../../deploy/AGENTS.kanban-mcp.template.md",
	"../../internal/config/config.go",
	"../../internal/mcptools/register.go",
	"../../internal/mcptools/events_tool.go",
}

func TestDocs_NoRetiredPhrases(t *testing.T) {
	check := func(name, text string) {
		for _, phrase := range retiredPhrases {
			if strings.Contains(text, phrase) {
				t.Errorf("%s still prescribes a retired contract: %q", name, phrase)
			}
		}
	}
	check("helpDoc (served by kanban_help)", helpDoc)
	for _, rel := range proseSurfaces {
		b, err := os.ReadFile(filepath.Clean(rel))
		if err != nil {
			t.Errorf("cannot read %s: %v (the gate is blind without it)", rel, err)
			continue
		}
		check(rel, string(b))
	}
}

// TestDocs_ServeTheNativeLaneText is the positive half: an agent reading the
// served help must be told where a completion GOES, not only what the old shape
// was. The 2026-10-16 fix shipped a helpDoc that warned against the block while
// the Workflow paragraph still routed agents at it.
func TestDocs_ServeTheNativeLaneText(t *testing.T) {
	if !strings.Contains(helpDoc, "NATIVE lane") {
		t.Error("helpDoc no longer teaches the native review lane; an agent would only know what NOT to do")
	}
}

// TestDocs_ServeTheRealWaitContract is the second positive half: the served help
// must state the wait rule the code implements (events, a status CHANGE from the
// status at call time, or the timeout). Negative-only gates let a doc say
// nothing at all about the contract, which is how the fossil stayed invisible.
func TestDocs_ServeTheRealWaitContract(t *testing.T) {
	for _, want := range []string{"status CHANGING", "timeout"} {
		if !strings.Contains(helpDoc, want) {
			t.Errorf("helpDoc no longer states the real wait contract (no %q); a lane would poll against the removed short-circuit", want)
		}
	}
}
