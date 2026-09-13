#!/usr/bin/env python3
"""Derive the read-path size caps from real tickets instead of guessing.

The caps in internal/mcptools/shape.go (MaxCommentBodyChars,
MaxTicketBodyChars, MaxRunSummaryChars) are policy thresholds, so they
should follow the corpus rather than stay at whatever round number was
picked first. This script measures the actual distributions on this
host's kanban databases and prints:

  - percentile tables for comment bodies and ticket bodies
  - what fraction of comments arrive WHOLE at a given cap
  - the window arithmetic: how many complete comments fit the partial
    budget once the ticket's own summary fields are paid for

It reads the SQLite files directly (read-only) because the MCP surface
and the REST API both clip before the data reaches a caller — measuring
the caps through them would just measure the caps.

Usage:
  python3 scripts/comment-length-stats.py [--cap N] [--budget N]

Run this before changing any cap, and update the snapshot below when you
do. The numbers DRIFT as the corpus grows: between the first and second
run on 2026-09-13 the comment p50 moved 552 -> 553 and the body p50
1818 -> 1782, purely from tickets created in between. Treat the snapshot
as a dated measurement, never as a live constant.

SNAPSHOT 2026-09-13 (631 comments / 251 tickets / 470 bodies, all boards)
  comments  p50 553  p75 1236  p90 2535  p95 3704  p99 5263  max 15307
  bodies    p50 1782 p90 3548  p95 3717  p99 5480  max 7385 (20 over 4000)
  coverage  cap  500 -> 47.4% whole   (the median comment was clipped)
            cap 1500 -> 79.4% whole   <- chosen (smallest round cap past p75)
            cap 2500 -> 89.5% whole
  window    the ticket's own body (p50 1782) and run summaries (per-ticket
            p50 508 / p90 1024 / max 2733, each capped at 1024) leave room for
            TWO TO THREE complete comments at cap 1500, four only when those
            fields are small; beyond that the fitter drops OLDEST comments
            (newest survive, flagged)
"""
import argparse
import glob
import os
import sqlite3
import statistics as st

BUDGET_DEFAULT = 8 * 1024
RUN_SUMMARY_CAP = 1024   # MaxRunSummaryChars in shape.go


def db_paths():
    """Every kanban database on this host: the default board plus per-project."""
    pats = [os.path.expanduser("~/.hermes/kanban.db"),
            os.path.expanduser("~/.hermes/kanban/boards/*/kanban.db")]
    out = []
    for pat in pats:
        out.extend(sorted(glob.glob(pat)))
    return out


def board_of(path):
    return path.split("/")[-2] if "/boards/" in path else "default"


def collect():
    """Read-only. Returns (comments, bodies, per_ticket_counts, overhead)."""
    comments, bodies, per_ticket, overhead = [], [], {}, {}
    for db in db_paths():
        board = board_of(db)
        con = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
        for tid, body in con.execute(
                "select task_id, coalesce(body,'') from task_comments"):
            comments.append((board, tid, len(body)))
            per_ticket[(board, tid)] = per_ticket.get((board, tid), 0) + 1
        for tid, body in con.execute(
                "select id, coalesce(body,'') from tasks"):
            bodies.append((board, tid, len(body)))
        # What the ticket pays for before any comment appears: its run
        # summaries, each capped exactly as the read path caps them.
        for tid, summary in con.execute(
                "select task_id, coalesce(summary,'') from task_runs"):
            key = (board, tid)
            overhead[key] = overhead.get(key, 0) + min(len(summary), RUN_SUMMARY_CAP)
        con.close()
    return comments, bodies, per_ticket, overhead


def pct(vals, p):
    """Percentile with linear interpolation (no numpy)."""
    if not vals:
        return 0
    s = sorted(vals)
    k = (len(s) - 1) * p / 100
    lo, hi = int(k), min(int(k) + 1, len(s) - 1)
    return s[lo] + (s[hi] - s[lo]) * (k - lo)


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--cap", type=int, default=1500,
                    help="candidate comment cap to evaluate (default: the current one)")
    ap.add_argument("--budget", type=int, default=BUDGET_DEFAULT,
                    help="partial-mode byte budget (default 8192)")
    args = ap.parse_args()

    comments, bodies, per_ticket, overhead = collect()
    cm = [c[2] for c in comments]
    bl = [b[2] for b in bodies]
    # Fail loudly rather than printing a table of zeros: a silent zero
    # table looks like a real measurement of an empty corpus.
    if not cm:
        raise SystemExit("no comment bodies found — wrong host, or the boards moved?")
    if not bl:
        raise SystemExit("no ticket bodies found — wrong host, or the boards moved?")
    if not overhead:
        raise SystemExit("no run summaries found — wrong host, or the schema changed?")

    print(f"comment bodies : {len(cm)} across {len(per_ticket)} tickets")
    print(f"ticket bodies  : {len(bl)} across {len(bodies)} tickets")
    print(f"summary fields : {len(overhead)} tickets carry runs; "
          f"per-ticket bytes p50 {pct(list(overhead.values()), 50):.0f} / "
          f"p90 {pct(list(overhead.values()), 90):.0f} / "
          f"max {max(overhead.values())}\n")

    print("COMMENT LENGTH DISTRIBUTION (runes)")
    for p in (50, 75, 90, 95, 97, 99, 99.5):
        print(f"  p{p:<5} {pct(cm, p):8.0f}")
    print(f"  max    {max(cm):8.0f}    mean {st.mean(cm):.0f}")

    print("\nTICKET BODY DISTRIBUTION (runes)")
    for p in (50, 75, 90, 95, 99):
        print(f"  p{p:<5} {pct(bl, p):8.0f}")
    print(f"  max    {max(bl):8.0f}    over 4000: {sum(1 for n in bl if n > 4000)}")

    print("\nCOVERAGE: fraction of comments arriving WHOLE at each candidate cap")
    for cap in (300, 400, 500, 600, 750, 1000, 1200, 1500, 2000, 2500, 3000):
        whole = sum(1 for n in cm if n <= cap)
        mark = "  <-- current" if cap == args.cap else ""
        print(f"  cap {cap:>5}: {100*whole/len(cm):5.1f}% whole ({whole:>4} of {len(cm)}){mark}")

    print("\nWINDOW ARITHMETIC: complete comments in one partial response")
    print(f"  budget {args.budget} B, minus the ticket's own fields, at cap {args.cap}:")
    ov = list(overhead.values())
    for label, o in (("light summaries (p50)", pct(ov, 50)),
                     ("typical (p90)", pct(ov, 90)),
                     ("heavy (max)", max(ov))):
        avail = args.budget - o - pct(bl, 50)   # summaries + a p50 body
        n = max(0, int(avail // args.cap))
        print(f"    {label:<24} {o:>6.0f} B overhead -> {n} complete comment(s)")
    print("    (estimate from measured inputs; the bridge's own fitter is authoritative)")
    print("\n  Note: exceeding the budget does NOT clip every comment — the fitter")
    print("  drops the OLDEST comments and keeps the newest whole. That is the")
    print("  intended trade for review threads, where the newest comment is the payload.")


if __name__ == "__main__":
    main()
