#!/usr/bin/env python3
"""lane_return — hand a review-finished card back to the lane that owns it.

WHY THIS EXISTS
---------------
An external-lane handoff is un-returnable inside the kernel. The ticket is
unassigned, so `request_review` records `implementer: null`, so
`request_changes` refuses ("review handoff has no valid implementer
provenance") and the reviewer's only exit is a block. The card is then
`blocked`, and a lane can only `ticket_claim` a `ready` card.

Returning it is NOT `unblock`. `unblock_task` is provenance-sensitive: it
restores the phase the card was blocked in, so a card blocked BY A REVIEWER
RUN goes back to `review`, where the lane still cannot claim it. `promote_task`
is the verb that forces `ready` for a card in this phase, and it is subject to
two guards, both checked before any write: the card's status must be `todo` or
`blocked` (a card parked in `review` is refused, not forced to `ready`) and
every parent must already be terminal. A review-finished block satisfies both,
which is why this tick uses it and nothing else.
See t_f16003a1 (the kernel gap) and t_02c3e7d2 (the incident).

WHAT IT DELIBERATELY IS NOT
---------------------------
No LLM. No reviewer spawn. No branch check. No ledger. No GitHub. It is the
unblock half of the retired review-sweeper and nothing else, because the
review half is the native lane's job now and duplicating it is what produced
three review runs on one commit.

WHAT MAKES A CARD ACTIONABLE
----------------------------
Two independent gates must agree, so no single mistake can trigger a write:

  1. STRUCTURAL — the blocked event's `source_status` must be exactly "review".
     Every run phase writes that field, so it is the VALUE that discriminates,
     not its presence: on this box worker-phase blocks carry "ready" and
     user/lane blocks carry nothing, while "review" appears only on blocks
     written by review-phase runs.
  2. PROSE — the reason announces a changes-requested verdict ("review-required:"
     + REQUEST_CHANGES), and is not an ESCALATE.

Anything else is left exactly where it is. `--verbose` prints the phase, kind
and reason for every card the tick declined to touch, so silence is auditable.

READS vs WRITES
---------------
Reads are read-only SQL (`mode=ro`) against the boards' own SQLite files: the
CLI's `list --json` carries no block reason and dumps full bodies, and `show` is
one process per card with a known crash bug. A read-only connection cannot
corrupt anything, but it does make the tick schema-coupled to
`tasks(id, status, assignee, claim_lock)` and
`task_events(task_id, kind, payload, created_at)` — a board-schema change breaks
it loudly (`ERROR: cannot read <db>`), which is the intended failure mode.

Writes go through the CLI (`hermes kanban promote`, `... comment`) so every
mutation is a kernel operation with an audit event, never a direct DB write.
That also keeps the CLI fence honest: the CLI refuses to mutate from a
delegated-child context, and the cron scheduler does not set that marker for a
`no_agent` script.

Silence contract: an empty tick prints nothing. Actions and warnings print.
Errors print loudly and set a non-zero exit so the cron surfaces them — including
a failed audit comment AFTER a successful promote, which is reported as an error
naming the state change that did happen.
"""

from __future__ import annotations

import argparse
import json
import os
import sqlite3
import subprocess
import sys
import time

REVIEW_PREFIX = "review-required:"
MARKER_CHANGES = ("REQUEST_CHANGES", "REQUEST CHANGES")
MARKER_ESCALATE = "ESCALATE"
AUTHOR = "lane-return"

STATE_SUBDIR = os.path.join("state", "lane-return")
LOCK_NAME = "lane-return.pid"


def hermes_home() -> str:
    return os.environ.get("HERMES_HOME") or os.path.expanduser("~/.hermes")


def hermes_bin() -> str:
    return os.environ.get("HERMES_BIN") or "hermes"


def board_db(home: str, slug: str) -> str:
    """Default board lives at the top level, project boards in boards/<slug>/."""
    if slug == "default":
        return os.path.join(home, "kanban.db")
    return os.path.join(home, "kanban", "boards", slug, "kanban.db")


def run_cli(*argv: str) -> tuple[int, str]:
    cmd = [hermes_bin(), "kanban", *argv]
    proc = subprocess.run(cmd, capture_output=True, text=True, timeout=120)
    return proc.returncode, (proc.stdout + proc.stderr).strip()


class BoardSelectionError(RuntimeError):
    """A `--board` request that resolves to nothing: loud, never a silent no-op.

    Silence means "nothing to do" and the cron delivers stdout, so an empty
    selection must not be able to impersonate an idle tick: with `--verbose`
    the contract is that no-ops are reported so silence is auditable, and the
    sweeper this tick replaces is remembered for `last_status: ok` while blind.
    """


