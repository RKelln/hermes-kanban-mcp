#!/usr/bin/env python3
"""Tests for scripts/lane_return.py.

Hermetic: a throwaway HERMES_HOME holding minimal board DBs (one at the default
path and one at the project path, because the tick derives those two layouts
differently), and a fake `hermes` CLI that records every argv it receives — so
the tests assert what the tick actually asked the kernel to do, not just what
it printed.

Run:  python3 scripts/tests/test_lane_return.py       (stdlib unittest)
"""

from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import os
import sqlite3
import tempfile
import textwrap
import time
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.normpath(os.path.join(HERE, "..", "lane_return.py"))

SCHEMA = """
CREATE TABLE tasks (id TEXT PRIMARY KEY, title TEXT, status TEXT,
                    assignee TEXT, claim_lock TEXT, created_at INTEGER);
CREATE TABLE task_events (id INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT,
                          kind TEXT, payload TEXT, created_at INTEGER);
"""

# The fake CLI. Mirrors the real board-path rule so `promote` mutates the DB the
# tick is reading, and appends every argv to $FAKE_LOG. FAKE_PROMOTE_NOOP makes
# promote report success WITHOUT changing state — the case the tick's
# post-verification exists to catch. FAKE_PROMOTE_FAIL is the parent-refusal.
FAKE_CLI = textwrap.dedent(
    """
    #!/usr/bin/env python3
    import json, os, sqlite3, sys
    argv = sys.argv[1:]
    with open(os.environ["FAKE_LOG"], "a", encoding="utf-8") as fh:
        fh.write(json.dumps(argv) + "\\n")
    if "boards" in argv and "list" in argv:
        # FAKE_BOARDS="" is the empty board list (`boards list --json` -> []).
        boards = [b for b in os.environ.get("FAKE_BOARDS", "probe").split(",") if b]
        print(json.dumps([{"slug": b} for b in boards]))
        sys.exit(0)
    if "promote" in argv:
        if os.environ.get("FAKE_PROMOTE_FAIL") == "1":
            print("unsatisfied parent dependencies: t_parent")
            sys.exit(1)
        if os.environ.get("FAKE_PROMOTE_NOOP") != "1":
            slug = argv[argv.index("--board") + 1]
            home = os.environ["HERMES_HOME"]
            db = (os.path.join(home, "kanban.db") if slug == "default"
                  else os.path.join(home, "kanban", "boards", slug, "kanban.db"))
            tid = argv[argv.index("promote") + 1]
            conn = sqlite3.connect(db)
            conn.execute("UPDATE tasks SET status = 'ready' WHERE id = ?", (tid,))
            conn.commit()
            conn.close()
        print(json.dumps({"ok": True}))
        sys.exit(0)
    if "comment" in argv:
        sys.exit(1 if os.environ.get("FAKE_COMMENT_FAIL") == "1" else 0)
    print("fake: unhandled argv %r" % (argv,))
    sys.exit(2)
    """
).lstrip()


def load_module():
    spec = importlib.util.spec_from_file_location("lane_return", SCRIPT)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


