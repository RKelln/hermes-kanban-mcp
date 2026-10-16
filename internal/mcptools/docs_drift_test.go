package mcptools

// docs_drift_test.go is the grep-gate the t_affa85ca review asked for.
//
// The retired review-required convention survived in the agent-facing docs
// TWICE while the sweep was believed complete, and one of those survivors was
// in this package's own helpDoc — the text the running bridge serves to every
// agent that calls kanban_help. Four separate documents describe what a
// completion does and nothing tied them to the behaviour they describe, so the
// same drift can land again on the next change.
//
// It pins the exact phrasings the fix removed, NOT the words "blocked" or
// "review-required". The docs still legitimately NAME the retired shape in order
// to warn against it ("does NOT write the older 'review-required' block"), so a
// bare keyword grep would fail on correct text. Every phrase below is a
// prescription that only made sense before t_affa85ca.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// retiredReviewPhrases are the exact substrings removed on 2026-10-16. Each one
// was a live instruction; none of them is a legitimate way to describe the
// current behaviour.
var retiredReviewPhrases = []string{
	"(review-required block)",                            // helpDoc: "ticket_complete is the older path (review-required block)"
	"review-gated (MCP_COMPLETE_MODE=done overrides)",    // helpDoc lifecycle facts
	"block with `review-required:`",                      // deploy AGENTS template + repo AGENTS.md
	"the ticket lands `blocked`",                         // repo AGENTS.md
	"or leaves `blocked`, on the `ticket_complete` path", // deploy AGENTS template wait line
	"comment + `review-required` block",                  // README knob text
	"block for human review",                             // config.go knob comment
	"review-required block_reason",                       // write_tools.go + shape.go prose
	"a `review-required` block is a strand, not a queue", // kanban-operations phrasing, kept honest in the skill
}

// agentFacingDocs are the files that tell an agent or an operator what a
// completion does. helpDoc is additionally checked in-process, because that is
// the copy the bridge actually serves.
var agentFacingDocs = []string{
	"../../AGENTS.md",
	"../../README.md",
	"../../deploy/AGENTS.kanban-mcp.template.md",
	"../../internal/config/config.go",
	"../../internal/mcptools/register.go",
}

func TestDocs_NoRetiredReviewPhrases(t *testing.T) {
	check := func(name, text string) {
		for _, phrase := range retiredReviewPhrases {
			if strings.Contains(text, phrase) {
				t.Errorf("%s still prescribes the retired review path: %q", name, phrase)
			}
		}
	}
	check("helpDoc (served by kanban_help)", helpDoc)
	for _, rel := range agentFacingDocs {
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
