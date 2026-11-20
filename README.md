# hermes-kanban-mcp

> **⚠️ EARLY — IMPLEMENTED, NOT YET STABLE.** This repository started as a 2026-08 design-phase experiment and is now a working implementation: all 12 MCP tools implemented, tested (`go vet` + `go test -race` green), and smoke-verified against a live Hermes kanban board. It is still young — expect API adjustments and breaking changes while it hardens. Use at your own risk; no support implied.

Go MCP server exposing a Hermes kanban board as MCP tools for remote coding agents (opencode, codex, Claude Code, or any MCP client) over streamable HTTP.

## Features

- **12 MCP tools** over streamable HTTP at `/mcp`, MCP v2 with protocol-version negotiation for older clients:

  | Tool | Purpose |
  |---|---|
  | `board_list` | List boards (slug, name, per-status counts) |
  | `ticket_list` | List tickets with status/assignee filters; summary-only, never full bodies |
  | `ticket_get` | Full ticket detail with truncation + size budgets; `detail: full` returns complete comment/body text |
  | `ticket_events` | Long-poll a ticket's events; the wait ends on a **matching** event newer than `since_event_id`, on the ticket's status CHANGING to a status you can act on, or on the timeout — `ticket_status` is reported on every return. `wake_on` picks the matcher: `verdicts` (default) ignores kernel bookkeeping (heartbeat/claimed/spawned/…) and the reviewer's own `review → running` claim; `any` returns on everything. `suppressed` counts what the filter swallowed (cumulative since `since_event_id`, distinct within one call — reusing a cursor reports the same bookkeeping again) |
  | `review_queue` | Single-call scan for review-required tickets across **all boards** (one call replaces per-board scans) |
  | `ticket_claim` | Atomically claim a ready ticket (`ready → running`), first **assigning the lane label** (`worker` > `MCP_CLAIM_WORKER` > the server default `remote-mcp`) while the card is still ready: identity is set once, at claim, the label is stable per lane, and the kernel then records it as the review handoff's implementer. A label that is an installed profile is refused before any write; a non-ready ticket is refused with the reason for its own status, not a generic one |
  | `ticket_comment` | Append a comment |
  | `ticket_complete` | Complete a ticket — **review-gated by default**, with a `review_tier` knob (`LOW` completes direct) |
  | `ticket_request_review` | Request a final review: `ready`/`running` → `review` (the native review lane — a review run on the reviewer profile, which force-loads the `sdlc-review` skill; not a block). `summary` required; **omit `reviewer`** — the server resolves it (`MCP_REVIEWER_PROFILE`), and supplying one is an operator override, verified against the installed profile roster |
  | `ticket_block` | Block a ticket; typed kinds via CLI with REST fallback |
  | `ticket_create` | Create a ticket (title required; parents supported; no `triage` — refused, it is Hermes' internal intake lane). `assignee: "default"` is refused — that is the kanban host's own worker profile and this bridge is the external lane (`MCP_ALLOW_DEFAULT_ASSIGNEE=true` opts out) |
  | `kanban_help` | Full MCP-native usage + lifecycle contract |

- All board-taking tools (`ticket_events`/`claim`/`comment`/`get`/`complete`/`block`/`list`/`create`) reject an omitted `board` — it is never silently defaulted, so multi-board setups can't land tickets in the wrong queue. The per-ticket tools also declare `id` and `board` **required** in their JSON Schema, so strict MCP clients fail fast on omission.

- **Static bearer auth** (custom middleware: constant-time compare, exact JSON 401 contract for opencode) + per-IP rate limiter.
- **Single static binary**, Go 1.25+, official `modelcontextprotocol/go-sdk` v1.7.0.
- **Context-budget discipline**: every tool result is truncated to a hard size budget (2 KB write tools, 6 KB `ticket_list`/`ticket_events`, 8 KB `ticket_get`/`review_queue`, 32 KB `ticket_get` with `detail: full`); the board is never dumped wholesale.
- **No silent truncation** (`ticket_get`): `detail: partial` (the default) applies the bounded caps; `detail: full` returns complete comment and ticket-body text for long-form content (steers, revision comments, review verdicts). Clipping is always visible — an inline `…(N more)` marker, a per-comment `truncated` flag, `truncated.body`/`truncated.comments`, and `comments_total`/`comments_returned`/`comments_dropped` counts. Under budget pressure the **oldest** comments are dropped first, so the newest (the live review thread) survive. The render guard never clips a payload to fit: if a projection is oversized it returns an explicit error instead of emitting invalid JSON, which is what the old 75%-chop loop did. The same rule governs the **backend read**: `GET /board` carries every card's full body, so a board's view grows with its history (the 295-card togather board is 1.14 MB). `doJSON` therefore reads a response up to a 16 MiB cap and *refuses* anything larger by name (`response exceeds the N-byte read cap`) — the earlier 1 MiB clip returned a half-document, which decoded to `unexpected end of JSON input` and read to every caller as a backend fault rather than a bridge limit.

## The claim mechanism (why `ticket_claim` shells out)

The kanban REST API has **no claim endpoint** (PATCH `status=running` is rejected by the kernel) — `ready → running` is kernel/CLI-native. `ticket_claim` therefore:

1. **Preflights over REST** — reads the ticket; `ready` is claimable, a running ticket with a held lock reports already-claimed, anything else is rejected *without* exec.
2. **Writes the lane's identity, then shells out** — `hermes kanban assign <id> <label>` (label from `worker`, else `MCP_CLAIM_WORKER`, else `remote-mcp`), reads it back, then `hermes kanban claim --ttl <seconds> <id>` (the sanctioned binding to the kernel's `claim_task`), with argv/env hardening, timeout, and injection tests. The order is the point: `assign_task` refuses only a RUNNING claimed card, and `_claim_and_open_run` copies `tasks.assignee` into the run row at claim time — which is the value `request_review` records as the handoff's implementer. So identity is set once, at claim, and the review handoff inherits it. The label must be stable per lane (changing a card's assignee resets the kernel's `consecutive_failures`), and a label that IS an installed profile is refused before any CLI call: the dispatcher claims a ready card assigned to an on-disk profile within about a minute, which would put a second worker on the lane's card. If the claim fails after the label landed, the tool assigns `none` back (and says so when even that fails).
3. **Re-reads the ticket** and returns the authoritative post-claim state — never the CLI's stdout — including the assignee and status, which are checked rather than assumed.

