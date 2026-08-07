package mcptools

// ticket_list and ticket_get: the read tools. These were the two tools
// never implemented by the worker wave (their tasks' workspaces were
// empty); they were written during the workspace consolidation pass
// (2026-08-03) against the verified API surface (planning/kanban-api-surface.md).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"

	"github.com/RKelln/hermes-kanban-mcp/internal/kanban"
)

// --- ticket_list ---

// TicketListInput is the ticket_list tool input. Board defaults to the
// configured default board; status filters client-side against the
// verified status vocabulary; limit clamps to MaxTicketListLimit.
type TicketListInput struct {
	Board    string   `json:"board"`
	Status   []string `json:"status"`
	Assignee string   `json:"assignee"`
	Limit    int      `json:"limit"`
}

// TicketListItem is the compact per-ticket projection in list output.
// Titles and block reasons are truncated so the marshalled result stays
// inside the hard size budget.
type TicketListItem struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Status      string `json:"status"`
	Assignee    string `json:"assignee,omitempty"`
	Priority    int    `json:"priority"`
	BlockReason string `json:"block_reason,omitempty"`
}

// TicketListOut is the ticket_list success projection.
type TicketListOut struct {
	Board        string           `json:"board"`
	TotalMatched int              `json:"total_matched"`
	Returned     int              `json:"returned"`
	Tickets      []TicketListItem `json:"tickets"`
}

// boardResponse is the GET /board?board=<slug> wire shape: the board is
// returned column-grouped by status, so filtering happens client-side.
type boardResponse struct {
	Columns []struct {
		Name  string               `json:"name"`
		Tasks []kanban.TaskSummary `json:"tasks"`
	} `json:"columns"`
}

// TicketList implements the ticket_list MCP tool.
func (s *Server) TicketList(ctx context.Context, in TicketListInput) *ToolResult {
	board := in.Board
	if board == "" {
		board = s.defaultBoard
	}
	if err := ValidateBoardSlug(board); err != nil {
		return ErrorResult("invalid_input: %v", err)
	}
	if err := ensureKnownBoard(ctx, board); err != nil {
		return ErrorResult("invalid_input: %v", err)
	}
	for _, st := range in.Status {
		if !IsValidStatus(st) {
			return ErrorResult("invalid_input: unknown status %q", st)
		}
	}
	limit := in.Limit
	if limit <= 0 {
		limit = DefaultTicketListLimit
	}
	if limit > MaxTicketListLimit {
		limit = MaxTicketListLimit
	}

	var resp boardResponse
	if err := s.doJSON(ctx, http.MethodGet, "/board", url.Values{"board": []string{board}}, nil, &resp); err != nil {
		return ErrorResult("%s", RestErrorMessage(err))
	}

	matched := make([]TicketListItem, 0, 64)
	for _, col := range resp.Columns {
		for _, t := range col.Tasks {
			if len(in.Status) > 0 && !containsStatus(in.Status, t.Status) {
				continue
			}
			if in.Assignee != "" && t.Assignee != in.Assignee {
				continue
			}
			matched = append(matched, TicketListItem{
				ID:          t.ID,
				Title:       truncateToRunes(t.Title, 120),
				Status:      t.Status,
				Assignee:    t.Assignee,
				Priority:    t.Priority,
				BlockReason: truncateToRunes(t.BlockReason, MaxBlockedReasonChars),
			})
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		return statusRank(matched[i].Status) < statusRank(matched[j].Status)
	})
	total := len(matched)
	if len(matched) > limit {
		matched = matched[:limit]
	}

	out := TicketListOut{Board: board, TotalMatched: total, Returned: len(matched), Tickets: matched}
	return renderResult(MaxTicketListOutputBytes, false, out)
}