class Harness(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.home = self.tmp.name
        self.log = os.path.join(self.home, "fake-cli.log")
        self.bin = os.path.join(self.home, "fake-hermes")
        with open(self.bin, "w", encoding="utf-8") as fh:
            fh.write(FAKE_CLI)
        os.chmod(self.bin, 0o755)
        for slug, path in (("default", self.db_path("default")),
                           ("probe", self.db_path("probe"))):
            os.makedirs(os.path.dirname(path), exist_ok=True)
            conn = sqlite3.connect(path)
            conn.executescript(SCHEMA)
            conn.commit()
            conn.close()
        self.mod = load_module()
        self.env = mock.patch.dict(
            os.environ,
            {"HERMES_HOME": self.home, "HERMES_BIN": self.bin, "FAKE_LOG": self.log,
             "FAKE_BOARDS": "default,probe"},
            clear=False,
        )
        self.env.start()
        for key in ("FAKE_PROMOTE_FAIL", "FAKE_PROMOTE_NOOP", "FAKE_COMMENT_FAIL"):
            os.environ.pop(key, None)
        self.addCleanup(self.env.stop)
        self.addCleanup(self.tmp.cleanup)

    def db_path(self, slug):
        if slug == "default":
            return os.path.join(self.home, "kanban.db")
        return os.path.join(self.home, "kanban", "boards", slug, "kanban.db")

    def add_card(self, tid, status="blocked", assignee=None, claim_lock=None,
                 reason=None, event_kind="blocked", age_seconds=0, board="probe",
                 source_status="review"):
        conn = sqlite3.connect(self.db_path(board))
        conn.execute("INSERT INTO tasks VALUES (?, 't', ?, ?, ?, 0)",
                     (tid, status, assignee, claim_lock))
        payload = None
        if reason is not None:
            payload = json.dumps({"reason": reason, "source_status": source_status})
        elif source_status is not None:
            payload = json.dumps({"source_status": source_status})
        conn.execute("INSERT INTO task_events (task_id, kind, payload, created_at) "
                     "VALUES (?, ?, ?, ?)",
                     (tid, event_kind, payload, int(time.time()) - age_seconds))
        conn.commit()
        conn.close()

    def status(self, tid, board="probe"):
        conn = sqlite3.connect("file:%s?mode=ro" % self.db_path(board), uri=True)
        row = conn.execute("SELECT status FROM tasks WHERE id = ?", (tid,)).fetchone()
        conn.close()
        return row[0] if row else None

    def calls(self):
        if not os.path.exists(self.log):
            return []
        with open(self.log, encoding="utf-8") as fh:
            return [json.loads(line) for line in fh if line.strip()]

    def run_tick(self, *argv):
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            rc = self.mod.main(list(argv))
        return rc, buf.getvalue()


class TestMarkerClassification(Harness):
    def test_changes_requested_marker_is_actionable(self):
        for reason in (
            "review-required: REQUEST_CHANGES (2 DEFECT) on feat/x",
            "review-required: request changes — see findings",
            "review-required: REQUEST_CHANGES",
        ):
            with self.subTest(reason=reason):
                self.assertEqual(self.mod.classify_block(reason), "changes")

    def test_escalate_is_left_for_a_human(self):
        for reason in (
            "review-required: ESCALATE — could not verify the branch",
            "review-required: REQUEST_CHANGES + ESCALATE ambiguity",
        ):
            with self.subTest(reason=reason):
                self.assertEqual(self.mod.classify_block(reason), "leave")

    def test_unmarked_and_unrelated_blocks_are_left_alone(self):
        for reason in (
            "review-required: branch feat/x @ abc123 (push pending)",
            "approval-required: approve the config change",
            "decision-needed: pick one",
            None,
            "",
        ):
            with self.subTest(reason=reason):
                self.assertEqual(self.mod.classify_block(reason), "leave")

    def test_marker_deep_in_the_body_is_not_matched(self):
        # Only the head is examined: a quoted finding must not turn a normal
        # handoff block into an actionable verdict.
        reason = "review-required: branch feat/x @ abc — " + ("padding " * 40) + "REQUEST_CHANGES"
        self.assertEqual(self.mod.classify_block(reason), "leave")


class TestStructuralGate(Harness):
    """The phase field must agree with the prose. Either alone is not enough."""

    def test_a_review_phase_block_with_the_marker_is_actionable(self):
        payload = {"source_status": "review", "kind": "needs_input",
                   "reason": "review-required: REQUEST_CHANGES (1 DEFECT)"}
        self.assertEqual(self.mod.classify_block_event(payload), "changes")

    def test_a_worker_phase_block_quoting_the_marker_is_left_alone(self):
        # A worker whose handoff text happens to quote a changes-request must
        # not be promoted past its review.
        payload = {"source_status": "ready",
                   "reason": "review-required: REQUEST_CHANGES was requested, fixing now"}
        self.assertEqual(self.mod.classify_block_event(payload), "leave")

    def test_a_block_with_no_phase_is_left_alone(self):
        for payload in ({"reason": "review-required: REQUEST_CHANGES"},
                        {"source_status": None, "reason": "review-required: REQUEST_CHANGES"},
                        {"source_status": "review"},   # phase but no reason
                        {}, None):
            with self.subTest(payload=payload):
                self.assertEqual(self.mod.classify_block_event(payload), "leave")

    def test_an_approved_but_blocked_card_is_left_alone(self):
        # Seen live on the animatic board: a review-phase block whose verdict
        # is not a changes-request. Somebody else's bug, not ours to move.
        payload = {"source_status": "review", "kind": "needs_input",
                   "reason": "review-required: APPROVED-technical — awaiting Ryan"}
        self.assertEqual(self.mod.classify_block_event(payload), "leave")

    def test_end_to_end_worker_block_stays_blocked(self):
        self.add_card("t_w", reason="review-required: REQUEST_CHANGES", source_status="ready")
        rc, out = self.run_tick()
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.status("t_w"), "blocked")
        self.assertEqual([c for c in self.calls() if "promote" in c], [])


