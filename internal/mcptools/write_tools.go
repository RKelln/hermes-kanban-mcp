package mcptools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// TicketCreateInput is the ticket_create tool input. Board is optional
// (defaults to KANBAN_DEFAULT_BOARD); title is required; every other
// field is optional. Only the fields below are accepted — the tool
// never forwards other POST fields to the backend.
type TicketCreateInput struct {
	Board          string   `json:"board"`
	Title          string   `json:"title"`
	Body           string   `json:"body"`
	Assignee       string   `json:"assignee"`
	Priority       int      `json:"priority"`
	WorkspaceKind  string   `json:"workspace_kind"`
	Parents        []string `json:"parents"`
	Skills         []string `json:"skills"`
	Triage         bool     `json:"triage"`
	IdempotencyKey string   `json:"idempotency_key"`
}

// TicketCreateOut is the ticket_create success projection: the id and
// status reported by the backend (never asserted locally), the title
// echoed back, and the board the ticket landed on.
type TicketCreateOut struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Title  string `json:"title"`
	Board  string `json:"board"`
}

// createTaskBody is the POST /tasks body. It carries only the allowed
// fields, and every optional field is omitted from the wire when zero
// or empty (omitempty) so the backend's defaults apply: priority 0,
// workspace_kind "scratch", triage false, empty parents/skills.
type createTaskBody struct {
	Title          string   `json:"title"`
	Body           string   `json:"body,omitempty"`
	Assignee       string   `json:"assignee,omitempty"`
	Priority       int      `json:"priority,omitempty"`
	WorkspaceKind  string   `json:"workspace_kind,omitempty"`
	Parents        []string `json:"parents,omitempty"`
	Skills         []string `json:"skills,omitempty"`
	Triage         bool     `json:"triage,omitempty"`
	IdempotencyKey string   `json:"idempotency_key,omitempty"`
}

// TicketCreate implements the ticket_create MCP tool: POST /tasks with
// board=<slug> and a body limited to the allowed fields. The backend's
// reported status/lane is echoed verbatim. All expected failures are
// one-line IsError results, and the rendered result never exceeds 2 KB.
func (s *Server) TicketCreate(ctx context.Context, in TicketCreateInput) *ToolResult {
	board := in.Board
	if board == "" {
		board = s.defaultBoard
	}
	if board == "" {
		return ErrorResult("invalid_input: no board specified; pass board or set KANBAN_DEFAULT_BOARD")
	}
	if err := ValidateBoardSlug(board); err != nil {
		return ErrorResult("invalid_input: %v", err)
	}
	if strings.TrimSpace(in.Title) == "" {
		return ErrorResult("invalid_input: title required")
	}
	for _, p := range in.Parents {
		if err := ValidateTicketID(p); err != nil {
			return ErrorResult("invalid_input: invalid parent id %q", p)
		}
	}

	key := in.IdempotencyKey
	if key == "" {
		key = synthesizedIdempotencyKey(board, in.Title, in.Body)
	}

	body := createTaskBody{
		Title:          in.Title,
		Body:           in.Body,
		Assignee:       in.Assignee,
		Priority:       in.Priority,
		WorkspaceKind:  in.WorkspaceKind,
		Parents:        in.Parents,
		Skills:         in.Skills,
		Triage:         in.Triage,
		IdempotencyKey: key,
	}

	var env taskEnvelope
	err := s.doJSON(ctx, http.MethodPost, "/tasks", url.Values{"board": []string{board}}, body, &env)
	if err != nil {
		return ErrorResult("%s", RestErrorMessage(err))
	}
	return SuccessResult(TicketCreateOut{
		ID:     env.Task.ID,
		Status: env.Task.Status,
		Title:  env.Task.Title,
		Board:  board,
	})
}

// synthesizedIdempotencyKey derives a retry-safe key from the request
// when the caller omits one: the first 16 hex characters of
// sha256(board + "|" + title + "|" + body[:200]). body is truncated to
// its first 200 runes (never splitting a multibyte character) before
// hashing.
func synthesizedIdempotencyKey(board, title, body string) string {
	prefix := []rune(body)
	if len(prefix) > 200 {
		prefix = prefix[:200]
	}
	sum := sha256.Sum256([]byte(board + "|" + title + "|" + string(prefix)))
	return hex.EncodeToString(sum[:])[:16]
}

