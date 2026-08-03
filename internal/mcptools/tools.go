// Package mcptools implements the MCP tool layer for the kanban-mcp server.
// It knows MCP schemas and result shaping only: the kanban REST client
// (internal/kanban) and the HTTP middleware (internal/httpauth) are separate
// concerns. Each tool is a typed handler of the form
//
//	func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error)
//
// installed on the server with the SDK's generic mcp.AddTool. Registration
// happens in the wiring layer (cmd/kanban-mcp), not here.
package mcptools

import (
	"context"
	"errors"
	"fmt"

	"github.com/experimance/kanban-mcp/internal/kanban"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// In is the board_list input. include_archived is optional and defaults to
// false; the JSON Schema on the tool definition declares the default so the
// SDK applies it before the handler runs.
type In struct {
	IncludeArchived bool `json:"include_archived"`
}

// Out is the board_list result: every board plus the default board slug
// (KANBAN_DEFAULT_BOARD) that the other tools resolve an omitted `board`
// argument to.
type Out struct {
	Boards       []kanban.Board `json:"boards"`
	DefaultBoard string         `json:"default_board"`
}

// BoardLister is the read surface board_list needs from the kanban backend.
// *kanban.Client satisfies it once the T2 endpoint methods land; tests use
// an in-memory fake.
type BoardLister interface {
	ListBoards(ctx context.Context, includeArchived bool) ([]kanban.Board, error)
}

// BoardListTool returns the board_list tool definition. The input schema is
// minimal by design: a single optional boolean, declared with its default so
// clients see the exact contract.
func BoardListTool() *mcp.Tool {
	return &mcp.Tool{
		Name:        "board_list",
		Description: "List kanban boards with their slug, name, and per-status task counts.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"include_archived": map[string]any{
					"type":    "boolean",
					"default": false,
				},
			},
		},
	}
}

// BoardList returns the typed handler for board_list, bound to a kanban
// backend and the server's default board slug. Wire it with
//
//	mcp.AddTool(srv, mcptools.BoardListTool(), mcptools.BoardList(client, cfg.DefaultBoard))
func BoardList(lister BoardLister, defaultBoard string) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		boards, err := lister.ListBoards(ctx, in.IncludeArchived)
		if err != nil {
			return nil, Out{}, toolError(err)
		}
		if boards == nil {
			// Preserve the documented wire shape: boards is always an array,
			// never null.
			boards = []kanban.Board{}
		}
		return nil, Out{Boards: boards, DefaultBoard: defaultBoard}, nil
	}
}

// toolError renders backend failures as one-line tool errors of the form
// "<kind>: <message>", per the MCP-layer error contract. The SDK packs the
// returned error into the CallToolResult with IsError set, so the model sees
// a recoverable tool error rather than a protocol failure. Errors that are
// not kanban.APIError pass through unchanged.
func toolError(err error) error {
	var apiErr *kanban.APIError
	if errors.As(err, &apiErr) {
		msg := fmt.Sprintf("%s: %s", apiErr.Kind, apiErr.Msg)
		if apiErr.Kind == kanban.KindUnavailable {
			msg += " (retryable: true)"
		}
		return errors.New(msg)
	}
	return err
}
