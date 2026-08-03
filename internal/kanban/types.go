// Package kanban defines the DTOs and error mapping for the kanban REST
// API (Hermes' kanban plugin, served by the dashboard at
// /api/plugins/kanban). It is limited to the fields surfaced by the MCP
// layer; session/transport concerns live in client.go and no MCP types
// appear in this package.
//
// Decoding is deliberately lenient: unknown JSON fields are ignored (no
// DisallowUnknownFields anywhere) and JSON nulls are no-ops on
// non-pointer fields, so these DTOs tolerate forward changes to the API.
// Task ids are opaque strings (e.g. "t_bc1ea8dd"); the numeric ids in
// this API are run ids (see Run) and unix timestamps (CreatedAt,
// StartedAt, ClaimExpires).
package kanban

import "encoding/json"

// Board is the board_list projection: identity plus per-status task counts.
type Board struct {
	Slug   string         `json:"slug"`
	Name   string         `json:"name"`
	Counts map[string]int `json:"counts"`
}

// TaskSummary is the lightweight ticket projection used by list/create
// flows. The MCP layer re-serializes it, so optional fields carry
// omitempty to drop empty keys.
type TaskSummary struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Status       string `json:"status"`
	Assignee     string `json:"assignee,omitempty"`
	Priority     int    `json:"priority"`
	ClaimLock    string `json:"claim_lock,omitempty"`
	ClaimExpires int64  `json:"claim_expires,omitempty"`
	BlockReason  string `json:"block_reason,omitempty"`
	BlockKind    string `json:"block_kind,omitempty"`
}

// Comment is the MCP-layer comment projection. CreatedAt is a unix
// timestamp in seconds.
type Comment struct {
	Author    string `json:"author"`
	CreatedAt int64  `json:"created_at"`
	Body      string `json:"body"`
}

// Run is the MCP-layer run projection. Run ids are numeric and StartedAt
// is a unix timestamp in seconds; extra run fields from the API are
// ignored during decode.
type Run struct {
	ID        int64  `json:"id"`
	Status    string `json:"status"`
	StartedAt int64  `json:"started_at"`
}

// Links carries the parent/child task id lists of the ticket_get
// envelope. The concrete schema is obvious (two arrays of task ids), so
// unlike the opaque arrays below it gets a typed form.
type Links struct {
	Parents  []string `json:"parents"`
	Children []string `json:"children"`
}

// TaskDetail is the full ticket projection: TaskSummary plus the
// envelope fields from GET /tasks/{id}. The opaque arrays (events,
// attachments, warnings, diagnostics) hold heterogeneous payloads that
// the MCP layer does not consume structurally, so they are kept as raw
// JSON. Comments and runs have concrete schemas and are typed.
type TaskDetail struct {
	TaskSummary
	Body          string            `json:"body,omitempty"`
	WorkspaceKind string            `json:"workspace_kind,omitempty"`
	BranchName    string            `json:"branch_name,omitempty"`
	Parents       []string          `json:"parents,omitempty"`
	Comments      []Comment         `json:"comments,omitempty"`
	Events        []json.RawMessage `json:"events,omitempty"`
	Runs          []Run             `json:"runs,omitempty"`
	Attachments   []json.RawMessage `json:"attachments,omitempty"`
	Links         *Links            `json:"links,omitempty"`
	Warnings      []json.RawMessage `json:"warnings,omitempty"`
	Diagnostics   []json.RawMessage `json:"diagnostics,omitempty"`
}

// CreateTaskRequest is the POST /tasks body. Title is required; every
// other field is optional and omitted from the wire when zero so the
// server's defaults apply (priority 0, workspace_kind "scratch",
// triage false, empty parents). Pointers for Priority and Triage
// distinguish "not provided" from an explicit zero value.
type CreateTaskRequest struct {
	Title          string   `json:"title"`
	Body           string   `json:"body,omitempty"`
	Assignee       string   `json:"assignee,omitempty"`
	Priority       *int     `json:"priority,omitempty"`
	WorkspaceKind  string   `json:"workspace_kind,omitempty"`
	Parents        []string `json:"parents,omitempty"`
	Skills         []string `json:"skills,omitempty"`
	Triage         *bool    `json:"triage,omitempty"`
	IdempotencyKey string   `json:"idempotency_key,omitempty"`
}
