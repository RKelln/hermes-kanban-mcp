// Package mcptools implements the kanban MCP read tools (ticket_list,
// ticket_get) with hard output-size caps and explicit truncation
// discipline. Every size limit, count cap, and truncation marker used by
// the tools lives here as a named constant so the budgets are auditable
// in one place and unit-testable without a live backend.
package mcptools

import "fmt"

// List-limit and output-size budgets. The byte budgets are hard: a
// marshalled tool result larger than the budget fails the size-budget
// regression test, because MCP tool results consume session context on
// every turn.
const (
	// DefaultTicketListLimit is applied when ticket_list is called
	// without an explicit limit.
	DefaultTicketListLimit = 25

	// MaxTicketListLimit clamps any requested limit; values above it
	// are treated as MaxTicketListLimit.
	MaxTicketListLimit = 50

	// MaxTicketListOutputBytes caps the marshalled ticket_list result.
	MaxTicketListOutputBytes = 6 * 1024

	// MaxTicketGetOutputBytes caps the marshalled ticket_get result
	// even on a maximally oversized source ticket.
	MaxTicketGetOutputBytes = 8 * 1024

	// MaxTicketGetFullOutputBytes caps the marshalled ticket_get result
	// in full detail mode. Full mode is an explicit opt-in: the caller
	// asked for complete content, so the budget is larger than partial's
	// — but it stays bounded, because no single call may push an
	// unbounded ticket into the caller's context.
	MaxTicketGetFullOutputBytes = 32 * 1024

	// MaxBlockedReasonChars truncates the blocked_reason surfaced in
	// list output.
	MaxBlockedReasonChars = 120

	// MaxRunSummaryChars truncates the latest/last run summary surfaced
	// in ticket_get output. It must be large enough to hold a
	// review-required block_reason at the maximum legal ref lengths
	// (summary[:100] + " | repo: X; branch: Y; sha: Z" with each ref at
	// its length cap ≈ 719 runes), so the sha tail survives read-back
	// for reviewers on hosts without the checkout.
	MaxRunSummaryChars = 1024

	// MaxBranchNameChars caps the branch_name identity field. Real branch
	// names are short; this only guards against a garbage value from the
	// backend, which would otherwise be the one unbounded field in the
	// projection (and would blow the whole envelope on its own).
	MaxBranchNameChars = 256

	// MaxTicketBodyChars truncates the ticket body in get output.
	MaxTicketBodyChars = 4000

	// MaxCommentBodyChars truncates each returned comment body,
	// marker included.
	MaxCommentBodyChars = 500

	// MaxCommentsReturned keeps only the last N comments in get output.
	MaxCommentsReturned = 10

	// MaxCommentsFullReturned is the comment window in full detail mode.
	// Full mode exists to make long-form comments readable, so its window
	// is wider than partial's; the size budget still governs, dropping
	// the oldest comments first.
	MaxCommentsFullReturned = 50

	// MinFieldRunes is the floor a clipped field (comment body or ticket
	// body) is reduced to before the fitter stops trying. Below this the
	// text carries no usable meaning, so the honest outcome is an explicit
	// oversized-payload error rather than a symbol soup.
	MinFieldRunes = 120

	// MaxEventsReturned keeps only the last N events in get output.
	MaxEventsReturned = 5

	// MaxAttachmentsNames caps the attachment names surfaced in get output.
	MaxAttachmentsNames = 5

	// MaxLinksNames caps the parent/child names surfaced in get output.
	MaxLinksNames = 5

	// MaxWarningsReturned keeps only the first N warnings in get output.
	MaxWarningsReturned = 3

	// OmittedMarkerFmt is the inline marker appended by truncateWithMarker;
	// the %d is the number of runes omitted from the source.
	OmittedMarkerFmt = "…(%d more)"

	// MarkerHeadroom is the worst-case rune length of an omission marker:
	// OmittedMarkerFmt's fixed part plus the longest decimal an int can
	// print. The shrinkers reserve this much before clipping, so the
	// marker they add cannot push the payload back over budget.
	// Over-reserving is free (the loop converges); under-reserving is not.
	MarkerHeadroom = 32
)

// ticket_get detail modes. Partial is the default and keeps the bounded
// caps, tuned for scanning and for context economy. Full is an explicit
// opt-in that returns complete comment and body text, for callers reading
// long-form content — revision comments and review verdicts — that
// partial mode clips. Truncation is reported in every mode: no caller
// ever has to guess whether what it received was complete.
const (
	DetailPartial = "partial"
	DetailFull    = "full"
)

// ValidStatuses is the full set of statuses a task can take, in the order
// used for error messages and status validation. The dashboard's board
// endpoint only returns the eight active columns (archived tasks are
// excluded unless explicitly requested), so status filtering is done
// client-side against this list.
var ValidStatuses = []string{
	"triage", "todo", "scheduled", "ready", "running", "blocked", "review", "done", "archived",
}

// ticketStatusOrder is the client-side sort order for ticket_list: work
// in flight and actionable columns first, finished/archived last.
var ticketStatusOrder = []string{
	"running", "ready", "todo", "review", "blocked", "scheduled", "triage", "done", "archived",
}

// truncateToRunes returns at most max runes of s. The result never
// exceeds max runes and carries no truncation marker; callers that need
// to signal omission must use truncateWithMarker or an explicit
// *_truncated/_omitted field.
func truncateToRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// truncateWithMarker returns s unchanged when it fits within max runes.
// When s exceeds max, it returns a prefix of s plus the OmittedMarkerFmt
// marker "…(N more)", where N is the number of runes actually omitted,
// such that the total rune length of the result never exceeds max. If the
// budget is so small that even the marker alone cannot fit, a truncated
// marker is returned instead (the result still signals omission).
func truncateWithMarker(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	// The marker's own length depends on how many runes it reports, and
	// that count depends on how much prefix we keep, so converge on a
	// prefix that leaves room for an accurate marker. Each non-returning
	// iteration shrinks the prefix (prefix' = max - mr < prefix), so the
	// loop terminates.
	prefix := max
	for {
		omitted := len(r) - prefix
		marker := fmt.Sprintf(OmittedMarkerFmt, omitted)
		mr := len([]rune(marker))
		if prefix+mr <= max {
			return string(r[:prefix]) + marker
		}
		prefix = max - mr
		if prefix < 0 {
			// Budget too small for the marker itself; emit a truncated
			// marker so the reader still sees that content was omitted.
			return truncateToRunes(marker, max)
		}
	}
}

// statusRank returns the position of status in ticketStatusOrder, used
// to sort mixed-status task lists by column priority. Unknown statuses
// return a rank larger than every known status so they sort last and
// never displace valid ones.
func statusRank(status string) int {
	for i, s := range ticketStatusOrder {
		if s == status {
			return i
		}
	}
	return len(ticketStatusOrder)
}
