#!/usr/bin/env python3
"""Unit tests for the v3 review-sweeper changes (t_21b38b58):
conf parsing (board -> repo + default branch), SHA extraction, stall counter.

Run: python3 unit4.py   (no network, no board access)
"""
import os
import sys
import tempfile

sys.path.insert(0, os.path.expanduser("~/.hermes/scripts"))
from review_sweeper import (  # noqa: E402
    extract_branch, extract_sha, load_conf, repo_for_board, stall_update,
)

ok = True


def check(name, got, want):
    global ok
    status = "PASS" if got == want else "FAIL"
    if got != want:
        ok = False
    print("%s %s: got=%r want=%r" % (status, name, got, want))


# --- load_conf --------------------------------------------------------------
with tempfile.NamedTemporaryFile("w", suffix=".conf", delete=False) as fh:
    fh.write("# comment line\n")
    fh.write("\n")
    fh.write("hermes-agent https://github.com/RKelln/hermes-kanban-mcp.git main\n")
    fh.write("togather https://github.com/Togather-Foundation/server.git\n")  # 2-col legacy
    fh.write("broken-line-with-one-token\n")
    conf_path = fh.name

conf = load_conf(conf_path)
check("conf 3-col", conf.get("hermes-agent"), ("https://github.com/RKelln/hermes-kanban-mcp.git", "main"))
check("conf 2-col defaults main", conf.get("togather"), ("https://github.com/Togather-Foundation/server.git", "main"))
check("conf skips 1-token line", "broken-line-with-one-token" in conf, False)
check("conf skips comments", len(conf), 2)

os.unlink(conf_path)
check("load_conf missing file", load_conf("/nonexistent/x.conf"), {})

# --- repo_for_board ----------------------------------------------------------
check("repo_for_board hit", repo_for_board("togather", conf), ("https://github.com/Togather-Foundation/server.git", "main"))
check("repo_for_board miss", repo_for_board("nope", conf), (None, None))

# --- extract_sha -------------------------------------------------------------
check("sha after 'commit'", extract_sha("branch x, commit d4de01917f264a1310b9e4dc837ef210698fcdf6 done"),
      "d4de01917f264a1310b9e4dc837ef210698fcdf6")
check("sha short", extract_sha("sha: 2f3bde81bfac"), "2f3bde81bfac")
check("sha after 'revision='", extract_sha("revision=abc1234"), "abc1234")
check("sha no keyword", extract_sha("the hash d4de01917f264a1310b9e4dc837ef210698fcdf6 here"), "")
check("sha word-boundary", extract_sha("commit d4de01917f264a1310b9e4dc837ef210698fcdf6x"), "")

# --- extract_branch (regression: v1 patterns still work) ----------------------
check("branch after 'branch:'", extract_branch("branch: feat/opencode-mcp-config-example"), "feat/opencode-mcp-config-example")
check("branch Created branch", extract_branch("Created branch feat/t_e375b1dc-review-queue"), "feat/t_e375b1dc-review-queue")

# --- stall_update ------------------------------------------------------------
check("stall clean scan resets", stall_update(2, False, False), 0)
check("stall ticket seen resets", stall_update(2, True, True), 0)
check("stall empty+errors increments", stall_update(2, False, True), 3)
check("stall starts at 0", stall_update(0, False, True), 1)

print("ALL GOOD" if ok else "PROBLEM")
sys.exit(0 if ok else 1)