// TicketCommentInput is the ticket_comment tool input. id and body are
// required; author is optional (defaults to the MCP_COMMENT_AUTHOR
// environment variable); board is optional (defaults to the server's
// configured board).
type TicketCommentInput struct {
	ID     string `json:"id"`
	Body   string `json:"body"`
	Author string `json:"author"`
	Board  string `json:"board"`
}

// TicketCommentOut is the ticket_comment success projection: the id of
// the commented ticket echoed back and a fixed ok flag.
type TicketCommentOut struct {
	ID string `json:"id"`
	OK bool   `json:"ok"`
}

// commentBody is the POST /tasks/{id}/comments body. It carries exactly
// the two fields the backend accepts — body and author — and no extra
// fields (id, board, metadata, etc. never reach the wire).
type commentBody struct {
	Body   string `json:"body"`
	Author string `json:"author"`
}

// TicketComment implements the ticket_comment MCP tool: POST
// /tasks/{id}/comments with a body limited to {body, author}. An
// empty/whitespace-only body is rejected before any HTTP call; author
// defaults to the MCP_COMMENT_AUTHOR environment variable when the
// caller omits it. id and board are validated with the T4 regexes
// before any HTTP call. All expected failures are one-line IsError
// results, and the rendered result never exceeds 2 KB.
func (s *Server) TicketComment(ctx context.Context, in TicketCommentInput) *ToolResult {
	board := in.Board
	if board == "" {
		board = s.defaultBoard
	}
	if board == "" {
		return ErrorResult("invalid_input: no board specified; pass board or set KANBAN_DEFAULT_BOARD")
	}
	if err := ValidateBoardSlug(board); err != nil {
		return ErrorResult("invalid_input: %v", err)
	}
	if err := ValidateTicketID(in.ID); err != nil {
		return ErrorResult("invalid_input: %v", err)
	}
	if strings.TrimSpace(in.Body) == "" {
		return ErrorResult("invalid_input: body required")
	}

	author := in.Author
	if author == "" {
		author = os.Getenv("MCP_COMMENT_AUTHOR")
	}

	if err := s.postComment(ctx, board, in.ID, in.Body, author); err != nil {
		return ErrorResult("%s", RestErrorMessage(err))
	}
	return SuccessResult(TicketCommentOut{ID: in.ID, OK: true})
}

// postComment issues the shared comment-posting request used by
// ticket_comment and ticket_complete: POST /tasks/{id}/comments with a
// body of exactly {body, author} and no extra fields. It returns the
// raw doJSON error so ticket_complete surfaces the exact same outbound
// request and error behavior (including 422 -> schema error mapping via
// RestErrorMessage). Callers are responsible for prior id/board
// validation and board resolution.
func (s *Server) postComment(ctx context.Context, board, id, body, author string) error {
	payload := commentBody{Body: body, Author: author}
	return s.doJSON(ctx, http.MethodPost, "/tasks/"+url.PathEscape(id)+"/comments",
		url.Values{"board": []string{board}}, payload, nil)
}

// TicketCompleteInput is the ticket_complete tool input. id and summary
// are required; board is optional (defaults to the server's configured
// board); result and metadata are optional free-form strings that are
// folded into the review comment and, in done mode, the PATCH body;
// review_tier is one of LOW|MEDIUM|HIGH, default MEDIUM when
// empty/omitted — LOW completes to done directly, MEDIUM/HIGH stay
// review-gated (subject to MCP_COMPLETE_MODE override). repo, branch,
// and sha are optional structured refs folded verbatim into the review
// block_reason and comment; they are ignored in done mode.
type TicketCompleteInput struct {
	ID         string `json:"id"`
	Board      string `json:"board"`
	Summary    string `json:"summary"`
	Result     string `json:"result"`
	Metadata   string `json:"metadata"`
	ReviewTier string `json:"review_tier"`
	Repo       string `json:"repo"`
	Branch     string `json:"branch"`
	SHA        string `json:"sha"`
}

