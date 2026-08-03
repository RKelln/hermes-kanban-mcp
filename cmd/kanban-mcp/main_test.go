package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RKelln/hermes-kanban-mcp/internal/config"
	"github.com/RKelln/hermes-kanban-mcp/internal/httpauth"
	"github.com/RKelln/hermes-kanban-mcp/internal/mcptools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const testToken = "test-token-ABCDEFGH0123456789"

// testMux builds the full wiring (newMux — the same code path main uses)
// with a Server whose backend client points at a dead base URL (the
// transport/auth/tools-list tests never issue backend calls) and a
// discard logger.
func testMux(t *testing.T, token string, rateLimit int) http.Handler {
	t.Helper()
	return testMuxLogger(t, token, rateLimit, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

// testMuxLogger is testMux with an explicit logger, for tests that must
// inspect the server's structured output.
func testMuxLogger(t *testing.T, token string, rateLimit int, logger *slog.Logger) http.Handler {
	t.Helper()
	cfg := &config.Config{
		BindAddrs:          "127.0.0.1:0",
		KanbanUsername:     "test-user",
		KanbanPassword:     "test-password-42",
		MCPBearerToken:     token,
		KanbanDefaultBoard: "hermes-agent",
		MCPRateLimit:       rateLimit,
	}
	toolServer := mcptools.NewServerWithClient(&http.Client{}, "http://127.0.0.1:9/api/plugins/kanban", "hermes-agent")
	return newMux(cfg, logger, toolServer)
}

// mcpClient is a minimal Streamable HTTP client over the full mux. It
// replays the Mcp-Session-Id captured from the initialize response, like
// a real client (opencode, the SDK client) does.
type mcpClient struct {
	h     http.Handler
	token string
	sid   string
}

func newMCPClient(h http.Handler, token string) *mcpClient {
	return &mcpClient{h: h, token: token}
}

// post sends a raw JSON-RPC POST exactly like the documented curl
// checks: Content-Type application/json, Accept carrying both media
// types, the bearer token when set, and the session id when captured.
func (c *mcpClient) post(t *testing.T, body string, wantStatus int) (http.Header, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.sid != "" {
		req.Header.Set("Mcp-Session-Id", c.sid)
	}
	rr := httptest.NewRecorder()
	c.h.ServeHTTP(rr, req)
	if rr.Code != wantStatus {
		t.Fatalf("POST /mcp returned %d, want %d (body: %s)", rr.Code, wantStatus, rr.Body.Bytes())
	}
	if sid := rr.Header().Get("Mcp-Session-Id"); sid != "" {
		c.sid = sid
	}
	return rr.Header(), rr.Body.Bytes()
}

// rpc posts a JSON-RPC request expecting an HTTP 200 with a decodable
// JSON-RPC response (inline JSON or SSE, both legal Streamable HTTP
// answers) and returns the decoded result member.
func (c *mcpClient) rpc(t *testing.T, id int, method string, params map[string]any) json.RawMessage {
	t.Helper()
	payload := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		payload["params"] = params
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s request: %v", method, err)
	}
	hdr, body := c.post(t, string(raw), http.StatusOK)
	parsed := extractJSON(t, hdr, body)
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(parsed, &resp); err != nil {
		t.Fatalf("%s response is not JSON: %v (body: %s)", method, err, body)
	}
	if len(resp.Error) > 0 && string(resp.Error) != "null" {
		t.Fatalf("%s returned a JSON-RPC error: %s", method, resp.Error)
	}
	return resp.Result
}

// extractJSON pulls the JSON-RPC payload out of a response that is either
// inline application/json or an SSE stream with data: lines.
func extractJSON(t *testing.T, hdr http.Header, body []byte) []byte {
	t.Helper()
	ct := hdr.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		var out bytes.Buffer
		sc := bufio.NewScanner(bytes.NewReader(body))
		for sc.Scan() {
			line := sc.Text()
			if rest, ok := strings.CutPrefix(line, "data:"); ok {
				out.WriteString(strings.TrimSpace(rest))
			}
		}
		if err := sc.Err(); err != nil {
			t.Fatalf("scan SSE body: %v", err)
		}
		return out.Bytes()
	}
	return body
}

