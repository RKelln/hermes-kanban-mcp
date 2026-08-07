#!/usr/bin/env python3
"""Unit checks: ledger dedup + lock semantics (run with system python3)."""
import json
import os
import sys
import tempfile
import time

sys.path.insert(0, os.path.expanduser("~/.hermes/scripts"))  # host fallback (may not exist)
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))  # repo sweeper/ copy wins
import review_sweeper as rs  # noqa: E402

failures = []


def check(label, got, want):
    ok = got == want
    print(("%s  %s: got=%r want=%r" % ("PASS" if ok else "FAIL", label, got, want)))
    if not ok:
        failures.append(label)


# --- ledger ---
with tempfile.TemporaryDirectory() as tmp:
    rs.STATE_DIR = tmp
    # no ledger yet
    check("ledger-missing", rs.ledger_has("review-sweeper-e2e", "t_d708e09b"), False)
    rs.ledger_append("review-sweeper-e2e", "t_d708e09b", "ESCALATE", "blocked")
    check("ledger-found", rs.ledger_has("review-sweeper-e2e", "t_d708e09b"), True)
    check("ledger-other-board", rs.ledger_has("other", "t_d708e09b"), False)
    check("ledger-other-tid", rs.ledger_has("review-sweeper-e2e", "t_zzz"), False)
    # multiple entries, same ticket twice (should still match)
    rs.ledger_append("review-sweeper-e2e", "t_d708e09b", "ESCALATE", "blocked")
    check("ledger-dupes-still-found", rs.ledger_has("review-sweeper-e2e", "t_d708e09b"), True)

# --- locks ---
with tempfile.TemporaryDirectory() as tmp:
    rs.STATE_DIR = tmp
    import time as _t
    ok1 = rs.lock_acquire("b", "t1", 99999999)  # dead pid: should acquire
    check("lock-first-acquire", ok1, True)
    ok2 = rs.lock_acquire("b", "t1", 99999999)  # fresh lock, dead pid -> stale -> takeover
    check("lock-dead-pid-takeover", ok2, True)
    ok3 = rs.lock_acquire("b", "t2", os.getpid())  # live pid (this process)
    check("lock-live-pid-acquire", ok3, True)
    ok4 = rs.lock_acquire("b", "t2", os.getpid())  # live pid -> must refuse (in progress)
    check("lock-live-pid-refused", ok4, False)
    rs.lock_release("b", "t2")
    ok5 = rs.lock_acquire("b", "t2", os.getpid())
    check("lock-released-reacquire", ok5, True)

# --- stale TTL takeover ---
with tempfile.TemporaryDirectory() as tmp:
    rs.STATE_DIR = tmp
    rs.lock_acquire("b", "t3", os.getpid())
    path = os.path.join(tmp, "locks", "b__t3.lock")
    meta = json.load(open(path))
    meta["started_at"] = int(time.time()) - rs.LOCK_TTL_SECONDS - 60  # very old
    json.dump(meta, open(path, "w"))
    check("lock-ttl-takeover", rs.lock_acquire("b", "t3", os.getpid()), True)

print("---")
print("FAILURES:", failures if failures else "none")
sys.exit(1 if failures else 0)
