#!/usr/bin/env python3
"""Create a REQUEST_CHANGES test ticket: the body names an expected content
that the actual artifact file does NOT match, so the reviewer must return
REQUEST_CHANGES and the sweeper must unblock the ticket to ready."""
import http.cookiejar
import json
import os
import sys
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.expanduser("~/.hermes/scripts"))
from review_sweeper import load_env, REST_LOGIN_URL  # noqa: E402

BOARD = "review-sweeper-e2e"
MARKER = "/tmp/review-sweeper-e2e-artifact-v3.txt"
EXPECTED = "v3-expected-content-20260807"
ACTUAL = "v3-WRONG-content-20260807"
TITLE = "E2E v3: REQUEST_CHANGES path (content mismatch)"
BODY = (
    "Acceptance criteria: verify that %s exists on this host and its content "
    "is exactly '%s'. NON-CODE verification ticket. If the content matches, "
    "verdict APPROVE; if it does not match, verdict REQUEST_CHANGES."
) % (MARKER, EXPECTED)


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
        assert resp.status == 200

    with open(MARKER, "w", encoding="utf-8") as fh:
        fh.write(ACTUAL)
    print("marker written with WRONG content:", ACTUAL)

    req = urllib.request.Request(
        "http://127.0.0.1:9119/api/plugins/kanban/tasks?board=" + urllib.parse.quote(BOARD),
        data=json.dumps({"title": TITLE, "body": BODY}).encode(),
        headers={"Content-Type": "application/json"})
    with opener.open(req, timeout=30) as resp:
        created = json.loads(resp.read().decode())
    tid = created.get("task", {}).get("id") or created.get("id")
    print("CREATED:", tid)
    req = urllib.request.Request(
        "http://127.0.0.1:9119/api/plugins/kanban/tasks/" + urllib.parse.quote(str(tid)) +
        "?board=" + urllib.parse.quote(BOARD),
        data=json.dumps({"status": "blocked",
                         "block_reason": "review-required: E2E v3 — expect REQUEST_CHANGES"}).encode(),
        headers={"Content-Type": "application/json"}, method="PATCH")
    with opener.open(req, timeout=30) as resp:
        print("PATCH blocked:", resp.status)
    print("E2E_TICKET=" + str(tid))


if __name__ == "__main__":
    main()
