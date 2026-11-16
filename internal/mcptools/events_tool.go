package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"
)

const (
	defaultEventsTimeoutSeconds = 120
	maxEventsTimeoutSeconds     = 900
	maxReturnedEvents           = 25
	maxEventNoteChars           = 200
	// maxConsecutivePollErrors is how many transient backend failures in
	// a row the long-poll tolerates before aborting. Persistent failures
	// still surface; a single blip does not discard the caller's wait.
	maxConsecutivePollErrors = 3
	// MaxTicketEventsOutputBytes caps the marshalled ticket_events
	// result. It is a read-tool budget (like ticket_list), so events tail
	// output is not squeezed into the 2 KB write-tool cap.
	MaxTicketEventsOutputBytes = 6 * 1024
)

// eventsPollInterval controls the tick between long-poll fetches in
// TicketEvents. Tests may override it to shorten poll-based test
// latencies without changing the public behavior.
var eventsPollInterval = 1 * time.Second

// WakeOnVerdicts and WakeOnAny are the accepted wake_on values. The
// default is WakeOnVerdicts because the tool's documented purpose is
// waiting for a verdict: a review cycle writes its own bookkeeping
// (claimed/spawned/heartbeat) every ~30s, and an MCP client pays a model
// turn for every return. WakeOnAny is the full tail, byte-for-byte the
// behaviour every caller got before the filter existed.
const (
	WakeOnVerdicts = "verdicts"
	WakeOnAny      = "any"
)

// churnEventKinds are the kernel's bookkeeping events: they describe the
// machinery moving, never new information for a waiter. They never end a
// verdicts-mode wait. This is a DENYLIST on purpose — an unknown or newly
// added kind WAKES, because a missed verdict is silent and unrecoverable
// (a waiter sleeps through the thing it was waiting for), while a missed
// noise-suppression is merely visible in the suppressed count.
var churnEventKinds = map[string]bool{
	"created":        true,
	"claimed":        true,
	"reclaimed":      true,
	"assigned":       true,
	"spawned":        true,
	"owner_admitted": true,
	"worker_bound":   true,
	"heartbeat":      true,
}

// settledStatuses are the statuses a waiter can act on. In verdicts mode a
// status CHANGE only ends the wait when the ticket lands on one of them.
// `running` and `review` are deliberately absent: a card sitting in review
// moves to running when the review run's own claim lands, and waking on
// that reports "something happened" when the something is the reviewer
// starting work. The status is still reported on every return, timeout
// included, so a caller always learns where the ticket stands.
var settledStatuses = map[string]bool{
	"done":      true,
	"blocked":   true,
	"ready":     true,
	"triage":    true,
	"archived":  true,
	"scheduled": true,
}

