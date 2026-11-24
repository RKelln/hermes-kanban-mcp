package mcptools

// capability_test.go pins the read/write bearer split at the TOOL layer.
//
// The split itself is decided in internal/httpauth (which token was
// presented) and recorded on the request context; this suite proves every
// write tool honours it and no read tool accidentally does. The enumerated
// tool table is the safety net: a write tool added without a
// requireWriteCapability call fails here rather than in production, where the
// failure mode would be "the read lane could create a card and the dispatcher
// spawned a worker for it".

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/RKelln/hermes-kanban-mcp/internal/config"
	"github.com/RKelln/hermes-kanban-mcp/internal/httpauth"
)

// readCtx is a request authenticated with the read-only token.
func readCtx() context.Context {
	return httpauth.WithCapability(context.Background(), httpauth.CapabilityRead)
}

// writeCtx is a request authenticated with the write token.
func writeCtx() context.Context {
	return httpauth.WithCapability(context.Background(), httpauth.CapabilityWrite)
}

// The bridge's write surface, exactly. There is no ticket_assign or
// ticket_link tool (link/assign are CLI-level verbs this bridge does not
// expose), so the list is these five. Keeping the table here is the point:
// adding a write tool means adding its guard AND its row.
var writeToolCalls = []struct {
	name string
	call func(*Server, context.Context) *ToolResult
}{
	{"ticket_create", func(s *Server, ctx context.Context) *ToolResult {
		return s.TicketCreate(ctx, TicketCreateInput{Board: testBoard, Title: "capability probe"})
	}},
	{"ticket_claim", func(s *Server, ctx context.Context) *ToolResult {
		return s.TicketClaim(ctx, TicketClaimInput{Board: testBoard, ID: "t_deadbeef"})
	}},
	{"ticket_complete", func(s *Server, ctx context.Context) *ToolResult {
		return s.TicketComplete(ctx, TicketCompleteInput{Board: testBoard, ID: "t_deadbeef", Summary: "capability probe"})
	}},
	{"ticket_block", func(s *Server, ctx context.Context) *ToolResult {
		return s.TicketBlock(ctx, TicketBlockInput{Board: testBoard, ID: "t_deadbeef", Reason: "capability probe"})
	}},
	{"ticket_request_review", func(s *Server, ctx context.Context) *ToolResult {
		return s.TicketRequestReview(ctx, TicketRequestReviewInput{Board: testBoard, ID: "t_deadbeef", Summary: "capability probe"})
	}},
}

// isCapabilityRefusal reports whether res is the read-token capability
// refusal rather than an ordinary error.
func isCapabilityRefusal(res *ToolResult) bool {
	for _, p := range res.Content {
		if strings.Contains(p.Text, "capability_denied:") || strings.Contains(p.Text, refusalWriteTokenRequired) {
			return true
		}
	}
	return false
}