The binary resolves the CLI via `HERMES_BIN` (default `hermes`). The claim TTL comes from `MCP_CLAIM_TTL_SECONDS` (default **14400s = 4 hours**) and is passed as an explicit `--ttl` on every claim, so the CLI's own 900s default never applies through this path — and neither does `HERMES_KANBAN_CLAIM_TTL_SECONDS`, because the CLI's argparse default is a call-site value that outranks the environment in the kernel's resolution order (measured 2026-10-18: with that knob at 3600, claims still expired at 900s). The tool reports the applied `claim_ttl_seconds` and its `claim_ttl_source`, and warns when the kernel's recorded expiry disagrees with what was asked. When a claim does expire the dispatcher re-queues the ticket to `ready` **and books a failed attempt against it** (`consecutive_failures` increments), so `ticket_claim` works again but the ticket is one step closer to its retry limit.

**The preflight refusal is written per status, and there is no promote tool.** A blanket "not claimable: requires another state" refusal tells a caller nothing about which lane it is in and reads like a missing step — an agent spent a run (2026-10-18) hunting for a promote verb that this bridge has never had. So the refusal names the reason for the status it found: `triage` is Hermes' internal intake lane for under-specified input (a specifier rewrites the card's title and body, so no agent action exists and the card goes to a human); a `todo` ticket names the parent dependencies it waits on, because the kernel promotes it when they complete; `scheduled` is waiting on time; `blocked` must be unblocked by someone else; `running` and `review` are held by a claim or a reviewer. `ticket_create` refuses `triage: true` for the same reason, and the right move for a card that is not yours to start is to comment why or block it with `needs_input`.