func containsStatus(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// --- ticket_get ---

// TicketGetInput is the ticket_get tool input. ID is required; board
// defaults to the configured default board.
type TicketGetInput struct {
	Board string `json:"board"`
	ID    string `json:"id"`
}

// CommentOut is the truncated comment projection in get output.
type CommentOut struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

// TicketGetOut is the ticket_get success projection. Body and comments
// are truncated; heavy sibling arrays (events, attachments, runs,
// warnings) are surfaced as counts plus the truncated flags, keeping the
// result inside the hard 8 KB budget.
type TicketGetOut struct {
	ID             string          `json:"id"`
	Title          string          `json:"title"`
	Status         string          `json:"status"`
	Assignee       string          `json:"assignee,omitempty"`
	Priority       int             `json:"priority"`
	ClaimExpires   int64           `json:"claim_expires,omitempty"`
	BlockReason    string          `json:"block_reason,omitempty"`
	BlockKind      string          `json:"block_kind,omitempty"`
	LatestSummary  string          `json:"latest_summary,omitempty"`
	LastRunSummary string          `json:"last_run_summary,omitempty"`
	BranchName     string          `json:"branch_name,omitempty"`
	Body           string          `json:"body,omitempty"`
	Comments       []CommentOut    `json:"comments,omitempty"`
	EventsCount    int             `json:"events_count"`
	RunsCount      int             `json:"runs_count"`
	AttachmentsN   int             `json:"attachments_count"`
	LinksParents   int             `json:"links_parents"`
	LinksChildren  int             `json:"links_children"`
	WarningsCount  int             `json:"warnings_count"`
	Truncated      TruncationFlags `json:"truncated"`
}

// TruncationFlags reports which fields were clipped so the calling model
// knows more detail exists.
type TruncationFlags struct {
	Body     bool `json:"body,omitempty"`
	Comments bool `json:"comments,omitempty"`
	Titles   bool `json:"titles,omitempty"`
}

// taskDetailEnvelope is the GET /tasks/{id} wire shape: the task dict
// under "task" (with body etc.), plus sibling comment/event/link/run
// arrays.
type taskDetailEnvelope struct {
	Task struct {
		kanban.TaskSummary
		Body          string   `json:"body,omitempty"`
		WorkspaceKind string   `json:"workspace_kind,omitempty"`
		BranchName    string   `json:"branch_name,omitempty"`
		Parents       []string `json:"parents,omitempty"`
		LatestSummary string   `json:"latest_summary,omitempty"`
	} `json:"task"`
	Comments    []kanban.Comment  `json:"comments"`
	Events      []json.RawMessage `json:"events"`
	Attachments []json.RawMessage `json:"attachments"`
	Links       *kanban.Links     `json:"links"`
	Runs        []kanban.Run      `json:"runs"`
	Warnings    []json.RawMessage `json:"warnings"`
	Diagnostics []json.RawMessage `json:"diagnostics"`
}

// TicketGet implements the ticket_get MCP tool.
func (s *Server) TicketGet(ctx context.Context, in TicketGetInput) *ToolResult {
	board := in.Board
	if board == "" {
		board = s.defaultBoard
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

	var env taskDetailEnvelope
	if err := s.doJSON(ctx, http.MethodGet, "/tasks/"+url.PathEscape(in.ID), url.Values{"board": []string{board}}, nil, &env); err != nil {
		var apiErr *kanban.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return ErrorResult("not found: ticket %s on board %s", in.ID, board)
		}
		return ErrorResult("%s", RestErrorMessage(err))
	}

	t := env.Task
	out := TicketGetOut{
		ID:            t.ID,
		Title:         truncateToRunes(t.Title, 120),
		Status:        t.Status,
		Assignee:      t.Assignee,
		Priority:      t.Priority,
		ClaimExpires:  t.ClaimExpires,
		BlockReason:   truncateToRunes(t.BlockReason, MaxBlockedReasonChars),
		BlockKind:     t.BlockKind,
		LatestSummary: truncateToRunes(t.LatestSummary, MaxRunSummaryChars),
		BranchName:    t.BranchName,
		EventsCount:   len(env.Events),
		RunsCount:     len(env.Runs),
		AttachmentsN:  len(env.Attachments),
		WarningsCount: len(env.Warnings),
	}
	// The REST API does not carry block_reason on the task dict — block
	// reasons live in the run summaries (e.g. "review-required: ...").
	// Surface the last run's summary so blocked tickets are never
	// reason-less through the MCP surface.
	if len(env.Runs) > 0 {
		out.LastRunSummary = truncateToRunes(env.Runs[len(env.Runs)-1].Summary, MaxRunSummaryChars)
	}
	if env.Links != nil {
		out.LinksParents = len(env.Links.Parents)
		out.LinksChildren = len(env.Links.Children)
	}
	// Body truncated with an explicit marker.
	if t.Body != "" {
		body, cut := truncateWithMarkerFlag(t.Body, MaxTicketBodyChars)
		out.Body = body
		out.Truncated.Body = cut
	}
	// Comments: newest-last per the API ordering; keep the last N, each
	// body truncated with a marker.
	comments := env.Comments
	if len(comments) > MaxCommentsReturned {
		comments = comments[len(comments)-MaxCommentsReturned:]
		out.Truncated.Comments = true
	}
	for _, c := range comments {
		body, cut := truncateWithMarkerFlag(c.Body, MaxCommentBodyChars)
		out.Comments = append(out.Comments, CommentOut{Author: c.Author, Body: body})
		out.Truncated.Comments = out.Truncated.Comments || cut
	}

	return renderResult(MaxTicketGetOutputBytes, false, out)
}

// truncateWithMarkerFlag is truncateWithMarker plus a "was anything
// omitted" flag.
func truncateWithMarkerFlag(s string, max int) (string, bool) {
	if len([]rune(s)) <= max {
		return s, false
	}
	return truncateWithMarker(s, max), true
}

// renderResult renders v as a ToolResult text under a byte budget that
// may exceed the default 2 KB write-tool cap (read tools have 6 KB /
// 8 KB budgets). It reuses the shrink-to-fit loop from buildResult.
func renderResult(budget int, isErr bool, v any) *ToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return ErrorResult("internal error: %v", err)
	}
	tr := &ToolResult{Content: []ContentPart{{Type: "text", Text: string(b)}}}
	if isErr {
		tr.IsError = true
	}
	for {
		rb, err := json.Marshal(tr)
		if err == nil && len(rb) <= budget {
			return tr
		}
		runes := []rune(tr.Content[0].Text)
		if len(runes) <= 32 {
			return tr
		}
		tr.Content[0].Text = string(runes[:len(runes)*3/4])
	}
}
