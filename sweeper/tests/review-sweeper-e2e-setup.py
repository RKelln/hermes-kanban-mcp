#!/usr/bin/env python3
"""E2E setup for the review-sweeper: create a scratch board + one
review-required test ticket (docs/artifact verification, no branch) that the
sweeper should review and complete end-to-end.

Reuses the sweeper's own env/auth code so the harness exercises the same
credential path. Idempotent: recreating the board returns the existing one;
a leftover ticket from a crashed run is detected and skipped.
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
MARKER = "/tmp/review-sweeper-e2e-artifact-v2.txt"
MARKER_CONTENT = "sweeper-e2e-ok-20260807-v2"
TITLE = "E2E v2: verify marker artifact (review-sweeper test)"
BODY = (
    "Acceptance criteria: verify that %s exists on this host and its content "
    "is exactly '%s'. This is a NON-CODE verification ticket for the "
    "review-sweeper end-to-end test; no repo, no branch, no merge. Verdict "
    "APPROVE if the artifact matches, ESCALATE otherwise."
) % (MARKER, MARKER_CONTENT)


def main():
    env = load_env()
    base = env.get("KANBAN_BASE_URL", "http://127.0.0.1:9119/api/plugins/kanban/")
    jar = http.cookiejar.CookieJar()
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))

    # login
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

    # marker artifact
    with open(MARKER, "w", encoding="utf-8") as fh:
        fh.write(MARKER_CONTENT)
    print("marker:", MARKER, "content:", MARKER_CONTENT)

    # list existing tickets on the board (reuse MCP? no — REST)
    req = urllib.request.Request(
        "http://127.0.0.1:9119/api/plugins/kanban/board?board=" + urllib.parse.quote(BOARD))
    with opener.open(req, timeout=30) as resp:
        data = json.loads(resp.read().decode())
    existing = [t for col in data.get("columns", []) for t in col.get("tasks", [])]
    for t in existing:
        if t.get("title") == TITLE:
            print("EXISTS:", t.get("id"), t.get("status"), t.get("block_reason"))
            print("E2E_TICKET=" + t.get("id", ""))
            return

    # create the ticket
    req = urllib.request.Request(
        "http://127.0.0.1:9119/api/plugins/kanban/tasks?board=" + urllib.parse.quote(BOARD),
        data=json.dumps({"title": TITLE, "body": BODY}).encode(),
        headers={"Content-Type": "application/json"})
    try:
        with opener.open(req, timeout=30) as resp:
            created = json.loads(resp.read().decode())
    except urllib.error.HTTPError as exc:
        print("CREATE FAILED:", exc.code, exc.read().decode()[:500])
        raise
    tid = created.get("task", {}).get("id") or created.get("id")
    print("CREATED:", tid, created.get("task", {}).get("status"))

    # block it with review-required
    req = urllib.request.Request(
        "http://127.0.0.1:9119/api/plugins/kanban/tasks/" + urllib.parse.quote(str(tid)) +
        "?board=" + urllib.parse.quote(BOARD),
        data=json.dumps({"status": "blocked",
                         "block_reason": "review-required: E2E test — verify marker artifact"}).encode(),
        headers={"Content-Type": "application/json"}, method="PATCH")
    with opener.open(req, timeout=30) as resp:
        print("PATCH blocked:", resp.status)
    print("E2E_TICKET=" + str(tid))


if __name__ == "__main__":
    main()
