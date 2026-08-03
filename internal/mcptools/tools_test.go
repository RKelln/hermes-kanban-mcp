package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/experimance/kanban-mcp/internal/kanban"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeLister is a minimal in-memory BoardLister. It records every call so
// tests can assert the exact arguments board_list passes through, and serves
// canned boards or a canned error.
type fakeLister struct {
	mu        sync.Mutex
	got       []bool // includeArchived per call
	boards    []kanban.Board
	err       error
	callCount int
	lastCtx   context.Context
}

func (f *fakeLister) ListBoards(ctx context.Context, includeArchived bool) ([]kanban.Board, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCount++
	f.got = append(f.got, includeArchived)
	f.lastCtx = ctx
	if f.err != nil {
		return nil, f.err
	}
	return f.boards, nil
}

// callThroughServer runs board_list through a real SDK server + client over
// in-memory transports, so input decoding, schema defaults, and output
// marshaling all go through the SDK's actual code paths (not a hand-rolled
// decode).
func callThroughServer(t *testing.T, lister BoardLister, args map[string]any) *mcp.CallToolResult {
	t.Helper()

	// Normalize nil to an empty object: a typed-nil map marshals as JSON
	// null, and go-sdk v1.7.0 + jsonschema-go panic on applying schema
	// defaults to a null arguments map. Real clients omit the key or send
	// {}, never null.
	if args == nil {
		args = map[string]any{}
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "kanban-mcp-test", Version: "0"}, nil)
	mcp.AddTool(srv, BoardListTool(), BoardList(lister, "hermes-agent"))

	clientT, serverT := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "board_list", Arguments: args})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	return res
}

// decodeOut converts a CallToolResult's structured content back into the Out
// shape. On the client side StructuredContent arrives as decoded JSON (map /
// []any), not RawMessage, so marshal-then-unmarshal handles both. The SDK
// also renders the same JSON as text content; both must agree.
func decodeOut(t *testing.T, res *mcp.CallToolResult) Out {
	t.Helper()
	if res.IsError {
		t.Fatalf("unexpected tool error: %v", res.Content)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal StructuredContent: %v", err)
	}
	var out Out
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode Out: %v", err)
	}
	return out
}

func TestBoardListDecodesOmittedIncludeArchived(t *testing.T) {
	fake := &fakeLister{boards: []kanban.Board{{Slug: "hermes-agent", Name: "Hermes Agent", Counts: map[string]int{"ready": 1}}}}

	// Omitted argument entirely (empty object on the wire): the SDK schema
	// default (false) must apply. NB: pass a non-nil map — a typed-nil map
	// marshals as JSON null, which hits a jsonschema-go applyDefaults panic
	// in go-sdk v1.7.0 (assignment to entry in nil map); real clients send
	// {} or omit the key, never null.
	args := map[string]any{}
	res := callThroughServer(t, fake, args)
	decodeOut(t, res)

	if fake.callCount != 1 {
		t.Fatalf("ListBoards called %d times, want 1", fake.callCount)
	}
	if fake.got[0] != false {
		t.Errorf("ListBoards includeArchived = %v, want false (default when omitted)", fake.got[0])
	}
	if fake.lastCtx == nil {
		t.Error("ListBoards received a nil context")
	}
}

func TestBoardListDecodesExplicitIncludeArchived(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want bool
	}{
		{"explicit true", map[string]any{"include_archived": true}, true},
		{"explicit false", map[string]any{"include_archived": false}, false},
		{"empty object same as omitted", map[string]any{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeLister{}
			callThroughServer(t, fake, tc.args)
			if fake.callCount != 1 {
				t.Fatalf("ListBoards called %d times, want 1", fake.callCount)
			}
			if fake.got[0] != tc.want {
				t.Errorf("ListBoards includeArchived = %v, want %v", fake.got[0], tc.want)
			}
		})
	}
}