// TestReadTokenRefusedOnEveryWriteTool is the acceptance test from the card:
// the read token is refused on every write tool, by name, before any backend
// call happens.
func TestReadTokenRefusedOnEveryWriteTool(t *testing.T) {
	backend, rec := newRecServer(t, http.StatusCreated, `{"id":"t_1","status":"ready","title":"probe"}`)
	defer backend.Close()
	s := newTestServer(backend)

	for _, tool := range writeToolCalls {
		t.Run(tool.name, func(t *testing.T) {
			*rec = nil
			res := tool.call(s, readCtx())

			if !res.IsError {
				t.Fatalf("%s under the read token: IsError = false, want a refusal (content: %+v)", tool.name, res.Content)
			}
			if !isCapabilityRefusal(res) {
				t.Fatalf("%s under the read token: not the capability refusal (content: %+v)", tool.name, res.Content)
			}
			if len(res.Content) < 2 {
				t.Fatalf("%s refusal has %d content parts, want prose + structured arm", tool.name, len(res.Content))
			}

			prose := res.Content[0].Text
			for _, want := range []string{"capability_denied:", tool.name, config.EnvWriteBearerToken, "read-only"} {
				if !strings.Contains(prose, want) {
					t.Errorf("%s prose does not name %q: %q", tool.name, want, prose)
				}
			}
			// "How to get one": the refusal points at the operator and the
			// file the token lives in, so a lane that has never seen the
			// split can still resolve it.
			for _, want := range []string{"operator", "/etc/kanban-mcp.env"} {
				if !strings.Contains(prose, want) {
					t.Errorf("%s refusal does not say %q: %q", tool.name, want, prose)
				}
			}

			var repair writeTokenRepair
			if err := json.Unmarshal([]byte(res.Content[1].Text), &repair); err != nil {
				t.Fatalf("%s structured arm is not JSON: %v (%q)", tool.name, err, res.Content[1].Text)
			}
			if repair.Code != refusalWriteTokenRequired {
				t.Errorf("%s structured code = %q, want %q", tool.name, repair.Code, refusalWriteTokenRequired)
			}
			if repair.Tool != tool.name {
				t.Errorf("%s structured arm names tool %q", tool.name, repair.Tool)
			}
			if repair.RequiredEnv != config.EnvWriteBearerToken {
				t.Errorf("%s structured required_env = %q, want %q", tool.name, repair.RequiredEnv, config.EnvWriteBearerToken)
			}
			if repair.Capability != "read" {
				t.Errorf("%s structured capability = %q, want read", tool.name, repair.Capability)
			}
			if _, ok := repair.Fix["Authorization"]; !ok {
				t.Errorf("%s structured fix does not carry an Authorization header: %v", tool.name, repair.Fix)
			}

			// The refusal must fit the write-tool budget, and it must happen
			// BEFORE the backend is touched: a refused call that still POSTed
			// would mean the guard sits after the write.
			if size := renderedSize(t, res); size > MaxToolResultBytes {
				t.Errorf("%s refusal renders to %d bytes, over the %d-byte budget", tool.name, size, MaxToolResultBytes)
			}
			if n := len(*rec); n != 0 {
				t.Errorf("%s under the read token made %d backend request(s) before refusing: %+v", tool.name, n, *rec)
			}
		})
	}
}

// TestWriteTokenOpensEveryWriteTool: with the write token (or the legacy
// no-capability context) the guard is transparent — the call proceeds past it
// to whatever the tool would otherwise do.
func TestWriteTokenOpensEveryWriteTool(t *testing.T) {
	backend, rec := newRecServer(t, http.StatusCreated, `{"id":"t_1","status":"ready","title":"probe"}`)
	defer backend.Close()
	s := newTestServer(backend)

	contexts := map[string]context.Context{
		"write token":       writeCtx(),
		"legacy (no value)": context.Background(),
		"explicit full":     httpauth.WithCapability(context.Background(), httpauth.CapabilityFull),
	}
	for name, ctx := range contexts {
		for _, tool := range writeToolCalls {
			t.Run(name+"/"+tool.name, func(t *testing.T) {
				*rec = nil
				res := tool.call(s, ctx)
				if res == nil {
					t.Fatalf("%s returned nil", tool.name)
				}
				if isCapabilityRefusal(res) {
					t.Fatalf("%s under %s was refused by the capability guard: %+v", tool.name, name, res.Content)
				}
			})
		}
	}
}

// TestReadTokenAllowedOnEveryReadTool: the read token keeps the whole read
// surface, ticket_comment included — that is the lane's job.
func TestReadTokenAllowedOnEveryReadTool(t *testing.T) {
	board := `{"slug":"hermes-agent","name":"Hermes Agent","counts":{}}`
	backend, _ := newRecServer(t, http.StatusOK, board)
	defer backend.Close()
	s := newTestServer(backend)

	readToolCalls := []struct {
		name string
		call func(context.Context) *ToolResult
	}{
		{"board_list", func(ctx context.Context) *ToolResult {
			return s.BoardList(ctx, BoardListInput{})
		}},
		{"ticket_list", func(ctx context.Context) *ToolResult {
			return s.TicketList(ctx, TicketListInput{Board: testBoard})
		}},
		{"ticket_get", func(ctx context.Context) *ToolResult {
			return s.TicketGet(ctx, TicketGetInput{Board: testBoard, ID: "t_deadbeef"})
		}},
		{"ticket_events", func(ctx context.Context) *ToolResult {
			return s.TicketEvents(ctx, TicketEventsInput{Board: testBoard, ID: "t_deadbeef", TimeoutSeconds: 1})
		}},
		{"review_queue", func(ctx context.Context) *ToolResult {
			return s.ReviewQueue(ctx, struct{}{})
		}},
		{"kanban_help", func(ctx context.Context) *ToolResult {
			return s.Help(ctx, struct{}{})
		}},
		{"ticket_comment", func(ctx context.Context) *ToolResult {
			return s.TicketComment(ctx, TicketCommentInput{Board: testBoard, ID: "t_deadbeef", Body: "read lane note"})
		}},
	}
	for _, tool := range readToolCalls {
		t.Run(tool.name, func(t *testing.T) {
			res := tool.call(readCtx())
			if res == nil {
				t.Fatalf("%s returned nil", tool.name)
			}
			if isCapabilityRefusal(res) {
				t.Fatalf("%s under the read token was refused by the capability guard: %+v", tool.name, res.Content)
			}
		})
	}
}

