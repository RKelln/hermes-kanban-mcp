package mcptools

import (
	"fmt"
	"regexp"
)

// The T4 id/board validation regexes. These are the canonical patterns
// for the whole tool layer: every tool (read and write) validates board
// slugs and ticket ids against these before issuing any HTTP request.
//
// T4 (read-tool validation) defines the same patterns as unexported
// helpers in this package; the exported forms below are the shared
// surface the write tools (ticket_create, ticket_comment,
// ticket_complete, ticket_block) use. Keep both in sync.
var (
	boardSlugRe = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	ticketIDRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// ValidateBoardSlug returns an error when slug does not match the T4
// board slug pattern ^[a-z0-9-]{1,64}$. The message is a single line
// suitable for an IsError tool result.
func ValidateBoardSlug(slug string) error {
	if !boardSlugRe.MatchString(slug) {
		return fmt.Errorf("invalid board %q", slug)
	}
	return nil
}

// ValidateTicketID returns an error when id does not match the T4
// ticket id pattern ^[A-Za-z0-9._-]{1,64}$. The message is a single
// line suitable for an IsError tool result.
func ValidateTicketID(id string) error {
	if !ticketIDRe.MatchString(id) {
		return fmt.Errorf("invalid ticket id %q", id)
	}
	return nil
}
