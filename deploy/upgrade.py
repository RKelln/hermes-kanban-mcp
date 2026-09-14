#!/usr/bin/env python3
"""Upgrade the running kanban-mcp bridge from this repo.

    python3 deploy/upgrade.py [--dry-run] [--sha <ref>] [--allow-dirty] [--skip-smoke]

WHY THIS IS BINARY-ONLY
-----------------------
It never touches /etc/systemd/system/kanban-mcp.service and never touches
/etc/kanban-mcp.env. Both were traps in practice:

* The repo's unit ships an unsubstituted `User=__SERVICE_USER__` placeholder.
  Installing it verbatim took the service down on 2026-09-14 — systemd cannot
  resolve that user, so every start died with `217/USER` and `Restart=on-failure`
  looped it every 3s, which `systemctl status` reports as `activating
  (auto-restart)`, i.e. it LOOKS like a slow start rather than a failure.
* The env file is 0600 and holds the real token; `deploy/kanban-mcp.env.example`
  would overwrite it. New keys belong appended by hand, never by install.

This script therefore has one job: put the compiled binary in place and prove it
is the binary that is actually serving. It refuses to run against the two traps
above, and it verifies the deployed VERSION after restart — because "I deployed
it" and "the thing that is running is what I built" are different claims, and
the difference is how a pre-fix build gets deployed by mistake.
"""

from __future__ import annotations

import argparse
import os
import re
import shutil
import subprocess
import sys
import time

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
UNIT = "/etc/systemd/system/kanban-mcp.service"
ENV_FILE = "/etc/kanban-mcp.env"
BIN = "/usr/local/bin/kanban-mcp"
SERVICE = "kanban-mcp.service"
SMOKE = os.path.join(REPO, "scripts", "smoke.sh")
URL = "http://127.0.0.1:9130"

PLACEHOLDER = "__SERVICE_USER__"


class Fail(Exception):
    """A check that must stop the upgrade."""


def run(cmd, **kw):
    kw.setdefault("capture_output", True)
    kw.setdefault("text", True)
    return subprocess.run(cmd, **kw)


def sudo(cmd, **kw):
    return run(["sudo", *cmd], **kw)


def say(step, msg):
    print("[%s] %s" % (step, msg))


def read(path, use_sudo=False):
    try:
        with open(path, encoding="utf-8", errors="replace") as fh:
            return fh.read()
    except FileNotFoundError:
        return None
    except PermissionError:
        if not use_sudo:
            raise
        p = sudo(["cat", path])
        return p.stdout if p.returncode == 0 else None


# --- pure checks (unit-tested) -----------------------------------------------


def unit_placeholder_lines(unit_text: str) -> list[str]:
    """Active lines still carrying the service-user placeholder.

    Comments are fine — the repo's unit deliberately documents the placeholder
    in two of them. Only a LIVE directive matters, because only a live
    directive reaches systemd.
    """
    out = []
    for line in (unit_text or "").splitlines():
        stripped = line.strip()
        if stripped.startswith("#") or "=" not in stripped:
            continue
        if PLACEHOLDER in stripped:
            out.append(stripped)
    return out


def env_keys(env_text: str) -> set[str]:
    return {m.group(1) for m in re.finditer(r"^([A-Z_]+)=", env_text or "", re.M)}


def parse_version_line(journal_text: str) -> str | None:
    """The last `"version":"..."` in the journal — what is ACTUALLY running."""
    hits = re.findall(r'"version":"([^"]*)"', journal_text or "")
    return hits[-1] if hits else None


def contains_commit(sha: str, head: str, cwd: str = REPO) -> bool:
    """True when `head` contains `sha` (equal, or sha is an ancestor).

    This is the check that makes intent explicit: naming the commit you MEAN to
    ship catches the case where a deploy lands a build that predates the fix —
    which is how a pre-fix binary got served on 2026-09-14.
    """
    if sha == head:
        return True
    p = run(["git", "-C", cwd, "merge-base", "--is-ancestor", sha, head])
    return p.returncode == 0


def stamp(describe: str, sha: str) -> str:
    """The version string embedded in the binary we build."""
    describe = describe.strip() or sha
    return "%s-%s" % (describe, sha)


# --- steps -------------------------------------------------------------------


