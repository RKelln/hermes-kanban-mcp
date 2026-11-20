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

	// The status report read as the LAST fetch's status (t_14b74923). The
	// wait tracks the last fetch that REPORTED one: a degraded poll serves an
	// empty status and the previous observation is kept (events_tool.go
	// lastStatus), so a client told "always ... on the last fetch" is told
	// the field is authoritative for a fetch that may have answered nothing.
	"always the ticket's status on the last fetch", // register.go tool description

	// The reviewer fallback that cannot be verified (t_b917c3c6, review
	// F1): request_review does keep the row's existing assignee when no
	// --reviewer is passed and there is no earlier verdict, but the
	// implementer stamp has just moved that assignee to the lane label, so
	// "verify the existing assignee as the reviewer" blessed a value the
	// handoff itself destroyed. These are the phrasings that stated it.
	"provenance, else the existing",       // helpDoc: the removed fallback
	"that value is verified the same way", // helpDoc: the removed conclusion
	"the kernel preserves the assignee",   // the belief review F1 disproved

	// The triage lane offered as an agent destination (t_6053ef34,
	// 2026-10-18). Two separate fossils: the create flag the bridge
	// advertised, and the one-line claim refusal that named no exit and
	// sent an agent hunting for a promote verb.
	"`triage: true` for triage", // repo AGENTS.md workflow line
	"claim requires ready",      // claim_tool.go's removed generic refusal

	// The sdlc-review SKILL named as if it were the reviewer's name
	// (2026-11-16, the togather handoff). `reviewer` is written to the review
	// row's assignee, so it must be a PROFILE; the skill is what a review run
	// force-loads. codex-orchestrator read "spawns the sdlc-review reviewer"
	// out of the served tool description, passed reviewer:"sdlc-review", and
	// lost its entire handoff summary to the roster refusal — a correct
	// refusal, from a sentence that taught the wrong name.
	"spawns the sdlc-review reviewer",     // helpDoc, register.go, write_tools.go
	"spawns the `sdlc-review` reviewer",   // the backticked form
	"sdlc-review`\n    reviewer",          // repo AGENTS.md: wrapped mid-name
	"a dispatched `sdlc-review` reviewer", // README tool table
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

// TestDocs_ServeTheImplementerRule pins the ONE thing an implementer must know
// to get a request-changes verdict back: the handoff records an implementer
// label, it is not a profile, and without it the reviewer can only block.
// (t_b917c3c6.) The positive pin is the only guard that survives a rewrite of
// these paragraphs in a later edit.
func TestDocs_ServeTheImplementerRule(t *testing.T) {
	t.Run("served help", func(t *testing.T) {
		for _, want := range []string{"MCP_CLAIM_WORKER", "IMPLEMENTER", "must NOT be an installed profile"} {
			if !strings.Contains(helpDoc, want) {
				t.Errorf("helpDoc no longer states the implementer rule (no %q)", want)
			}
		}
	})

	// The lane's AGENTS template and the install guide are the two prose
	// surfaces an operator or implementer actually reads.
	surfaces := []struct{ name, text string }{
		{"deploy/AGENTS.kanban-mcp.template.md", ""},
		{"deploy/install.md", ""},
	}
	for i := range surfaces {
		b, err := os.ReadFile(filepath.Join("..", "..", surfaces[i].name))
		if err != nil {
			t.Fatalf("read %s: %v", surfaces[i].name, err)
		}
		surfaces[i].text = string(b)
	}
	for _, s := range surfaces {
		for _, want := range []string{"MCP_CLAIM_WORKER", "implementer"} {
			if !strings.Contains(strings.ToLower(s.text), strings.ToLower(want)) {
				t.Errorf("%s no longer states the implementer rule (no %q)", s.name, want)
			}
		}
	}

	// register.go is the surface an agent actually reads first: it is what
	// tools/list returns for ticket_request_review, in both the tool
	// description and the `worker` property. The parent review
	// (t_cb0d23c0, F2) flagged that this positive half skipped it, so a
	// rewrite that dropped the rule from the served schema would have gone
	// green.
	t.Run("served tool description", func(t *testing.T) {
		b, err := os.ReadFile("register.go")
		if err != nil {
			t.Fatalf("read register.go: %v", err)
		}
		text := string(b)
		for _, want := range []string{
			"IMPLEMENTER label",
			"worker is the IMPLEMENTER label",
			"explicit worker > MCP_CLAIM_WORKER > the label already on the card",
			"must NOT be an installed profile",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("register.go no longer serves the implementer rule to agents (no %q)", want)
			}
		}
	})
}

