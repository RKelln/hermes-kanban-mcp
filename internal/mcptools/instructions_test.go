package mcptools

// instructions_test.go covers the server-level MCP `instructions` string.
//
// Two failure modes matter and neither is visible to a compile:
//
//  1. The text never reaches the wire. main.go's newMux is in package main,
//     so this file rebuilds the SAME wiring (Implementation, ServerOptions
//     with LaneInstructions, Register, StreamableHTTPHandler) in-process and
//     asserts what a connected client actually receives in its
//     InitializeResult — not what the constant happens to say.
//  2. The text teaches a call that does not exist. A lane that reads
//     "call ticket_xyz" and gets refused loses the whole call it was
//     building, which is exactly the flailing this string exists to stop.
//     The drift test resolves every `ticket_*` token against the tool
//     roster the same client lists, so a renamed or removed tool turns the
//     suite red instead of a remote lane.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxLaneInstructionsBytes caps the per-session overhead every MCP client
// pays on connect. It is a budget on one response, not a prose diet: the
// full contract lives in kanban_help and is fetched on demand.
const maxLaneInstructionsBytes = 2000

// newInstructionsSession wires a server the way cmd/kanban-mcp does and
// returns a connected client session.
func newInstructionsSession(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	// The tool roster needs a Server, but an instructions handshake and a
	// tools/list never reach the backend, so an unroutable URL is fine.
	toolServer := NewServer("http://127.0.0.1:1", "hermes-agent")
	srv := mcp.NewServer(
		&mcp.Implementation{Name: "hermes-kanban-mcp", Version: "test"},
		&mcp.ServerOptions{Instructions: LaneInstructions},
	)
	Register(srv, toolServer)

	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return srv }, nil)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             httpSrv.URL,
		DisableStandaloneSSE: true,
	}, &mcp.ClientSessionOptions{})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// TestInitializeServesLaneInstructions performs a real initialize handshake
// and pins the served string: non-empty, inside the budget, and carrying
// the four literals the contract cannot work without.
func TestInitializeServesLaneInstructions(t *testing.T) {
	cs := newInstructionsSession(t)

	res := cs.InitializeResult()
	if res == nil {
		t.Fatal("InitializeResult is nil after Connect")
	}
	got := res.Instructions
	if got == "" {
		t.Fatal("server served no instructions: a client's only briefing is the tool roster again")
	}
	if len(got) > maxLaneInstructionsBytes {
		t.Errorf("instructions are %d bytes, over the %d-byte per-session budget", len(got), maxLaneInstructionsBytes)
	}
	t.Logf("served instructions: %d bytes", len(got))
	for _, want := range []string{"ticket_claim", "MCP_CLAIM_WORKER", "ticket_request_review", "since_event_id"} {
		if !strings.Contains(got, want) {
			t.Errorf("served instructions do not mention %q", want)
		}
	}
}

// ticketToken matches a tool-name-shaped ticket_* token. The trailing
// boundary keeps `ticket_claim,` from matching a phantom `ticket_claim_`.
var ticketToken = regexp.MustCompile(`\bticket_[a-z0-9_]+\b`)

// TestMainWiresLaneInstructions pins the one caller the handshake test
// cannot reach: main.go lives in package main, so the wiring above is a
// mirror, and a mirror drifts silently -- main could go back to passing nil
// options while every wire test stays green. This reads the real file.
func TestMainWiresLaneInstructions(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "cmd", "kanban-mcp", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	want := "&mcp.ServerOptions{Instructions: mcptools.LaneInstructions}"
	if !strings.Contains(string(src), want) {
		t.Errorf("cmd/kanban-mcp/main.go does not pass %s to mcp.NewServer", want)
	}
}

// TestLaneInstructionsOnlyNameRegisteredTools is the drift gate: every
// ticket_* token in the text must be a tool this server actually lists.
func TestLaneInstructionsOnlyNameRegisteredTools(t *testing.T) {
	cs := newInstructionsSession(t)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	registered := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		registered[tool.Name] = true
	}

	tokens := ticketToken.FindAllString(LaneInstructions, -1)
	if len(tokens) == 0 {
		t.Fatal("no ticket_* token found in the instructions: the drift gate would pass vacuously")
	}
	for _, tok := range tokens {
		if !registered[tok] {
			t.Errorf("instructions name %q, which is not a registered tool", tok)
		}
	}
}
