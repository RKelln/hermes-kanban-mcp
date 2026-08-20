#!/usr/bin/env python3
"""Unit checks for reviewer-skill configuration (run with system python3).

Covers reviewer_skill_name (env, not code), the pre-spawn resolvability
probe skill_status/iter_skill_roots (missing vs collision vs ok vs none,
mirroring hermes' own skill scanner), the spawn command built by
spawn_reviewer (with/without '-s'), and the visible SKIPPED comment +
dedup. Regression for t_44d19d72 (2026-08-14..19 outage: hardcoded
'sdlc-review' collided across two skill trees; every reviewer crashed at
agent init and tickets stranded silently in blocked).
"""
import os
import sys
import tempfile

sys.path.insert(0, os.path.expanduser("~/.hermes/scripts"))  # host fallback (may not exist)
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))  # repo sweeper/ copy wins
import review_sweeper as rs  # noqa: E402

failures = []


def check(label, got, want):
    ok = got == want
    print(("%s  %s: got=%r want=%r" % ("PASS" if ok else "FAIL", label, got, want)))
    if not ok:
        failures.append(label)


def make_tree(base):
    """Build a synthetic skills tree; returns base path."""
    for rel in [
        "devops/dup/SKILL.md",
        "software-development/dup/SKILL.md",
        "software-development/sdlc-review/SKILL.md",
        "tools/foo/SKILL.md",
        "tools/foo/references/old-package/SKILL.md",  # support dir -> not a skill
        ".archive/dup/SKILL.md",                      # archived -> excluded
        "_org/orgA/skillA/SKILL.md",                  # no .active_org marker -> gated off
    ]:
        path = os.path.join(base, rel)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as fh:
            fh.write("---\nname: %s\n---\n" % os.path.basename(os.path.dirname(path)))
    return base


# --- reviewer_skill_name (env, not code) ---------------------------------
rs_skill_env = rs.REVIEWER_SKILL_ENV
saved = os.environ.get(rs_skill_env)
try:
    os.environ[rs_skill_env] = "sdlc-review"
    check("rsk-env-set", rs.reviewer_skill_name(), "sdlc-review")
    os.environ[rs_skill_env] = "  "
    check("rsk-env-blank", rs.reviewer_skill_name(), "")
    os.environ.pop(rs_skill_env, None)
    check("rsk-env-unset", rs.reviewer_skill_name(), "")
finally:
    if saved is None:
        os.environ.pop(rs_skill_env, None)
    else:
        os.environ[rs_skill_env] = saved

# --- skill_status against a synthetic tree --------------------------------
with tempfile.TemporaryDirectory() as tmp:
    make_tree(tmp)
    status, detail = rs.skill_status("foo", tmp)
    check("st-ok-status", status, "ok")
    check("st-ok-path", detail.endswith(os.path.join("tools", "foo", "SKILL.md")), True)
    status, detail = rs.skill_status("dup", tmp)
    check("st-collision-status", status, "collision")
    check("st-collision-count", len(detail), 2)
    check("st-collision-paths",
          sorted(os.path.relpath(p, tmp) for p in detail),
          ["devops/dup/SKILL.md", "software-development/dup/SKILL.md"])
    check("st-missing", rs.skill_status("bar", tmp), ("missing", ""))
    check("st-support-dir-ignored", rs.skill_status("old-package", tmp), ("missing", ""))
    check("st-archive-ignored", rs.skill_status("dup", tmp)[0] != "ok", True)
    check("st-org-no-marker", rs.skill_status("skillA", tmp), ("missing", ""))
    # org mirror activated via .active_org marker -> skillA resolves
    os.makedirs(os.path.join(tmp, "_org"), exist_ok=True)
    with open(os.path.join(tmp, "_org", ".active_org"), "w", encoding="utf-8") as fh:
        fh.write("orgA")
    status, detail = rs.skill_status("skillA", tmp)
    check("st-org-active-status", status, "ok")
    check("st-org-active-path", detail.endswith(os.path.join("_org", "orgA", "skillA", "SKILL.md")), True)
    check("st-none-empty", rs.skill_status("", tmp), ("none", ""))
    check("st-none-blank", rs.skill_status("   ", tmp), ("none", ""))

# Real-tree smoke: the production default (wrapper sets sdlc-review) must
# resolve today — exactly one SKILL.md in ~/.hermes/skills.
real_status, real_detail = rs.skill_status("sdlc-review")
check("st-real-sdlc-ok", real_status, "ok")
check("st-real-sdlc-path", real_detail.endswith("sdlc-review/SKILL.md"), True)

