package mcptools

// review_queue: the single-call, all-boards scan for tickets awaiting
// human review. The review sweeper previously called ticket_list once per
// board (7+ calls per tick, the main rate-limit pressure); this tool
// returns the same result set in one call.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/RKelln/hermes-kanban-mcp/internal/kanban"
)

// reviewRequiredPrefix is the block_reason prefix ticket_complete stamps
// on review-gated completions ("review-required: <summary>"). A blocked
// ticket whose reason matches this prefix is awaiting human review. The
// colon binds the boundary so an unrelated reason like
// "review-required-urgent" never matches.
const reviewRequiredPrefix = "review-required"

// MaxReviewQueueOutputBytes caps the marshalled review_queue result. It
// aggregates across every board, so it gets the ticket_get-sized read
// budget rather than the single-board ticket_list budget.
const MaxReviewQueueOutputBytes = 8 * 1024

// MaxReviewQueueItems is the maximum number of review-required tickets
// returned in one review_queue call. Beyond it the oldest (lowest
// priority) items are dropped and Truncated is set, mirroring
// ticket_list's limit clamp so the payload always renders as valid JSON.
const MaxReviewQueueItems = 50

// ReviewQueueInput is the review_queue tool input. It takes no
// arguments: the scan covers every non-archived board automatically.
type ReviewQueueInput struct{}

// ReviewQueueItem is one review-required ticket across all boards. Board
// is included so the caller can resolve the ticket with the per-ticket
// tools (which require both board and id).
type ReviewQueueItem struct {
	Board       string `json:"board"`
	ID          string `json:"id"`
	Title       string `json:"title"`
	Status      string `json:"status"`
	Assignee    string `json:"assignee,omitempty"`
	Priority    int    `json:"priority"`
	BlockReason string `json:"block_reason,omitempty"`
}

// ReviewQueueOut is the review_queue success projection. Total is the
// number of review-required tickets found across all boards; Returned is
// how many survived the MaxReviewQueueItems cap and the size budget.
// Truncated is set when either dropped items.
type ReviewQueueOut struct {
	Total     int               `json:"total"`
	Returned  int               `json:"returned"`
	Truncated bool              `json:"truncated,omitempty"`
	Tickets   []ReviewQueueItem `json:"tickets"`
}

// ReviewQueue implements the review_queue MCP tool: every board's blocked
// tickets whose block_reason marks a review-required completion, in one
// call. Tickets are ordered by board slug, then by priority (highest
// first) then id, so the ordering is deterministic and the cap keeps the
// highest-priority items.
func (s *Server) ReviewQueue(ctx context.Context, _ ReviewQueueInput) *ToolResult {
	boards, err := s.ListBoards(ctx, false)
	if err != nil {
		return ErrorResult("%s", RestErrorMessage(err))
	}

	var items []ReviewQueueItem
	for _, b := range boards {
		if b.Slug == "" {
			continue
		}
		var resp boardResponse
		if err := s.doJSON(ctx, http.MethodGet, "/board", url.Values{"board": []string{b.Slug}}, nil, &resp); err != nil {
			return ErrorResult("%s", RestErrorMessage(err))
		}
		for _, col := range resp.Columns {
			for _, t := range col.Tasks {
				if !isReviewRequired(&t) {
					continue
				}
				items = append(items, ReviewQueueItem{
					Board:    b.Slug,
					ID:       t.ID,
					Title:    truncateToRunes(t.Title, 120),
					Status:   t.Status,
					Assignee: t.Assignee,
					Priority: t.Priority,
					// Truncated like ticket_list (parity with today's
					// per-board scans): the structured repo/branch/sha
					// refs are resolved via ticket_get when needed.
					BlockReason: truncateToRunes(t.BlockReason, MaxBlockedReasonChars),
				})
			}
		}
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Board != items[j].Board {
			return items[i].Board < items[j].Board
		}
		if items[i].Priority != items[j].Priority {
			return items[i].Priority > items[j].Priority
		}
		return items[i].ID < items[j].ID
	})

	truncated := false
	total := len(items)
	if len(items) > MaxReviewQueueItems {
		items = items[:MaxReviewQueueItems]
		truncated = true
	}
	return renderReviewQueue(total, items, truncated)
}

// isReviewRequired reports whether a task is awaiting human review: its
// status is blocked and its block_reason carries the review-required
// prefix (colon-bounded) stamped by ticket_complete.
func isReviewRequired(t *kanban.TaskSummary) bool {
	return t.Status == "blocked" &&
		(t.BlockReason == reviewRequiredPrefix || strings.HasPrefix(t.BlockReason, reviewRequiredPrefix+":"))
}

// renderReviewQueue renders a review_queue result within
// MaxReviewQueueOutputBytes, always producing valid JSON. total is the
// number of review-required tickets found across all boards; items are
// what fits after the MaxReviewQueueItems cap and the size budget. If the
// payload overflows, it drops the lowest-priority tickets (the tail of
// the sorted slice), re-marshalling with Truncated set each pass until it
// fits.
func renderReviewQueue(total int, items []ReviewQueueItem, truncated bool) *ToolResult {
	if tr := reviewQueueResult(total, items, truncated); tr != nil {
		return tr
	}
	truncated = true
	work := append([]ReviewQueueItem(nil), items...)
	for {
		if len(work) <= 1 {
			break
		}
		work = work[:len(work)*3/4]
		if tr := reviewQueueResult(total, work, true); tr != nil {
			return tr
		}
	}
	return reviewQueueResult(total, nil, true)
}

// reviewQueueResult renders out as a non-error ToolResult whose
// marshalled envelope is <= MaxReviewQueueOutputBytes, or nil when it does
// not fit. The text is the exact JSON of out — never truncated mid-string.
func reviewQueueResult(total int, items []ReviewQueueItem, truncated bool) *ToolResult {
	text, err := json.Marshal(ReviewQueueOut{Total: total, Returned: len(items), Truncated: truncated, Tickets: items})
	if err != nil {
		return ErrorResult("internal error: %v", err)
	}
	tr := &ToolResult{Content: []ContentPart{{Type: "text", Text: string(text)}}}
	if b, err := json.Marshal(tr); err != nil || len(b) > MaxReviewQueueOutputBytes {
		return nil
	}
	return tr
}
