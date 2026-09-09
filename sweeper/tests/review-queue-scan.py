#!/usr/bin/env python3
"""Unit tests for the review_queue single-call scan (t_0cbde51b).

The whole-board scan previously probed ticket_list per board + ticket_get
per blocked ticket (~30 calls/tick), outrunning the bridge rate-limiter's
token-bucket burst (capacity 20) and silently starving later-scanned boards.
review_queue_items() replaces that probe with one call; these tests pin the
parsing and the loud-failure contract (malformed payload = McpError, never
a silent empty queue).

Run: python3 review-queue-scan.py   (no network, no board access)
"""
import json
import os
import sys

sys.path.insert(0, os.path.expanduser("~/.hermes/scripts"))  # host fallback (may not exist)
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))  # repo sweeper/ copy wins
import review_sweeper as rs  # noqa: E402

ok = True


def check(name, got, want):
    global ok
    status = "PASS" if got == want else "FAIL"
    if got != want:
        ok = False
    print("%s %s: got=%r want=%r" % (status, name, got, want))


def raises(name, exc_type, fn, *args):
    """Assert fn raises exc_type (or a subclass of it)."""
    global ok
    try:
        fn(*args)
    except exc_type:
        print("PASS %s: raised %s" % (name, exc_type.__name__))
        return
    except Exception as exc:  # noqa: BLE001
        ok = False
        print("FAIL %s: raised %r, want %s" % (name, exc, exc_type.__name__))
        return
    ok = False
    print("FAIL %s: no exception, want %s" % (name, exc_type.__name__))


class StubMcp:
    """Minimal fake: returns canned text for the named tool."""

    def __init__(self, text):
        self._text = text
        self.called = []

    def call(self, name, arguments):
        self.called.append((name, arguments))
        return self._text


def sample_queue():
    return {
        "total": 2,
        "returned": 2,
        "truncated": False,
        "tickets": [
            {"board": "togather", "id": "t_abc123", "title": "Some fix",
             "status": "blocked", "assignee": "", "priority": 1,
             "block_reason": "review-required: fix shipped | repo: x; branch: fix/t_abc123; sha: deadbeef"},
            {"board": "default", "id": "t_def456", "title": "Another",
             "status": "blocked", "assignee": "default", "priority": 2,
             "block_reason": "review-required: docs"},
        ],
    }


# --- happy path: parse + board/id extraction ---
mcp = StubMcp(json.dumps(sample_queue()))
out = rs.review_queue_items(mcp)
check("call is single review_queue with no args",
      mcp.called, [("review_queue", {})])
check("total preserved", out["total"], 2)
check("truncated surfaced", out["truncated"], False)
check("tickets list parsed", len(out["tickets"]), 2)
check("ticket carries board+id", (out["tickets"][0]["board"], out["tickets"][0]["id"]),
      ("togather", "t_abc123"))
check("block_reason carried", out["tickets"][1]["block_reason"].startswith("review-required:"), True)

# --- malformed payloads must raise (never a silent empty queue) ---
raises("malformed JSON raises McpError",
       rs.McpError, rs.review_queue_items, StubMcp("{not json"))
raises("non-dict payload raises McpError",
       rs.McpError, rs.review_queue_items, StubMcp("[1, 2, 3]"))
raises("dict without tickets list raises McpError",
       rs.McpError, rs.review_queue_items, StubMcp(json.dumps({"total": 0})))

# --- empty queue arrives as "tickets": null (Go nil slice) = normal all-clear ---
empty = rs.review_queue_items(StubMcp(json.dumps({"total": 0, "returned": 0,
                                                   "tickets": None})))
check("null tickets parses to empty list", empty["tickets"], [])
check("null tickets keeps totals", (empty["total"], empty["returned"]), (0, 0))

# --- truncation must be visible to the caller (main() warns on it) ---
tq = sample_queue()
tq["truncated"] = True
tq["returned"] = 45
tq["total"] = 61
out = rs.review_queue_items(StubMcp(json.dumps(tq)))
check("truncated flag visible", out["truncated"], True)
check("partial counts visible", (out["returned"], out["total"]), (45, 61))

# --- extraction still works off a queue item's block_reason (regression guard:
#     the queue reason carries the structured refs the sweep extracts later) ---
reason = out["tickets"][0]["block_reason"] if out["tickets"] else ""
check("branch extractable from block_reason",
      rs.extract_branch("repo: x; branch: fix/t_abc123; sha: deadbeef"),
      "fix/t_abc123")
check("sha extractable from block_reason",
      rs.extract_sha("repo: x; branch: fix/t_abc123; sha: deadbeef"),
      "deadbeef")

print("ALL GOOD" if ok else "PROBLEM")
sys.exit(0 if ok else 1)