// TicketCompleteOut is the ticket_complete success projection: the
// ticket id, the final status the PATCH requested, whether a human
// review is required, the effective review tier, the fixed note
// warning that a REST completion bypasses the kernel's created_cards
// audit gate, and the structured refs echoed from the input.
type TicketCompleteOut struct {
	ID             string `json:"id"`
	FinalStatus    string `json:"final_status"`
	ReviewRequired bool   `json:"review_required"`
	ReviewTier     string `json:"review_tier"`
	Note           string `json:"note"`
	Repo           string `json:"repo,omitempty"`
	Branch         string `json:"branch,omitempty"`
	SHA            string `json:"sha,omitempty"`
}

// completeReviewBody is the review-mode PATCH /tasks/{id} body: a plain
// status=blocked transition carrying the review-required reason. Kept
// distinct from ticket_block's blockTaskBody so the two tools' wire
// shapes cannot drift.
type completeReviewBody struct {
	Status      string `json:"status"`
	BlockReason string `json:"block_reason"`
}

// completeDoneBody is the done-mode PATCH /tasks/{id} body. status and
// summary are always present; result and metadata are omitted from the
// wire when empty so the backend only receives the fields the caller
// actually supplied.
type completeDoneBody struct {
	Status   string `json:"status"`
	Summary  string `json:"summary"`
	Result   string `json:"result,omitempty"`
	Metadata string `json:"metadata,omitempty"`
}

// completeNote is the fixed warning attached to every ticket_complete
// success. PATCH status=done accepts no created_cards field, so REST
// completions bypass the kernel's created_cards audit; follow-up
// tickets must be created explicitly with ticket_create.
const completeNote = "REST completion does not record created_cards; create follow-up tickets with ticket_create"

// TicketComplete implements the ticket_complete MCP tool: post the
// completion comment, then PATCH /tasks/{id} to the final status.
// Behaviour is keyed on review_tier (LOW completes direct to done) and,
// for MEDIUM/HIGH/omitted, the MCP_COMPLETE_MODE environment variable
// (default "review"): review posts a comment with the summary plus
// result/metadata rendered compactly, then blocks the ticket with
// reason "review-required: <summary[:100]>"; done posts the summary
// comment, then transitions to done carrying summary/result/metadata.
// The comment always precedes the PATCH.
//
// Claim guard: unless MCP_ALLOW_SKIP_CLAIM=true, the ticket's current
// status is read with GetTask first, and any status other than
// "running" aborts with the exact unclaimed message and no mutation.
// id and board are validated with the T4 regexes before any HTTP call;
// all expected failures are one-line IsError results, and the rendered
// result never exceeds 2 KB. Claiming itself is out of scope (the
// ticket_claim tool owns that path).
func (s *Server) TicketComplete(ctx context.Context, in TicketCompleteInput) *ToolResult {
	board := in.Board
	if board == "" {
		board = s.defaultBoard
	}
	if board == "" {
		return ErrorResult("invalid_input: no board specified; pass board or set KANBAN_DEFAULT_BOARD")
	}
	if err := ValidateBoardSlug(board); err != nil {
		return ErrorResult("invalid_input: %v", err)
	}
	if err := ValidateTicketID(in.ID); err != nil {
		return ErrorResult("invalid_input: %v", err)
	}
	if strings.TrimSpace(in.Summary) == "" {
		return ErrorResult("invalid_input: summary required")
	}

	// Review tier: LOW forces done directly; MEDIUM/HIGH/empty use the
	// existing MCP_COMPLETE_MODE behaviour (default review-gated).
	reviewTier := strings.ToUpper(strings.TrimSpace(in.ReviewTier))
	if reviewTier == "" {
		reviewTier = "MEDIUM"
	}
	switch reviewTier {
	case "LOW", "MEDIUM", "HIGH":
	default:
		return ErrorResult("invalid_input: review_tier must be one of LOW|MEDIUM|HIGH, got %q", reviewTier)
	}
	var doneMode bool
	if reviewTier == "LOW" {
		doneMode = true
	} else {
		doneMode = strings.EqualFold(os.Getenv("MCP_COMPLETE_MODE"), "done")
	}

	// Claim guard: without explicit opt-out, only tickets the
	// dispatcher has actually started (status running) may be
	// completed. GetTask is a read, so a guard failure never mutates.
	allowSkipClaim := strings.EqualFold(os.Getenv("MCP_ALLOW_SKIP_CLAIM"), "true")
	ts, err := s.GetTask(ctx, board, in.ID)
	if err != nil {
		return ErrorResult("%s", RestErrorMessage(err))
	}
	if !allowSkipClaim && ts.Status != "running" {
		return ErrorResult("ticket is %s and unclaimed; call ticket_claim first (or set MCP_ALLOW_SKIP_CLAIM=true)", ts.Status)
	}

	var patchBody any
	out := TicketCompleteOut{
		ID:         in.ID,
		ReviewTier: reviewTier,
		Note:       completeNote,
		Repo:       in.Repo,
		Branch:     in.Branch,
		SHA:        in.SHA,
	}
	comment := in.Summary
	if doneMode {
		patchBody = completeDoneBody{
			Status:   "done",
			Summary:  in.Summary,
			Result:   in.Result,
			Metadata: in.Metadata,
		}
		out.FinalStatus = "done"
	} else {
		comment = completeCommentBody(in.Summary, in.Result, in.Metadata)
		patchBody = completeReviewBody{
			Status:      "blocked",
			BlockReason: "review-required: " + firstRunes(in.Summary, 100) + completeRefSuffix(in.Repo, in.Branch, in.SHA),
		}
		if refs := completeCommentRefs(in.Repo, in.Branch, in.SHA); refs != "" {
			comment += "\n" + refs
		}
		out.FinalStatus = "blocked"
		out.ReviewRequired = true
	}

	// Comment first, then the status transition: the review convention
	// is that the unblock path respawns the worker with the comment
	// thread, so the summary must be on the ticket before it blocks.
	if err := s.postComment(ctx, board, in.ID, comment, os.Getenv("MCP_COMMENT_AUTHOR")); err != nil {
		return ErrorResult("%s", RestErrorMessage(err))
	}
	if err := s.doJSON(ctx, http.MethodPatch, "/tasks/"+url.PathEscape(in.ID),
		url.Values{"board": []string{board}}, patchBody, nil); err != nil {
		return ErrorResult("%s", RestErrorMessage(err))
	}
	return SuccessResult(out)
}