// TestDocs_ServeTheReviewerPrecedence pins the F1 fix (t_b917c3c6 round 2):
// the reviewer is settled from a source that does not depend on the
// ticket's assignee, before the implementer stamp, and the handoff always
// passes it. A rewrite that restores "the kernel keeps the existing
// assignee" would re-create a review row assigned to the lane label, and
// nothing else in the suite would notice.
func TestDocs_ServeTheReviewerPrecedence(t *testing.T) {
	t.Run("served help", func(t *testing.T) {
		for _, want := range []string{
			"reviewer recorded on the ticket's last changes-requested round",
			"keeps\n    whatever assignee the card carries",
			"REFUSED before the stamp",
		} {
			if !strings.Contains(helpDoc, want) {
				t.Errorf("helpDoc no longer states the reviewer precedence (no %q)", want)
			}
		}
	})

	t.Run("served tool description", func(t *testing.T) {
		b, err := os.ReadFile("register.go")
		if err != nil {
			t.Fatalf("read register.go: %v", err)
		}
		for _, want := range []string{
			"last changes-requested round",
			"always carries --reviewer",
			"REFUSED before the stamp",
		} {
			if !strings.Contains(string(b), want) {
				t.Errorf("register.go no longer serves the reviewer precedence (no %q)", want)
			}
		}
	})

	// The two operator/implementer prose surfaces, each in its own words.
	surfaces := []struct {
		name string
		want []string
	}{
		{"deploy/AGENTS.kanban-mcp.template.md", []string{"last changes-requested round", "refused BEFORE anything is written"}},
		{"deploy/install.md", []string{"settled BEFORE the implementer stamp", "keeps whatever assignee the card carries"}},
		{"deploy/kanban-mcp.env.example", []string{"last changes-requested round", "keeps whatever assignee the card"}},
	}
	for _, s := range surfaces {
		b, err := os.ReadFile(filepath.Join("..", "..", s.name))
		if err != nil {
			t.Fatalf("read %s: %v", s.name, err)
		}
		for _, want := range s.want {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s no longer states the reviewer precedence (no %q)", s.name, want)
			}
		}
	}
}

