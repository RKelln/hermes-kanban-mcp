# AGENTS.md — kanban workflow drop-in (copy to any project repo using the hermes-kanban MCP)

> This project's work items live on a **Hermes kanban board**. Use the `hermes-kanban` MCP server
> (remote MCP, URL `http://100.126.212.105:9130/mcp`, bearer token in your opencode config).
> Board slug for this project: `<BOARD_SLUG>` (default: `hermes-agent`).

## Workflow (extremely succinct)

1. **Orient** — `board_list` first. Then `ticket_list` (status filters; summaries only).
2. **Read** — `ticket_get` for full detail on a ticket you'll touch (bodies are truncated; use it when you need more).
3. **Claim before work** — `ticket_claim` (`ready → running`, TTL ~15m). Never edit without claiming. Re-claim if it expired.
4. **Track** — `ticket_comment` for decisions/context as you work.
5. **Finish** — `ticket_complete` with a summary. **It's review-gated** by default: it blocks the ticket with `review-required:` and a human reviews. `review_tier: "LOW"` completes direct to done; `"MEDIUM"` (default) / `"HIGH"` stay review-gated. `MCP_COMPLETE_MODE=done` also forces done for MEDIUM/HIGH. Only use done semantics when the task body explicitly says done is fine.
6. **Blockers** — `ticket_block` with a reason; typed kinds: `dependency | needs_input | capability | transient`. Never silently stall.

## Rules

- **Never touch the live Hermes install tree** (`~/.hermes`) — work in your repo/worktree.
- **Ask before deploy/publish** — block with `ticket_block(reason="approval-required: ...")` and wait for the human.
- **Don't create tickets for yourself** — create follow-ups assigned to the right lane, or comment on the goal.
- Ticket lifecycle details: claims are kernel-enforced (a ticket with an open parent stays `todo`; gates are wired as parents). Full docs: call the MCP server's help tool if present, or see the repo's README/design docs.