def board_slugs(only: str | None) -> list[str]:
    rc, out = run_cli("boards", "list", "--json")
    if rc != 0:
        raise RuntimeError("boards list failed: %s" % out)
    slugs = [b.get("slug") for b in json.loads(out) if b.get("slug")]
    if not slugs:
        # Every box has at least the default board, so an empty list is a read
        # that failed, not a board set that is genuinely empty.
        raise BoardSelectionError(
            "boards list --json returned no boards%s; refusing to report an idle "
            "tick while blind" % (" (requested --board %r)" % only if only else ""))
    if only is not None and only not in slugs:
        raise BoardSelectionError(
            "--board %r is not a board on this box (known: %s); refusing to report "
            "an idle tick for a board that does not exist"
            % (only, ", ".join(slugs)))
    return [s for s in slugs if only is None or s == only]


def connect(db: str) -> sqlite3.Connection:
    return sqlite3.connect("file:%s?mode=ro" % db, uri=True)


def latest_blocked_event(conn: sqlite3.Connection, task_id: str) -> dict | None:
    """The payload of a card's most recent `blocked` event."""
    row = conn.execute(
        "SELECT payload FROM task_events WHERE task_id = ? AND kind = 'blocked' "
        "ORDER BY id DESC LIMIT 1",
        (task_id,),
    ).fetchone()
    if not row or not row[0]:
        return None
    try:
        payload = json.loads(row[0])
    except ValueError:
        return None
    return payload if isinstance(payload, dict) else None


def blocked_cards(conn: sqlite3.Connection) -> list[tuple[str, str | None, dict | None]]:
    """(id, assignee, latest blocked event payload) for every blocked card."""
    out = []
    for tid, assignee in conn.execute("SELECT id, assignee FROM tasks WHERE status = 'blocked'"):
        out.append((tid, assignee, latest_blocked_event(conn, tid)))
    return out


def parked_reviews(conn: sqlite3.Connection) -> list[tuple[str, float]]:
    """(id, minutes since the newest event) for review cards holding no claim.

    A card parked in `review` is re-claimed and re-reviewed EVERY tick:
    check_respawn_guard deliberately skips its dedup for the review lane
    (kanban_db_dispatch.py:1180-1181). We warn rather than act, because the
    semantics of a parked review are ambiguous and guessing is how the
    sweeper acquired its failure catalogue.
    """
    out = []
    for tid, in conn.execute(
        "SELECT id FROM tasks WHERE status = 'review' AND (claim_lock IS NULL OR claim_lock = '')"
    ).fetchall():
        row = conn.execute(
            "SELECT MAX(created_at) FROM task_events WHERE task_id = ?", (tid,)
        ).fetchone()
        if row and row[0]:
            out.append((tid, (time.time() - float(row[0])) / 60.0))
    return out


def classify_block(reason: str | None) -> str:
    """'changes' to act, 'leave' to do nothing — prose half.

    Deliberately conservative. Only a reason that ANNOUNCES ITSELF as a
    review handoff carrying a changes-requested verdict is actionable; an
    ESCALATE (or anything ambiguous) is a human's call, and unmarked
    `review-required:` blocks belong to whatever workflow wrote them.
    """
    if not reason:
        return "leave"
    head = reason.strip()[:200]
    if not head.lower().startswith(REVIEW_PREFIX):
        return "leave"
    upper = head.upper()
    if MARKER_ESCALATE in upper:
        return "leave"
    if any(m in upper for m in MARKER_CHANGES):
        return "changes"
    return "leave"


def classify_block_event(payload: dict | None) -> str:
    """'changes' to act, 'leave' to do nothing — the full gate.

    Structural half FIRST: the block must have been written while the card was
    in the review phase (`source_status == "review"`). The field is written by
    every phase, so the discriminating fact is its VALUE: measured on this box,
    worker-phase blocks carry "ready" and user/lane blocks carry nothing, while
    "review" appears only on review-phase blocks. So it says "a reviewer wrote
    this" without reading a word of the reviewer's prose. That matters: a
    worker whose handoff reason happened to quote a changes-request must not be
    promoted past its review.

    Then the prose half decides WHICH reviewer block this is, because a
    reviewer's verdicts are not all returns — an ESCALATE stays blocked for a
    human, and an APPROVED-but-blocked card is somebody else's bug.
    """
    if not payload or payload.get("source_status") != "review":
        return "leave"
    return classify_block(payload.get("reason"))


