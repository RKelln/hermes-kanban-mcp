package mcptools

// capability.go is the tool-side half of the read/write bearer split.
//
// Why it exists: `MCP_BEARER_TOKEN` used to authorise the whole surface, and
// ticket_create is not a board-text operation. A `ready` card is spawned by
// this host's kanban dispatcher as a real worker process within a tick, so
// "can create a card" is code execution with a one-minute delay — a different
// blast radius from "can read tickets". A lane that only reads the board and
// comments has no business holding that.
//
// The decision is made in internal/httpauth (which token was presented); this
// file turns it into the tool's answer. The refusal is IN BAND — an MCP tool
// result with IsError, a prose arm and a machine-readable arm — never a bare
// HTTP 401, because a 401 tells a client its credential is wrong when it is
// merely insufficient, and an agent reading that retries with the same token.
//
// The prose arm is the contract a model reads; the JSON arm carries the code
// (`write_token_required`) and the variable to obtain. Keep the prose short:
// the consequence belongs in helpDoc, not in a paragraph a caller reads after
// failing.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/RKelln/hermes-kanban-mcp/internal/config"
	"github.com/RKelln/hermes-kanban-mcp/internal/httpauth"
)

// refusalWriteTokenRequired is the structured code for a read-token write
// attempt. It is part of the served contract: the tests pin it to every write
// tool, so renaming it is a contract change, not a refactor.
const refusalWriteTokenRequired = "write_token_required"

// writeTokenRepair is the machine-readable arm of a capability refusal.
// tool names the call that was refused, RequiredEnv the variable that would
// authorise it, and Fix the header a retry should carry.
type writeTokenRepair struct {
	Code        string            `json:"code"`
	Tool        string            `json:"tool"`
	Capability  string            `json:"capability"`
	RequiredEnv string            `json:"required_env"`
	Message     string            `json:"message"`
	Fix         map[string]string `json:"fix"`
}

// requireWriteCapability refuses a write tool when the request authenticated
// with the read token, and allows it otherwise.
//
// It returns nil when the call may proceed. A context with no recorded
// capability is the legacy single-token deployment (see
// httpauth.CapabilityFromContext), so the in-process tests and any future
// non-HTTP transport keep the full surface.
//
// Every write tool calls this FIRST, before input validation: a read token
// should learn why it cannot write, not which argument it also got wrong.
// Adding a write tool means adding this line and a case in
// capability_test.go — the test enumerates the tools, so a forgotten guard
// fails there rather than in production.
func requireWriteCapability(ctx context.Context, tool string) *ToolResult {
	capability := httpauth.CapabilityFromContext(ctx)
	if capability.AllowsWrite() {
		return nil
	}
	message := fmt.Sprintf("%s needs the write token; this request carried the read-only token", tool)
	repair, err := json.Marshal(writeTokenRepair{
		Code:        refusalWriteTokenRequired,
		Tool:        tool,
		Capability:  capability.String(),
		RequiredEnv: config.EnvWriteBearerToken,
		Message:     message,
		Fix:         map[string]string{"Authorization": "Bearer <" + config.EnvWriteBearerToken + ">"},
	})
	if err != nil {
		return ErrorResult("internal error: marshal capability refusal for %s: %v", tool, err)
	}
	prose := "capability_denied: " + message + ". Ask the bridge operator for " +
		config.EnvWriteBearerToken + " (it is set in the bridge's /etc/kanban-mcp.env) and send it as the " +
		"Authorization bearer token; MCP_BEARER_TOKEN stays valid for reads and ticket_comment."
	return &ToolResult{
		Content: []ContentPart{
			{Type: "text", Text: prose},
			{Type: "text", Text: string(repair)},
		},
		IsError: true,
	}
}
