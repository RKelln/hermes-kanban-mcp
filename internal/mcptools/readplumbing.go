// Package-level read-tool plumbing shared by ticket_list and ticket_get
// (and reused by the write tools): input validation, the cached known-
// board slug set, consistent backend error mapping, and the tool-error
// result constructor.
package mcptools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/RKelln/hermes-kanban-mcp/internal/kanban"
)

// ensureKnownBoard validates that slug is a known board before a tool
// issues a backend request. Unknown boards return the canonical error
// so the caller can suggest board_list.
func ensureKnownBoard(ctx context.Context, slug string) error {
	if err := ValidateBoardSlug(slug); err != nil {
		return err
	}
	return defaultSlugCache.ensure(ctx, slug)
}

// IsValidStatus reports whether value is one of the nine ValidStatuses.
// Status filtering is client-side (the backend exposes no status query
// parameter), so tools validate callers' status input against this set
// before filtering.
func IsValidStatus(value string) bool {
	for _, s := range ValidStatuses {
		if s == value {
			return true
		}
	}
	return false
}

// BoardLister is the read-only board-listing capability the slug cache
// needs. The Server satisfies it (its ListBoards uses the kanban REST
// backend); the wiring layer installs one via SetBoardLister at
// startup.
type BoardLister interface {
	ListBoards(ctx context.Context, includeArchived bool) ([]kanban.Board, error)
}

// boardSlugCacheTTL is how long the known-board slug set is cached
// before a refresh is forced.
const boardSlugCacheTTL = 60 * time.Second

// boardSlugCache memoises the set of known board slugs for a bounded
// TTL. It refreshes on expiry or on a cache miss; concurrent callers
// share a single refresh (mutex held across the fetch), so parallel
// tool calls never stampede the backend.
type boardSlugCache struct {
	mu      sync.Mutex
	lister  BoardLister
	slugs   map[string]struct{}
	fetched time.Time
	ttl     time.Duration
	now     func() time.Time // injectable clock for tests
}

// newBoardSlugCache constructs a cache with the given lister and TTL.
func newBoardSlugCache(l BoardLister, ttl time.Duration) *boardSlugCache {
	return &boardSlugCache{lister: l, ttl: ttl, now: time.Now}
}

// known returns the cached slug set, refreshing from the backend when
// the cache is empty or has expired. The returned map must not be
// mutated by callers.
func (c *boardSlugCache) known(ctx context.Context) (map[string]struct{}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.slugs != nil && c.now().Sub(c.fetched) < c.ttl {
		return c.slugs, nil
	}
	if c.lister == nil {
		return nil, errors.New("board lister not installed")
	}
	boards, err := c.lister.ListBoards(ctx, false)
	if err != nil {
		return nil, err
	}
	slugs := make(map[string]struct{}, len(boards))
	for _, b := range boards {
		if b.Slug != "" {
			slugs[b.Slug] = struct{}{}
		}
	}
	c.slugs = slugs
	c.fetched = c.now()
	return c.slugs, nil
}

// ensure returns nil when slug is a known board, and the canonical
// unknown-board error otherwise.
func (c *boardSlugCache) ensure(ctx context.Context, slug string) error {
	slugs, err := c.known(ctx)
	if err != nil {
		return err
	}
	if _, ok := slugs[slug]; !ok {
		return fmt.Errorf("unknown board %q; call board_list", slug)
	}
	return nil
}

// defaultSlugCache is the package-wide cache used by knownBoardSlugs
// and ensureKnownBoard. Its lister is installed at startup by the
// wiring layer via SetBoardLister.
var defaultSlugCache = newBoardSlugCache(nil, boardSlugCacheTTL)

// SetBoardLister installs the backend lister backing the known-board
// slug cache. It is called once at startup by the wiring layer; a
// subsequent call drops any cached slugs so the new backend is
// consulted on the next request.
func SetBoardLister(l BoardLister) {
	defaultSlugCache.mu.Lock()
	defer defaultSlugCache.mu.Unlock()
	defaultSlugCache.lister = l
	defaultSlugCache.slugs = nil
}

// knownBoardSlugs returns the cached set of known board slugs,
// refreshing it from the backend when stale or empty.
func knownBoardSlugs(ctx context.Context) (map[string]struct{}, error) {
	return defaultSlugCache.known(ctx)
}

// readToolError builds an MCP tool error result (IsError: true) with
// the formatted message as its single text content. Expected failures
// (validation, unknown board, backend 404/down) are returned as tool
// results, never as Go errors, so the calling model sees the failure
// text and can self-correct.
func readToolError(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
		IsError: true,
	}
}

// mapTicketError converts a backend error from a ticket read into the
// canonical one-line tool error text:
//
//   - a 404 (from GET tasks/{id}) becomes "not found: ticket <id> on
//     board <slug>";
//   - a transport error (no HTTP status) or any unexpected status
//     becomes "kanban backend unavailable: <status>".
func mapTicketError(err error, id, slug string) string {
	var apiErr *kanban.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Status == http.StatusNotFound {
			return fmt.Sprintf("not found: ticket %s on board %s", id, slug)
		}
		return fmt.Sprintf("kanban backend unavailable: status %d", apiErr.Status)
	}
	return fmt.Sprintf("kanban backend unavailable: %v", err)
}

// ticketReadError wraps mapTicketError into a tool-error result, the
// form read tools return to the client.
func ticketReadError(err error, id, slug string) *mcp.CallToolResult {
	return readToolError("%s", mapTicketError(err, id, slug))
}
