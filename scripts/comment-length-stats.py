#!/usr/bin/env python3
"""Derive the read-path size caps from real tickets instead of guessing.

The caps in internal/mcptools/shape.go (MaxCommentBodyChars,
MaxTicketBodyChars, the sweeper's digest caps) are policy thresholds, so
they should follow the corpus rather than stay at whatever round number
was picked first. This script measures the actual distributions on this
host's kanban databases and prints:

  - percentile/coverage tables for comment bodies and ticket bodies
  - what fraction of comments arrive WHOLE at a given cap
  - the envelope arithmetic: how many complete comments fit the partial
    budget at a given cap

It reads the SQLite files directly (read-only) because the MCP surface
and REST API both clip before the data reaches a caller — measuring the
caps through them would just measure the caps.

Usage:
  python3 scripts/comment-length-stats.py [--cap N] [--budget N]

Snapshots taken with this script (re-derive when the corpus grows):
  2026-09-13, 630 comments / 250 tickets / 467 bodies, all boards
    comments  p50 552  p75 1238  p90 2536  p95 3704  p99 5264  max 15307
    bodies    p50 1818 p90 3548  p95 3720  p99 5487  max 7385
    coverage  cap  500 -> 47.5% whole   (the median comment was clipped)
              cap 1500 -> 79.4% whole   <- chosen
              cap 2500 -> 89.5% whole
    envelopes 10 x cap + ~2 KB fixed: 500 fits 8 KB; 1500+ exceeds it, so
              long threads rely on drop-oldest (newest survive) rather
              than on clipping every comment
"""
import argparse
import glob
import os
import sqlite3
import statistics as st


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
    comments, bodies, per_ticket = [], [], {}
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
        con.close()
    return comments, bodies, per_ticket


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
    ap.add_argument("--budget", type=int, default=8 * 1024,
                    help="partial-mode byte budget (default 8192)")
    args = ap.parse_args()

    comments, bodies, per_ticket = collect()
    cm = [c[2] for c in comments]
    bl = [b[2] for b in bodies]
    if not cm:
        raise SystemExit("no comments found — wrong host?")

    print(f"comment bodies : {len(cm)} across {len(per_ticket)} tickets")
    print(f"ticket bodies  : {len(bl)} across {len(bodies)} tickets\n")

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

    print("\nENVELOPE ARITHMETIC: complete comments that fit the partial budget")
    fixed = 2000  # summaries + identity fields, roughly
    for cap in (500, 1000, 1500, 2000):
        n = max(0, (args.budget - fixed) // cap)
        print(f"  cap {cap:>5}: ~{n} complete comment(s) per response "
              f"({cap*n + fixed} of {args.budget} bytes)")
    print("\n  Note: exceeding the budget does NOT clip every comment — the fitter")
    print("  drops the OLDEST comments and keeps the newest whole. That is the")
    print("  intended trade for review threads, where the newest comment is the payload.")


if __name__ == "__main__":
    main()
