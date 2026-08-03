package mcptools

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaxToolResultBytes is the hard cap on the rendered size of every MCP
// tool result (success or error). Write-tool results consume session
// context on every turn, so the whole tool family is budgeted at 2 KB.
const MaxToolResultBytes = 2 * 1024

// maxResultTextRunes is the initial rune budget for a result's text
// payload; the JSON envelope ({"content":[{"type":"text","text":...}],
// "isError":...}) needs headroom inside the byte budget, and JSON
// escaping of quotes/backslashes/<>& can inflate a message further.
const maxResultTextRunes = MaxToolResultBytes - 256

// ContentPart is one content block of an MCP tool result. Text is the
// only content type this layer emits; the shape mirrors the MCP
// CallToolResult wire format so the SDK adapter in register.go can
// convert without reshaping.
type ContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ToolResult is the MCP tool-result wire shape used across the tool
// layer. IsError marks expected failures (the MCP isError flag);
// success results are plain text content. Every ToolResult produced by
// this package renders to <= MaxToolResultBytes.
type ToolResult struct {
	Content []ContentPart `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// SuccessResult builds a non-error result whose text is the compact
// JSON rendering of v. The rendered result is guaranteed <= 2 KB.
func SuccessResult(v any) *ToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return ErrorResult("internal error: %v", err)
	}
	return buildResult(false, string(b))
}

// ErrorResult builds an IsError result with a single-line message.
// Newlines in the message (e.g. from backend detail payloads) are
// collapsed to spaces so expected failures render as one line. The
// rendered result is guaranteed <= 2 KB.
func ErrorResult(format string, args ...any) *ToolResult {
	msg := fmt.Sprintf(format, args...)
	msg = strings.ReplaceAll(msg, "\r", " ")
	msg = strings.ReplaceAll(msg, "\n", " ")
	return buildResult(true, msg)
}

// buildResult renders a ToolResult and, if the marshalled JSON would
// exceed the byte budget (message contains characters that blow up
// under JSON escaping), shrinks the text until it fits.
func buildResult(isErr bool, text string) *ToolResult {
	r := &ToolResult{Content: []ContentPart{{Type: "text", Text: clampRunes(text, maxResultTextRunes)}}}
	if isErr {
		r.IsError = true
	}
	for {
		b, err := json.Marshal(r)
		if err == nil && len(b) <= MaxToolResultBytes {
			return r
		}
		runes := []rune(r.Content[0].Text)
		if len(runes) <= 32 {
			// Cannot shrink meaningfully further; even this short
			// message only overflows in pathological escaping cases.
			return r
		}
		r.Content[0].Text = string(runes[:len(runes)*3/4])
	}
}

// clampRunes cuts s to at most max runes (never splitting a rune),
// appending an ellipsis when truncated so the result stays <= max.
func clampRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 0 {
		return ""
	}
	return string(runes[:max-1]) + "…"
}
