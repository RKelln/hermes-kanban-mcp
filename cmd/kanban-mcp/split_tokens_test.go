package main

// split_tokens_test.go proves the read/write bearer split END TO END over the
// real mux: a real initialize, a real session id, a real tools/call.
//
// The tool-level suite (internal/mcptools/capability_test.go) pins the guard
// itself. This file exists for the layer between: the middleware records the
// capability on the request context, and the MCP SDK hands that context to the
// tool handler. The SDK builds a session's context from the request that
// created it, so the split is only real if the capability survives that path —
// which is exactly what an in-process unit test cannot show.

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RKelln/hermes-kanban-mcp/internal/config"
	"github.com/RKelln/hermes-kanban-mcp/internal/mcptools"
)

const (
	splitReadToken  = "read-token-ABCDEFGH0123456789"
	splitWriteToken = "write-token-ZYXWVUTS9876543210"
)

// toolCallResult is the part of an MCP tools/call result these tests read.
type toolCallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// text joins the result's text blocks, so an assertion does not care which
// arm (prose or structured) carries a string.
func (r toolCallResult) text() string {
	var b strings.Builder
	for _, p := range r.Content {
		b.WriteString(p.Text)
	}
	return b.String()
}

// fakeKanban is the smallest backend that lets a write succeed: it answers the
// ticket_create POST and any read with a fixed document. Nothing here is
// schema-sensitive; the assertions read the TOOL result, not the payload.
func fakeKanban(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/tasks"):
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"task":{"id":"t_created01","status":"ready","title":"probe"}}`)
		case strings.HasSuffix(r.URL.Path, "/boards"):
			_, _ = io.WriteString(w, `{"boards":[{"slug":"hermes-agent","name":"Hermes Agent","counts":{}}]}`)
		case strings.HasSuffix(r.URL.Path, "/comments"):
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"c_1","body":"note"}`)
		default:
			_, _ = io.WriteString(w, `{"task":{"id":"t_created01","status":"ready","title":"probe","comments":[]}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// splitMux wires newMux with a read token, an optional write token, and a
// working (fake) kanban backend.
func splitMux(t *testing.T, readToken, writeToken string, backend *httptest.Server) http.Handler {
	t.Helper()
	cfg := &config.Config{
		BindAddrs:           "127.0.0.1:0",
		KanbanUsername:      "test-user",
		KanbanPassword:      "test-password-42",
		MCPBearerToken:      readToken,
		MCPWriteBearerToken: writeToken,
		KanbanDefaultBoard:  "hermes-kanban-mcp",
		MCPRateLimit:        600,
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	toolServer := mcptools.NewServerWithClient(&http.Client{}, backend.URL+"/api/plugins/kanban", "hermes-kanban-mcp")
	return newMux(cfg, logger, toolServer)
}

// handshake opens a session with the given token: initialize, then the
// notifications/initialized the spec requires before any other request.
func handshake(t *testing.T, h http.Handler, token string) *mcpClient {
	t.Helper()
	c := newMCPClient(h, token)
	c.rpc(t, 1, "initialize", map[string]any{
		"protocolVersion": "2026-07-28",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "split-test", "version": "0"},
	})
	c.post(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, http.StatusAccepted)
	return c
}

// callTool runs one tools/call and returns the MCP tool result.
func callTool(t *testing.T, c *mcpClient, id int, name string, args map[string]any) toolCallResult {
	t.Helper()
	result := c.rpc(t, id, "tools/call", map[string]any{"name": name, "arguments": args})
	var out toolCallResult
	if err := json.Unmarshal(result, &out); err != nil {
		t.Fatalf("%s result is not the expected shape: %v (result: %s)", name, err, result)
	}
	return out
}

// TestSplitReadTokenCannotCreateOverHTTP is the card's verification command
// with the transport in front of it: a client that opened its session with the
// read token is refused on ticket_create, in band, by name.
func TestSplitReadTokenCannotCreateOverHTTP(t *testing.T) {
	backend := fakeKanban(t)
	h := splitMux(t, splitReadToken, splitWriteToken, backend)
	c := handshake(t, h, splitReadToken)

	res := callTool(t, c, 2, "ticket_create", map[string]any{
		"board": "hermes-agent",
		"title": "the read lane must not be able to file this",
	})
	body := res.text()
	if !res.IsError {
		t.Fatalf("ticket_create with the read token: IsError = false (content: %s)", body)
	}
	for _, want := range []string{"capability_denied:", "ticket_create", "MCP_WRITE_BEARER_TOKEN", `"code":"write_token_required"`} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal does not carry %q: %s", want, body)
		}
	}
	if strings.Contains(body, "invalid_input:") {
		t.Errorf("capability refusal is prefixed invalid_input: %s", body)
	}
}

// TestSplitReadTokenStillReadsOverHTTP: the same session keeps every read tool
// and ticket_comment. The backend answers, so a refusal here would be the
// capability guard leaking into the read path.
func TestSplitReadTokenStillReadsOverHTTP(t *testing.T) {
	backend := fakeKanban(t)
	h := splitMux(t, splitReadToken, splitWriteToken, backend)
	c := handshake(t, h, splitReadToken)

	calls := []struct {
		name string
		args map[string]any
	}{
		{"board_list", map[string]any{}},
		{"ticket_list", map[string]any{"board": "hermes-agent"}},
		{"ticket_get", map[string]any{"board": "hermes-agent", "id": "t_created01"}},
		{"ticket_comment", map[string]any{"board": "hermes-agent", "id": "t_created01", "body": "read lane note"}},
	}
	for i, call := range calls {
		t.Run(call.name, func(t *testing.T) {
			res := callTool(t, c, 10+i, call.name, call.args)
			if strings.Contains(res.text(), "capability_denied:") {
				t.Fatalf("%s under the read token was refused: %s", call.name, res.text())
			}
		})
	}
}

// TestSplitWriteTokenOpensEverythingOverHTTP: the write token creates, and the
// create really happened (the fake backend's id comes back).
func TestSplitWriteTokenOpensEverythingOverHTTP(t *testing.T) {
	backend := fakeKanban(t)
	h := splitMux(t, splitReadToken, splitWriteToken, backend)
	c := handshake(t, h, splitWriteToken)

	res := callTool(t, c, 2, "ticket_create", map[string]any{
		"board": "hermes-agent",
		"title": "the write lane may file this",
	})
	if strings.Contains(res.text(), "capability_denied:") {
		t.Fatalf("ticket_create with the write token was refused: %s", res.text())
	}
	if res.IsError {
		t.Fatalf("ticket_create with the write token failed: %s", res.text())
	}
	if !strings.Contains(res.text(), "t_created01") {
		t.Errorf("ticket_create did not return the backend's ticket id: %s", res.text())
	}
}

// TestLegacySingleTokenStillWritesOverHTTP: with MCP_WRITE_BEARER_TOKEN unset
// the deployment is exactly today's — the one token opens the whole surface.
// This is the no-regression case for every existing client on deploy.
func TestLegacySingleTokenStillWritesOverHTTP(t *testing.T) {
	backend := fakeKanban(t)
	h := splitMux(t, splitReadToken, "", backend)
	c := handshake(t, h, splitReadToken)

	res := callTool(t, c, 2, "ticket_create", map[string]any{
		"board": "hermes-agent",
		"title": "legacy token still writes",
	})
	if res.IsError {
		t.Fatalf("legacy single-token ticket_create failed: %s", res.text())
	}
	if strings.Contains(res.text(), "capability_denied:") {
		t.Errorf("legacy single-token ticket_create was refused by the capability guard: %s", res.text())
	}
}

// TestUnknownTokenStillGets401: the split must not turn a bad credential into a
// tool-level answer. Only the two configured values are admitted, and anything
// else keeps the byte-exact 401 the clients are built against.
func TestUnknownTokenStillGets401(t *testing.T) {
	backend := fakeKanban(t)
	h := splitMux(t, splitReadToken, splitWriteToken, backend)

	status, hdr, body := rawPost(t, h, "wrong", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	assert401Contract(t, status, hdr, body)
}