// TestCapabilityRefusalIsNotInvalidInput keeps the prefixes distinct: a
// capability refusal is not an argument mistake, and a client that retries
// with corrected args must still be refused.
func TestCapabilityRefusalIsNotInvalidInput(t *testing.T) {
	backend, _ := newRecServer(t, http.StatusCreated, `{"id":"t_1","status":"ready","title":"probe"}`)
	defer backend.Close()
	s := newTestServer(backend)

	// Deliberately empty input: the capability guard runs BEFORE validation,
	// so the answer is still the capability refusal, not "board required".
	res := s.TicketCreate(readCtx(), TicketCreateInput{})
	if !res.IsError || !isCapabilityRefusal(res) {
		t.Fatalf("read token with empty input: want the capability refusal first, got %+v", res.Content)
	}
	if strings.Contains(res.Content[0].Text, "invalid_input:") {
		t.Errorf("capability refusal is prefixed invalid_input: %q", res.Content[0].Text)
	}
}

// readTokenAllowedTools is every registered tool the READ token may call.
// ticket_comment belongs here despite mutating the board: commenting is the
// read lane's job (it is how a reviewer or a sweeper annotates a card) and it
// cannot put work on the dispatcher.
var readTokenAllowedTools = []string{
	"board_list",
	"ticket_list",
	"ticket_get",
	"ticket_events",
	"review_queue",
	"kanban_help",
	"ticket_comment",
}

// TestEveryRegisteredToolIsClassified keeps the split honest as the surface
// grows. register.go is the one place a tool appears, so every name there must
// be on exactly one side: read-token-allowed, or a row in writeToolCalls whose
// guard is proven by TestReadTokenRefusedOnEveryWriteTool. A new tool fails
// here until its author classifies it — which is the point, because the
// failure it prevents is a new write path that silently accepts the read token.
func TestEveryRegisteredToolIsClassified(t *testing.T) {
	src, err := os.ReadFile("register.go")
	if err != nil {
		t.Fatalf("reading register.go: %v", err)
	}
	registered := map[string]bool{}
	for _, m := range regexp.MustCompile(`addTool\(srv, "([a-z_]+)"`).FindAllStringSubmatch(string(src), -1) {
		registered[m[1]] = true
	}
	if len(registered) == 0 {
		t.Fatal("no addTool registrations found in register.go; the scan proves nothing")
	}

	write := map[string]bool{}
	for _, tool := range writeToolCalls {
		write[tool.name] = true
		if !registered[tool.name] {
			t.Errorf("writeToolCalls lists %q, which register.go does not register (renamed tool?)", tool.name)
		}
	}
	read := map[string]bool{}
	for _, name := range readTokenAllowedTools {
		read[name] = true
		if write[name] {
			t.Errorf("%q is in BOTH lists; the read token cannot be allowed and refused", name)
		}
		if !registered[name] {
			t.Errorf("readTokenAllowedTools lists %q, which register.go does not register (renamed tool?)", name)
		}
	}
	for name := range registered {
		if !read[name] && !write[name] {
			t.Errorf("registered tool %q is neither read-token-allowed nor in writeToolCalls; classify it and, if it writes, guard it with requireWriteCapability", name)
		}
	}
}
