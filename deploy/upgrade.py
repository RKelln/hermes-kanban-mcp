#!/usr/bin/env python3
"""Upgrade the running kanban-mcp bridge from this repo.

    python3 deploy/upgrade.py [--dry-run] [--expect <ref>] [--allow-dirty] [--skip-smoke]

WHY THIS IS BINARY-ONLY
-----------------------
It never WRITES /etc/systemd/system/kanban-mcp.service and never writes
/etc/kanban-mcp.env. It reads them, and it says so in its output. Both were
traps in practice:

* The repo's unit ships an unsubstituted `User=__SERVICE_USER__` placeholder.
  Installing it verbatim took the service down on 2026-09-14 — systemd cannot
  resolve that user, so every start died with `217/USER` and `Restart=on-failure`
  looped it every 3s, which `systemctl status` reports as `activating
  (auto-restart)`, i.e. it LOOKS like a slow start rather than a failure.
* The env file is 0600 and holds the real token; `deploy/kanban-mcp.env.example`
  would overwrite it. New keys belong appended by hand, never by install.

WHAT IT PROVES
--------------
One job: put the compiled binary in place and prove it is the binary that is
actually serving. After the restart it reads the version the RUNNING process
logged and fails if that differs from the version it just built — because
"I deployed it" and "the thing that is running is what I built" are different
claims, and that difference is how a pre-fix build gets deployed by mistake.

PRIVILEGE, AND WHAT IT TOUCHES
------------------------------
sudo is used for exactly two writes: the install of /usr/local/bin/kanban-mcp
and the `systemctl restart`. The backup is a plain user-owned copy under
~/.local/state/kanban-mcp/backups — no sudo, because the installed binary is
world-readable. Reading a root:root 0600 env file (what deploy/install.md §3
installs) falls back to `sudo -n cat`, which never prompts, so --dry-run cannot
stall on a password; if that is not possible the script refuses and says to run
`sudo -v` first.
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
BACKUP_DIR = os.path.join(os.path.expanduser("~"), ".local", "state",
                          "kanban-mcp", "backups")

PLACEHOLDER = "__SERVICE_USER__"
TOKEN_KEY = "MCP_BEARER_TOKEN"
PASSWORD_KEY = "KANBAN_PASSWORD"
REQUIRED_KEYS = (TOKEN_KEY, PASSWORD_KEY)

# A restart is not instant: the process has to bind. A single probe would report
# a good deploy as FAILED, which is the verdict this script exists to make
# trustworthy — so both post-restart reads are bounded RETRIES, not one shot.
HEALTH_TIMEOUT = 10.0
VERSION_TIMEOUT = 10.0


class Fail(Exception):
    """A check that must stop the upgrade."""


def run(cmd, **kw):
    kw.setdefault("capture_output", True)
    kw.setdefault("text", True)
    return subprocess.run(cmd, **kw)


def stdin_is_tty() -> bool:
    try:
        return bool(sys.stdin and sys.stdin.isatty())
    except (ValueError, AttributeError):
        return False


def sudo(cmd, **kw):
    """Run a privileged command (the two writes: install, restart).

    Non-interactive callers pipe the password in (`echo pw | ... upgrade.py`):
    that is the `sudo -S -p ''` form — read the password from stdin, print no
    prompt. On a terminal we let sudo prompt normally, because `-S` reads the
    typed password from the tty WITHOUT disabling echo, which would leave it in
    the scrollback.
    """
    prefix = ["sudo"] if stdin_is_tty() else ["sudo", "-S", "-p", ""]
    return run([*prefix, *cmd], **kw)


def say(step, msg):
    print("[%s] %s" % (step, msg))


def read_installed(path):
    """Read an installed (possibly root-owned) file; never prompts.

    Plain read first — on a host where the file is user-readable that is the
    whole story. Otherwise `sudo -n`, which needs an already-cached credential
    rather than a password prompt, so `--dry-run` stays side-effect free and
    a non-interactive run cannot hang on a hidden prompt.
    """
    try:
        with open(path, encoding="utf-8", errors="replace") as fh:
            return fh.read()
    except FileNotFoundError:
        return None
    except PermissionError:
        pass

    p = run(["sudo", "-n", "cat", path])
    if p.returncode == 0:
        return p.stdout
    raise Fail(
        "cannot read %s, and `sudo -n cat` did not work either (%s).\n"
        "If this file is root:root 0600 (deploy/install.md §3 installs it that "
        "way), run `sudo -v` once to cache a credential and re-run — this "
        "script deliberately does not prompt mid-run."
        % (path, (p.stderr or "").strip() or "no error output")
    )


# --- pure checks (unit-tested) -----------------------------------------------


def unit_placeholder_lines(unit_text: str | None) -> list[str]:
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


def env_keys(env_text: str | None) -> set[str]:
    return {m.group(1) for m in re.finditer(r"^([A-Z_]+)=", env_text or "", re.M)}


def smoke_token(env_text: str) -> str:
    """The bearer token for the smoke test, read from the env file.

    Only the value's presence is ever reported; it is never printed, never
    placed on a command line (smoke.sh takes it as an env var).
    """
    m = re.search(r"^%s=(.*)$" % re.escape(TOKEN_KEY), env_text or "", re.M)
    value = (m.group(1) if m else "").strip()
    if not value:
        raise Fail("cannot read %s from %s for the smoke test" % (TOKEN_KEY, ENV_FILE))
    return value


def parse_version_line(journal_text: str | None) -> str | None:
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


def stamp(describe: str, sha: str, dirty: bool = False) -> str:
    """The version string embedded in the binary we build.

    `dirty` is part of it on purpose: an --allow-dirty deploy of unreviewed code
    must not be journal-indistinguishable from a clean build of the same commit.
    """
    base = "%s-%s" % (describe.strip() or sha, sha)
    return base + "-dirty" if dirty else base


def rollback_line(backup: str | None) -> str:
    """How to undo this deploy — or the honest statement that there is nothing.

    Printing an install of the binary we just installed (or of a path that does
    not exist) is a lie you discover with the service down.
    """
    if backup and os.path.exists(backup):
        return ("rollback:  sudo install -m 0755 %s %s && sudo systemctl restart %s"
                % (backup, BIN, SERVICE))
    return ("rollback:  no previous binary to restore — nothing existed at %s "
            "before this run, so nothing was overwritten (the unit and the env "
            "file are unchanged)" % BIN)


# --- steps -------------------------------------------------------------------


def preflight(args) -> dict:
    if not os.path.exists(os.path.join(REPO, "go.mod")):
        raise Fail("not a repo root: no go.mod in %s" % REPO)
    if not os.path.isdir(os.path.join(REPO, "cmd", "kanban-mcp")):
        raise Fail("missing cmd/kanban-mcp — is this the hermes-kanban-mcp repo?")
    if shutil.which("go") is None:
        raise Fail("go is not on PATH")

    # HEAD is the only thing this script can build: `go build` compiles the
    # working tree, so stamping or gating on any OTHER commit would describe an
    # artifact that does not exist. There is no --sha for that reason: be on the
    # commit you mean to ship, and name it with --expect.
    sha = run(["git", "-C", REPO, "rev-parse", "--short", "HEAD"])
    if sha.returncode != 0:
        raise Fail("cannot resolve HEAD: %s" % sha.stderr.strip())
    sha = sha.stdout.strip()
    branch = run(["git", "-C", REPO, "rev-parse", "--abbrev-ref", "HEAD"]).stdout.strip()
    describe = run(["git", "-C", REPO, "describe", "--tags", "--always"]).stdout.strip()

    dirty_text = run(["git", "-C", REPO, "status", "--porcelain"]).stdout.strip()
    dirty = bool(dirty_text)
    if dirty and not args.allow_dirty:
        raise Fail(
            "the working tree is dirty:\n%s\n"
            "A dirty tree means you would deploy code that no review has seen. "
            "Commit and merge it, or pass --allow-dirty if you truly mean it."
            % "\n".join("    " + l for l in dirty_text.splitlines()[:10])
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
                "fix you meant to ship. Merge it, check out the commit you "
                "really mean, and re-run." % (sha, want)
            )

    unit = read_installed(UNIT)
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

    envtext = read_installed(ENV_FILE)
    if envtext is None:
        raise Fail("%s missing — the service would start without its config" % ENV_FILE)
    keys = env_keys(envtext)
    for required in REQUIRED_KEYS:
        if required not in keys:
            raise Fail("%s has no %s" % (ENV_FILE, required))

    return {"sha": sha, "branch": branch, "describe": describe, "dirty": dirty,
            "version": stamp(describe, sha, dirty), "env_keys": keys,
            "env_text": envtext}


def build(version: str, out: str) -> None:
    say("build", "CGO_ENABLED=0 go build -> %s (version %s)" % (out, version))
    env = dict(os.environ, CGO_ENABLED="0")
    p = run(["go", "build", "-trimpath",
             "-ldflags", "-s -w -X main.version=%s" % version,
             "-o", out, "./cmd/kanban-mcp"], cwd=REPO, env=env)
    if p.returncode != 0:
        raise Fail("go build failed:\n%s" % (p.stderr or p.stdout))


def backup_binary(stamp_ts: str) -> str | None:
    """Copy the installed binary aside; None when there is nothing to copy.

    No sudo: the installed binary is world-readable and the backup tree is the
    operator's own state dir. Nothing here is a write to the unit or the env
    file.
    """
    if not os.path.exists(BIN):
        say("backup", "no binary at %s yet — nothing to back up (the unit is "
            "installed but the binary is missing; this run will place it)" % BIN)
        return None
    os.makedirs(BACKUP_DIR, mode=0o700, exist_ok=True)
    dest = os.path.join(BACKUP_DIR, "kanban-mcp.bak-%s" % stamp_ts)
    shutil.copy2(BIN, dest)
    say("backup", "%s -> %s" % (BIN, dest))
    return dest


def install_and_restart(tmp_bin: str) -> None:
    say("install", "%s -> %s" % (tmp_bin, BIN))
    p = sudo(["install", "-m", "0755", tmp_bin, BIN])
    if p.returncode != 0:
        raise Fail("install failed: %s" % (p.stderr or p.stdout).strip())

    say("restart", "systemctl restart %s (no daemon-reload: the unit is unchanged)" % SERVICE)
    p = sudo(["systemctl", "restart", SERVICE])
    if p.returncode != 0:
        raise Fail("restart failed: %s" % (p.stderr or p.stdout).strip())


def probe_health() -> str:
    """One health probe. 'ok', or a short description of what is wrong."""
    active = (run(["systemctl", "is-active", SERVICE]).stdout or "").strip() or "unknown"
    p = run(["curl", "-sS", "--max-time", "5", URL + "/healthz"])
    body = (p.stdout or "").strip()
    if active == "active" and p.returncode == 0 and body == "ok":
        return "ok"
    detail = body or (p.stderr or "").strip() or "curl rc=%d" % p.returncode
    return "%s is %s; /healthz said %r" % (SERVICE, active, detail)


def wait_for_health(probe=None, timeout_s: float = HEALTH_TIMEOUT) -> None:
    """Poll until the service answers, or fail loudly with the last answer.

    A single probe would call a slow bind a failed deploy; a retry cannot hide a
    service that never comes up.
    """
    probe = probe or probe_health
    deadline = time.monotonic() + timeout_s
    last = "no probe ran"
    while True:
        last = probe()
        if last == "ok":
            say("verify", "/healthz ok, %s active" % SERVICE)
            return
        if time.monotonic() >= deadline:
            raise Fail("the service is not healthy %gs after the restart (%s)"
                       % (timeout_s, last))
        time.sleep(1)


def journal_since(since: str) -> str:
    """The service's journal since `since`, as one string."""
    p = run(["journalctl", "-u", SERVICE, "--since", since, "--no-pager"])
    return p.stdout or ""


