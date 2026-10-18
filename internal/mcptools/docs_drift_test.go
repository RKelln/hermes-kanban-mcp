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
//
// The gate has two halves. The negative one is below: no surface may still
// prescribe a retired contract. The positive ones (TestDocs_Serve*) cover the
// other failure mode — a surface that says NOTHING about the contract, which is
// invisible to a grep for retired phrasings. t_6168a2d3 extended the positive
// half to README.md and deploy/install.md and added install.md to the list
// below: README's tool table had described ticket_events without its
// status-change return for as long as the gate existed.

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

	// The triage lane offered as an agent destination (t_6053ef34,
	// 2026-10-18). Two separate fossils: the create flag the bridge
	// advertised, and the one-line claim refusal that named no exit and
	// sent an agent hunting for a promote verb.
	"`triage: true` for triage", // repo AGENTS.md workflow line
	"claim requires ready",      // claim_tool.go's removed generic refusal

	// The 15-minute claim TTL (t_cb0d23c0, 2026-10-18). Every one of these
	// was a true statement about the CLI's built-in default and a false
	// statement about what the bridge asked for: the whole defect was that
	// the docs described the default as the contract.
	"TTL ~15m",                  // helpDoc tool list
	"Claims TTL ~15 min",        // repo AGENTS.md
	"Claim TTL is ~15 minutes",  // README claim mechanism
}

// proseSurfaces are the files that describe behaviour without executing it. A
// doc comment in a .go file is in this set on purpose: the events_tool.go fossil
// WAS a doc comment, and it survived a review and two sweeps. claim_tool.go and
// write_tools.go joined for the same reason (t_6053ef34): the claim refusal and
// the triage flag are both described in prose next to the code that changed.
var proseSurfaces = []string{
	"../../AGENTS.md",
	"../../README.md",
	"../../deploy/AGENTS.kanban-mcp.template.md",
	"../../deploy/install.md",
	"../../internal/config/config.go",
	"../../internal/mcptools/register.go",
	"../../internal/mcptools/claim_tool.go",
	"../../internal/mcptools/write_tools.go",
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

// TestDocs_ProseSurfacesNameTheStatusChange is the third positive half, over
// the surfaces that are NOT the served help. README's tool table described
// ticket_events as "returns events newer than since_event_id or empty on
// timeout": every retired phrase absent, and still wrong about the return a
// review-lane waiter most needs — which is exactly what a negative-only gate
// cannot see. deploy/install.md is included because the smoke test it
// documents pins the tool roster and nothing about the wait, so a binary can
// pass that test while serving the wrong contract (the state the deployed
// bridge was in when t_ff1915b0 was reviewed).
func TestDocs_ProseSurfacesNameTheStatusChange(t *testing.T) {
	for _, rel := range []string{"../../README.md", "../../deploy/install.md"} {
		b, err := os.ReadFile(filepath.Clean(rel))
		if err != nil {
			t.Errorf("cannot read %s: %v (the gate is blind without it)", rel, err)
			continue
		}
		if !strings.Contains(string(b), "status CHANGING") {
			t.Errorf("%s no longer names the status-change return (%q): a reader would take the wait for an event-only tail and never learn that a verdict writing no event still ends it", rel, "status CHANGING")
		}
	}
}

// TestDocs_ServeTheTriageLaneText is the positive half for the lane the bridge
// used to advertise and cannot use (t_6053ef34). A negative-only gate would pass
// a help doc that simply dropped the triage flag from the schema list: the agent
// that hits `ticket_claim` on a triaged card would still learn nothing about
// what the lane is, and "why can't I create one" would cost another run. Every
// surface an outside lane reads has to say BOTH what triage is and where a card
// that is not yours to start belongs.
func TestDocs_ServeTheTriageLaneText(t *testing.T) {
	surfaces := []struct{ name, text string }{
		{"helpDoc (served by kanban_help)", helpDoc},
	}
	for _, rel := range []string{
		"../../AGENTS.md",
		"../../deploy/AGENTS.kanban-mcp.template.md",
		"../../README.md",
	} {
		b, err := os.ReadFile(filepath.Clean(rel))
		if err != nil {
			t.Errorf("cannot read %s: %v (the gate is blind without it)", rel, err)
			continue
		}
		surfaces = append(surfaces, struct{ name, text string }{rel, string(b)})
	}
	for _, s := range surfaces {
		for _, want := range []string{"internal intake lane", "needs_input"} {
			if !strings.Contains(s.text, want) {
				t.Errorf("%s does not state the triage lane contract (no %q): a reader would take triage for a lane it can use, and the refusal in ticket_claim would read as a missing step", s.name, want)
			}
		}
	}
}

// TestDocs_ServeTheClaimRefusalRule is the positive half for the refusal
// itself. The 2026-10-18 incident was not a missing sentence in the help: it was
// an agent reading "cannot claim" and concluding there must be a companion verb.
// So the served help has to deny the tool that does not exist, and has to say
// what a `todo` refusal actually names.
func TestDocs_ServeTheClaimRefusalRule(t *testing.T) {
	for _, want := range []string{"no promote tool", "parent dependencies"} {
		if !strings.Contains(helpDoc, want) {
			t.Errorf("helpDoc no longer states the claim-refusal rule (no %q): an agent refused on a non-ready ticket would go looking for a verb instead of reading the reason", want)
		}
	}
}

// TestDocs_ServeTheClaimTTLKnob is the positive half for the claim TTL
// (t_cb0d23c0). The knob had been "fixed" twice on the deployment side without
// ever reaching argv, and the docs were why nobody noticed: every surface
// described the CLI's 900-second default as the contract, so a 15-minute expiry
// read as correct behaviour. The fix is only complete if an operator can learn
// the knob's name from the surfaces they read, and an agent can learn where the
// number in its claim response came from. Two surfaces carry the default value
// itself (the served help and the env example), because those are the two an
// operator reads before changing it.
func TestDocs_ServeTheClaimTTLKnob(t *testing.T) {
	type surface struct {
		name        string
		text        string
		pinsDefault bool
	}
	read := func(rel string, pinsDefault bool) (surface, bool) {
		b, err := os.ReadFile(filepath.Clean(rel))
		if err != nil {
			t.Errorf("cannot read %s: %v (the gate is blind without it)", rel, err)
			return surface{}, false
		}
		return surface{name: rel, text: string(b), pinsDefault: pinsDefault}, true
	}

	surfaces := []surface{{name: "helpDoc (served by kanban_help)", text: helpDoc, pinsDefault: true}}
	for _, rel := range []string{
		"../../AGENTS.md",
		"../../README.md",
		"../../deploy/AGENTS.kanban-mcp.template.md",
	} {
		if s, ok := read(rel, false); ok {
			surfaces = append(surfaces, s)
		}
	}
	if s, ok := read("../../deploy/kanban-mcp.env.example", true); ok {
		surfaces = append(surfaces, s)
	}

	for _, s := range surfaces {
		if !strings.Contains(s.text, "MCP_CLAIM_TTL_SECONDS") {
			t.Errorf("%s does not name the claim-TTL knob MCP_CLAIM_TTL_SECONDS: a reader would take whatever TTL the CLI defaults to as the contract, which is how a 15-minute claim survived two deployment-side fixes", s.name)
		}
		if s.pinsDefault && !strings.Contains(s.text, "14400") {
			t.Errorf("%s no longer states the default claim TTL (14400): an operator cannot tell what unset means", s.name)
		}
	}
}