// TestDocs_ServeTheReviewerIsAProfile pins the one distinction the roster
// refusal enforces and no surface used to state: `reviewer` is a PROFILE name,
// and `sdlc-review` is the SKILL a review run force-loads. (2026-11-16: a
// remote lane read "spawns the sdlc-review reviewer" out of the served tool
// description, passed reviewer:"sdlc-review" on board togather, and the bridge
// refused the handoff — correctly, the card would have sat in 'review'
// unclaimed — but only after the description had taught it the wrong name, and
// the refusal cost the agent its entire summary. The negative half above
// catches the old sentence; this half catches a rewrite that drops the
// distinction while saying nothing false.)
func TestDocs_ServeTheReviewerIsAProfile(t *testing.T) {
	surfaces := []struct{ name, text string }{
		{"helpDoc (served by kanban_help)", helpDoc},
	}
	for _, rel := range []string{
		"../../AGENTS.md",
		"../../README.md",
		"../../deploy/AGENTS.kanban-mcp.template.md",
	} {
		b, err := os.ReadFile(filepath.Clean(rel))
		if err != nil {
			t.Errorf("cannot read %s: %v (the gate is blind without it)", rel, err)
			continue
		}
		surfaces = append(surfaces, struct{ name, text string }{rel, string(b)})
	}
	// register.go is what tools/list serves: the agent never reads the Go
	// constant, only the string it carries.
	if b, err := os.ReadFile("register.go"); err != nil {
		t.Errorf("cannot read register.go: %v", err)
	} else {
		surfaces = append(surfaces, struct{ name, text string }{"register.go (served tool description)", string(b)})
	}

	for _, s := range surfaces {
		low := strings.ToLower(s.text)
		for _, want := range []string{"reviewer profile", "sdlc-review", "skill"} {
			if !strings.Contains(low, want) {
				t.Errorf("%s does not say the reviewer is a profile and that the sdlc-review skill is what a review run loads (no %q): an agent will pass the skill name, and the roster refusal will cost it the whole handoff", s.name, want)
			}
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

// TestDocs_ServeTheWakeFilter pins the ticket_events wake contract
// (t_0e55e8dc). The filter is invisible from the outside: a bridge that
// ignores `wake_on` behaves EXACTLY like the old one for a caller who never
// passes it, so nothing in a smoke test or a green unit suite would notice a
// rewrite that dropped the option from the served schema or the prose. That
// silence is the whole failure mode — an agent told "long-poll until it
// leaves review" against a bridge that wakes on heartbeats burns a model turn
// per heartbeat and concludes the tool is broken, which is the report this
// card came from.
func TestDocs_ServeTheWakeFilter(t *testing.T) {
	// Served help: an agent reads this first, so both halves must be there —
	// the option, and the fact that suppression is COUNTED rather than
	// silent (the house truncation rule applied to a filter).
	for _, want := range []string{"wake_on", "suppressed", "heartbeat"} {
		if !strings.Contains(helpDoc, want) {
			t.Errorf("helpDoc no longer states the ticket_events wake filter (no %q): a client would long-poll against churn and pay a turn per heartbeat", want)
		}
	}

	// register.go is what tools/list serves: the schema property AND its
	// description travel to the client, so a dropped `wake_on` here is a
	// dropped capability even though the handler still supports it.
	t.Run("served tool description", func(t *testing.T) {
		b, err := os.ReadFile("register.go")
		if err != nil {
			t.Fatalf("read register.go: %v", err)
		}
		text := string(b)
		for _, want := range []string{
			`"wake_on"`,
			WakeOnVerdicts,
			WakeOnAny,
			"suppressed counts the events the filter swallowed",
			// The count's SEMANTICS, not just its existence: the review of
			// t_0e55e8dc measured that a client reusing its cursor sees the
			// same bookkeeping counted again, and "distinct" alone reads as
			// "new since your last call". Pin the qualifier so a later
			// shortening cannot quietly change what the number means to the
			// client that reads this description.
			"cumulative since since_event_id",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("register.go no longer serves the wake filter (no %q)", want)
			}
		}
	})

	// The operator/implementer prose surfaces, each in its own words.
	surfaces := []struct {
		name string
		want []string
	}{
		{"../../README.md", []string{"wake_on", "suppressed"}},
		{"../../deploy/install.md", []string{"wake_on", "suppressed"}},
		{"../../deploy/AGENTS.kanban-mcp.template.md", []string{"wake_on", "suppressed"}},
		{"../../AGENTS.md", []string{"wake_on", "suppressed"}},
	}
	for _, s := range surfaces {
		b, err := os.ReadFile(filepath.Clean(s.name))
		if err != nil {
			t.Errorf("cannot read %s: %v (the gate is blind without it)", s.name, err)
			continue
		}
		for _, want := range s.want {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s no longer states the wake filter (no %q)", s.name, want)
			}
		}
	}
}

// TestDocs_ServeTheDefaultAssigneeGuard pins the guard's served contract
// (t_5498c280). An external lane must be told, where it reads, that `default`
// is the kanban HOST's profile and that the bridge refuses it — otherwise the
// refusal is a surprise discovered by a failed create, and the lane reaches for
// the profile that just cost it a handoff's worth of work. Operators need the
// knob on the surfaces they read, or the opt-out is undiscoverable.
func TestDocs_ServeTheDefaultAssigneeGuard(t *testing.T) {
	// Surfaces an AGENT reads before it calls ticket_create.
	surfaces := []struct{ name, text string }{
		{"helpDoc (served by kanban_help)", helpDoc},
	}
	if b, err := os.ReadFile("register.go"); err != nil {
		t.Errorf("cannot read register.go: %v", err)
	} else {
		surfaces = append(surfaces, struct{ name, text string }{"register.go (served tool description)", string(b)})
	}
	for _, s := range surfaces {
		// Case-insensitive on the lane phrase: the served help flows as prose
		// ("this bridge is the external lane"), the tool description shouts it.
		if !strings.Contains(strings.ToLower(s.text), "external lane") {
			t.Errorf("%s does not say this bridge is the external lane: the refusal has no reason attached", s.name)
		}
		for _, want := range []string{"MCP_ALLOW_DEFAULT_ASSIGNEE"} {
			if !strings.Contains(s.text, want) {
				t.Errorf("%s does not state the default-assignee guard (no %q): a lane will assign the host's own profile and learn only from the refusal", s.name, want)
			}
		}
	}

	// Surfaces an OPERATOR reads before changing the setting.
	for _, rel := range []string{
		"../../README.md",
		"../../deploy/kanban-mcp.env.example",
		"../../deploy/AGENTS.kanban-mcp.template.md",
		"../../deploy/install.md",
	} {
		b, err := os.ReadFile(filepath.Clean(rel))
		if err != nil {
			t.Errorf("cannot read %s: %v (the gate is blind without it)", rel, err)
			continue
		}
		if !strings.Contains(string(b), "MCP_ALLOW_DEFAULT_ASSIGNEE") {
			t.Errorf("%s does not name MCP_ALLOW_DEFAULT_ASSIGNEE: neither the operator nor the lane can find the refusal's opt-out", rel)
		}
	}
}

// TestDocs_ServeTheReportedStatusQualifier pins the qualifier the served
// ticket_events description used to omit (t_14b74923). The wait reports the
// status of the last fetch THAT REPORTED ONE: events_tool.go keeps lastStatus
// only from non-empty observations, because a degraded poll serves an empty
// status and blanking the field would erase the one signal a timed-out wait can
// still report. "ticket_status is always the ticket's status on the last fetch"
// told a client the opposite — that the latest fetch is authoritative even when
// it answered nothing. The negative half above catches that old sentence coming
// back; this half catches a rewrite that keeps the sentence but shortens the
// qualifier away, which is how the false claim got there in the first place.
func TestDocs_ServeTheReportedStatusQualifier(t *testing.T) {
	b, err := os.ReadFile("register.go")
	if err != nil {
		t.Fatalf("read register.go: %v", err)
	}
	desc := string(b)
	for _, want := range []string{
		"the last fetch that reported one",
		"leaves the previous value in place",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("register.go no longer serves the ticket_status qualifier (no %q): a client reads the field as the last fetch's status and treats a status-less poll as an observation", want)
		}
	}
}

