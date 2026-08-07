# AGENTS.md — kanban workflow drop-in (copy to any project repo using the hermes-kanban MCP)

> This project's work items live on a **Hermes kanban board**. Use the `hermes-kanban` MCP server
> (remote MCP, URL `http://100.126.212.105:9130/mcp`, bearer token in your opencode config).
> Board slug for this project: `<BOARD_SLUG>` (default: `hermes-agent`).

## Workflow (extremely succinct)

0. **Always pass `board` and `id`** — all board-taking tools require `board` (omitting it is rejected, never defaulted) and the per-ticket tools require `id` too. Capture the `id` from the `ticket_create` response and reuse it verbatim.
1. **Orient** — `board_list` first. Then `ticket_list` (status filters; summaries only).
2. **Read** — `ticket_get` for full detail on a ticket you'll touch (bodies are truncated; use it when you need more).
3. **Claim before work** — `ticket_claim` (`ready → running`, TTL ~15m). Never edit without claiming. Re-claim if it expired.
4. **Track** — `ticket_comment` for decisions/context as you work.
5. **Finish** — `ticket_complete` with a summary. **Review-gated by default**: blocks with `review-required:` for a human. `review_tier: "LOW"` (or `MCP_COMPLETE_MODE=done`) completes to done; `MEDIUM`/`HIGH` stay review-gated. Only use done when the task says done is fine. Push + record repo/branch/commit first.
6. **Wait for the review** — long-poll `ticket_events` (pass last event id) or `ticket_get` until the ticket leaves `blocked`: `done` → merge your branch to main (the reviewer never merges); `ready` → REQUEST CHANGES → re-claim, fix, re-complete; still `blocked` → ESCALATED → surface to the human; do not re-loop.
7. **Blockers** — `ticket_block` with a reason; kinds: `dependency | needs_input | capability | transient`. Never silently stall.

## Rules

- **Never touch the live Hermes install tree** (`~/.hermes`) — work in your repo/worktree.
- **Ask before deploy/publish** — block with `ticket_block(reason="approval-required: ...")` and wait for the human.
- **Don't create tickets for yourself** — create follow-ups assigned to the right lane, or comment on the goal.
- Ticket lifecycle details: claims are kernel-enforced (a ticket with an open parent stays `todo`; gates are wired as parents). Full docs: call the MCP server's help tool if present, or see the repo's README/design docs.
