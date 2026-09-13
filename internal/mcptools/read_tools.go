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

// TicketListInput is the ticket_list tool input. Board is required;
// status filters client-side against the verified status vocabulary;
// limit clamps to MaxTicketListLimit.
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

// TicketListOut is the ticket_list success projection. All three counts
// are reported so a windowed result is legible: TotalMatched is what the
// filters matched, Returned is what this call carried, and Truncated
// marks that the difference is due to the size budget rather than a
// filter.
type TicketListOut struct {
	Board        string           `json:"board"`
	TotalMatched int              `json:"total_matched"`
	Returned     int              `json:"returned"`
	Truncated    bool             `json:"truncated,omitempty"`
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
		return ErrorResult("invalid_input: board required; pass board")
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
				BlockReason: truncateToRunes(effectiveBlockReason(&t), MaxBlockedReasonChars),
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
	// Drop from the tail (the lowest-priority columns sort last) rather
	// than letting renderResult damage the payload: every drop is counted
	// in Returned vs TotalMatched and flagged.
	for len(out.Tickets) > 0 && resultBytes(out) > MaxTicketListOutputBytes {
		out.Tickets = out.Tickets[:len(out.Tickets)-1]
		out.Returned = len(out.Tickets)
		out.Truncated = true
	}
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

// TicketGetInput is the ticket_get tool input. Board and ID are both
// required; Detail selects the retrieval mode (empty = partial).
type TicketGetInput struct {
	Board string `json:"board"`
	ID    string `json:"id"`
	// Detail is the retrieval mode: DetailPartial (the default) applies
	// the bounded comment/body caps, DetailFull returns complete comment
	// and body text for callers reading long-form content. Any other
	// value is rejected rather than silently treated as partial.
	Detail string `json:"detail"`
}

// CommentOut is the comment projection in get output. Body carries an
// inline "…(N more)" marker when clipped, and Truncated is the
// machine-readable form of the same fact so no clip is ever silent.
type CommentOut struct {
	Author    string `json:"author"`
	Body      string `json:"body"`
	Truncated bool   `json:"truncated,omitempty"`
}

// TicketGetOut is the ticket_get success projection. Fields are
// truncated according to the requested detail mode; heavy sibling arrays
// (events, attachments, runs, warnings) are surfaced as counts plus the
// truncation flags, keeping the result inside the mode's byte budget.
// CommentsTotal/CommentsReturned/CommentsDropped make any loss of the
// comment thread legible to the caller instead of silent.
type TicketGetOut struct {
	ID               string          `json:"id"`
	Title            string          `json:"title"`
	Status           string          `json:"status"`
	Assignee         string          `json:"assignee,omitempty"`
	Priority         int             `json:"priority"`
	ClaimExpires     int64           `json:"claim_expires,omitempty"`
	BlockReason      string          `json:"block_reason,omitempty"`
	BlockKind        string          `json:"block_kind,omitempty"`
	LatestSummary    string          `json:"latest_summary,omitempty"`
	LastRunSummary   string          `json:"last_run_summary,omitempty"`
	BranchName       string          `json:"branch_name,omitempty"`
	Body             string          `json:"body,omitempty"`
	Comments         []CommentOut    `json:"comments,omitempty"`
	CommentsTotal    int             `json:"comments_total"`
	CommentsReturned int             `json:"comments_returned"`
	CommentsDropped  int             `json:"comments_dropped,omitempty"`
	EventsCount      int             `json:"events_count"`
	RunsCount        int             `json:"runs_count"`
	AttachmentsN     int             `json:"attachments_count"`
	LinksParents     int             `json:"links_parents"`
	LinksChildren    int             `json:"links_children"`
	WarningsCount    int             `json:"warnings_count"`
	Detail           string          `json:"detail"`
	Truncated        TruncationFlags `json:"truncated"`
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
		return ErrorResult("invalid_input: board required; pass board")
	}
	if err := ValidateBoardSlug(board); err != nil {
		return ErrorResult("invalid_input: %v", err)
	}
	if err := ValidateTicketID(in.ID); err != nil {
		return ErrorResult("invalid_input: %v", err)
	}
	// Detail is validated BEFORE any backend call: a typo must not cost a
	// round-trip, and a rejected call must not have touched the board.
	proj, ok := projectionFor(in.Detail)
	if !ok {
		return ErrorResult("invalid_input: unknown detail %q (want %q or %q)", in.Detail, DetailPartial, DetailFull)
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
		Detail:        proj.mode,
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

	// Body and comments are projected under the mode's policy: partial
	// applies the bounded caps, full returns complete text. The fitter
	// then guarantees the rendered payload fits the mode's budget, and
	// reports every drop and clip — nothing is ever clipped silently.
	// The fitter writes the projected body, comments, counts and
	// truncation flags into out; its return value reports whether the
	// payload fits and how much of the thread it carried.
	fit := fitGetProjection(&out, t.Body, env.Comments, proj)
	if !fit.Fits {
		// The remaining overage is in fields this tool does not reduce:
		// latest_summary / last_run_summary / block_reason are capped at
		// rune counts by design (MaxRunSummaryChars exists so review refs
		// survive), and JSON escaping can inflate a rune cap past the byte
		// budget. Name the ticket and the real cause — this is an
		// input-size limit, not a projection bug, and the caller cannot
		// discover which ticket it was from a generic message.
		return ErrorResult(
			"oversized ticket %s (status %s): not representable within the %d-byte %s budget after body/comments were reduced to their %d-rune floors; the remaining bulk is in latest_summary/last_run_summary/block_reason, which are capped by rune count rather than by bytes",
			t.ID, t.Status, proj.budget, proj.mode, MinFieldRunes)
	}

	return renderResult(proj.budget, false, out)
}

// commentFit reports how a ticket's comment thread was projected into the
// get-result budget.
type commentFit struct {
	Total   int  // comments the backend served
	Dropped int  // comments omitted (window or budget), oldest first
	Clipped bool // a returned comment body was clipped
	Fits    bool // the rendered payload is within the mode's budget
}

// getProjection is the mode-derived sizing policy for one ticket_get
// call: which caps apply and how large the rendered envelope may be.
type getProjection struct {
	mode          string
	budget        int // byte budget for the rendered tool result
	commentCap    int // per-comment rune cap (0 = complete)
	commentWindow int // max comments returned
	bodyCap       int // ticket body rune cap (0 = complete)
}

// projectionFor maps a ticket_get detail mode onto its sizing policy.
// An unknown mode is rejected (ok = false) instead of being silently
// treated as partial, so a typo cannot quietly cost the caller content.
func projectionFor(detail string) (getProjection, bool) {
	switch detail {
	case "", DetailPartial:
		return getProjection{
			mode:          DetailPartial,
			budget:        MaxTicketGetOutputBytes,
			commentCap:    MaxCommentBodyChars,
			commentWindow: MaxCommentsReturned,
			bodyCap:       MaxTicketBodyChars,
		}, true
	case DetailFull:
		return getProjection{
			mode:          DetailFull,
			budget:        MaxTicketGetFullOutputBytes,
			commentCap:    0,
			commentWindow: MaxCommentsFullReturned,
			bodyCap:       0,
		}, true
	}
	return getProjection{}, false
}

// getComment is the working projection of one source comment: its
// original body plus the rune cap currently applied (0 = uncapped).
// Clipping always re-derives from the original body, so the inline
// "…(N more)" marker reports the true number of omitted runes at every
// stage and markers can never layer.
type getComment struct {
	author string
	orig   string
	cap    int
}

func (c getComment) render() CommentOut {
	body, clipped := c.orig, false
	if c.cap > 0 {
		body, clipped = truncateWithMarkerFlag(c.orig, c.cap)
	}
	return CommentOut{Author: c.author, Body: body, Truncated: clipped}
}

// bodyProjection is the same idea for the ticket body.
type bodyProjection struct {
	orig string
	cap  int // 0 = complete
}

func (b bodyProjection) render() (string, bool) {
	if b.cap <= 0 {
		return b.orig, false
	}
	return truncateWithMarkerFlag(b.orig, b.cap)
}

// current is render()'s text, for size arithmetic.
func (b bodyProjection) current() string {
	s, _ := b.render()
	return s
}

// fitGetProjection fills out's Body and Comments under proj's caps, then
// shrinks the payload until the rendered result fits proj.budget. It
// enforces the two rules the read path must never break:
//
//  1. The newest comments survive. Review verdicts, steers and revision
//     comments live at the tail of the thread, so the fitter drops the
//     OLDEST comments first and clips the newest only when a single
//     comment cannot fit on its own.
//  2. No loss is silent. Every drop and clip is counted here and flagged
//     on the result; the body is clipped only after the comment thread
//     has been reduced as far as it can go.
func fitGetProjection(out *TicketGetOut, body string, comments []kanban.Comment, proj getProjection) commentFit {
	fit := commentFit{Total: len(comments)}
	bp := bodyProjection{orig: body, cap: proj.bodyCap}
	rendered, clipped := bp.render()
	out.Body, out.Truncated.Body = rendered, clipped

	source := comments
	if proj.commentWindow > 0 && len(source) > proj.commentWindow {
		fit.Dropped += len(source) - proj.commentWindow
		source = source[len(source)-proj.commentWindow:]
	}
	live := make([]getComment, 0, len(source))
	for _, c := range source {
		live = append(live, getComment{author: c.Author, orig: c.Body, cap: proj.commentCap})
	}
	// render writes the projection AND its counts/flags. It must do both
	// before any size measurement: the flags themselves occupy envelope
	// bytes, so a payload fitted without them can land a few bytes over
	// the budget the moment they are set.
	render := func() {
		rendered := make([]CommentOut, 0, len(live))
		for _, c := range live {
			oc := c.render()
			if oc.Truncated {
				fit.Clipped = true
			}
			rendered = append(rendered, oc)
		}
		out.Comments = rendered
		out.CommentsTotal = fit.Total
		out.CommentsReturned = len(rendered)
		out.CommentsDropped = fit.Dropped
		out.Truncated.Comments = fit.Clipped || fit.Dropped > 0
	}

	for {
		render()
		if resultBytes(out) <= proj.budget {
			fit.Fits = true
			break
		}
		if len(live) > 1 {
			// Over budget: drop the oldest comment, keep the tail. The
			// window is sacrificed before any text is clipped.
			live = live[1:]
			fit.Dropped++
			continue
		}
		var newest *getComment
		if len(live) == 1 {
			newest = &live[0]
		}
		if !shrinkLargest(newest, &bp, out, proj.budget) {
			// Nothing can give any more: whatever is left over is in
			// fields this fitter does not reduce (the summaries and the
			// identity fields, which are capped at rune counts that JSON
			// escaping can inflate past the byte budget). The caller
			// reports that honestly rather than clipping a field that
			// has nothing left to give.
			break
		}
		out.Body, out.Truncated.Body = bp.render()
	}

	render() // idempotent: guarantees counts/flags match the final text
	return fit
}

// shrinkLargest makes the field that actually caused the overage give way.
// Both the newest comment and the ticket body are shrinkable, but a fixed
// order between them is wrong in one direction or the other: shrinking the
// comment first shreds a comment that would have fitted on its own while the
// BODY carried the overage (and nothing re-expands it afterwards), while
// shrinking the body first throws away the spec when a single comment is the
// oversized field. So whichever field has more runes still to give absorbs
// the cut, and a field is clipped only once it is genuinely the one with
// room to spare.
//
// Returns false when neither field can give any more.
func shrinkLargest(newest *getComment, bp *bodyProjection, out *TicketGetOut, budget int) bool {
	if resultBytes(out) <= budget {
		return false
	}
	commentSlack := 0
	if newest != nil {
		commentSlack = len([]rune(newest.render().Body)) - MinFieldRunes
	}
	bodySlack := len([]rune(bp.current())) - MinFieldRunes
	if commentSlack <= 0 && bodySlack <= 0 {
		return false
	}
	if bodySlack >= commentSlack {
		return shrinkBody(bp, out, budget)
	}
	return shrinkComment(newest, out, budget)
}

// shrinkComment reduces the cap on c until out fits in budget, always
// re-deriving the rendered body from the original comment. It returns
// false when c is not reducible further (or already fits).
func shrinkComment(c *getComment, out *TicketGetOut, budget int) bool {
	current := len([]rune(c.render().Body))
	if current <= MinFieldRunes {
		return false
	}
	size := resultBytes(out)
	if size <= budget {
		return false
	}
	// Keep enough to clear the overage, less headroom for the marker's
	// own runes; fall back to a proportional cut when the overage alone
	// would not change the length (multi-byte runes, JSON escaping).
	next := current - (size - budget) - MarkerHeadroom
	if next >= current {
		next = current - current/4
	}
	if next < MinFieldRunes {
		next = MinFieldRunes
	}
	if next >= current {
		return false
	}
	c.cap = next
	return true
}

// shrinkBody reduces the body cap until out fits in budget, re-deriving
// from the original body so markers never layer. It returns false when
// the body is already at the floor (or already fits).
func shrinkBody(b *bodyProjection, out *TicketGetOut, budget int) bool {
	current := len([]rune(b.current()))
	if current <= MinFieldRunes {
		return false
	}
	size := resultBytes(out)
	if size <= budget {
		return false
	}
	next := current - (size - budget) - MarkerHeadroom
	if next >= current {
		next = current - current/4
	}
	if next < MinFieldRunes {
		next = MinFieldRunes
	}
	if next >= current {
		return false
	}
	b.cap = next
	return true
}

// truncateWithMarkerFlag is truncateWithMarker plus a "was anything
// omitted" flag.
func truncateWithMarkerFlag(s string, max int) (string, bool) {
	if len([]rune(s)) <= max {
		return s, false
	}
	return truncateWithMarker(s, max), true
}

// renderResult renders v as a ToolResult under a byte budget that may
// exceed the default 2 KB write-tool cap (read tools have 6 KB / 8 KB /
// 32 KB budgets).
//
// It never clips the payload to fit. The former shrink-to-fit loop
// chopped the marshalled JSON to 75% repeatedly, which (a) emits invalid
// JSON, so the client cannot parse the result at all, (b) destroys
// whatever was encoded last — for ticket_get that is the newest comments,
// i.e. the review thread — and (c) set no flag, so the loss was invisible
// to the caller. Callers size their projection to the budget instead
// (see fitGetProjection); when a value still does not fit, the caller
// gets an explicit error naming the overage rather than a silently
// damaged payload.
func renderResult(budget int, isErr bool, v any) *ToolResult {
	text, err := json.Marshal(v)
	if err != nil {
		return ErrorResult("internal error: %v", err)
	}
	tr := &ToolResult{Content: []ContentPart{{Type: "text", Text: string(text)}}}
	if isErr {
		tr.IsError = true
	}
	// Measure the result that is actually returned, not just its payload:
	// the content envelope and the isError field occupy bytes too, and
	// measuring anything narrower is how a guard drifts out of agreement
	// with the fitter that sized the value.
	raw, err := json.Marshal(tr)
	if err != nil {
		return ErrorResult("internal error: %v", err)
	}
	if len(raw) > budget {
		return ErrorResult("internal error: shaped result is %d bytes, over the %d-byte budget; this is a projection sizing bug, not a client error", len(raw), budget)
	}
	return tr
}

// unknownResultBytes is reported when a value cannot be marshalled at all.
// It is deliberately larger than any budget: a measurement failure must
// never look like a payload that comfortably fits, or the fitter would stop
// shrinking and a caller would emit something that cannot be rendered.
const unknownResultBytes = 1 << 30

// resultBytes returns the rendered size of v as a tool result — the exact
// measure the budgets are defined against, including the content envelope
// and any JSON escaping the payload's characters incur.
func resultBytes(v any) int {
	text, err := json.Marshal(v)
	if err != nil {
		return unknownResultBytes
	}
	b, err := json.Marshal(ToolResult{Content: []ContentPart{{Type: "text", Text: string(text)}}})
	if err != nil {
		return unknownResultBytes
	}
	return len(b)
}