func TestBoardListOutputShape(t *testing.T) {
	fake := &fakeLister{boards: []kanban.Board{
		{Slug: "hermes-agent", Name: "Hermes Agent", Counts: map[string]int{"ready": 3, "running": 1}},
		{Slug: "bard", Name: "Bard", Counts: map[string]int{"todo": 0}},
	}}

	res := callThroughServer(t, fake, nil)
	out := decodeOut(t, res)

	if out.DefaultBoard != "hermes-agent" {
		t.Errorf("default_board = %q, want %q", out.DefaultBoard, "hermes-agent")
	}
	if len(out.Boards) != 2 {
		t.Fatalf("len(boards) = %d, want 2", len(out.Boards))
	}
	b0 := out.Boards[0]
	if b0.Slug != "hermes-agent" || b0.Name != "Hermes Agent" {
		t.Errorf("boards[0] = %+v, want slug=hermes-agent name=Hermes Agent", b0)
	}
	if b0.Counts["ready"] != 3 || b0.Counts["running"] != 1 {
		t.Errorf("boards[0].counts = %v, want {ready:3 running:1}", b0.Counts)
	}

	// Wire shape: boards must be an array with the exact keys, never null.
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode wire JSON: %v", err)
	}
	boardsAny, ok := wire["boards"]
	if !ok {
		t.Fatal(`wire JSON missing "boards" key`)
	}
	if _, ok := boardsAny.([]any); !ok {
		t.Errorf(`wire "boards" is %T, want array`, boardsAny)
	}
	if _, ok := wire["default_board"]; !ok {
		t.Fatal(`wire JSON missing "default_board" key`)
	}

	// Text content must carry the same compact JSON as StructuredContent.
	if len(res.Content) == 0 {
		t.Fatal("result has no content blocks")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want *TextContent", res.Content[0])
	}
	if strings.TrimSpace(text.Text) != string(raw) {
		t.Errorf("text content != structured content:\n%s\nvs\n%s", text.Text, string(raw))
	}
}

func TestBoardListEmptyBoardsIsArrayNotNil(t *testing.T) {
	fake := &fakeLister{boards: nil}
	res := callThroughServer(t, fake, nil)
	out := decodeOut(t, res)
	if out.Boards == nil {
		t.Fatal(`boards must be [] not null when the backend returns none`)
	}
	if len(out.Boards) != 0 {
		t.Errorf("len(boards) = %d, want 0", len(out.Boards))
	}
}

func TestBoardListBackendErrorIsToolError(t *testing.T) {
	fake := &fakeLister{err: &kanban.APIError{Status: 503, Kind: kanban.KindUnavailable, Msg: "backend down"}}
	res := callThroughServer(t, fake, nil)

	if !res.IsError {
		t.Fatal("expected IsError=true for a backend failure")
	}
	if len(res.Content) == 0 {
		t.Fatal("error result has no content")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want *TextContent", res.Content[0])
	}
	want := "unavailable: backend down (retryable: true)"
	if !strings.Contains(text.Text, want) {
		t.Errorf("error text %q does not contain %q", text.Text, want)
	}

	// A non-APIError passes through verbatim.
	fake2 := &fakeLister{err: errors.New("boom")}
	res2 := callThroughServer(t, fake2, nil)
	if !res2.IsError {
		t.Fatal("expected IsError=true for a generic backend failure")
	}
	text2, _ := res2.Content[0].(*mcp.TextContent)
	if !strings.Contains(text2.Text, "boom") {
		t.Errorf("error text %q does not contain %q", text2.Text, "boom")
	}
}

func TestBoardListToolDefinition(t *testing.T) {
	tool := BoardListTool()
	if tool.Name != "board_list" {
		t.Errorf("tool name = %q, want %q", tool.Name, "board_list")
	}
	if len(tool.Description) > 200 {
		t.Errorf("description %d chars, want <= 200 (T5 context budget)", len(tool.Description))
	}
	// Input schema: single optional boolean include_archived with default false.
	schema, ok := tool.InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("InputSchema is %T, want map[string]any", tool.InputSchema)
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties is %T, want map[string]any", schema["properties"])
	}
	if _, exists := props["include_archived"]; !exists {
		t.Fatal(`input schema missing "include_archived" property`)
	}
	if _, exists := props["board"]; exists {
		t.Fatal(`board_list must not declare a "board" input (list is global)`)
	}
}