def preflight(args) -> dict:
    if not os.path.exists(os.path.join(REPO, "go.mod")):
        raise Fail("not a repo root: no go.mod in %s" % REPO)
    if not os.path.isdir(os.path.join(REPO, "cmd", "kanban-mcp")):
        raise Fail("missing cmd/kanban-mcp — is this the hermes-kanban-mcp repo?")
    if shutil.which("go") is None:
        raise Fail("go is not on PATH")

    ref = args.sha or "HEAD"
    sha = run(["git", "-C", REPO, "rev-parse", "--short", ref])
    if sha.returncode != 0:
        raise Fail("cannot resolve %r: %s" % (ref, sha.stderr.strip()))
    sha = sha.stdout.strip()
    branch = run(["git", "-C", REPO, "rev-parse", "--abbrev-ref", "HEAD"]).stdout.strip()
    describe = run(["git", "-C", REPO, "describe", "--tags", "--always"]).stdout.strip()

    dirty = run(["git", "-C", REPO, "status", "--porcelain"]).stdout.strip()
    if dirty and not args.allow_dirty:
        raise Fail(
            "the working tree is dirty:\n%s\n"
            "A dirty tree means you would deploy code that no review has seen. "
            "Commit and merge it, or pass --allow-dirty if you truly mean it."
            % "\n".join("    " + l for l in dirty.splitlines()[:10])
        )

    if args.expect:
        want = run(["git", "-C", REPO, "rev-parse", "--short", args.expect])
        if want.returncode != 0:
            raise Fail("cannot resolve --expect %r: %s" % (args.expect, want.stderr.strip()))
        want = want.stdout.strip()
        if not contains_commit(want, sha):
            raise Fail(
                "the commit being deployed (%s) does NOT contain --expect %s.\n"
                "That is the shape of a deploy that lands a build predating the "
                "fix you meant to ship. Merge first (or pass the ref you really "
                "want with --sha)." % (sha, want)
            )

    unit = read(UNIT, use_sudo=True)
    if unit is None:
        raise Fail(
            "%s does not exist — this host has no bridge installed. Use the "
            "fresh-install path in deploy/install.md first." % UNIT
        )
    live = unit_placeholder_lines(unit)
    if live:
        raise Fail(
            "the INSTALLED unit still carries the placeholder:\n%s\n"
            "Restarting would fail with 217/USER and enter a restart loop. Fix "
            "the unit first (substitute the real service user), then re-run. "
            "This script will not install a unit — see deploy/install.md's "
            "upgrade section, and the reason it is dangerous is in this file's "
            "docstring." % "\n".join("    " + l for l in live)
        )

    envtext = read(ENV_FILE, use_sudo=True)
    if envtext is None:
        raise Fail("%s missing — the service would start without its config" % ENV_FILE)
    keys = env_keys(envtext)
    for required in ("MCP_BEARER_TOKEN", "KANBAN_PASSWORD"):
        if required not in keys:
            raise Fail("%s has no %s" % (ENV_FILE, required))

    return {"sha": sha, "branch": branch, "describe": describe,
            "version": stamp(describe, sha), "env_keys": keys}


def build(version: str, out: str) -> None:
    say("build", "CGO_ENABLED=0 go build -> %s (version %s)" % (out, version))
    env = dict(os.environ, CGO_ENABLED="0")
    p = run(["go", "build", "-trimpath",
             "-ldflags", "-s -w -X main.version=%s" % version,
             "-o", out, "./cmd/kanban-mcp"], cwd=REPO, env=env)
    if p.returncode != 0:
        raise Fail("go build failed:\n%s" % (p.stderr or p.stdout))


def install_and_restart(tmp_bin: str) -> str:
    stamp_ts = time.strftime("%Y%m%d-%H%M%S")
    backup = "%s.bak-%s" % (BIN, stamp_ts)
    if os.path.exists(BIN):
        say("backup", "%s -> %s" % (BIN, backup))
        p = sudo(["cp", "-a", BIN, backup])
        if p.returncode != 0:
            raise Fail("cannot back up the current binary: %s" % p.stderr.strip())
    else:
        backup = None
        say("backup", "no existing binary at %s (first install)" % BIN)

    say("install", "%s -> %s" % (tmp_bin, BIN))
    p = sudo(["install", "-m", "0755", tmp_bin, BIN])
    if p.returncode != 0:
        raise Fail("install failed: %s" % p.stderr.strip())

    say("restart", "systemctl restart %s (no daemon-reload: the unit is unchanged)" % SERVICE)
    p = sudo(["systemctl", "restart", SERVICE])
    if p.returncode != 0:
        raise Fail("restart failed: %s" % p.stderr.strip())
    time.sleep(2)
    return backup or BIN