**Related knobs** (env):
- `MCP_CLAIM_TTL_SECONDS` — claim TTL in seconds, passed as an explicit `--ttl`; default `14400` (4 hours), must be a positive integer (an unusable value falls back to the default and `claim_ttl_source` says so). See the claim mechanism above for why this is a flag and not `HERMES_KANBAN_CLAIM_TTL_SECONDS`.
- `MCP_ALLOW_SKIP_CLAIM=true` — `ticket_complete` skips its claim guard (it refuses to complete an unclaimed ticket otherwise).
- `MCP_ALLOW_DEFAULT_ASSIGNEE=true` — re-enables `ticket_create` assigning a ticket to the LOCAL `default` profile. Off by default, and the refusal is loud on purpose: MCP is the external lane, whose repo lives on another host, while `default` is the profile the kanban host's dispatcher spawns — so the card would start a local worker there, on a box that normally cannot push. Pass the lane's implementer label instead: the deploy template ships `MCP_CLAIM_WORKER=remote-mcp` and `MCP_COMMENT_AUTHOR=remote-mcp`, and `ticket_claim` assigns that label before claiming, so the review handoff inherits it and needs no `worker` argument. Set the knob to the lane's **stable** control-plane label: a review handoff still REFUSES when the card carries no label and neither `worker` nor the knob supplies one (the tool reads the variable, not the config default).
- `MCP_COMPLETE_MODE=done` — complete to `done` instead of the default review path. The default hands a `review_tier` `MEDIUM`/`HIGH`/omitted completion to the **native review lane**: the tool posts the completion comment, then shells out to `hermes kanban request-review`, so the card lands in `review` — the state the dispatcher claims as a REVIEW run and spawns a reviewer on. It does **not** write the older `review-required` block, which has had no consumer since `review-sweeper` was retired (`lane_return.py` ignores worker-phase blocks by design). An explicit `review_tier: "LOW"` always completes to `done`. `MCP_REVIEWER_PROFILE` names the reviewer (verified against the installed profile roster).
- `MCP_COMMENT_AUTHOR` — default comment author.
- `KANBAN_DEFAULT_BOARD` — board slug surfaced by `board_list` (informational only; board-taking tools reject an omitted `board`).
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
- `deploy/install.md` — install steps + MCP client configuration (opencode, codex, Claude Code, raw HTTP) in §7
- `deploy/client-config.sh` — prints a ready-to-paste client config with the token already in it (`opencode`, `codex`, `claude`, `curl`)
- `deploy/codex-kanban.sh` — launches codex with `MCP_BEARER_TOKEN` set from the service env file, so codex's `bearer_token_env_var` resolves without exporting a secret everywhere
- `scripts/smoke.sh` — end-to-end smoke + secret-hygiene checks
- `cmd/kanban-mcp/probe` — read-only live probe CLI
- `sweeper/` — the review-sweeper cron job (Python) that reviews `review-required` tickets end-to-end; see `sweeper/README.md` + `deploy/review-sweeper.md`

## Status

Implementation complete and smoke-verified (2026-08-03): all tools wired, `go build` / `go vet` / `go test -race` green, live smoke against a real dashboard (login, claim, create verified). Since then: `kanban_help` (2026-08), `ticket_events` (2026-08), required id+board schemas, the `review_tier` completion knob, the single-call `review_queue` scan (2026-08), the review-sweeper cron job adopted into `sweeper/` (2026-08), `ticket_complete` passing `created_cards` over REST in done mode (kernel-verified, 2026-08), and the `ticket_get` detail modes + no-silent-truncation read path (2026-09: `detail: partial|full`, flagged drops/clips, oldest-dropped-first, `renderResult` no longer raw-chops an oversized payload into invalid JSON). The remaining Hermes-side backlog (REST claim endpoint on the kanban plugin) is tracked on the project board. Design + experiment record: `~/Documents/assistant/research/planning/hermes-kanban-mcp-design.md` and `notes/2026-08-03-hermes-kanban-mcp-pipeline-experiment.md` (local wiki).
