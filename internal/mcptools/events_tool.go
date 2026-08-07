package mcptools

import (
	"context"
	"encoding/json"
	"net/url"
	"time"
)

const (
	defaultEventsTimeoutSeconds = 30
	maxEventsTimeoutSeconds     = 120
	maxReturnedEvents           = 25
	maxEventNoteChars           = 200
)

// eventsPollInterval controls the tick between long-poll fetches in
// TicketEvents. Tests may override it to shorten poll-based test
// latencies without changing the public behavior.
var eventsPollInterval = 1 * time.Second

// TicketEventsInput is the ticket_events tool input. id and board are
// required; since_event_id defaults to 0 (only events with id >
// since_event_id are returned); timeout_seconds defaults to 30 and is
// capped at maxEventsTimeoutSeconds.
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

// TicketEventsOut is the ticket_events success result.
type TicketEventsOut struct {
	Events   []EventOut `json:"events,omitempty"`
	TimedOut bool       `json:"timed_out"`
	Note     string     `json:"note,omitempty"`
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
// timeout_seconds.
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
	collected := collectEvents(events, in.SinceEventID)
	if len(collected) > 0 {
		return SuccessResult(TicketEventsOut{Events: collected})
	}

	for {
		select {
		case <-ctx.Done():
			return ErrorResult("%s", ctx.Err())
		case <-ticker.C:
			if time.Now().After(deadline) {
				return SuccessResult(TicketEventsOut{TimedOut: true})
			}
			events, err := s.GetTaskEvents(ctx, board, in.ID)
			if err != nil {
				return ErrorResult("%s", RestErrorMessage(err))
			}
			collected := collectEvents(events, in.SinceEventID)
			if len(collected) > 0 {
				return SuccessResult(TicketEventsOut{Events: collected})
			}
		}
	}
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
// slice at maxReturnedEvents (keeping the newest).
func collectEvents(raw []json.RawMessage, sinceID int64) []EventOut {
	if len(raw) == 0 {
		return nil
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
	if len(out) > maxReturnedEvents {
		out = out[len(out)-maxReturnedEvents:]
	}
	return out
}
