#!/usr/bin/env python3
#
# kernel-provenance-parity.py — build a parity corpus for the request-review
# pre-check's mirror of the kernel's re-review provenance rule.
#
# Usage:
#   python3 scripts/kernel-provenance-parity.py --out /tmp/parity.json
#   KANBAN_MCP_PARITY_FIXTURE=/tmp/parity.json \
#       go test ./internal/mcptools -run TestKernelProvenanceParity -v
#
# Env overrides: KANBAN_HOME (default ~/.hermes), KANBAN_BOARDS_GLOB
# (default <KANBAN_HOME>/kanban/boards/*/kanban.db), HERMES_SRC (default
# ~/.hermes/hermes-agent, the Hermes install that owns hermes_cli/kanban_db.py).
#
# What it does, per board database (read-only):
#
#   1. For EVERY ticket whose run history has a `changes_requested` run,
#      records the verdict of the KERNEL's own resolver
#      hermes_cli.kanban_db._prior_reviewer, plus the ticket-detail envelope
#      (task + runs + events) exactly as the kanban REST API serves it.
#   2. For a sample of tickets WITHOUT such a run, records the same (the
#      kernel returns None there: a first review, where the assignee is
#      preserved).
#   3. For a set of MUTATED COPIES of one board database under a temp dir
#      (the live databases are never written), records the malformed-
#      provenance paths the live data does not contain: the round's event
#      deleted, its reviewer removed/blanked, the event attributed to another
#      run, a NEWER round with no usable provenance shadowing an older usable
#      one, and a newer round with its own reviewer. The kernel function runs
#      for real against each copy, so those verdicts are kernel verdicts
#      rather than assumptions about the kernel.
#
# The corpus is consumed by internal/mcptools/review_provenance_parity_test.go,
# which asserts the tool's kernelPriorReviewer returns the reviewer the kernel
# LANDS exactly when the kernel reports a usable one, and no reviewer exactly
# when the kernel reports None (first review) or False (unusable provenance).
# Re-run it whenever kanban_db.py changes (an upstream Hermes update can move
# this rule).
#
# Coverage note: the value/False paths are exercised by the mutation cases and
# not by the live boards, because live data contains very few tickets that ever
# had a request-changes round (one at the time of writing). That is a property
# of the boards, not of the check: every corpus case still gets its verdict
# from the kernel's own resolver. Point --mutation-source at a board with a
# busier review history to widen the real-data share.
#
import argparse
import json
import os
import pathlib
import shutil
import sqlite3
import sys
import tempfile

KANBAN_HOME = pathlib.Path(os.environ.get("KANBAN_HOME", pathlib.Path.home() / ".hermes"))
BOARDS_GLOB = os.environ.get("KANBAN_BOARDS_GLOB", "kanban/boards/*/kanban.db")
HERMES_SRC = pathlib.Path(os.environ.get("HERMES_SRC", KANBAN_HOME / "hermes-agent"))
NO_CR_SAMPLE = 25


def load_kernel():
    sys.path.insert(0, str(HERMES_SRC))
    import hermes_cli.kanban_db as kb  # noqa: E402

    return kb


def ro(db):
    con = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    con.row_factory = sqlite3.Row
    con.execute("PRAGMA query_only = 1")
    return con


def rw(db):
    con = sqlite3.connect(db)
    con.row_factory = sqlite3.Row
    return con


def envelope(con, tid):
    """The task-detail slice the pre-check reads, in the API's own shape."""
    runs = [dict(r) for r in con.execute(
        "SELECT id, outcome FROM task_runs WHERE task_id = ? ORDER BY id", (tid,))]
    events = []
    for r in con.execute(
            "SELECT id, kind, run_id, payload FROM task_events WHERE task_id = ? ORDER BY id",
            (tid,)):
        payload = None
        if r["payload"]:
            try:
                payload = json.loads(r["payload"])
            except ValueError:
                payload = None
        events.append({"id": r["id"], "kind": r["kind"], "run_id": r["run_id"],
                       "payload": payload})
    row = con.execute("SELECT id, status, assignee FROM tasks WHERE id = ?", (tid,)).fetchone()
    return {"task": {"id": tid, "status": row["status"], "assignee": row["assignee"]},
            "runs": runs, "events": events}


