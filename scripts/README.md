# scripts/

## `lane_return.py` — returning a review-finished card to its lane

The native review lane can *run* a review but cannot *return* one. A card that a
reviewer blocks sits in `blocked`, and a lane can only claim a `ready` card. This
tick closes that gap: no LLM, no reviewer spawn, no branch check, no ledger.

    python3 scripts/lane_return.py [--board <slug>] [--dry-run] [--verbose]

Deployed as the `lane-return` cron job (`*/2 * * * *`, `no_agent`) via
`~/.hermes/scripts/lane_return_wrapper.sh`, which execs this repo copy. The
wrapper must stay a real file inside `~/.hermes/scripts/` — the scheduler refuses
a symlink that resolves outside it.

### The state machine: which verb returns which state

This is the question that cost a round trip, so it is written down here rather
than only in the module docstring.

| verb | `blocked` card → | `review` card → | notes |
|---|---|---|---|
| `unblock` | **restores the phase it was blocked in** | n/a | `unblock_task` (`kanban_db.py:3264-3305`) reads `resume_status` from the block event's `source_status`, so a card blocked *by a review run* unblocks back to **`review`** — where the lane still cannot claim it, and the dispatcher spawns another reviewer on an unchanged tree. |
| `promote` | **`ready`** unless a parent is unfinished | refused | `promote_task` (`:3185-3231`) has no run-provenance logic, but it has two guards, and both are checked *before* any write: the card's status must be `todo` or `blocked` (`:3196-3200` — a card parked in `review` is **refused**, not forced to `ready`), and every parent must already be terminal (`:3205-3217`). **This is the verb for a reviewer-blocked card** — the one status pair both guards admit. |
| `reopen-review` | refused | `ready` / `todo` | the exit for a card already parked in `review`; provenance-*tolerant*. |
| `request-changes` | — | → `ready` assigned to the implementer | **refuses outright** when the handoff recorded `implementer: null` (`:3146-3147`), which is the gap this tick exists for. |

There is no REST route for `promote`: the dashboard's `_drag_to` maps
`blocked`→`ready` to `unblock_task` and `review`→`ready` to `reopen_review_task`.
So the tick shells out to the CLI.

### What makes a card actionable

Two independent gates, so no single mistake triggers a write:

1. **Structural** — the newest `blocked` event's `source_status` must be exactly
   `"review"`. Every phase writes that field, so the *value* discriminates:
   worker-phase blocks carry `"ready"`, user/lane blocks carry nothing, and
   `"review"` appears only on blocks written by a review-phase run.
2. **Prose** — the reason head announces `review-required:` plus a
   changes-requested verdict, and is not an `ESCALATE`.

A reviewer block that is an ESCALATE, an approval, or anything unclassifiable is
left exactly where it is, for a human. `--verbose` prints the phase, kind and
reason of every card it declined to touch.

### Read path (deliberate drift from the original ticket)

Reads are **read-only SQL** (`mode=ro`) against the boards' own SQLite files, not
`hermes kanban`. Reason: `list --json` carries no block reason and dumps full
bodies, and `show` is one process per card with a known crash bug. A read-only
connection cannot corrupt state, and the cost is that this script is
schema-coupled to `tasks(id, status, assignee, claim_lock)` and
`task_events(task_id, kind, payload, created_at)` — a schema change breaks it
loudly (`ERROR: cannot read <db>`), which is the intended failure mode.

Writes go through the CLI (`promote`, `comment`) so every mutation is a kernel
operation with an audit event. The CLI also refuses to mutate from a
delegated-child context, and a `no_agent` cron script is not marked as one.

### Behaviour contract

- An idle tick prints **nothing** (the cron delivers stdout, so silence is the
  correct idle signal). Actions and warnings print.
- `--board <slug>` that names no board on the box, or a `boards list --json`
  that comes back empty, is a **loud error** with a non-zero exit that names the
  requested slug — a selection that resolves to nothing must not impersonate an
  idle tick. A registered board that has no DB yet is still skipped in silence:
  that is a board the tick can see, not one it cannot find.
- A promote that reports success but does not change the board is an **error**,
  not a success — the tick re-reads the status before claiming anything.
- A failed audit comment *after* a successful promote is an **error** with a
  non-zero exit that names the state change that did happen: the card is fine,
  the trail is not.
- At most `--max-actions` (default 20) promotions per tick.
- It never touches a card blocked for a human decision (`decision-needed:`,
  `approval-required:`, plain handoff blocks, unmarked `review-required:` blocks).

### Tests

    python3 scripts/tests/test_lane_return.py

Hermetic: a throwaway `HERMES_HOME` with board DBs at both the default and
project paths, and a fake `hermes` CLI that records every argv. Negative
controls disable each guard in the source in turn and require the guarding test
to fail; the file is restored byte-identically afterwards.

### Related

`t_f16003a1` (the kernel gap: `request_changes` refuses a handoff with no
implementer provenance — and, measured 2026-09-14, that is any run that records
no profile, i.e. every hand-claimed/external handoff, not just remote ones),
`t_2a8c802c` (this tick), `t_a13f3c53` (retiring the review-sweeper),
`t_ff666dc2` (the cutover).
