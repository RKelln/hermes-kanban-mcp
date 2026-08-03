// Command probe is a read-only live probe for the kanban REST API
// client. It logs in with KANBAN_USER/KANBAN_PASSWORD, lists the boards
// visible to the session, and prints each board slug on its own line.
// It never mutates live data: the only requests issued are the session
// login (POST /auth/password-login, which only sets cookies) and a GET
// of the boards list.
//
// It is a helper for humans verifying a deployment, not part of the
// automated test suite — do not wire it into tests.
//
// Exit status:
//
//	2 — KANBAN_USER or KANBAN_PASSWORD missing (usage printed to stderr)
//	1 — login or list failed
//	0 — slugs printed to stdout
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/RKelln/hermes-kanban-mcp/internal/kanban"
)

const usage = `usage: probe

Read-only live probe for the kanban REST API client: logs in, lists the
boards visible to the session, and prints each board slug, one per line.
Nothing is created, patched, or deleted.

Environment:
  KANBAN_USER       dashboard username (required)
  KANBAN_PASSWORD   dashboard password (required)
`

func main() {
	user := os.Getenv("KANBAN_USER")
	pass := os.Getenv("KANBAN_PASSWORD")
	if user == "" || pass == "" {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	boards, err := kanban.New(user, pass).ListBoards(ctx, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe: %v\n", err)
		os.Exit(1)
	}

	for _, b := range boards {
		fmt.Println(b.Slug)
	}
}