// TestDocs_TellTheLaneToOmitTheReviewer is the counterpart rule (2026-11-16,
// Ryan): the reviewer is a DEPLOYMENT property, not a lane argument. A lane
// that is told it must name a profile ends up naming the host's own — which is
// exactly the "why is default in my handoff" that reading the roster taught.
// Both served surfaces must say OMIT before they say what a profile is.
func TestDocs_TellTheLaneToOmitTheReviewer(t *testing.T) {
	surfaces := []struct{ name, text string }{
		{"helpDoc (served by kanban_help)", helpDoc},
	}
	if b, err := os.ReadFile("register.go"); err != nil {
		t.Errorf("cannot read register.go: %v", err)
	} else {
		surfaces = append(surfaces, struct{ name, text string }{"register.go (served tool description)", string(b)})
	}
	for _, s := range surfaces {
		if !strings.Contains(s.text, "OMIT") {
			t.Errorf("%s never tells the lane to OMIT reviewer: a lane reading it concludes the profile name is its business", s.name)
		}
		if !strings.Contains(s.text, "operator override") {
			t.Errorf("%s does not call an explicit reviewer an operator override, so omitting it reads as incomplete", s.name)
		}
	}
}

// TestDocs_ServeTheClaimIdentityRule pins the contract of t_7f1caa0a: identity
// is set ONCE, at claim, and the review handoff inherits it. The failure it
// prevents is not a wrong sentence but a MISSING one: a remote lane pushed its
// branch, sent a full handoff summary, and only then learned that the review
// call wanted a second identity argument it had never been told about (board
// `togather`, t_03346236). Every surface an implementer reads before claiming
// has to say where the label comes from, that it is stable per lane, and that
// the review step therefore needs no worker argument — and the two surfaces an
// agent reads (served help, served tool description) also have to say why a
// profile label is refused.
func TestDocs_ServeTheClaimIdentityRule(t *testing.T) {
	t.Run("served help", func(t *testing.T) {
		for _, want := range []string{
			"Identity is set once, at claim",
			"stable per lane",
			"needs no worker",
			"must NOT be an installed profile",
		} {
			if !strings.Contains(helpDoc, want) {
				t.Errorf("helpDoc no longer states the claim identity rule (no %q): a lane supplies the label at claim and then reads a second demand at request-review", want)
			}
		}
	})

	t.Run("served tool description", func(t *testing.T) {
		b, err := os.ReadFile("register.go")
		if err != nil {
			t.Fatalf("read register.go: %v", err)
		}
		text := string(b)
		for _, want := range []string{
			"identity is set once, at claim",
			"stable per lane",
			"needs no worker argument for a card claimed here",
			"must NOT be an installed profile",
			"claimed by the dispatcher within about a minute",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("register.go no longer serves the claim identity rule (no %q): the agent never reads the Go constant, only the string tools/list carries", want)
			}
		}
	})

	// The two prose surfaces an implementer or operator actually reads.
	surfaces := []struct {
		name string
		want []string
	}{
		{"../../deploy/AGENTS.kanban-mcp.template.md", []string{
			"Identity is set once, at claim",
			"stable per lane",
			"needs no `worker` argument for a card claimed here",
			"refused before any CLI call",
		}},
		{"../../README.md", []string{
			"identity is set once, at claim",
			"stable per lane",
			"assigns that label before claiming",
			"would put a second worker on the lane's card",
		}},
	}
	for _, s := range surfaces {
		b, err := os.ReadFile(filepath.Clean(s.name))
		if err != nil {
			t.Errorf("cannot read %s: %v (the gate is blind without it)", s.name, err)
			continue
		}
		for _, want := range s.want {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s no longer states the claim identity rule (no %q): the lane meets a second identity demand after the work is done", s.name, want)
			}
		}
	}
}
