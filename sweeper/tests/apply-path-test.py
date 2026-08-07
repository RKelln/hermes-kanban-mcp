#!/usr/bin/env python3
"""Apply-path unit test (reviewer-mandated): canned subagent response ->
clean verdict comment + correct PATCH transition. Also regression-tests the
v1 dedup trap: a malformed comment must NOT count as a sweeper verdict.

Run: python3 apply-path-test.py
"""
import os
import sys
import tempfile

sys.path.insert(0, os.path.expanduser("~/.hermes/scripts"))
import review_sweeper as rs  # noqa: E402

failures = []


def check(label, got, want):
    ok = got == want
    print(("%s  %s: got=%r want=%r" % ("PASS" if ok else "FAIL", label, got, want)))
    if not ok:
        failures.append(label)


class FakeMcp:
    """Records tool calls; ticket_comment returns an empty string."""

    def __init__(self):
        self.calls = []

    def call(self, name, arguments):
        self.calls.append((name, dict(arguments)))
        return ""


class FakeRest:
    """Records PATCH bodies."""

    def __init__(self):
        self.patches = []

    def patch_task(self, board, tid, body):
        self.patches.append((board, tid, dict(body)))
        return {}


def make_detail(comments=()):
    return {
        "id": "t_x", "title": "Test", "body": "AC: verify marker",
        "block_reason": "review-required: test",
        "comments": [{"author": "default", "body": c} for c in comments],
    }


# ---- 1. APPROVE: canned contract output -> clean comment + PATCH done ----
mcp, rest = FakeMcp(), FakeRest()
canned_approve = (
    "- src/app.py:214 — nil deref — expected: guard before use\n"
    "- tests: 3 new cases added and passing\n"
    "VERDICT: APPROVE\n"
)
status = rs.apply_verdict(mcp, rest, "board-x", "t_x", make_detail(),
                          "APPROVE", canned_approve)
check("approve-status", status, "done")
check("approve-comment-count", len(mcp.calls), 1)
comment = mcp.calls[0][1]["body"] if mcp.calls else ""
check("approve-comment-starts-clean", comment.splitlines()[0], "review-sweeper: APPROVED")
check("approve-comment-has-findings", "nil deref" in comment, True)
check("approve-comment-no-verdict-line", comment.count("VERDICT:"), 0)
check("approve-patch-status", rest.patches[0][2]["status"] if rest.patches else None, "done")
check("approve-patch-has-summary", rest.patches[0][2].get("summary") == "review-sweeper: APPROVED", True)

# ---- 2. REQUEST_CHANGES -> PATCH ready ----
mcp, rest = FakeMcp(), FakeRest()
canned_rc = (
    "- src/main.go:88 — error ignored — expected: handle err\n"
    "VERDICT: REQUEST_CHANGES\n"
)
status = rs.apply_verdict(mcp, rest, "board-x", "t_x", make_detail(),
                          "REQUEST_CHANGES", canned_rc)
check("rc-status", status, "ready")
check("rc-comment-starts-clean", mcp.calls[0][1]["body"].splitlines()[0],
      "review-sweeper: REQUEST CHANGES")
check("rc-patch-status", rest.patches[0][2]["status"], "ready")

# ---- 3. ESCALATE -> stays blocked, comments once ----
mcp, rest = FakeMcp(), FakeRest()
canned_esc = (
    "- branch not found on origin\n"
    "VERDICT: ESCALATE\n"
)
status = rs.apply_verdict(mcp, rest, "board-x", "t_x", make_detail(),
                          "ESCALATE", canned_esc)
check("esc-status", status, "blocked")
check("esc-comment-once", len(mcp.calls), 1)
check("esc-comment-starts-clean", mcp.calls[0][1]["body"].splitlines()[0],
      "review-sweeper: ESCALATED")
check("esc-no-patch", len(rest.patches), 0)

# ---- 4. Dedup trap regression: malformed v1 comment is NOT a verdict ----
detail = make_detail(comments=[
    "review-sweeper: ESCALATED (Verdict: APPROVE.VERDICT line.Verdict: "
    "APPROVE.verdict + complete).\"verdict satisfies AC3.verdict + complete)\""
])
check("malformed-comment-not-verdict", rs.comments_have_sweeper_verdict(detail), False)

# ---- 5. Clean sweeper verdict comment IS recognized ----
detail = make_detail(comments=["review-sweeper: APPROVED\n- bullet one"])
check("clean-comment-is-verdict", rs.comments_have_sweeper_verdict(detail), True)
detail = make_detail(comments=["review-sweeper: REQUEST CHANGES\n- fix it"])
check("clean-rc-comment-is-verdict", rs.comments_have_sweeper_verdict(detail), True)
detail = make_detail(comments=["review-sweeper: ESCALATED\nhuman review"])
check("clean-esc-comment-is-verdict", rs.comments_have_sweeper_verdict(detail), True)

# ---- 6. Unparseable output -> clean ESCALATE fallback, no raw leak ----
mcp, rest = FakeMcp(), FakeRest()
garbage = (
    "(Verdict: APPROVE.VERDICT line.Verdict: APPROVE.verdict + complete)"
    ". more garbage that must not land in the comment"
)
status = rs.apply_verdict(mcp, rest, "board-x", "t_x", make_detail(),
                          "ESCALATE", garbage)
check("garbage-esc-status", status, "blocked")
body = mcp.calls[0][1]["body"] if mcp.calls else ""
check("garbage-comment-clean", "garbage that must not land" not in body, True)
check("garbage-comment-fallback", "human review required" in body, True)

# ---- 7. Findings extraction is bullets-only (no reasoning box) ----
with open(os.path.join(os.path.dirname(__file__), "..", "reviews",
                       "review-sweeper-e2e__t_4890adb0_1786113018.log")) as fh:
    saved = fh.read()
f = rs.extract_findings(saved)
check("saved-log-no-box", "┌─" not in f, True)
check("saved-log-bullets-only", all(l.strip().startswith("- ") for l in f.splitlines()), True)
check("saved-log-empty-fallback", f != "", True)

print("---")
print("FAILURES:", failures if failures else "none")
sys.exit(1 if failures else 0)