def case(kb, board, tid, con, origin):
    verdict = kb._prior_reviewer(con, tid)
    raw = verdict if isinstance(verdict, str) else ""
    # The kernel reads the raw payload string but LANDS the canonicalised one
    # (request_review -> _canonical_assignee -> normalize_profile_name), and
    # that landing form is what the pre-check verifies. Canonicalise through
    # the kernel's own function so the expectation cannot drift from it.
    want, want_ok = "", False
    if raw:
        try:
            want = kb._canonical_assignee(raw)
        except Exception:
            want, want_ok = "", False
        else:
            want_ok = bool(want and want.strip())
            want = want if want_ok else ""
    return {
        "board": board, "id": tid, "origin": origin,
        "envelope": envelope(con, tid),
        "kernel_kind": ("value" if isinstance(verdict, str) else
                        "none" if verdict is None else
                        "false" if verdict is False else repr(verdict)),
        "kernel_raw": raw,
        "want": want,
        "want_ok": want_ok,
    }


def live_cases(kb, cases):
    for db in sorted(KANBAN_HOME.glob(BOARDS_GLOB)):
        board = db.parent.name
        con = ro(db)
        cr = [r["task_id"] for r in con.execute(
            "SELECT DISTINCT task_id FROM task_runs WHERE outcome = 'changes_requested'")]
        nocr = [r["id"] for r in con.execute(
            "SELECT id FROM tasks WHERE id NOT IN "
            "(SELECT DISTINCT task_id FROM task_runs WHERE outcome = 'changes_requested') "
            "ORDER BY random() LIMIT ?", (NO_CR_SAMPLE,))]
        for tid in cr:
            cases.append(case(kb, board, tid, con, "live:changes_requested"))
        for tid in nocr:
            cases.append(case(kb, board, tid, con, "live:no-changes_requested"))
        con.close()


