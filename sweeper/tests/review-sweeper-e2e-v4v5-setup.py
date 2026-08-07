#!/usr/bin/env python3
"""E2E v4+v5 setup (t_21b38b58): create two review-required test tickets on
the review-sweeper-e2e board.

v4 — repo-map smoke: the body intentionally omits the repo URL so resolution
     MUST come from review-sweeper.conf (board -> RKelln/hermes-kanban-mcp),
     then the reviewer fetches commit d4de0191 (origin/main tip) BY SHA from
     the shared per-board checkout and confirms the review_queue files. This
     is the regression for the t_ccc76018 ESCALATE (repo-less spawn with no
     terminal could not verify anything).

v5 — artifact regression: a no-repo marker-file ticket, proving the
     unconditional terminal+file spawn did not break docs/artifact reviews.

Idempotent: skips tickets whose title already exists on the board.
Reuses the sweeper's own env/auth code (same credential path as production).
"""
import http.cookiejar
import json
import os
import sys
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.expanduser("~/.hermes/scripts"))
from review_sweeper import load_env, REST_LOGIN_URL  # noqa: E402

BOARD = "review-sweeper-e2e"
MARKER_V5 = "/tmp/review-sweeper-e2e-artifact-v5.txt"
SHA = "d4de01917f264a1310b9e4dc837ef210698fcdf6"

TICKETS = [
    {
        "title": "E2E v4: repo-map smoke — resolve repo via conf, fetch commit by SHA",
        "body": (
            "Acceptance criteria:\n"
            "1. Resolve this board's repo from the per-board repo map "
            "(review-sweeper.conf). The ticket body intentionally omits the "
            "URL so resolution MUST come from the board mapping.\n"
            "2. In the shared CHECKOUT, fetch commit %s by SHA and confirm it "
            "resolves on origin (it is the current origin/main tip).\n"
            "3. Confirm `git show --stat <sha>` lists the review-queue "
            "deliverable files internal/mcptools/review_queue.go and "
            "internal/mcptools/review_queue_test.go, and read one of them to "
            "confirm it is real Go code.\n"
            "4. No secrets in the commit's patch.\n"
            "This is a SWEEPER MECHANISM smoke test (t_21b38b58): the "
            "deliverable under review is the repo-resolution/fetch-by-SHA "
            "machinery itself. VERDICT APPROVE if you resolved the repo via "
            "the board map and verified the commit + files via fetch from the "
            "shared checkout; ESCALATE only if the repo or commit could not "
            "be resolved." % SHA
        ),
        "block_reason": "review-required: E2E v4 — repo-map smoke (t_21b38b58)",
    },
    {
        "title": "E2E v5: artifact regression — no-repo ticket still reviews clean",
        "body": (
            "Acceptance criteria: verify that %s exists on this host and its "
            "content is exactly 'sweeper-e2e-ok-v5'. NON-CODE verification "
            "ticket (no repo, no branch). Verdict APPROVE if the artifact "
            "matches; ESCALATE otherwise." % MARKER_V5
        ),
        "block_reason": "review-required: E2E v5 — artifact regression (t_21b38b58)",
    },
]


def main():
    env = load_env()
    jar = http.cookiejar.CookieJar()
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
    req = urllib.request.Request(
        REST_LOGIN_URL,
        data=json.dumps({"provider": "basic", "username": env.get("KANBAN_USERNAME"),
                         "password": env.get("KANBAN_PASSWORD")}).encode(),
        headers={"Content-Type": "application/json"})
    with opener.open(req, timeout=30) as resp:
        assert resp.status == 200, "login failed"

    # board (idempotent)
    req = urllib.request.Request(
        "http://127.0.0.1:9119/api/plugins/kanban/boards",
        data=json.dumps({"slug": BOARD, "name": "Review Sweeper E2E", "switch": False}).encode(),
        headers={"Content-Type": "application/json"})
    with opener.open(req, timeout=30) as resp:
        print("boards:", resp.status)

    # v5 marker artifact
    with open(MARKER_V5, "w", encoding="utf-8") as fh:
        fh.write("sweeper-e2e-ok-v5")
    print("marker:", MARKER_V5)

    # existing tickets
    req = urllib.request.Request(
        "http://127.0.0.1:9119/api/plugins/kanban/board?board=" + urllib.parse.quote(BOARD))
    with opener.open(req, timeout=30) as resp:
        data = json.loads(resp.read().decode())
    existing = {t.get("title"): t for col in data.get("columns", []) for t in col.get("tasks", [])}

    for spec in TICKETS:
        if spec["title"] in existing:
            t = existing[spec["title"]]
            print("EXISTS:", t.get("id"), t.get("status"), t.get("block_reason"))
            print("E2E_TICKET=" + t.get("id", ""))
            continue
        req = urllib.request.Request(
            "http://127.0.0.1:9119/api/plugins/kanban/tasks?board=" + urllib.parse.quote(BOARD),
            data=json.dumps({"title": spec["title"], "body": spec["body"]}).encode(),
            headers={"Content-Type": "application/json"})
        with opener.open(req, timeout=30) as resp:
            created = json.loads(resp.read().decode())
        tid = created.get("task", {}).get("id") or created.get("id")
        print("CREATED:", tid)
        req = urllib.request.Request(
            "http://127.0.0.1:9119/api/plugins/kanban/tasks/" + urllib.parse.quote(str(tid)) +
            "?board=" + urllib.parse.quote(BOARD),
            data=json.dumps({"status": "blocked", "block_reason": spec["block_reason"]}).encode(),
            headers={"Content-Type": "application/json"}, method="PATCH")
        with opener.open(req, timeout=30) as resp:
            print("PATCH blocked:", resp.status)
        print("E2E_TICKET=" + str(tid))


if __name__ == "__main__":
    main()