// rawPost sends a raw HTTP request to /mcp without any session handling,
// for the auth and rate-limit cases where the request must be rejected
// before the MCP transport is ever reached. auth is "none", "wrong", or
// the correct token value.
func rawPost(t *testing.T, h http.Handler, auth, body string) (int, http.Header, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	switch auth {
	case "none":
	case "wrong":
		req.Header.Set("Authorization", "Bearer wrong-token-0000000000000000")
	default:
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code, rr.Header(), rr.Body.Bytes()
}

// The 401 wire contract: exact WWW-Authenticate challenge, JSON content
// type, and the byte-exact JSON body negotiated for opencode.
func assert401Contract(t *testing.T, status int, hdr http.Header, body []byte) {
	t.Helper()
	const wantBody = `{"error":"invalid_token","error_description":"invalid or missing bearer token"}`
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if got := hdr.Get("WWW-Authenticate"); got != `Bearer error="invalid_token", error_description="invalid or missing bearer token"` {
		t.Errorf("WWW-Authenticate = %q, want the exact bearer challenge", got)
	}
	if got := hdr.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if string(body) != wantBody {
		t.Errorf("body = %q, want %q", body, wantBody)
	}
}

func TestMCPServerRejectsBadTokens(t *testing.T) {
	h := testMux(t, testToken, 60)

	t.Run("POST without token", func(t *testing.T) {
		status, hdr, body := rawPost(t, h, "none", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
		assert401Contract(t, status, hdr, body)
	})

	t.Run("POST with wrong token", func(t *testing.T) {
		status, hdr, body := rawPost(t, h, "wrong", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
		assert401Contract(t, status, hdr, body)
	})

	t.Run("GET without token is also protected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		assert401Contract(t, rr.Code, rr.Header(), rr.Body.Bytes())
	})

	t.Run("healthz stays open", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /healthz = %d, want 200 (must be unauthenticated)", rr.Code)
		}
	})
}

func TestMCPServerInitializeAndToolsList(t *testing.T) {
	h := testMux(t, testToken, 60)
	c := newMCPClient(h, testToken)

	// Full client handshake, exactly as opencode and the curl checks do
	// it: initialize -> notifications/initialized -> tools/list, with the
	// session id captured from the initialize response replayed.
	result := c.rpc(t, 1, "initialize", map[string]any{
		"protocolVersion": "2026-07-28",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0"},
	})

	var initResult struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(result, &initResult); err != nil {
		t.Fatalf("initialize result is not the expected shape: %v (result: %s)", err, result)
	}
	if initResult.ServerInfo.Name != "hermes-kanban-mcp" {
		t.Errorf("serverInfo.name = %q, want hermes-kanban-mcp", initResult.ServerInfo.Name)
	}
	if initResult.ServerInfo.Version == "" {
		t.Error("serverInfo.version is empty")
	}
	if initResult.ProtocolVersion == "" {
		t.Error("protocolVersion was not negotiated")
	}

	// The spec requires notifications/initialized before any other
	// request on the session; go-sdk v1.7.0 answers it with 202.
	c.post(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, http.StatusAccepted)

	listResult := c.rpc(t, 2, "tools/list", map[string]any{})
	var listed struct {
		Tools []mcp.Tool `json:"tools"`
	}
	if err := json.Unmarshal(listResult, &listed); err != nil {
		t.Fatalf("tools/list result is not the expected shape: %v (result: %s)", err, listResult)
	}
	names := make(map[string]bool, len(listed.Tools))
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	want := []string{"board_list", "ticket_list", "ticket_get", "ticket_claim",
		"ticket_comment", "ticket_complete", "ticket_block", "ticket_create"}
	if len(names) != len(want) {
		t.Errorf("tools/list has %d tools, want %d (%v)", len(names), len(want), names)
	}
	for _, w := range want {
		if !names[w] {
			t.Errorf("tools/list is missing %s (have %v)", w, names)
		}
	}
}

func TestMCPServerRateLimitIsWired(t *testing.T) {
	// rate 1/min with the default burst of 20: the first 20 requests
	// pass, the 21st is rejected with 429 before it reaches the SDK.
	h := testMux(t, testToken, 1)
	const body = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	for i := 0; i < httpauth.DefaultBurst; i++ {
		status, _, _ := rawPost(t, h, testToken, body)
		if status == http.StatusTooManyRequests {
			t.Fatalf("request %d was rate-limited early (burst %d)", i+1, httpauth.DefaultBurst)
		}
	}
	status, hdr, _ := rawPost(t, h, testToken, body)
	if status != http.StatusTooManyRequests {
		t.Fatalf("request past the burst = %d, want 429", status)
	}
	if got := hdr.Get("Retry-After"); got == "" {
		t.Error("429 response is missing Retry-After")
	}
}