def mutation_cases(kb, cases, source, tid, workdir):
    """Malformed-provenance paths, evaluated by the kernel on a temp copy."""
    seed = ro(source)
    last = seed.execute(
        "SELECT id FROM task_runs WHERE task_id = ? AND outcome = 'changes_requested' "
        "ORDER BY id DESC LIMIT 1", (tid,)).fetchone()
    if last is None:
        seed.close()
        print(f"note: {tid} on {source.name} has no changes_requested run; "
              f"skipping the mutation cases", file=sys.stderr)
        return
    run_id = last["id"]
    event_id = seed.execute(
        "SELECT id FROM task_events WHERE task_id = ? AND kind = 'changes_requested' "
        "AND run_id = ? ORDER BY id DESC LIMIT 1", (tid, run_id)).fetchone()["id"]
    other_row = seed.execute(
        "SELECT id FROM task_runs WHERE task_id = ? AND id < ? ORDER BY id DESC LIMIT 1",
        (tid, run_id)).fetchone()
    seed.close()
    other_run = other_row["id"] if other_row else None

    new_run = ("INSERT INTO task_runs (task_id, profile, status, started_at, ended_at, "
               "outcome, summary) VALUES (?, 'default', 'done', ?, ?, 'changes_requested', ?)")
    new_event = ("INSERT INTO task_events (task_id, run_id, kind, payload, created_at) "
                 "VALUES (?, ?, 'changes_requested', ?, ?)")

    def mutate(label, stmts):
        dst = workdir / f"{label}.db"
        shutil.copy2(source, dst)
        con = rw(dst)
        for sql, params in stmts:
            con.execute(sql, params)
        con.commit()
        cases.append(case(kb, "mutation", tid, con, label))
        con.close()

    mutate("m1_reviewer_removed", [
        ("UPDATE task_events SET payload = '{\"reason\":\"fix it\"}' WHERE id = ?", (event_id,))])
    mutate("m2_reviewer_empty", [
        ("UPDATE task_events SET payload = '{\"reviewer\":\"   \"}' WHERE id = ?", (event_id,))])
    mutate("m3_event_deleted", [
        ("DELETE FROM task_events WHERE id = ?", (event_id,))])
    if other_run is None:
        # The selected run is the ticket's FIRST run: there is no earlier run
        # to mis-attribute the event to. Skipped, not crashed on.
        print("note: no earlier run on the seed ticket; skipping m4_run_id_mismatch",
              file=sys.stderr)
    else:
        mutate("m4_run_id_mismatch", [
            ("UPDATE task_events SET run_id = ? WHERE id = ?", (other_run, event_id))])

    # Reviewer padded with whitespace: the kernel returns it RAW and lands the
    # canonicalised form. A mirror that returned the raw string, or refused the
    # padded value against the roster, diverges here.
    mutate("m8_reviewer_padded", [
        ("UPDATE task_events SET payload = ? WHERE id = ?",
         ('{"reviewer":"  bob  ","reason":"fix it"}', event_id))])

    # Reviewer recorded in mixed case: the kernel returns it RAW and lands the
    # lowercased form (normalize_profile_name). A mirror that reported the raw
    # case would name a profile that does not exist on disk.
    mutate("m10_reviewer_uppercase", [
        ("UPDATE task_events SET payload = ? WHERE id = ?",
         ('{"reviewer":"BOB","reason":"fix it"}', event_id))])

    # A NEWER changes_requested event for the SAME run whose provenance is
    # blank shadows an older usable one: the kernel reads only the newest
    # (_latest_event) and returns False. A mirror that scanned for any usable
    # event would report the older reviewer and bless a row the kernel then
    # refuses.
    mutate("m9_newer_event_blank_shadows_older", [
        ("UPDATE task_events SET payload = ? WHERE id = ?",
         ('{"reviewer":"carol","reason":"older"}', event_id)),
        ("INSERT INTO task_events (task_id, run_id, kind, payload, created_at) "
         "VALUES (?, ?, 'changes_requested', ?, ?)",
         (tid, run_id, '{"reason":"no reviewer recorded"}', 1800000000))])

    def newer_round(label, event_payload, created, stamp):
        dst = workdir / f"{label}.db"
        shutil.copy2(source, dst)
        con = rw(dst)
        run = con.execute(new_run, (tid, stamp, stamp + 100, label)).lastrowid
        if event_payload is not None:
            con.execute(new_event, (tid, run, event_payload, created))
        con.commit()
        cases.append(case(kb, "mutation", tid, con, label))
        con.close()

    # A newer round whose provenance is missing/blank must win the selection
    # and yield the kernel's "no durable provenance" refusal — an older
    # reviewer that is still on the ticket must NOT be picked up.
    newer_round("m5_newer_round_no_event", None, 1800000100, 1800000000)
    newer_round("m6_newer_round_blank_reviewer", '{"reviewer":"","reason":"lost"}',
                1800000300, 1800000200)
    newer_round("m7_newer_round_valid_reviewer", '{"reviewer":"carol"}',
                1800000500, 1800000400)


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--out", required=True, help="corpus JSON to write")
    ap.add_argument("--mutation-source",
                    help="board DB to copy for the malformed-provenance cases "
                         "(default: the first database that has a changes_requested run)")
    ap.add_argument("--mutation-ticket",
                    help="ticket id inside --mutation-source (default: the newest one)")
    args = ap.parse_args()

    kb = load_kernel()
    cases = []
    live_cases(kb, cases)

    source = pathlib.Path(args.mutation_source) if args.mutation_source else None
    tid = args.mutation_ticket
    if source is None or tid is None:
        for db in sorted(KANBAN_HOME.glob(BOARDS_GLOB)):
            con = ro(db)
            row = con.execute(
                "SELECT task_id FROM task_runs WHERE outcome = 'changes_requested' "
                "ORDER BY id DESC LIMIT 1").fetchone()
            con.close()
            if row:
                source, tid = db, row["task_id"]
                break
    if source is not None and tid is not None:
        with tempfile.TemporaryDirectory(prefix="kernel-parity-") as td:
            mutation_cases(kb, cases, source, tid, pathlib.Path(td))

    out = pathlib.Path(args.out)
    out.write_text(json.dumps(cases, indent=1))
    kinds, origins = {}, {}
    for c in cases:
        kinds[c["kernel_kind"]] = kinds.get(c["kernel_kind"], 0) + 1
        origins[c["origin"]] = origins.get(c["origin"], 0) + 1
    print(f"{len(cases)} cases -> kernel verdicts {kinds}")
    print(f"origins: {origins}")
    live_value = sum(1 for c in cases
                     if c["origin"] == "live:changes_requested" and c["kernel_kind"] == "value")
    print(f"note: {live_value} live value-path case(s); the remaining value/False "
          f"evidence comes from kernel-evaluated mutations")
    print(f"corpus -> {out}")
    print(f"now run: KANBAN_MCP_PARITY_FIXTURE={out} "
          f"go test ./internal/mcptools -run TestKernelProvenanceParity -v")


if __name__ == "__main__":
    main()
