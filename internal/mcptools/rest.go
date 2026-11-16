package mcptools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/RKelln/hermes-kanban-mcp/internal/kanban"
)

const (
	// defaultRequestTimeout bounds every backend request issued through
	// a Server built by NewServer. Callers needing a different budget
	// pass their own *http.Client via NewServerWithClient.
	defaultRequestTimeout = 15 * time.Second
	// maxRestBodyBytes is the read cap on every backend response. It is
	// not a size budget for what the tools RETURN — those project down
	// (ticket_list returns at most MaxTicketListLimit rows) — it is the
	// hard stop past which a response is refused instead of decoded.
	//
	// It must clear a whole board view: GET /board carries every card's
	// full body, so the payload grows with a board's history and never
	// shrinks. Measured 2026-11-16: the togather board's view is
	// 1,143,909 bytes across 295 cards. The 1 MiB value this replaced
	// cut that response mid-JSON, and the read helper reported the cut
	// as "decode kanban backend response: unexpected end of JSON input"
	// — a size problem wearing a malformed-JSON costume, which is why it
	// read as a backend fault rather than a bridge limit. The togather
	// board became unlistable that way on 2026-11-09, the day its
	// payload crossed 1 MiB. An over-cap response now fails by name; see
	// doJSON and errResponseTooLarge.
	maxRestBodyBytes = 16 << 20
)

// Server is the shared write-tool backend handle: the kanban REST base
// URL, the default board slug, and the HTTP client used for every
// backend request. register.go constructs one Server from config and
// hands it to every registered tool.
type Server struct {
	baseURL      string
	defaultBoard string
	client       *http.Client
	// maxBodyBytes is the per-response read cap; both constructors set
	// it to maxRestBodyBytes. It is a field so tests can shrink it
	// instead of allocating a 16 MiB fixture to prove the refusal.
	maxBodyBytes int
}

// NewServer builds a Server with a default 15s-timeout client. baseURL
// is the kanban REST base including the plugin prefix (e.g.
// http://127.0.0.1:9119/api/plugins/kanban); a trailing slash is
// tolerated. defaultBoard is informational only (surfaced by
// board_list); board-taking tools reject an omitted board.
func NewServer(baseURL, defaultBoard string) *Server {
	return &Server{
		baseURL:      strings.TrimRight(baseURL, "/"),
		defaultBoard: defaultBoard,
		client:       &http.Client{Timeout: defaultRequestTimeout},
		maxBodyBytes: maxRestBodyBytes,
	}
}

// NewServerWithClient builds a Server around a caller-provided HTTP
// client (used by tests and by wiring that needs a custom timeout).
func NewServerWithClient(client *http.Client, baseURL, defaultBoard string) *Server {
	if client == nil {
		client = &http.Client{Timeout: defaultRequestTimeout}
	}
	return &Server{
		baseURL:      strings.TrimRight(baseURL, "/"),
		defaultBoard: defaultBoard,
		client:       client,
		maxBodyBytes: maxRestBodyBytes,
	}
}

// DefaultBoard returns the configured default board slug (informational
// only; board-taking tools reject an omitted board).
func (s *Server) DefaultBoard() string {
	return s.defaultBoard
}

// RestError carries the HTTP status and raw body of a non-2xx backend
// response. RestErrorMessage maps it to the one-line tool-error message.
type RestError struct {
	Status int
	Body   []byte
}

// Error implements error.
func (e *RestError) Error() string {
	return fmt.Sprintf("kanban backend responded with HTTP %d", e.Status)
}

// doJSON issues a JSON HTTP request to the kanban REST backend. method
// is the HTTP verb, path is relative to the configured base URL (e.g.
// "/tasks" or "/tasks/t_abc123/comments"), query is optional URL query
// parameters, body is any value marshalled as the JSON request body
// (nil sends no body), and out, when non-nil, is the decode target for
// a 2xx response. Non-2xx responses return a *RestError carrying the
// status and raw body; transport and decode failures return a plain
// error. The MCP bearer token is never forwarded: this helper only
// talks to the cookie-authenticated kanban backend.
func (s *Server) doJSON(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	u := s.baseURL + path
	label := path
	if len(query) > 0 {
		q := "?" + query.Encode()
		u += q
		label += q
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := readCapped(resp.Body, s.maxBodyBytes)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, label, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &RestError{Status: resp.StatusCode, Body: raw}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode kanban backend response: %w", err)
		}
	}
	return nil
}