class TestActing(Harness):
    def test_changes_requested_is_promoted_then_verified(self):
        self.add_card("t_a", reason="review-required: REQUEST_CHANGES (1 DEFECT)")
        rc, out = self.run_tick()
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.status("t_a"), "ready")
        promotes = [c for c in self.calls() if "promote" in c]
        self.assertEqual(len(promotes), 1)
        self.assertIn("--board", promotes[0])
        self.assertIn("probe", promotes[0])
        self.assertIn("t_a", promotes[0])
        self.assertIn("--json", promotes[0])
        comments = [c for c in self.calls() if "comment" in c]
        self.assertEqual(len(comments), 1, "an audit comment must be posted")
        self.assertIn("lane-return", comments[0], "the comment must be attributed")
        self.assertIn("promoted", out)

    def test_the_default_board_path_is_also_handled(self):
        # The default board's DB lives at the top level, not boards/<slug>/.
        self.add_card("t_default", reason="review-required: REQUEST_CHANGES", board="default")
        rc, out = self.run_tick()
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.status("t_default", board="default"), "ready")

    def test_escalate_is_not_promoted(self):
        self.add_card("t_b", reason="review-required: ESCALATE — cannot verify")
        rc, out = self.run_tick()
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.status("t_b"), "blocked")
        self.assertEqual([c for c in self.calls() if "promote" in c], [])
        self.assertNotIn("promote", out)

    def test_verification_catches_a_silent_no_op(self):
        # promote reports success but nothing changed: the tick must NOT claim
        # it worked. This is the failure the sweeper never checked for.
        os.environ["FAKE_PROMOTE_NOOP"] = "1"
        self.add_card("t_c", reason="review-required: REQUEST_CHANGES")
        rc, out = self.run_tick()
        self.assertEqual(rc, 1, "a failed post-verification must exit non-zero: %r" % out)
        self.assertEqual(self.status("t_c"), "blocked")
        self.assertIn("reported success but the board says", out)

    def test_promote_refusal_is_reported_and_fails_the_tick(self):
        os.environ["FAKE_PROMOTE_FAIL"] = "1"
        self.add_card("t_d", reason="review-required: REQUEST_CHANGES")
        rc, out = self.run_tick()
        self.assertEqual(rc, 1, out)
        self.assertIn("unsatisfied parent", out)

    def test_max_actions_caps_a_runaway_tick(self):
        for i in range(5):
            self.add_card("t_%d" % i, reason="review-required: REQUEST_CHANGES")
        rc, out = self.run_tick("--max-actions", "2")
        self.assertEqual(rc, 0, out)
        self.assertEqual(len([c for c in self.calls() if "promote" in c]), 2)

    def test_a_failed_audit_comment_fails_the_tick(self):
        # The promote committed, so the state is right — but an unattributed
        # action is not a warning: exit non-zero and say the move happened.
        os.environ["FAKE_COMMENT_FAIL"] = "1"
        self.add_card("t_n", reason="review-required: REQUEST_CHANGES")
        rc, out = self.run_tick()
        self.assertEqual(rc, 1, out)
        self.assertEqual(self.status("t_n"), "ready", "the promote did commit")
        self.assertIn("WAS moved to ready", out)
        self.assertIn("not in the trail", out)

    def test_the_audit_comment_names_the_resulting_status(self):
        self.add_card("t_s", reason="review-required: REQUEST_CHANGES")
        self.run_tick()
        comment = [c for c in self.calls() if "comment" in c][0]
        self.assertIn("status=ready", " ".join(comment))

    def test_board_limits_the_tick_to_one_board(self):
        self.add_card("t_here", reason="review-required: REQUEST_CHANGES", board="probe")
        self.add_card("t_other", reason="review-required: REQUEST_CHANGES", board="default")
        rc, out = self.run_tick("--board", "probe")
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.status("t_here", board="probe"), "ready")
        self.assertEqual(self.status("t_other", board="default"), "blocked",
                         "--board must not touch another board")
        promotes = [c for c in self.calls() if "promote" in c]
        self.assertEqual(len(promotes), 1)
        self.assertIn("probe", promotes[0])

    def test_an_unknown_board_is_a_loud_error(self):
        # Was a silent no-op (rc 0, no output) even with --verbose, whose whole
        # job is to make no-ops auditable: an idle tick and a blind tick were
        # indistinguishable.
        self.add_card("t_x", reason="review-required: REQUEST_CHANGES")
        rc, out = self.run_tick("--board", "no-such-board", "--verbose")
        self.assertEqual(rc, 1, "an unknown --board must not pass for an idle tick: %r" % out)
        self.assertIn("no-such-board", out, "the error must name the requested slug")
        self.assertEqual(self.status("t_x"), "blocked", "nothing may be touched")
        self.assertEqual([c for c in self.calls() if "promote" in c], [])

    def test_an_empty_board_list_is_a_loud_error(self):
        os.environ["FAKE_BOARDS"] = ""   # `boards list --json` -> []
        self.add_card("t_y", reason="review-required: REQUEST_CHANGES")
        rc, out = self.run_tick("--board", "probe")
        self.assertEqual(rc, 1, out)
        self.assertIn("probe", out)
        self.assertEqual(self.status("t_y"), "blocked")
        self.assertEqual([c for c in self.calls() if "promote" in c], [])

    def test_an_empty_board_list_is_loud_without_a_board_flag(self):
        # The deployed cron passes no --board, so this is the tick-while-blind
        # case: an empty read must not be reported as "nothing to do".
        os.environ["FAKE_BOARDS"] = ""
        rc, out = self.run_tick()
        self.assertEqual(rc, 1, out)
        self.assertIn("no boards", out)

    def test_a_registered_board_with_no_db_yet_is_still_silent(self):
        # The loud cases are selection failures. A board the tick can see but
        # that has no DB file yet stays a quiet skip.
        os.unlink(self.db_path("default"))
        rc, out = self.run_tick()
        self.assertEqual(rc, 0, out)
        self.assertEqual(out, "", "a registered board without a DB is not an error")