def verify(want_version: str, since: str, skip_smoke: bool) -> None:
    p = run(["systemctl", "is-active", SERVICE])
    if p.stdout.strip() != "active":
        raise Fail("%s is %r, not active" % (SERVICE, p.stdout.strip()))

    p = run(["curl", "-sS", "--max-time", "5", URL + "/healthz"])
    if p.returncode != 0 or p.stdout.strip() != "ok":
        raise Fail("/healthz said %r" % (p.stdout.strip() or p.stderr.strip()))
    say("verify", "/healthz ok")

    # The claim that matters: the RUNNING process is the binary we just built.
    time.sleep(1)
    p = run(["journalctl", "-u", SERVICE, "--since", since, "--no-pager"])
    running = parse_version_line(p.stdout)
    if running != want_version:
        raise Fail(
            "the running process reports version %r but we built %r. The "
            "restart may not have taken, or the service is serving a different "
            "binary. Do not trust this deploy." % (running, want_version)
        )
    say("verify", "running version == built version (%s)" % running)

    if skip_smoke:
        say("verify", "smoke test skipped (--skip-smoke)")
        return
    envtext = read(ENV_FILE, use_sudo=True) or ""
    m = re.search(r"^MCP_BEARER_TOKEN=(.*)$", envtext, re.M)
    if not m:
        raise Fail("cannot read MCP_BEARER_TOKEN for the smoke test")
    say("verify", "tools/list roster via scripts/smoke.sh")
    p = run(["bash", SMOKE], env=dict(os.environ, URL=URL,
                                      MCP_BEARER_TOKEN=m.group(1).strip()))
    tail = (p.stdout + p.stderr).strip().splitlines()
    for line in tail[-3:]:
        print("        " + line)
    if p.returncode != 0:
        raise Fail("the smoke test failed — see its output above")


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--dry-run", action="store_true", help="preflight and print the plan only")
    ap.add_argument("--sha", help="build this ref instead of HEAD (must be committed)")
    ap.add_argument("--expect",
                    help="refuse unless the deployed commit contains this ref — name the "
                         "SHA you intend to ship, e.g. the fix you just merged")
    ap.add_argument("--allow-dirty", action="store_true",
                    help="deploy a dirty working tree (unreviewed code — you were warned)")
    ap.add_argument("--skip-smoke", action="store_true", help="skip the smoke test")
    args = ap.parse_args(argv)

    since = time.strftime("%Y-%m-%d %H:%M:%S")
    try:
        info = preflight(args)
    except Fail as exc:
        print("REFUSED: %s" % exc, file=sys.stderr)
        return 2

    say("preflight", "branch %s @ %s -> version %s"
        % (info["branch"], info["sha"], info["version"]))
    if "MCP_REVIEWER_PROFILE" not in info["env_keys"]:
        say("preflight", "note: no MCP_REVIEWER_PROFILE in %s — ticket_request_review "
            "will rely on the kernel's own re-review provenance" % ENV_FILE)
    say("preflight", "unit and env file will NOT be touched")

    tmp_bin = "/tmp/kanban-mcp-upgrade-%s" % info["sha"]
    if args.dry_run:
        say("dry-run", "would build -> %s, back up %s, install, restart, then verify "
            "the running version and run the smoke test" % (tmp_bin, BIN))
        return 0

    try:
        build(info["version"], tmp_bin)
        backup = install_and_restart(tmp_bin)
        verify(info["version"], since, args.skip_smoke)
    except Fail as exc:
        print("\nFAILED: %s" % exc, file=sys.stderr)
        print("rollback:  sudo install -m 0755 %s %s && sudo systemctl restart %s"
              % (backup, BIN, SERVICE), file=sys.stderr)
        return 1

    print("\ndeployed %s (%s)" % (info["version"], info["sha"]))
    print("rollback:  sudo install -m 0755 %s %s && sudo systemctl restart %s"
          % (backup, BIN, SERVICE))
    return 0


if __name__ == "__main__":
    sys.exit(main())
