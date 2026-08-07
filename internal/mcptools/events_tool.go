package mcptools

import (
	"context"
	"encoding/json"
	"errors"
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
// cursor that skipped unseen events.
type TicketEventsOut struct {
	Events    []EventOut `json:"events,omitempty"`
	TimedOut  bool       `json:"timed_out"`
	Truncated bool       `json:"truncated,omitempty"`
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
// ticket's event log, returning events with id > since_event_id or an
// empty timed_out result when nothing new arrives within
// timeout_seconds. Transient backend failures during the wait are retried
// (up to maxConsecutivePollErrors in a row) so a single blip does not
// discard the caller's wait; definitive failures (4xx) abort immediately.
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
		board = s.defaultBoard
	}
	if board == "" {
		return ErrorResult("invalid_input: no board specified; pass board or set KANBAN_DEFAULT_BOARD")
	}
	if err := ValidateBoardSlug(board); err != nil {
		return ErrorResult("invalid_input: %v", err)
	}
	if err := ValidateTicketID(in.ID); err != nil {
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

	events, err := s.GetTaskEvents(ctx, board, in.ID)
	if err != nil {
		return ErrorResult("%s", RestErrorMessage(err))
	}
	collected, truncated := collectEvents(events, in.SinceEventID)
	if len(collected) > 0 {
		return renderEvents(collected, truncated, false)
	}

	var pollErrs int
	for {
		select {
		case <-ctx.Done():
			return ErrorResult("%s", ctx.Err())
		case <-ticker.C:
			if time.Now().After(deadline) {
				return renderEvents(nil, truncated, true)
			}
			events, err := s.GetTaskEvents(ctx, board, in.ID)
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
			collected, truncated = collectEvents(events, in.SinceEventID)
			if len(collected) > 0 {
				return renderEvents(collected, truncated, false)
			}
		}
	}
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
// already exercised by TicketGet.
func (s *Server) GetTaskEvents(ctx context.Context, board, id string) ([]json.RawMessage, error) {
	var env taskDetailEnvelope
	if err := s.doJSON(ctx, "GET", "/tasks/"+url.PathEscape(id), url.Values{"board": []string{board}}, nil, &env); err != nil {
		return nil, err
	}
	return env.Events, nil
}

// collectEvents decodes raw JSON events, filters to those with id >
// sinceID, extracts the optional payload note, and caps the returned
// slice at maxReturnedEvents (keeping the newest). The second return
// value reports whether events were dropped to the cap, so the caller
// can surface a truncated signal instead of silently breaking the cursor
// contract.
func collectEvents(raw []json.RawMessage, sinceID int64) ([]EventOut, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	out := make([]EventOut, 0, len(raw))
	for _, r := range raw {
		var e rawEvent
		if err := json.Unmarshal(r, &e); err != nil {
			continue
		}
		if e.ID <= sinceID {
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
	return out, truncated
}

// renderEvents renders a ticket_events result within
// MaxTicketEventsOutputBytes, always producing valid JSON. It builds the
// ToolResult directly (bypassing SuccessResult's 2 KB buildResult clamp,
// which would cut the JSON text mid-string) and, if the payload overflows
// (max-legal notes on many events), shortens notes and then drops the
// oldest events, with Truncated set, re-marshalling each pass until it
// fits.
func renderEvents(events []EventOut, truncated, timedOut bool) *ToolResult {
	out := TicketEventsOut{Events: events, TimedOut: timedOut, Truncated: truncated}
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
			if tr := eventResult(TicketEventsOut{Events: work, TimedOut: timedOut, Truncated: true}); tr != nil {
				return tr
			}
			continue
		}
		// Notes empty; drop the oldest half and retry.
		if len(work) <= 1 {
			break
		}
		work = work[len(work)/2:]
		if tr := eventResult(TicketEventsOut{Events: work, TimedOut: timedOut, Truncated: true}); tr != nil {
			return tr
		}
	}
	// Pathological: nothing fit. Emit the smallest valid result.
	return eventResult(TicketEventsOut{TimedOut: timedOut, Truncated: true})
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