// TicketEventsInput is the ticket_events tool input. id and board are
// required; since_event_id defaults to 0 (only events with id >
// since_event_id are returned); timeout_seconds defaults to 120 and is
// capped at 900 (15 minutes) — long enough to cover a full automated
// review cycle without re-polling.
type TicketEventsInput struct {
	ID             string `json:"id"`
	Board          string `json:"board"`
	SinceEventID   int64  `json:"since_event_id"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	// WakeOn selects which events end the wait. "" and "verdicts" end it
	// on anything that is not kernel bookkeeping (and on a status change
	// to a status a waiter can act on); "any" ends it on every event and
	// every status change, which is the original behaviour.
	WakeOn string `json:"wake_on"`
}

// EventOut is the compact event projection surfaced by ticket_events.
type EventOut struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	CreatedAt int64  `json:"created_at"`
	Note      string `json:"note,omitempty"`
}

// TicketEventsOut is the ticket_events success result. Truncated is set
// when events were dropped to fit the size budget or the maxReturnedEvents
// cap, so callers know to fall back to ticket_get rather than trusting a
// cursor that skipped unseen events. Suppressed counts events that were
// past the cursor but did not match the caller's wake filter (kernel
// bookkeeping, in verdicts mode): they are NOT returned and NOT buffered,
// and the count is the honest accounting a silent drop would deny —
// "nothing to do, but 40 heartbeats went by" is a different fact from
// "nothing happened". It is cumulative since since_event_id and distinct
// within one call: invoking again with the same cursor reports the same
// bookkeeping again, because the question it answers is "how much sits
// between my cursor and now", not "what arrived since my last call".
// TicketStatus is the ticket's status as
// of the last fetch that reported one; a wait that ends because the status
// CHANGED reports the new value here with no events, which is how a caller
// learns a review verdict landed. A degraded fetch that reports no status
// leaves the previous observation in place rather than blanking it.
type TicketEventsOut struct {
	Events       []EventOut `json:"events,omitempty"`
	TimedOut     bool       `json:"timed_out"`
	Truncated    bool       `json:"truncated,omitempty"`
	Suppressed   int        `json:"suppressed,omitempty"`
	TicketStatus string     `json:"ticket_status,omitempty"`
}

// rawEvent is the per-event wire shape inside the task detail envelope's
// events array.
type rawEvent struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	CreatedAt int64           `json:"created_at"`
	Payload   json.RawMessage `json:"payload"`
}

// TicketEvents implements the ticket_events MCP tool: long-poll a
// ticket's event log. The wait ends as soon as either
//
//   - a matching event exists with id > since_event_id (returned in Events), or
//   - the ticket's status has changed from the status observed on the
//     first fetch, in a way the caller must act on (returned as
//     TicketStatus, with no events), or
//   - timeout_seconds elapse (TimedOut).
//
// WHAT COUNTS AS A MATCH is the caller's choice (`wake_on`), and the
// default is the verdicts filter:
//
//   - "verdicts" (default): every kind except the kernel's own bookkeeping
//     (created/claimed/reclaimed/assigned/spawned/owner_admitted/
//     worker_bound/heartbeat) ends the wait, and so does a status change
//     landing on a status a waiter can act on (done/blocked/ready/triage/
//     archived/scheduled). A move into running or review does NOT end it.
//     An unknown kind always ends it: a missed verdict is silent, missed
//     noise-suppression is visible in `suppressed`.
//   - "any": every event after the cursor and every status change ends the
//     wait — the behaviour every caller got before the filter existed.
//
// The filter is not cosmetic. A review cycle writes its own bookkeeping
// every ~30s, so an unfiltered waiter returns dozens of times per review
// and, for an MCP client, every return is a model turn. The status-change
// rule had the same defect from the other side: a card sitting in `review`
// flips to `running` when the review run's own claim lands, which used to
// read as "the ticket moved" and ended the wait on the reviewer starting.
// `suppressed` counts what the filter swallowed, so "nothing happened" and
// "40 heartbeats happened" stay distinguishable.
//
// The status-change path is what makes this usable for the native review
// lane. A review runs ready -> review -> running -> done|ready, and the
// tool's original rule — return instantly whenever the status is anything
// but "blocked", because a non-blocked ticket must have already left review
// — was written for the BLOCK-based convention, where a review request WAS
// a block. Nothing blocks in the review lane, so that rule fired an instant
// return in every state a waiter needed to wait through, and a caller
// polling for a verdict could never actually wait for one.
//
// A verdict that ALREADY landed is served by the event path, provided the
// caller passes its cursor: the verdict wrote an event (completed for
// APPROVE, the landing-promote for REQUEST_CHANGES, review_requested for the
// request). A caller that polls an already-finished ticket with a fully
// caught-up cursor waits out the timeout and learns the status then. That is
// deliberate: every extra "if the status is X, return early" rule is another
// convention assumption, which is exactly what broke.
//
// TicketStatus is set on every return path, the timeout included: there it
// carries the status of the last fetch that could be read, so "nothing new
// arrived" still tells the caller where the ticket stood. A fetch that
// serves no status does not erase an earlier one.
//
// Transient backend failures during the wait are retried (up to
// maxConsecutivePollErrors in a row) so a single blip does not discard the
// caller's wait; definitive failures (4xx) abort immediately.
//
// Note on the timeout budget: the immediate first fetch runs before the
// deadline starts, so wall-clock can exceed timeout_seconds by up to one
// backend latency; and because the deadline is checked before each poll,
// events arriving in the final partial interval are not observed. Both
// are acceptable for a tail tool (the caller passes its cursor and can
// poll again).
func (s *Server) TicketEvents(ctx context.Context, in TicketEventsInput) *ToolResult {
	board := in.Board
	if board == "" {
		return ErrorResult("invalid_input: board required; pass board")
	}
	if err := ValidateBoardSlug(board); err != nil {
		return ErrorResult("invalid_input: %v", err)
	}
	if err := ValidateTicketID(in.ID); err != nil {
		return ErrorResult("invalid_input: %v", err)
	}
	wakeOn, err := normalizeWakeOn(in.WakeOn)
	if err != nil {
		return ErrorResult("invalid_input: %v", err)
	}
	if err := ensureKnownBoard(ctx, board); err != nil {
		return ErrorResult("invalid_input: %v", err)
	}

	timeout := in.TimeoutSeconds
	if timeout <= 0 {
		timeout = defaultEventsTimeoutSeconds
	}
	if timeout > maxEventsTimeoutSeconds {
		timeout = maxEventsTimeoutSeconds
	}

	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	ticker := time.NewTicker(eventsPollInterval)
	defer ticker.Stop()

	events, status, err := s.GetTaskEvents(ctx, board, in.ID)
	if err != nil {
		return ErrorResult("%s", RestErrorMessage(err))
	}
	// The status the caller found the ticket in. A CHANGE from this is the
	// signal a waiter wants, and it is the only status rule this tool makes:
	// the previous rule ("anything but blocked means the ticket already left
	// review") assumed the block-based convention and made every review-lane
	// state return instantly.
	baseline := status
	// lastStatus is the most recent status the wait could actually READ (a
	// degraded fetch serves an empty status, and an empty value is not an
	// observation). The timeout path reports THIS rather than the first
	// fetch's value: a caller that waited out the full window should learn
	// where the ticket stood when the wait expired. Two cases make that the
	// only signal it can get, and both are the same shape — an empty status
	// disables statusChanged, so the wait runs out with no other news:
	//
	//   - an unreadable (empty) FIRST fetch has no baseline to compare to;
	//   - a degraded fetch that lands LAST would, if tracked literally, ERASE
	//     the status the wait already knew.
	//
	// The second one is why this tracks non-empty observations rather than
	// the last fetch: reporting nothing is strictly worse than reporting a
	// status that was true a few seconds ago.
	lastStatus := status
	filter := newEventFilter(in.SinceEventID, wakeOn)
	collected, truncated, suppressed := filter.collect(events)
	if len(collected) > 0 {
		return renderEvents(collected, truncated, suppressed, false, lastStatus)
	}

	var pollErrs int
	for {
		select {
		case <-ctx.Done():
			return ErrorResult("%s", ctx.Err())
		case <-ticker.C:
			if time.Now().After(deadline) {
				return renderEvents(nil, truncated, suppressed, true, lastStatus)
			}
			polledEvents, current, err := s.GetTaskEvents(ctx, board, in.ID)
			if err != nil {
				if !transientPollError(err) {
					return ErrorResult("%s", RestErrorMessage(err))
				}
				pollErrs++
				if pollErrs >= maxConsecutivePollErrors {
					return ErrorResult("%s", RestErrorMessage(err))
				}
				continue
			}
			pollErrs = 0
			// Keep the last status that could actually be read. A degraded
			// fetch serves an empty status; tracking it literally would discard
			// the one signal a timeout can still report, and that same empty
			// value is what keeps statusChanged from firing, so nothing else
			// would tell the caller anything either.
			if current != "" {
				lastStatus = current
			}
			var pollTruncated bool
			var pollSuppressed int
			collected, pollTruncated, pollSuppressed = filter.collect(polledEvents)
			truncated = truncated || pollTruncated
			suppressed += pollSuppressed
			if len(collected) > 0 {
				return renderEvents(collected, truncated, suppressed, false, lastStatus)
			}
			if waitSettled(baseline, lastStatus, wakeOn) {
				return renderEvents(nil, false, suppressed, false, lastStatus)
			}
		}
	}
}

// normalizeWakeOn validates wake_on and resolves the default. An unknown
// value is REFUSED rather than silently treated as the default: a caller
// that meant "any" and typed it wrong would otherwise wait out every
// timeout and conclude the tool was broken.
func normalizeWakeOn(v string) (string, error) {
	switch v {
	case "":
		return WakeOnVerdicts, nil
	case WakeOnVerdicts, WakeOnAny:
		return v, nil
	default:
		return "", fmt.Errorf("wake_on must be %q or %q, got %q", WakeOnVerdicts, WakeOnAny, v)
	}
}

// matchesWake reports whether an event past the cursor should END the
// caller's wait. Any mode takes everything (the pre-filter behaviour);
// verdicts mode takes everything except the kernel's own bookkeeping,
// including kinds this build has never heard of.
func matchesWake(e rawEvent, wakeOn string) bool {
	if wakeOn == WakeOnAny {
		return true
	}
	return !churnEventKinds[e.Kind]
}

// waitSettled reports whether the ticket's status moved in a way the
// caller must act on. In any mode it is the original rule — any change
// from the baseline. In verdicts mode the new status must also be one a
// waiter can act on, so the review lane's own claim (review -> running)
// does not end the wait; the timeout still reports where the card stood.
func waitSettled(baseline, current, wakeOn string) bool {
	if !statusChanged(baseline, current) {
		return false
	}
	if wakeOn == WakeOnAny {
		return true
	}
	return settledStatuses[current]
}

// statusChanged reports whether the ticket moved out of the state the caller
// found it in — the "something happened while you waited" signal. Requiring
// a KNOWN baseline means an unreadable status on the first fetch degrades to
// a plain event tail rather than a false "it moved".
func statusChanged(baseline, current string) bool {
	return baseline != "" && current != "" && current != baseline
}

// transientPollError reports whether a backend error is worth retrying
// during the long-poll: 5xx and transport/context errors are transient;
// 4xx (not found, schema, auth) are definitive and abort.
func transientPollError(err error) bool {
	var re *RestError
	if errors.As(err, &re) {
		return re.Status >= 500
	}
	return true
}

// GetTaskEvents fetches the raw events array for a ticket from the
// kanban REST backend. It reuses the taskDetailEnvelope decode path
// already exercised by TicketGet. It also returns the task's current
// status: the long-poll keeps the first fetch's value as the baseline for
// its status-change test and reports the newest value to the caller.
// Nothing short-circuits on the ticket being (or leaving) 'blocked'.
func (s *Server) GetTaskEvents(ctx context.Context, board, id string) ([]json.RawMessage, string, error) {
	var env taskDetailEnvelope
	if err := s.doJSON(ctx, "GET", "/tasks/"+url.PathEscape(id), url.Values{"board": []string{board}}, nil, &env); err != nil {
		return nil, "", err
	}
	return env.Events, env.Task.Status, nil
}

// eventFilter applies the caller's cursor and wake filter across the whole
// long-poll. It is STATEFUL for one reason: every poll re-reads the same
// window from the backend, so a stateless suppressed count would count the
// same heartbeat once per tick (40 heartbeats becoming 400). suppressedSeen
// is the high-water mark of ids already counted as suppressed, so the count
// reported for ONE call is the number of distinct events that call
// swallowed.
//
// Across CALLS the count is cumulative since since_event_id, not a delta:
// a caller that reuses its cursor after a timeout sees the same events
// counted again on the next call (measured, t_0e55e8dc review N1). That is
// intended — the number answers "how much bookkeeping sits between my
// cursor and now", which is what a caller deciding whether to widen its
// cursor needs — but it is not "events since your last call", so say which.
type eventFilter struct {
	since          int64
	wakeOn         string
	suppressedSeen int64
}

func newEventFilter(since int64, wakeOn string) *eventFilter {
	return &eventFilter{since: since, wakeOn: wakeOn, suppressedSeen: since}
}

// collect decodes raw JSON events, filters to those with id > since,
// applies the wake filter, extracts the optional payload note, and caps the
// returned slice at maxReturnedEvents (keeping the newest). It reports
// whether MATCHING events were dropped to the cap, so the caller can
// surface a truncated signal instead of silently breaking the cursor
// contract, and how many NEW events it suppressed, which is the only trace
// of the bookkeeping a verdicts-mode wait slept through.
func (f *eventFilter) collect(raw []json.RawMessage) ([]EventOut, bool, int) {
	if len(raw) == 0 {
		return nil, false, 0
	}
	out := make([]EventOut, 0, len(raw))
	suppressed := 0
	for _, r := range raw {
		var e rawEvent
		if err := json.Unmarshal(r, &e); err != nil {
			continue
		}
		if e.ID <= f.since {
			continue
		}
		if !matchesWake(e, f.wakeOn) {
			if e.ID > f.suppressedSeen {
				f.suppressedSeen = e.ID
				suppressed++
			}
			continue
		}
		ev := EventOut{
			ID:        e.ID,
			Kind:      e.Kind,
			CreatedAt: e.CreatedAt,
		}
		if e.Payload != nil {
			var p struct {
				Note string `json:"note"`
			}
			if json.Unmarshal(e.Payload, &p) == nil && p.Note != "" {
				ev.Note = clampRunes(p.Note, maxEventNoteChars)
			}
		}
		out = append(out, ev)
	}
	truncated := false
	if len(out) > maxReturnedEvents {
		out = out[len(out)-maxReturnedEvents:]
		truncated = true
	}
	return out, truncated, suppressed
}

// renderEvents renders a ticket_events result within
// MaxTicketEventsOutputBytes, always producing valid JSON. It builds the
// ToolResult directly (bypassing SuccessResult's 2 KB buildResult clamp,
// which would cut the JSON text mid-string) and, if the payload overflows
// (max-legal notes on many events), shortens notes and then drops the
// oldest events, with Truncated set, re-marshalling each pass until it
// fits. Suppressed is carried through every pass unchanged: it is a count
// of events the caller chose not to be woken by, so size pressure must not
// turn an honest report of them into a silent one.
func renderEvents(events []EventOut, truncated bool, suppressed int, timedOut bool, status string) *ToolResult {
	out := TicketEventsOut{Events: events, TimedOut: timedOut, Truncated: truncated, Suppressed: suppressed, TicketStatus: status}
	if tr := eventResult(out); tr != nil {
		return tr
	}

	truncated = true
	work := append([]EventOut(nil), events...)
	for {
		// Shorten notes to 3/4 and retry.
		changed := false
		for i := range work {
			if n := len([]rune(work[i].Note)); n > 1 {
				work[i].Note = string([]rune(work[i].Note)[:n*3/4])
				changed = true
			}
		}
		if changed {
			if tr := eventResult(TicketEventsOut{Events: work, TimedOut: timedOut, Truncated: true, Suppressed: suppressed, TicketStatus: status}); tr != nil {
				return tr
			}
			continue
		}
		// Notes empty; drop the oldest half and retry.
		if len(work) <= 1 {
			break
		}
		work = work[len(work)/2:]
		if tr := eventResult(TicketEventsOut{Events: work, TimedOut: timedOut, Truncated: true, Suppressed: suppressed, TicketStatus: status}); tr != nil {
			return tr
		}
	}
	// Pathological: nothing fit. Emit the smallest valid result.
	return eventResult(TicketEventsOut{TimedOut: timedOut, Truncated: true, Suppressed: suppressed, TicketStatus: status})
}

// eventResult renders out as a non-error ToolResult whose marshalled
// envelope is <= MaxTicketEventsOutputBytes, or nil when it does not fit.
// The text is the exact JSON of out — never truncated mid-string.
func eventResult(out TicketEventsOut) *ToolResult {
	text, err := json.Marshal(out)
	if err != nil {
		return ErrorResult("internal error: %v", err)
	}
	tr := &ToolResult{Content: []ContentPart{{Type: "text", Text: string(text)}}}
	if b, err := json.Marshal(tr); err != nil || len(b) > MaxTicketEventsOutputBytes {
		return nil
	}
	return tr
}