// completeCommentBody renders the ticket_complete review comment: the
// summary followed by result and metadata when present, each on its own
// labeled line. In done mode the comment is just the summary, so this
// helper is only used on the review path.
func completeCommentBody(summary, result, metadata string) string {
	var b strings.Builder
	b.WriteString(summary)
	if result != "" {
		b.WriteString("\nresult: ")
		b.WriteString(result)
	}
	if metadata != "" {
		b.WriteString("\nmetadata: ")
		b.WriteString(metadata)
	}
	return b.String()
}

// completeRefSuffix renders the structured repo/branch/sha suffix
// appended to the review-required block_reason, or "" when all three
// are empty. Rendered verbatim (no parsing, no reformatting).
func completeRefSuffix(repo, branch, sha string) string {
	repo = strings.TrimSpace(repo)
	branch = strings.TrimSpace(branch)
	sha = strings.TrimSpace(sha)
	if repo == "" && branch == "" && sha == "" {
		return ""
	}
	var parts []string
	if repo != "" {
		parts = append(parts, "repo: "+repo)
	}
	if branch != "" {
		parts = append(parts, "branch: "+branch)
	}
	if sha != "" {
		parts = append(parts, "sha: "+sha)
	}
	return " | " + strings.Join(parts, "; ")
}

// completeCommentRefs returns the multiline structured ref block for the
// review comment, or "" when all three fields are empty. One line per
// present field, verbatim.
func completeCommentRefs(repo, branch, sha string) string {
	repo = strings.TrimSpace(repo)
	branch = strings.TrimSpace(branch)
	sha = strings.TrimSpace(sha)
	if repo == "" && branch == "" && sha == "" {
		return ""
	}
	var lines []string
	if repo != "" {
		lines = append(lines, "repo: "+repo)
	}
	if branch != "" {
		lines = append(lines, "branch: "+branch)
	}
	if sha != "" {
		lines = append(lines, "sha: "+sha)
	}
	return strings.Join(lines, "\n")
}

// firstRunes returns at most n runes of s without splitting a multibyte
// character (the same convention as the idempotency-key truncation).
// Used to cap the review-required reason at summary[:100] runes.
func firstRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