class TestNotActing(Harness):
    def test_dry_run_changes_nothing(self):
        self.add_card("t_e", reason="review-required: REQUEST_CHANGES")
        rc, out = self.run_tick("--dry-run")
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.status("t_e"), "blocked")
        self.assertEqual([c for c in self.calls() if "promote" in c], [])
        self.assertIn("DRY-RUN", out)

    def test_parked_review_is_warned_not_touched(self):
        self.add_card("t_f", status="review", assignee="default",
                      event_kind="review_requested", age_seconds=3 * 3600)
        rc, out = self.run_tick("--warn-after-minutes", "60")
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.status("t_f"), "review")
        self.assertIn("WARN", out)
        self.assertIn("t_f", out)
        self.assertEqual([c for c in self.calls() if "promote" in c], [])

    def test_fresh_review_card_is_not_warned(self):
        self.add_card("t_g", status="review", assignee="default",
                      event_kind="review_requested", age_seconds=60)
        rc, out = self.run_tick("--warn-after-minutes", "60")
        self.assertEqual(rc, 0, out)
        self.assertNotIn("WARN", out)

    def test_claimed_review_card_is_ignored(self):
        self.add_card("t_h", status="review", assignee="default", claim_lock="w:1",
                      event_kind="review_requested", age_seconds=3 * 3600)
        rc, out = self.run_tick("--warn-after-minutes", "60")
        self.assertEqual(rc, 0, out)
        self.assertNotIn("WARN", out)

    def test_empty_tick_is_silent(self):
        rc, out = self.run_tick()
        self.assertEqual(rc, 0, out)
        self.assertEqual(out, "", "an idle tick must print nothing (cron delivers output)")


class TestLocking(Harness):
    def _lock_path(self):
        return os.path.join(self.home, "state", "lane-return", "lane-return.pid")

    def test_a_live_lock_blocks_a_second_instance(self):
        self.add_card("t_i", reason="review-required: REQUEST_CHANGES")
        os.makedirs(os.path.dirname(self._lock_path()), exist_ok=True)
        with open(self._lock_path(), "w", encoding="utf-8") as fh:
            fh.write(str(os.getpid()))  # this test process: alive
        rc, out = self.run_tick("--verbose")
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.status("t_i"), "blocked", "must not act while locked")
        self.assertIn("another instance holds the lock", out)

    def test_a_dead_lock_is_taken_over(self):
        self.add_card("t_j", reason="review-required: REQUEST_CHANGES")
        os.makedirs(os.path.dirname(self._lock_path()), exist_ok=True)
        with open(self._lock_path(), "w", encoding="utf-8") as fh:
            fh.write("999999999")  # no such pid
        rc, out = self.run_tick()
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.status("t_j"), "ready", "a dead lock must be taken over")

    def test_the_lock_is_released_after_a_run(self):
        self.add_card("t_k", reason="review-required: REQUEST_CHANGES")
        self.run_tick()
        self.assertFalse(os.path.exists(self._lock_path()),
                         "the lock must not survive the tick")


if __name__ == "__main__":
    unittest.main(verbosity=2)