class Lock:
    """Single-instance guard: a live pid blocks a concurrent tick; a dead one
    is taken over (the sweeper's singleton, minus the cron-specific parts)."""

    def __init__(self, path: str):
        self.path = path
        self.acquired = False

    def acquire(self) -> bool:
        os.makedirs(os.path.dirname(self.path), exist_ok=True)
        if os.path.exists(self.path):
            try:
                with open(self.path, encoding="utf-8") as fh:
                    pid = int(fh.read().strip() or 0)
            except (OSError, ValueError):
                pid = 0
            if pid and _alive(pid):
                return False
            # Stale (dead pid, or unreadable): clear it before the O_EXCL
            # create. Two processes may both unlink; one gets FileNotFoundError
            # (ignored) and the O_EXCL create still admits only one winner.
            try:
                os.unlink(self.path)
            except OSError:
                pass
        # O_EXCL: create-or-fail atomically, so two first-runs in the same
        # instant cannot both acquire (a read-then-write test-then-open can).
        try:
            fd = os.open(self.path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        except FileExistsError:
            return False
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.write(str(os.getpid()))
        self.acquired = True
        return True

    def release(self) -> None:
        if self.acquired:
            try:
                os.unlink(self.path)
            except OSError:
                pass


def _alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="Return review-finished cards to their lane.")
    ap.add_argument("--board", help="limit to one board slug")
    ap.add_argument("--dry-run", action="store_true", help="report, change nothing")
    ap.add_argument("--max-actions", type=int, default=20, help="safety cap per tick")
    ap.add_argument("--warn-after-minutes", type=int, default=60,
                    help="warn about a claimless review card idle this long")
    ap.add_argument("--verbose", action="store_true", help="report no-ops too")
    args = ap.parse_args(argv)

    home = hermes_home()
    lock = Lock(os.path.join(home, STATE_SUBDIR, LOCK_NAME))
    if not lock.acquire():
        if args.verbose:
            print("lane-return: another instance holds the lock; nothing done")
        return 0

    failures = 0
    actions = 0
    try:
        try:
            slugs = board_slugs(args.board)
        except (RuntimeError, ValueError) as exc:
            print("lane-return: ERROR: %s" % exc)
            return 1

        for slug in slugs:
            db = board_db(home, slug)
            if not os.path.exists(db):
                continue  # board dir with no DB yet
            try:
                conn = connect(db)
            except sqlite3.Error as exc:
                print("lane-return: ERROR: cannot read %s: %s" % (db, exc))
                failures += 1
                continue
            try:
                for tid, assignee, payload in blocked_cards(conn):
                    verdict = classify_block_event(payload)
                    if verdict != "changes":
                        if args.verbose:
                            reason = (payload or {}).get("reason") or "(no reason)"
                            print("lane-return: %s/%s leave [phase=%s kind=%s] (%s)"
                                  % (slug, tid, (payload or {}).get("source_status") or "-",
                                     (payload or {}).get("kind") or "-",
                                     reason.strip()[:60]))
                        continue
                    if actions >= args.max_actions:
                        print("lane-return: %s: hit --max-actions=%d; stopping here"
                              % (slug, args.max_actions))
                        break
                    actions += 1
                    if args.dry_run:
                        print("lane-return: DRY-RUN would promote %s/%s (changes-requested "
                              "verdict; assignee=%s)" % (slug, tid, assignee or "-"))
                        continue
                    ok = _promote(slug, tid, (payload or {}).get("reason") or "", conn)
                    if ok:
                        print("lane-return: %s/%s promoted to ready (changes-requested "
                              "verdict returned to the lane)" % (slug, tid))
                    else:
                        failures += 1

                for tid, idle in parked_reviews(conn):
                    if idle >= args.warn_after_minutes:
                        print("lane-return: WARN %s/%s has been in 'review' with no claim for "
                              "%.0fm — the dispatcher re-claims and re-reviews such a card every "
                              "tick; check for a reviewer that exited without a verdict."
                              % (slug, tid, idle))
            finally:
                conn.close()
    finally:
        lock.release()

    return 1 if failures else 0


def _promote(slug: str, tid: str, reason: str, conn: sqlite3.Connection) -> bool:
    """Promote, then VERIFY the effect from the board — never trust the exit code.

    Returns True only when the card is verifiably `ready` AND the audit comment
    landed. A failed comment after a successful promote is deliberately a
    FAILURE (non-zero exit, so the cron alerts): the state change is correct, so
    the message says so explicitly, but an unattributed action on a board whose
    whole purpose is a trustworthy trail is not a warning-level event.
    """
    note = ("%s: changes-requested verdict returned to the lane so it can re-claim "
            "-> status=ready (block reason matched: %s)"
            % (AUTHOR, (reason or "").strip()[:80]))
    rc, out = run_cli("--board", slug, "promote", tid, note, "--json")
    if rc != 0:
        print("lane-return: ERROR: promote %s/%s failed: %s" % (slug, tid, out))
        return False
    row = conn.execute("SELECT status FROM tasks WHERE id = ?", (tid,)).fetchone()
    if not row or row[0] != "ready":
        print("lane-return: ERROR: promote %s/%s reported success but the board says %r "
              "(expected ready)" % (slug, tid, row[0] if row else None))
        return False
    # Audit comment, attributed so it can never be mistaken for a reviewer verdict.
    rc, out = run_cli("--board", slug, "comment", tid, note, "--author", AUTHOR)
    if rc != 0:
        print("lane-return: ERROR: %s/%s WAS moved to ready, but the audit comment "
              "failed so the action is not in the trail: %s" % (slug, tid, out))
        return False
    return True


if __name__ == "__main__":
    sys.exit(main())
