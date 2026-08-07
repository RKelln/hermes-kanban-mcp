# hermes-kanban-mcp

> **⚠️ EARLY — IMPLEMENTED, NOT YET STABLE.** This repository started as a 2026-08 design-phase experiment and is now a working implementation: all 8 MCP tools implemented, tested (`go vet` + `go test -race` green), and smoke-verified against a live Hermes kanban board. It is still young — expect API adjustments and breaking changes while it hardens. Use at your own risk; no support implied.

Go MCP server exposing a Hermes kanban board as MCP tools for remote opencode agents (or any MCP client) over streamable HTTP.

## Features

- **8 MCP tools** over streamable HTTP at `/mcp`, MCP v2 with protocol-version negotiation for older clients:

  | Tool | Purpose |
  |---|---|
  | `board_list` | List boards (slug, name, per-status counts) |
  | `ticket_list` | List tickets with status/assignee filters; summary-only, never full bodies |
  | `ticket_get` | Full ticket detail with truncation + size budgets |
  | `ticket_claim` | Atomically claim a ready ticket (`ready → running`) |
  | `ticket_comment` | Append a comment |
  | `ticket_complete` | Complete a ticket — **review-gated by default** |
  | `ticket_block` | Block a ticket; typed kinds via CLI with REST fallback |
  | `ticket_create` | Create a ticket (title required; parents supported) |

- **Static bearer auth** (custom middleware: constant-time compare, exact JSON 401 contract for opencode) + per-IP rate limiter.
- **Single static binary**, Go 1.25+, official `modelcontextprotocol/go-sdk` v1.7.0.
- **Context-budget discipline**: every tool result is truncated to a hard size budget (2 KB write tools, 6 KB `ticket_list`, 8 KB `ticket_get`); the board is never dumped wholesale.

## The claim mechanism (why `ticket_claim` shells out)

The kanban REST API has **no claim endpoint** (PATCH `status=running` is rejected by the kernel) — `ready → running` is kernel/CLI-native. `ticket_claim` therefore:

1. **Preflights over REST** — reads the ticket; `ready` is claimable, a running ticket with a held lock reports already-claimed, anything else is rejected *without* exec.
2. **Shells out** to `hermes kanban claim <id>` (the sanctioned binding to the kernel's `claim_task`), with argv/env hardening, timeout, and injection tests.
3. **Re-reads the ticket** and returns the authoritative post-claim state — never the CLI's stdout.

The binary resolves the CLI via `HERMES_BIN` (default `hermes`). Claim TTL is ~15 minutes; when it expires the dispatcher re-queues the ticket to `ready` and `ticket_claim` works again.

**Related knobs** (env):
- `MCP_ALLOW_SKIP_CLAIM=true` — `ticket_complete` skips its claim guard (it refuses to complete an unclaimed ticket otherwise).
- `MCP_COMPLETE_MODE=done` — complete to `done` instead of the default review-gated path (comment + `review-required` block). Applies to `review_tier` `MEDIUM`/`HIGH`/omitted; an explicit `review_tier: "LOW"` always completes to `done`.
- `MCP_COMMENT_AUTHOR` — default comment author.
- `KANBAN_DEFAULT_BOARD` — board used when a tool call omits `board`.
- `KANBAN_BASE_URL`, `KANBAN_USERNAME`, `KANBAN_PASSWORD` — the kanban REST backend + dashboard credentials. Note: the login route lives at the **dashboard root** (`/auth/password-login`), not under the plugin mount — the server strips the mount prefix before logging in.

Typed blocks (`dependency|needs_input|capability|transient`) route through the hardened CLI path when available; when the CLI is unavailable they fall back to an untyped REST block with `kind_applied=false` in the result.

## Build & run

```sh
go build -o kanban-mcp ./cmd/kanban-mcp
export KANBAN_USERNAME=... KANBAN_PASSWORD=... MCP_BEARER_TOKEN=$(openssl rand -hex 32)
./kanban-mcp
# health: curl localhost:9130/healthz
# MCP endpoint: http://localhost:9130/mcp  (Bearer token required)
```

## Deploy

- `deploy/kanban-mcp.service` — systemd unit (install binary to `/usr/local/bin/kanban-mcp`)
- `deploy/kanban-mcp.env.example` — all configuration variables with defaults
- `deploy/install.md` — install steps + opencode remote-MCP configuration snippet
- `scripts/smoke.sh` — end-to-end smoke + secret-hygiene checks
- `cmd/kanban-mcp/probe` — read-only live probe CLI

## Status

Implementation complete and smoke-verified (2026-08-03): all 8 tools wired, `go build` / `go vet` / `go test -race` green, live smoke against a real dashboard (login, claim, create verified). The Hermes-side follow-ups (REST claim endpoint on the kanban plugin, typed blocks/created_cards over REST) are tracked as backlog on the project board. Design + experiment record: `~/Documents/assistant/research/planning/hermes-kanban-mcp-design.md` and `notes/2026-08-03-hermes-kanban-mcp-pipeline-experiment.md` (local wiki).
