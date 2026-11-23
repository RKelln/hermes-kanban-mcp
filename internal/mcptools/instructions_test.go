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
//  3. The text teaches a REAL tool with the WRONG parameter. That is not
//     hypothetical: round 1 shipped `worker` on ticket_claim, which accepts
//     the argument and discards it, while the handoff that actually needs
//     it refused. Tool-name drift alone cannot see that, so a pinned table
//     of tool↔parameter pairs is checked against each tool's inputSchema
//     AND against the line that teaches it.

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

// laneInstructionParams pins the tool↔parameter pairs the text teaches.
// Every entry is a claim about the served contract, so each one is checked
// twice: the parameter must exist in that tool's inputSchema, and both the
// tool and the parameter must appear on ONE line of the text.
//
// The second half is the point. Round 1 taught `worker` on ticket_claim:
// the tool existed and so did the parameter, but the claim path discarded
// it and the handoff that needed an identity refused. A schema-only check
// passed that sentence; a same-line check fails it, because a line that
// teaches a pair must name both for the same reason.
//
// The table also pins the two identity/force rules that only the merged
// claim path makes true: identity is settled by ticket_claim (the label is
// assigned to the card before the claim), and a handoff on that claimed,
// RUNNING card needs force — a line naming ticket_request_review without
// force is how a lane lost its first handoff.
//
// Add a pair here whenever the text starts teaching a new parameter.
var laneInstructionParams = []struct{ tool, param string }{
	{"ticket_claim", "worker"},            // lane label: written to the card before the claim
	{"ticket_request_review", "force"},    // a handoff on the card this lane claimed is RUNNING
	{"ticket_request_review", "reviewer"}, // omit it; the server resolves a profile
	{"ticket_request_review", "summary"},  // the reviewer's handoff, required
	{"ticket_events", "since_event_id"},   // resume the wait from the last event seen
}

// TestLaneInstructionsOnlyTeachRealParams is the parameter-level drift
// gate: every tool↔param pair the text teaches must exist in that tool's
// advertised inputSchema, and must be taught on a line that names the tool.
// Both halves read the same wire response a client gets, so a renamed
// parameter or a moved sentence turns the suite red here.
func TestLaneInstructionsOnlyTeachRealParams(t *testing.T) {
	cs := newInstructionsSession(t)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	tools := make(map[string]*mcp.Tool, len(res.Tools))
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
	}

	lines := strings.Split(LaneInstructions, "\n")
	for _, pin := range laneInstructionParams {
		tool, ok := tools[pin.tool]
		if !ok {
			t.Errorf("instructions pair %s.%s names a tool that is not registered", pin.tool, pin.param)
			continue
		}
		props := wireSchemaProps(tool.InputSchema)
		if props == nil {
			t.Errorf("%s: InputSchema carries no properties map: %#v", pin.tool, tool.InputSchema)
			continue
		}
		if _, ok := props[pin.param]; !ok {
			t.Errorf("instructions teach %s.%s, but that parameter is not in the tool's inputSchema", pin.tool, pin.param)
		}

		taught := false
		for _, line := range lines {
			if strings.Contains(line, pin.tool) && strings.Contains(line, pin.param) {
				taught = true
				break
			}
		}
		if !taught {
			t.Errorf("instructions pin %s.%s but no single line teaches both: the text names one without the other", pin.tool, pin.param)
		}
	}

	// A pinned pair nobody teaches is a dead claim: it would keep passing
	// after the sentence it describes was deleted. Refuse the vacuous case
	// outright, the same way the tool-token gate does.
	if len(laneInstructionParams) == 0 {
		t.Fatal("no tool↔param pair pinned: the param gate would pass vacuously")
	}
}

// TestLaneInstructionsTeachTheMergedIdentityRule pins the DIRECTION of the
// two rules no schema can witness.
//
// Existence gates cannot see a sentence that names the right tool and the
// right parameter and still describes the opposite contract. That is not
// hypothetical here: round 2 shipped `ticket_claim records no label: its
// worker is a no-op` — the pre-t_7f1caa0a truth — and every parameter in
// it existed. The merged claim path writes the label BEFORE the claim and
// the handoff inherits it, so the served text has to say that, and this is
// the text that says it.
func TestLaneInstructionsTeachTheMergedIdentityRule(t *testing.T) {
	for _, want := range []string{
		"ticket_claim stamps worker",        // identity is written at the claim
		"ticket_request_review inherits it", // and never has to be passed again
	} {
		if !strings.Contains(LaneInstructions, want) {
			t.Errorf("instructions do not say %q: the identity rule is inverted or missing, which no schema check can see", want)
		}
	}
}
