package kanban

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Error kinds produced by MapError. These are the stable machine-readable
// categories the MCP layer renders as tool errors.
const (
	KindInvalidRequest = "invalid_request"
	KindAuth           = "auth"
	KindForbidden      = "forbidden"
	KindNotFound       = "not_found"
	KindConflict       = "conflict"
	KindSchema         = "schema"
	KindUnavailable    = "unavailable"
)

// maxMsgLen caps the user-facing message on an APIError.
const maxMsgLen = 300

// APIError is a typed kanban REST error: the HTTP status, a stable
// machine-readable kind, and a human-readable message.
type APIError struct {
	Status int
	Kind   string
	Msg    string
}

// Error implements error.
func (e *APIError) Error() string {
	return fmt.Sprintf("kanban api error: status %d kind %s: %s", e.Status, e.Kind, e.Msg)
}

// MapError converts a kanban REST response status and body into a *APIError.
//
// The body is expected to carry the FastAPI error envelope {"detail": ...}.
// A string detail is used verbatim; an array detail (422 schema
// validation) contributes its first two issue messages joined with "; ".
// Bodies without a parseable detail fall back to the raw body text
// (trimmed), then to the HTTP status text. The message is truncated to
// 300 characters (runes).
//
// Status mapping: 400 -> invalid_request, 401 -> auth, 403 -> forbidden,
// 404 -> not_found, 409 -> conflict, 422 -> schema, any other 4xx ->
// invalid_request, any 5xx -> unavailable. MapError always returns a
// non-nil error; statuses below 400 (a caller bug) map to
// invalid_request rather than nil.
func MapError(status int, body []byte) error {
	msg := detailMessage(body)
	if msg == "" {
		msg = fallbackMessage(status, body)
	}
	return &APIError{
		Status: status,
		Kind:   kindForStatus(status),
		Msg:    truncateRunes(msg, maxMsgLen),
	}
}

// kindForStatus maps an HTTP status to the stable error kind.
func kindForStatus(status int) string {
	if status >= 500 {
		return KindUnavailable
	}
	if status >= 400 {
		switch status {
		case 400:
			return KindInvalidRequest
		case 401:
			return KindAuth
		case 403:
			return KindForbidden
		case 404:
			return KindNotFound
		case 409:
			return KindConflict
		case 422:
			return KindSchema
		default:
			return KindInvalidRequest
		}
	}
	// Not an error status; callers should not reach here. Keep the
	// mapping total so MapError never returns nil.
	return KindInvalidRequest
}

// detailMessage extracts a message from the FastAPI {"detail": ...}
// envelope. detail may be a JSON string or an array of issue objects
// ({"type":..., "loc":..., "msg":...}); for arrays the first two issue
// messages are joined with "; ". Returns "" when the body has no
// parseable detail field.
func detailMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var envelope struct {
		Detail json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.Detail) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(envelope.Detail, &s); err == nil {
		return s
	}
	var issues []struct {
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(envelope.Detail, &issues); err == nil {
		msgs := make([]string, 0, 2)
		for _, issue := range issues {
			if issue.Msg != "" {
				msgs = append(msgs, issue.Msg)
			}
			if len(msgs) == 2 {
				break
			}
		}
		return strings.Join(msgs, "; ")
	}
	return ""
}

// fallbackMessage produces a message when the body carries no parseable
// detail: the raw body text (trimmed) when non-empty, else the HTTP
// status text. The raw-body fallback keeps the full server payload (e.g.
// the {"error":"unauthenticated","reason":"no_cookie"} auth shape) so
// callers can string-match on it.
func fallbackMessage(status int, body []byte) string {
	if msg := strings.TrimSpace(string(body)); msg != "" {
		return msg
	}
	if text := http.StatusText(status); text != "" {
		return text
	}
	return fmt.Sprintf("unexpected HTTP status %d", status)
}

// truncateRunes cuts s to at most n runes, replacing the tail with a
// single ellipsis character so the result never exceeds n runes.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n == 0 {
		return ""
	}
	return string(runes[:n-1]) + "…"
}
