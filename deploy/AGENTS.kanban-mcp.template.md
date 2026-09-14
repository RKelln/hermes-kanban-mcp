# AGENTS.md — kanban workflow drop-in (copy to any project repo using the hermes-kanban MCP)

> This project's work items live on a **Hermes kanban board**. Use the `hermes-kanban` MCP server
> (remote MCP, URL `http://100.126.212.105:9130/mcp`, bearer token in your opencode config).
> Board slug for this project: `<BOARD_SLUG>` (default: `hermes-kanban-mcp`).

## Workflow (extremely succinct)

0. **Always pass `board` and `id`** — all board-taking tools require `board` (omitting it is rejected, never defaulted) and the per-ticket tools require `id` too. Capture the `id` from the `ticket_create` response and reuse it verbatim.
1. **Orient** — `board_list` first. Then `ticket_list` (status filters; summaries only).
2. **Read** — `ticket_get` for full detail on a ticket you'll touch. **Pass `detail: "full"` whenever you must read a comment verbatim** (a steer, a revision comment, a review verdict): the default `partial` mode **caps comment bodies**, and the clipped tail is usually the payload — the design decision, the requested change, the commit ref. Keep `partial` for scanning and for the ticket's own body/metadata; `kanban_help` reports the current caps.
3. **Claim before work** — `ticket_claim` (`ready → running`). The claim expires; re-claim if it did. Never edit without claiming.
4. **Track** — `ticket_comment` for decisions/context as you work.
5. **Finish — push, then request review.** Push your branch FIRST, then `ticket_request_review` with a `summary` that is a real handoff: what changed, the repo/branch/sha to verify, and what you already proved (do not make the reviewer re-derive it). The ticket moves to `review`, the dispatcher claims it as a REVIEW run and spawns the `sdlc-review` reviewer, and the verdict comes back to you. `reviewer` is optional — the server default (`MCP_REVIEWER_PROFILE`) or the ticket's own assignee is used — but if none of those resolves, the call is **refused**: an unassigned review ticket is never dispatched, so the tool will not perform a transition nobody will act on.
   - `ticket_complete` is the **older** review path: `review_tier: "LOW"` (or `MCP_COMPLETE_MODE=done`) completes direct to done, while `MEDIUM`/`HIGH` block with `review-required:` for a human/sweeper. Use it when the server has no reviewer profile configured. Push + record repo/branch/commit either way.
6. **Wait for the verdict — and do not merge before it.** Long-poll `ticket_events` (pass the last seen event id; `kanban_help` has the default and maximum wait) or `ticket_get` until the ticket leaves `review` (or leaves `blocked`, on the `ticket_complete` path):
   - `done` → **now** merge your branch to main. The reviewer never merges; `done` is what authorises it. Merging before the verdict is a workflow violation — it can delete the very branch the reviewer needs, and a ticket whose branch is gone cannot be reviewed at all.
   - back to `ready` → REQUEST CHANGES → re-claim, fix, push, `ticket_request_review` again.
   - still `blocked` → ESCALATED → surface to the human; do not re-loop.
7. **Blockers** — `ticket_block` with a reason; kinds: `dependency | needs_input | capability | transient`. Never silently stall.

## Rules

- **Never touch the live Hermes install tree** (`~/.hermes`) — work in your repo/worktree.
- **Trust the truncation flags, don't guess.** Every `ticket_get` result reports what it lost: `truncated.body` / `truncated.comments`, a per-comment `truncated` flag, inline `…(N more)` markers, and `comments_total` / `comments_returned` / `comments_dropped`. If any of those is set and you needed the whole thing, re-read with `detail: "full"` — do not reconstruct clipped text, and do not assume an unflagged result was cut.
- **Ask before deploy/publish** — block with `ticket_block(reason="approval-required: ...")` and wait for the human.
- **Don't create tickets for yourself** — create follow-ups assigned to the right lane, or comment on the goal.
- Ticket lifecycle details: claims are kernel-enforced (a ticket with an open parent stays `todo`; gates are wired as parents). Full docs: call the MCP server's help tool if present, or see the repo's README/design docs.
