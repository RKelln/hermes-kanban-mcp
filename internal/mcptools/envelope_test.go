package mcptools

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newEnvelopeTestServer wires Register() behind a real Streamable HTTP handler,
// the same way cmd/kanban-mcp does, so the envelope asserted here is the one
// the SDK actually writes on the wire.
func newEnvelopeTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	// The backend URL is never reached: kanban_help renders compiled-in text.
	srv := mcp.NewServer(&mcp.Implementation{Name: "hermes-kanban-mcp", Version: "test"}, nil)
	Register(srv, NewServer("http://127.0.0.1:1", "hermes-agent"))
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(ts.Close)
	return ts
}

// mcpPost posts one JSON-RPC message and returns the raw response body plus the
// session id the server minted (empty for a notification).
func mcpPost(t *testing.T, url, sessionID string, msg map[string]any) (string, string) {
	t.Helper()
	payload, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode >= 400 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
	return string(body), resp.Header.Get("Mcp-Session-Id")
}

// sseFrame returns the raw JSON text of the first SSE `data:` frame carrying the
// given JSON-RPC id, so callers can assert on BYTES rather than on a decoded
// result.
func sseFrame(t *testing.T, body string, id int) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" {
			continue
		}
		var probe struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal([]byte(raw), &probe); err != nil {
			continue
		}
		if probe.ID == id {
			return raw
		}
	}
	t.Fatalf("no data frame with id %d in body: %s", id, body)
	return ""
}

// TestResultEnvelopeOmitsStructuredContent pins the wire envelope of a
// SUCCESSFUL tool call.
//
// The SDK publishes whatever the typed handler returns as the result's
// structuredContent, so returning a zero value there made every tool answer
//
//	"content":[{"type":"text","text":"{...real payload...}"}],"structuredContent":{}
//
// and advertise a matching outputSchema of
// {"type":"object","additionalProperties":false} — literally "my output is {}".
// Per the MCP tools spec clients prefer structured content when it and a text
// block are BOTH present, so spec-following clients (Claude Code) rendered every
// tool as an empty object while clients that read content[0].text (opencode)
// worked perfectly against the same server.
//
// Two properties are asserted:
//
//  1. No structuredContent KEY at all. Byte-level on purpose: an absent field
//     and an explicit `null` are different defects with the same blast radius,
//     and only the raw bytes distinguish them.
//  2. No outputSchema on any tool definition, because nothing returns
//     structured content and a declared output contract that nothing fulfils is
//     what invited a client to trust the empty object in the first place.
//
// kanban_help is the tool under test deliberately: it renders compiled-in text
// with no backend call, so this needs no kanban API and still exercises the
// SUCCESS path. The SDK returns early on a handler error, never reaching the
// structured-content marshalling, so an error result would prove nothing here.
func TestResultEnvelopeOmitsStructuredContent(t *testing.T) {
	ts := newEnvelopeTestServer(t)

	_, sid := mcpPost(t, ts.URL, "", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "envelope-test", "version": "1"},
		}})
	if sid == "" {
		t.Fatal("initialize minted no Mcp-Session-Id header")
	}
	mcpPost(t, ts.URL, sid, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})

	body, _ := mcpPost(t, ts.URL, sid, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "kanban_help", "arguments": map[string]any{}}})
	frame := sseFrame(t, body, 2)

	if strings.Contains(frame, "structuredContent") {
		t.Errorf("tools/call response carries a structuredContent field; spec-following clients prefer it over content[0].text and render the tool as an empty object.\nframe: %.240s", frame)
	}

	var res struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(frame), &res); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if res.Result.IsError {
		t.Fatalf("kanban_help returned an error result, which returns early in the SDK and would not exercise this path: %s", frame)
	}
	if len(res.Result.Content) == 0 || res.Result.Content[0].Text == "" {
		t.Fatalf("kanban_help returned no text payload: %s", frame)
	}

	tbody, _ := mcpPost(t, ts.URL, sid, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/list", "params": map[string]any{}})
	tframe := sseFrame(t, tbody, 3)
	if strings.Contains(tframe, "outputSchema") {
		t.Errorf("tools/list advertises an outputSchema while no tool returns structured content; that declares an output contract nothing fulfils.\nframe: %.240s", tframe)
	}
}
