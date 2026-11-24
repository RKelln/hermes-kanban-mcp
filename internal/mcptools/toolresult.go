package mcptools

// toolresult.go holds the structured half of an identity refusal.
//
// Why it exists: the refusals a remote lane can actually REPAIR used to be
// narratives. A model reading one re-derived the whole handoff instead of
// changing one field, and an operator reading the log saw a fault statement
// where the contract was being restated (t_03346236: a lane sent a complete
// summary, got "no implementer label resolved … pass worker, or set
// MCP_CLAIM_WORKER", and only a re-typed second call succeeded).
//
// The rule this file encodes:
//
//   - the PROSE arm stays first, so a client that ignores structured content
//     still sees a message;
//   - the JSON arm names the repair: {code, field, message, fix}, where fix
//     is a partial-args object the caller merges into the call it just made;
//   - `message` is ONE sentence under 200 runes, because the consequence
//     belongs in helpDoc — the contract a client reads BEFORE calling, not a
//     paragraph it reads after failing.
//
// This is not a general error envelope. Ordinary validation and backend
// failures keep ErrorResult's single prose block; only the refusals whose
// repair is MECHANICAL get a second block — the identity refusals in
// review_tool.go (change one argument) and the capability refusal in
// capability.go (present a different token).

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The five identity-refusal codes. They are part of the served contract:
// helpDoc names them, and review_tool_test.go pins each one to the input that
// still refuses. Adding a code is a contract change, not a refactor.
const (
	// refusalMissingWorker: no source supplied an implementer label.
	refusalMissingWorker = "missing_worker"
	// refusalWorkerIsProfile: the label is an installed profile, which the
	// dispatcher would spawn when a request-changes verdict lands the card.
	refusalWorkerIsProfile = "worker_is_profile"
	// refusalWorkerEqualsReviewer: the kernel records the assignee as the
	// implementer only when it DIFFERS from the reviewer.
	refusalWorkerEqualsReviewer = "worker_equals_reviewer"
	// refusalRosterUnreadable: GET /profiles did not answer, so neither
	// direction of the roster check could be made. Transient: retry.
	refusalRosterUnreadable = "roster_unreadable"
	// refusalReviewerUnresolved: no reviewer could be settled, or the one
	// settled is not an installed profile the dispatcher can spawn.
	refusalReviewerUnresolved = "reviewer_unresolved"
)

// maxRefusalMessageRunes is the one-sentence budget. It is enforced at
// construction rather than left to review: a message that needs a second
// sentence is failing to respect the delivery shape this file exists for, and
// the consequence text has a home in helpDoc.
const maxRefusalMessageRunes = 200

// identityRepair is the machine-readable arm of a refusal. Field names the
// argument to change (`worker` or `reviewer`); Fix carries that argument and
// the value (or placeholder) a retry should use.
type identityRepair struct {
	Code    string            `json:"code"`
	Field   string            `json:"field"`
	Message string            `json:"message"`
	Fix     map[string]string `json:"fix"`
}

// identityRefusal builds an IsError result with two text blocks: the prose
// message (prefixed `invalid_input:` like every other tool refusal) and the
// JSON repair. detail is optional dynamic data — an installed roster, a
// backend error — appended to the prose arm only; it never enters `message`,
// because it is host state, not a second sentence of contract.
//
// A shape mistake (unknown code, wrong field, empty or mismatched fix,
// semicolon chain) is returned as a loud internal error, never as a
// downgraded refusal: a silently prose-only refusal is exactly the defect this
// file removes. An overlong message is the one input that is cut rather than
// refused, and the cut is visible — the repair arm must survive even a
// pathological label.
func identityRefusal(code, field, message, detail string, fix map[string]string) *ToolResult {
	if !validRefusalCode(code) {
		return refusalBug("unknown identity refusal code %q", code)
	}
	if field != "worker" && field != "reviewer" {
		return refusalBug("identity refusal %s names field %q, want worker or reviewer", code, field)
	}
	if len(fix) == 0 {
		return refusalBug("identity refusal %s has an empty fix", code)
	}
	if _, ok := fix[field]; !ok {
		return refusalBug("identity refusal %s has no %q key in its fix", code, field)
	}
	if n := len([]rune(message)); n > maxRefusalMessageRunes {
		// The budget never breaks the refusal: a pathological label would
		// otherwise turn a repairable refusal into an internal error, which is
		// worse for every client. The marker makes the cut visible and the
		// repair arm below still carries code/field/fix in full.
		message = string([]rune(message)[:maxRefusalMessageRunes-1]) + "…"
	}
	if strings.Contains(message, ";") {
		return refusalBug("identity refusal %s message uses a semicolon-chained clause", code)
	}

	repair, err := json.Marshal(identityRepair{Code: code, Field: field, Message: message, Fix: fix})
	if err != nil {
		return refusalBug("marshal identity repair for %s: %v", code, err)
	}
	prose := "invalid_input: " + message
	if detail != "" {
		prose += " " + detail
	}
	tr := &ToolResult{
		Content: []ContentPart{
			{Type: "text", Text: prose},
			{Type: "text", Text: string(repair)},
		},
		IsError: true,
	}
	// Measure the envelope the client receives, not just the payload: the
	// write-tool budget is 2 KB and a refusal that overran it would take the
	// prose or the repair down with it.
	raw, err := json.Marshal(tr)
	if err != nil {
		return refusalBug("marshal identity refusal result for %s: %v", code, err)
	}
	if len(raw) > MaxToolResultBytes {
		return refusalBug("identity refusal %s renders to %d bytes, over the %d-byte tool budget", code, len(raw), MaxToolResultBytes)
	}
	return tr
}

func validRefusalCode(code string) bool {
	switch code {
	case refusalMissingWorker, refusalWorkerIsProfile, refusalWorkerEqualsReviewer, refusalRosterUnreadable, refusalReviewerUnresolved:
		return true
	}
	return false
}

func refusalBug(format string, args ...any) *ToolResult {
	return ErrorResult("internal error: "+format+"; this is a refusal-authoring bug, not a client error", args...)
}

// rosterDetail renders the installed roster beside a refusal's prose arm. The
// list is host state, so it stays out of the one-sentence message and out of
// the structured arm; it is still served, because the old messages named it
// and losing it would make "not an installed profile" harder to act on. An
// oversized roster is marked, never clipped silently.
func rosterDetail(names []string) string {
	const maxShown = 12
	if len(names) == 0 {
		return ""
	}
	if len(names) > maxShown {
		return fmt.Sprintf("Installed profiles: %s, and %d more.", strings.Join(names[:maxShown], ", "), len(names)-maxShown)
	}
	return "Installed profiles: " + strings.Join(names, ", ") + "."
}