def wait_for_running_version(want_version: str, since: str, journal=None,
                             timeout_s: float = VERSION_TIMEOUT) -> None:
    """The claim that matters: the RUNNING process is the binary we just built.

    Retried, because the startup line appears a moment after the restart. A
    version line that is present and WRONG fails immediately — that is a
    different binary already serving, not a slow start.
    """
    read_journal = journal or (lambda: journal_since(since))
    deadline = time.monotonic() + timeout_s
    seen = None
    while True:
        seen = parse_version_line(read_journal())
        if seen == want_version:
            say("verify", "running version == built version (%s)" % seen)
            return
        if seen is not None:
            break
        if time.monotonic() >= deadline:
            break
        time.sleep(1)
    raise Fail(
        "the running process reports version %r but we built %r. The restart "
        "may not have taken, or the service is serving a different binary. Do "
        "not trust this deploy." % (seen, want_version)
    )


def verify(want_version: str, since: str, env_text: str, skip_smoke: bool) -> None:
    wait_for_health()
    wait_for_running_version(want_version, since)

    if skip_smoke:
        say("verify", "smoke test skipped (--skip-smoke)")
        return
    token = smoke_token(env_text)
    say("verify", "tools/list roster via scripts/smoke.sh")
    p = run(["bash", SMOKE], env=dict(os.environ, URL=URL, **{TOKEN_KEY: token}))
    lines = (p.stdout + p.stderr).strip().splitlines()
    if p.returncode != 0:
        for line in lines[-15:]:
            print("        " + line)
        raise Fail("the smoke test failed (rc=%d) — see its output above" % p.returncode)
    for line in lines[-3:]:
        print("        " + line)


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--dry-run", action="store_true", help="preflight and print the plan only")
    ap.add_argument("--expect",
                    help="refuse unless HEAD contains this ref — name the SHA you "
                         "intend to ship, e.g. the fix you just merged")
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
    if info["branch"] == "HEAD":
        say("preflight", "note: detached HEAD — you are deploying commit %s, not a branch"
            % info["sha"])
    if info["dirty"]:
        say("preflight", "note: --allow-dirty — the label carries -dirty, so this "
            "deploy is distinguishable from a clean build of %s" % info["sha"])
    if not args.expect:
        say("preflight", "note: no --expect given. The version check still proves "
            "the binary is what was built, not that it is the build you meant to ship.")
    if PASSWORD_KEY not in info["env_keys"]:
        say("preflight", "note: no %s in %s" % (PASSWORD_KEY, ENV_FILE))
    if "MCP_REVIEWER_PROFILE" not in info["env_keys"]:
        say("preflight", "note: no MCP_REVIEWER_PROFILE in %s — ticket_request_review "
            "will rely on the kernel's own re-review provenance" % ENV_FILE)
    say("preflight", "%s and %s are READ, never written (no new key is added here; "
        "append one by hand if you need it)" % (UNIT, ENV_FILE))

    tmp_bin = "/tmp/kanban-mcp-upgrade-%s" % info["sha"]
    if args.dry_run:
        say("dry-run", "would build -> %s, back up %s to %s, install it, restart, "
            "then verify the running version against %s and run the smoke test"
            % (tmp_bin, BIN, BACKUP_DIR, info["version"]))
        say("dry-run", "nothing was changed")
        return 0

    # `backup` is bound BEFORE anything that can fail, so every failure path can
    # name the binary to roll back to — including a failure after the install.
    backup = None
    try:
        build(info["version"], tmp_bin)
        backup = backup_binary(time.strftime("%Y%m%d-%H%M%S"))
        install_and_restart(tmp_bin)
        verify(info["version"], since, info["env_text"], args.skip_smoke)
    except Fail as exc:
        print("\nFAILED: %s" % exc, file=sys.stderr)
        print(rollback_line(backup), file=sys.stderr)
        return 1
    except OSError as exc:
        print("\nFAILED: unexpected error before/while installing: %s" % exc, file=sys.stderr)
        print(rollback_line(backup), file=sys.stderr)
        return 1

    print("\ndeployed %s (%s)" % (info["version"], info["sha"]))
    print(rollback_line(backup))
    return 0


if __name__ == "__main__":
    sys.exit(main())
