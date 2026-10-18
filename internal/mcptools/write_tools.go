package mcptools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/RKelln/hermes-kanban-mcp/internal/kanban"
)

// TicketCreateInput is the ticket_create tool input. Board is required
// (board required); title is required; every other
// field is optional. Only the fields below are accepted — the tool
// never forwards other POST fields to the backend.
//
// Triage stays on the struct even though the tool does not advertise it
// (register.go omits it from the input schema) and refuses it in the
// handler: a strict client that drops unknown properties is not the only
// kind of caller, and an MCP call can still carry a field the schema does
// not list. Silently ignoring it would file a ticket into triage that no
// agent can spec or claim — the exact trap t_a6e3ba60 fell into.
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
// workspace_kind "scratch", empty parents/skills. triage is on the wire
// struct because the kernel's create accepts it, but this bridge never
// sets it true: the handler refuses triage before any request is built,
// so the field only ever marshals to nothing.
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
		return ErrorResult("invalid_input: board required; pass board")
	}
	if err := ValidateBoardSlug(board); err != nil {
		return ErrorResult("invalid_input: %v", err)
	}
	if strings.TrimSpace(in.Title) == "" {
		return ErrorResult("invalid_input: title required")
	}
	// Triage is refused, not ignored. It is the kernel's intake lane for
	// under-specified input: the designed exit (`hermes kanban specify`)
	// calls the auxiliary LLM and rewrites the card, and nothing on this
	// box runs it. A ticket the bridge filed there would wait for a
	// specifier that never comes, and no agent-facing verb can leave it
	// (ticket_claim refuses triage for the same reason).
	if in.Triage {
		return ErrorResult("invalid_input: triage is an internal intake lane (a specifier rewrites the card); the bridge cannot spec, so create the ticket ready or block it with kind=needs_input")
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
		return ErrorResult("invalid_input: board required; pass board")
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
// empty/omitted — LOW completes to done directly, MEDIUM/HIGH hand the
// card to the native review lane (subject to MCP_COMPLETE_MODE override).
// repo, branch, and sha are optional structured refs carried into the
// review comment and the reviewer's summary (trimmed, single-line);
// ignored in done mode. The handoff summary is capped at
// kanban.MaxReviewSummaryBytes (16 KiB) and refused before anything is
// written when longer.
// created_cards is the optional manifest of child ticket ids created
// during this run; it is forwarded only in done mode, where the kernel's
// created_cards audit gate verifies each id (phantom ids are rejected
// with a 400 naming the offenders).
type TicketCompleteInput struct {
	ID           string   `json:"id"`
	Board        string   `json:"board"`
	Summary      string   `json:"summary"`
	Result       string   `json:"result"`
	Metadata     string   `json:"metadata"`
	ReviewTier   string   `json:"review_tier"`
	Repo         string   `json:"repo"`
	Branch       string   `json:"branch"`
	SHA          string   `json:"sha"`
	CreatedCards []string `json:"created_cards"`
}

// TicketCompleteOut is the ticket_complete success projection: the ticket
// id, the final status the transition asked for, whether a review is
// outstanding, the effective review tier, the reviewer the handoff landed
// on (review path only) and any note the handoff wants the caller to see,
// plus the structured refs echoed from the input.
//
// FinalStatus is "done" or "review". It is never "blocked": the review
// path hands off to the native review lane, because a `review-required:`
// block has had no consumer since the review-sweeper was retired
// (t_affa85ca).
type TicketCompleteOut struct {
	ID             string `json:"id"`
	FinalStatus    string `json:"final_status"`
	ReviewRequired bool   `json:"review_required"`
	ReviewTier     string `json:"review_tier"`
	Reviewer       string `json:"reviewer,omitempty"`
	Note           string `json:"note,omitempty"`
	Repo           string `json:"repo,omitempty"`
	Branch         string `json:"branch,omitempty"`
	SHA            string `json:"sha,omitempty"`
}

// completeDoneBody is the done-mode PATCH /tasks/{id} body. status and
// summary are always present; result, metadata, and created_cards are
// omitted from the wire when empty so the backend only receives the
// fields the caller actually supplied. created_cards is forwarded to
// complete_task so the kernel's audit gate verifies each id — phantom
// ids produce a 400 the tool surfaces via CompleteErrorMessage.
type completeDoneBody struct {
	Status       string   `json:"status"`
	Summary      string   `json:"summary"`
	Result       string   `json:"result,omitempty"`
	Metadata     string   `json:"metadata,omitempty"`
	CreatedCards []string `json:"created_cards,omitempty"`
}

// TicketComplete implements the ticket_complete MCP tool: post the
// completion comment, then PATCH /tasks/{id} to the final status.
// Behaviour is keyed on review_tier (LOW completes direct to done) and,
// for MEDIUM/HIGH/omitted, the MCP_COMPLETE_MODE environment variable
// (default "review"): review posts a comment carrying the summary plus
// result/metadata rendered compactly, then hands the card to the NATIVE
// review lane (see completeIntoReviewLane) so it lands in 'review', where
// the dispatcher claims it and spawns the reviewer. It must not write a
// `review-required:` block: that block's only consumer, the review-sweeper
// cron, was retired 2026-09-14, so a blocked completion went to nobody
// (t_affa85ca). done posts the summary comment, then transitions to done
// carrying summary/result/metadata and, when supplied, created_cards
// (forwarded to the kernel's created_cards audit gate — phantom ids get a
// 400 the tool surfaces via CompleteErrorMessage). The comment always
// precedes the transition.
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
		return ErrorResult("invalid_input: board required; pass board")
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

	// Structured refs are folded (trimmed, single-line) into the comment and
	// the reviewer's summary, so enforce generous single-line length caps up
	// front: oversized or multiline refs are rejected before any HTTP call,
	// keeping the record bounded and single-line.
	for _, r := range []struct {
		name string
		val  string
		max  int
	}{
		{"repo", in.Repo, maxRepoChars},
		{"branch", in.Branch, maxBranchChars},
		{"sha", in.SHA, maxSHAChars},
	} {
		if len([]rune(r.val)) > r.max {
			return ErrorResult("invalid_input: %s exceeds %d characters", r.name, r.max)
		}
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
	if len(in.CreatedCards) > 0 && !doneMode {
		return ErrorResult("invalid_input: created_cards requires done mode (review_tier LOW or MCP_COMPLETE_MODE=done)")
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

	out := TicketCompleteOut{
		ID:         in.ID,
		ReviewTier: reviewTier,
		Repo:       in.Repo,
		Branch:     in.Branch,
		SHA:        in.SHA,
	}
	if !doneMode {
		return s.completeIntoReviewLane(ctx, board, in, ts, out)
	}

	patchBody := completeDoneBody{
		Status:       "done",
		Summary:      in.Summary,
		Result:       in.Result,
		Metadata:     in.Metadata,
		CreatedCards: in.CreatedCards,
	}
	out.FinalStatus = "done"

	// Comment first, then the PATCH: the ticket carries the handoff
	// before it closes.
	if err := s.postComment(ctx, board, in.ID, in.Summary, os.Getenv("MCP_COMMENT_AUTHOR")); err != nil {
		return ErrorResult("%s", RestErrorMessage(err))
	}
	if err := s.doJSON(ctx, http.MethodPatch, "/tasks/"+url.PathEscape(in.ID),
		url.Values{"board": []string{board}}, patchBody, nil); err != nil {
		return ErrorResult("%s", CompleteErrorMessage(err))
	}
	return SuccessResult(out)
}

// completeIntoReviewLane finalises a MEDIUM/HIGH completion by handing the
// card to the NATIVE review lane (status='review' via `hermes kanban
// request-review`) instead of writing a `review-required:` BLOCK.
//
// Why the block had to go (t_affa85ca): a `review-required:` block needs a
// consumer, and the only one was the review-sweeper cron, disabled
// 2026-09-14. Its successor lane_return.py deliberately ignores unmarked
// worker-phase `review-required:` blocks ("belong to whatever workflow
// wrote them"), so a bridge-driven completion landed on a card nothing
// would ever claim: t_ac950fce sat that way with no verdict and no
// reviewer, and no queue entry anywhere held it. The native lane needs no
// consumer: the dispatcher claims a review row itself and spawns the
// sdlc-review reviewer.
//
// Order matters. The comment lands first, because it is the reviewer's
// record of what was claimed and a handoff with no comment is a reviewer
// with nothing to review against. Then the handoff runs through the same
// guarded implementation ticket_request_review uses (reviewer resolution,
// installed-profile verification, post-condition re-read), so the two
// tools cannot drift.
//
// force: the claim guard above admits a RUNNING card, whose live claim
// only request-review --force can release. Passing force here adds no
// capability the old path did not already have — the block and the done
// transition both dropped that same claim.
//
// Implementer provenance is the one thing this path cannot repair, and it
// is announced rather than hidden: request_review records the implementer
// from the current run's profile (a bridge claim writes none) or the
// ticket's assignee. `assign_task` refuses to assign a card that is
// running under a claim, so the bridge cannot supply one at completion
// time. When the ticket is unassigned, the handoff carries
// implementer=null: an APPROVE still completes the card, but a
// request-changes verdict has no route back and needs a human.
func (s *Server) completeIntoReviewLane(ctx context.Context, board string, in TicketCompleteInput,
	ts *kanban.TaskSummary, out TicketCompleteOut) *ToolResult {

	// The handoff summary is the brief the reviewer receives, and it is also the
	// value the CLI bounds: request-review refuses anything past
	// kanban.MaxReviewSummaryBytes. Check it BEFORE the comment lands — failing
	// after the post leaves a RUNNING card holding a completion comment for a
	// transition that never happened, which needs a human to unpick.
	//
	// The structured refs travel in this summary as well as the comment: the
	// summary is what ticket_request_review hands the reviewer as the brief, and
	// a reviewer told which branch to verify does not have to go looking for it.
	summary := strings.TrimSpace(in.Summary + completeRefSuffix(in.Repo, in.Branch, in.SHA))
	if len(summary) > kanban.MaxReviewSummaryBytes {
		return ErrorResult("invalid_input: handoff summary too long (%d bytes, max %d); nothing was written — shorten the summary, or use review_tier LOW / MCP_COMPLETE_MODE=done when no review round is needed", len(summary), kanban.MaxReviewSummaryBytes)
	}

	comment := completeCommentBody(in.Summary, in.Result, in.Metadata)
	if refs := completeCommentRefs(in.Repo, in.Branch, in.SHA); refs != "" {
		comment += "\n" + refs
	}
	if err := s.postComment(ctx, board, in.ID, comment, os.Getenv("MCP_COMMENT_AUTHOR")); err != nil {
		return ErrorResult("%s", RestErrorMessage(err))
	}

	reviewRes := s.TicketRequestReview(ctx, TicketRequestReviewInput{
		Board:   board,
		ID:      in.ID,
		Summary: summary,
		Force:   ts.Status == "running",
	})
	if reviewRes.IsError {
		// The refusal is the caller's to see. The comment above is already
		// durable, which is the right residue: the work is recorded, the
		// card is still in its previous state, and nothing claims a
		// transition that did not happen.
		return reviewRes
	}
	var requested TicketRequestReviewOut
	if err := json.Unmarshal([]byte(reviewRes.Content[0].Text), &requested); err != nil {
		return ErrorResult("review handoff returned an unreadable projection: %v", err)
	}

	out.FinalStatus = "review"
	out.ReviewRequired = true
	out.Reviewer = requested.Assignee

	var notes []string
	if requested.Note != "" {
		notes = append(notes, requested.Note)
	}
	if strings.TrimSpace(ts.Assignee) == "" {
		notes = append(notes, noImplementerNote)
	}
	out.Note = strings.Join(notes, "; ")
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

// Ref length caps: refs are folded (trimmed, single-line) into the comment
// and the reviewer's summary, so unbounded values could bloat the record.
// Reasonable generous ceilings keep each ref single-line without rejecting
// legitimate values.
const (
	maxRepoChars   = 256
	maxBranchChars = 256
	maxSHAChars    = 64
)

// collapseLineBreaks replaces CR/LF with spaces so a value folds into a
// single line. Used so one ref cannot inject a fake extra line into the
// comment or the reviewer's summary, however it was supplied.
func collapseLineBreaks(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// completeRefParts returns the present refs in canonical order (repo,
// branch, sha), trimmed of surrounding whitespace and with CR/LF
// collapsed to spaces so each ref stays one line.
func completeRefParts(repo, branch, sha string) []string {
	clean := func(s string) string {
		return strings.TrimSpace(collapseLineBreaks(s))
	}
	var parts []string
	if r := clean(repo); r != "" {
		parts = append(parts, "repo: "+r)
	}
	if b := clean(branch); b != "" {
		parts = append(parts, "branch: "+b)
	}
	if sh := clean(sha); sh != "" {
		parts = append(parts, "sha: "+sh)
	}
	return parts
}

// completeRefSuffix renders the structured repo/branch/sha suffix
// appended to the review handoff summary, or "" when all three are
// empty. Rendered from completeRefParts (trimmed, newlines collapsed to
// spaces, canonical order); never splits a rune.
func completeRefSuffix(repo, branch, sha string) string {
	parts := completeRefParts(repo, branch, sha)
	if len(parts) == 0 {
		return ""
	}
	return " | " + strings.Join(parts, "; ")
}

// completeCommentRefs returns the multiline structured ref block for the
// review comment, or "" when all three fields are empty. One line per
// present field, in canonical order (trimmed, newlines collapsed).
func completeCommentRefs(repo, branch, sha string) string {
	parts := completeRefParts(repo, branch, sha)
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n")
}