// taskEnvelope is the wire shape of the kanban REST API's single-task
// responses: GET /tasks/{id} and POST /tasks both return the task
// payload wrapped under the "task" key, alongside comments/events/runs
// in the GET case. The MCP layer only consumes the task dict, so the
// envelope is decoded and discarded here.
type taskEnvelope struct {
	Task kanban.TaskSummary `json:"task"`
}

// GetTask fetches a ticket summary (id, title, status, ...) from
// GET /tasks/{id}?board=<slug>. It exists so tools that need to
// pre-flight on current status (ticket_claim, ticket_complete) can read
// it without depending on the kanban client package. Non-2xx responses
// surface as *RestError via RestErrorMessage.
func (s *Server) GetTask(ctx context.Context, board, id string) (*kanban.TaskSummary, error) {
	var env taskEnvelope
	err := s.doJSON(ctx, http.MethodGet, "/tasks/"+url.PathEscape(id), url.Values{"board": []string{board}}, nil, &env)
	if err != nil {
		return nil, err
	}
	return &env.Task, nil
}

// errResponseTooLarge is returned when a backend response exceeds the
// read cap. It exists so an oversized payload is reported as what it is
// — bigger than this bridge will read — instead of as a JSON decode
// error. The two looked identical from the caller's side while the cap
// cut bodies silently: "unexpected end of JSON input" is what a
// truncated document decodes to, and it names neither the size nor the
// limit, so the failure pointed at the backend.
type errResponseTooLarge struct {
	cap  int
	read int
}

func (e *errResponseTooLarge) Error() string {
	return fmt.Sprintf("response exceeds the %d-byte read cap (at least %d bytes)", e.cap, e.read)
}

// readCapped reads r up to limit bytes and refuses anything longer. It
// reads limit+1 so a response that exactly fills the cap still decodes
// while a byte more is detected as overflow rather than discovered later
// as broken JSON. Overflow is refused for any status: a body we could
// not read whole cannot be decoded OR trusted as an error detail.
func readCapped(r io.Reader, limit int) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > limit {
		return nil, &errResponseTooLarge{cap: limit, read: len(raw)}
	}
	return raw, nil
}

// RestErrorMessage maps a doJSON error to the one-line tool-error
// message every write tool emits on backend failure:
//
//	HTTP 422 -> "schema error: <first two backend issue messages joined by '; '>"
//	other statuses -> "<kanban kind>: <message>" (invalid_request, auth,
//	                   forbidden, not_found, conflict, unavailable)
//	transport/context errors -> "unavailable: <err>"
//
// The 422 mapping uses the kanban.MapError detail extraction, which
// joins the first two FastAPI issue messages with "; ".
func RestErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	var re *RestError
	if !errors.As(err, &re) {
		return "unavailable: " + err.Error()
	}
	apiErr, ok := kanban.MapError(re.Status, re.Body).(*kanban.APIError)
	if !ok {
		return fmt.Sprintf("unavailable: HTTP %d", re.Status)
	}
	if re.Status == http.StatusUnprocessableEntity {
		return "schema error: " + apiErr.Msg
	}
	return apiErr.Kind + ": " + apiErr.Msg
}

// createdCardsRejectedPrefix is the leading phrase of the backend's 400
// detail when a done PATCH carries phantom created_cards ids. The full
// detail names the offenders, e.g. "created_cards rejected: t_bogus1,
// t_bogus2 do not exist or were not created by this worker".
const createdCardsRejectedPrefix = "created_cards rejected: "

// CompleteErrorMessage maps a ticket_complete PATCH error to the
// one-line tool-error message. The backend rejects a done PATCH whose
// created_cards manifest contains phantom ids (ids that do not exist or
// were not created by this worker) with HTTP 400 and a detail naming
// the offenders; that message is surfaced verbatim so the caller sees
// exactly which ids failed the kernel's audit gate. Every other error
// falls through to RestErrorMessage unchanged, keeping the schema-422
// mapping, kind prefixes, and transport wrapping identical to the rest
// of the tool family.
func CompleteErrorMessage(err error) string {
	var re *RestError
	if errors.As(err, &re) && re.Status == http.StatusBadRequest {
		if apiErr, ok := kanban.MapError(re.Status, re.Body).(*kanban.APIError); ok &&
			strings.HasPrefix(apiErr.Msg, createdCardsRejectedPrefix) {
			return apiErr.Msg
		}
	}
	return RestErrorMessage(err)
}
