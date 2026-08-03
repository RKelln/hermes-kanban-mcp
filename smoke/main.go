// Command smoke is the T6 live smoke harness for ticket_claim.
//
// It drives the REAL tool implementation (mcptools.Server.TicketClaim)
// against the live kanban REST API and the real hermes CLI, exactly the
// way the deployed bridge would: an authenticated *http.Client (cookie
// jar from password-login) is handed to NewServerWithClient, and
// TicketClaim preflights via REST, shells out to `hermes kanban claim`,
// and re-reads the authoritative state. Nothing here reimplements the
// tool logic — it only wires the client and prints the results.
//
// Usage:
//
//	HERMES_DASHBOARD_BASIC_AUTH_USERNAME=<u> \
//	HERMES_DASHBOARD_BASIC_AUTH_PASSWORD=<p> \
//	go run ./smoke <ready-ticket-id> <todo-ticket-id>
//
// Credentials are read from env and never printed. The harness is
// throwaway (scratch workspace), not part of the module's test suite.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"time"

	"github.com/RKelln/hermes-kanban-mcp/internal/mcptools"
)

const (
	apiBase      = "http://127.0.0.1:9119/api/plugins/kanban"
	loginURL     = "http://127.0.0.1:9119/auth/password-login"
	defaultBoard = "hermes-agent"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: go run ./smoke <ready-ticket-id> <todo-ticket-id>")
		os.Exit(2)
	}
	readyID, todoID := os.Args[1], os.Args[2]

	user, pass := os.Getenv("HERMES_DASHBOARD_BASIC_AUTH_USERNAME"), os.Getenv("HERMES_DASHBOARD_BASIC_AUTH_PASSWORD")
	if user == "" || pass == "" {
		fmt.Fprintln(os.Stderr, "HERMES_DASHBOARD_BASIC_AUTH_USERNAME/PASSWORD not set")
		os.Exit(2)
	}

	ctx := context.Background()

	// Authenticated client: cookie jar populated by password-login,
	// mirroring internal/kanban.Client's session flow.
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 20 * time.Second}
	body, _ := json.Marshal(map[string]string{"provider": "basic", "username": user, "password": pass})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, bytes.NewReader(body))
	if err != nil {
		fatal("build login request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		fatal("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fatal("login failed: HTTP %d", resp.StatusCode)
	}

	srv := mcptools.NewServerWithClient(client, apiBase, defaultBoard)

	// 1) Claim the ready ticket: expect success + authoritative state.
	fmt.Println("== ticket_claim on READY ticket", readyID, "==")
	r1 := srv.TicketClaim(ctx, mcptools.TicketClaimInput{ID: readyID, Board: defaultBoard})
	printResult("ready-claim", r1)

	// 2) Claim the SAME ticket again: expect "already claimed (expires <ts>)".
	fmt.Println("== ticket_claim AGAIN on", readyID, "(already running) ==")
	r2 := srv.TicketClaim(ctx, mcptools.TicketClaimInput{ID: readyID, Board: defaultBoard})
	printResult("re-claim", r2)

	// 3) Claim the todo ticket: expect tool error naming current status.
	fmt.Println("== ticket_claim on TODO ticket", todoID, "==")
	r3 := srv.TicketClaim(ctx, mcptools.TicketClaimInput{ID: todoID, Board: defaultBoard})
	printResult("todo-claim", r3)

	// 4) Create a throwaway ticket through the REAL create tool (POST
	// also returns the {"task": ...} envelope — regression check for the
	// envelope decode fix) and archive it right after via the CLI.
	fmt.Println("== ticket_create via tool (envelope decode regression) ==")
	r4 := srv.TicketCreate(ctx, mcptools.TicketCreateInput{
		Board:         defaultBoard,
		Title:         "SMOKE TEST create-tool envelope check (throwaway)",
		Body:          "Created by the smoke harness to verify POST /tasks envelope decoding. Safe to archive.",
		Assignee:      "smoke-test-ghost",
		WorkspaceKind: "scratch",
	})
	printResult("create", r4)
	if !r4.IsError {
		var created struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(r4.Content[0].Text), &created); err == nil && created.ID != "" {
			fmt.Println("ARCHIVING", created.ID, "via CLI")
			os.Setenv("HERMES_KANBAN_BOARD", defaultBoard)
			// best-effort cleanup; the CLI prints its own confirmation
			_ = runCLI("kanban", "archive", created.ID)
		}
	}
}

func printResult(label string, r *mcptools.ToolResult) {
	b, err := json.Marshal(r)
	if err != nil {
		fatal("marshal result: %v", err)
	}
	fmt.Printf("%s: %s\n\n", label, b)
}

// runCLI execs the hermes CLI with the given args and returns its exit
// error, best-effort (used only for smoke-test cleanup, never for the
// claim path under test).
func runCLI(args ...string) error {
	cmd := exec.Command("hermes", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FATAL: "+format+"\n", args...)
	os.Exit(1)
}
