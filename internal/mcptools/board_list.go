package mcptools

// board_list as a new-generation Server method (the old typed-handler
// version in tools.go was removed during the workspace consolidation).

import (
	"context"
	"net/http"
	"net/url"

	"github.com/RKelln/hermes-kanban-mcp/internal/kanban"
)

// BoardListInput is the board_list tool input. include_archived is
// optional and defaults to false.
type BoardListInput struct {
	IncludeArchived bool `json:"include_archived"`
}

// BoardListOut is the board_list result: every board plus the default
// board slug that the other tools resolve an omitted `board` argument
// to.
type BoardListOut struct {
	Boards       []kanban.Board `json:"boards"`
	DefaultBoard string         `json:"default_board"`
}

// boardsEnvelope is the GET /boards wire shape.
type boardsEnvelope struct {
	Boards []kanban.Board `json:"boards"`
}

// ListBoards satisfies the BoardLister interface used by the known-board
// slug cache (SetBoardLister installs the Server itself at startup).
func (s *Server) ListBoards(ctx context.Context, includeArchived bool) ([]kanban.Board, error) {
	q := url.Values{}
	if includeArchived {
		q.Set("include_archived", "true")
	}
	var env boardsEnvelope
	if err := s.doJSON(ctx, http.MethodGet, "/boards", q, nil, &env); err != nil {
		return nil, err
	}
	if env.Boards == nil {
		// Preserve the documented wire shape: boards is always an array,
		// never null.
		env.Boards = []kanban.Board{}
	}
	return env.Boards, nil
}

// BoardList implements the board_list MCP tool.
func (s *Server) BoardList(ctx context.Context, in BoardListInput) *ToolResult {
	boards, err := s.ListBoards(ctx, in.IncludeArchived)
	if err != nil {
		return ErrorResult("%s", RestErrorMessage(err))
	}
	return SuccessResult(BoardListOut{Boards: boards, DefaultBoard: s.defaultBoard})
}