# --- spawn_reviewer cmd construction (skill flag from env, not code) ------
class FakeProc:
    captured_cmd = None

    def __init__(self, cmd, **kw):
        FakeProc.captured_cmd = cmd
        self.pid = 4242
        self.returncode = 0

    def communicate(self, timeout=None):
        return ("", "")


real_popen = rs.subprocess.Popen
try:
    rs.subprocess.Popen = FakeProc
    os.environ[rs_skill_env] = "sdlc-review"
    rs.spawn_reviewer("PROMPT", "terminal,file")
    check("spawn-with-skill", FakeProc.captured_cmd,
          [rs.HERMES_BIN, "chat", "-q", "PROMPT", "-s", "sdlc-review", "-Q",
           "-t", "terminal,file", "--max-turns", "40", "--reasoning", "low"])
    os.environ.pop(rs_skill_env, None)
    rs.spawn_reviewer("PROMPT", "terminal,file")
    check("spawn-no-skill", FakeProc.captured_cmd,
          [rs.HERMES_BIN, "chat", "-q", "PROMPT", "-Q",
           "-t", "terminal,file", "--max-turns", "40", "--reasoning", "low"])
finally:
    rs.subprocess.Popen = real_popen
    os.environ.pop(rs_skill_env, None)

# --- SKIPPED comment: posted once, deduped, no spawn ----------------------
class FakeMcp:
    def __init__(self):
        self.calls = []

    def call(self, tool, params):
        self.calls.append((tool, params))
        return "{}"


def fake_detail(comments=None):
    return {
        "body": "review-required: verify the fix.",
        "title": "test ticket",
        "comments": comments or [],
        "latest_summary": "",
    }


detail = fake_detail()
check("skip-dedup-false", rs.comments_have_sweeper_skip(detail), False)
check("skip-dedup-true",
      rs.comments_have_sweeper_skip(fake_detail([{"body": "review-sweeper: SKIPPED — broken"}])) or
      rs.comments_have_sweeper_skip(fake_detail([{"body": "note\nreview-sweeper: skipped elsewhere"}])),
      True)

mcp = FakeMcp()
posted = rs.comment_skill_skip(mcp, "hermes-agent", "t_x", detail, "ghost-skill",
                              "missing", "")
check("skip-posted-once", posted, True)
check("skip-comment-tool", mcp.calls[0][0], "ticket_comment")
body = mcp.calls[0][1]["body"]
check("skip-body-marker", body.startswith("review-sweeper: SKIPPED — reviewer skill 'ghost-skill'"), True)
check("skip-body-env", rs.REVIEWER_SKILL_ENV in body, True)
check("skip-body-author", mcp.calls[0][1].get("author"), "review-sweeper")
# collision wording
mcp2 = FakeMcp()
rs.comment_skill_skip(mcp2, "b", "t_y", fake_detail(), "dup", "collision",
                      ["/a/dup/SKILL.md", "/b/dup/SKILL.md"])
check("skip-collision-lists-paths", "/a/dup/SKILL.md" in mcp2.calls[0][1]["body"], True)
# dedup: a second call on a ticket that now carries the comment posts nothing
posted2 = rs.comment_skill_skip(mcp, "hermes-agent", "t_x", fake_detail([{"body": body}]),
                                "ghost-skill", "missing", "")
check("skip-dedup-no-resend", posted2, False)
check("skip-dedup-no-call-growth", len(mcp.calls), 1)

# --- process_one gate: bad skill -> skip with comment, NO spawn -----------
class FakeArgs:
    dry_run = False


args = FakeArgs()
spawned = []


def boom_spawn(prompt, toolsets):
    spawned.append(prompt)
    raise AssertionError("spawn_reviewer must NOT be called on a bad skill")


real_spawn = rs.spawn_reviewer
rs.spawn_reviewer = boom_spawn
try:
    os.environ[rs_skill_env] = "definitely-not-a-real-skill-xyz"
    mcp3 = FakeMcp()
    result = rs.process_one(mcp3, lambda: None, "hermes-agent", "t_z",
                            fake_detail(), "fp", {}, args)
    check("gate-skip-returns-true", result, True)
    check("gate-no-spawn", spawned, [])
    check("gate-comment-posted", len(mcp3.calls), 1)
    check("gate-comment-skill",
          "definitely-not-a-real-skill-xyz" in mcp3.calls[0][1]["body"], True)
finally:
    rs.spawn_reviewer = real_spawn
    os.environ.pop(rs_skill_env, None)

print("\n%s" % ("ALL PASS" if not failures else "FAILURES: %s" % failures))
sys.exit(1 if failures else 0)
