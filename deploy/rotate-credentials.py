#!/usr/bin/env python3
"""Rotate the dashboard password and re-sync the kanban-mcp bridge in one step.

WHY THIS EXISTS. The bridge authenticates to Hermes' HTTP surface with
`KANBAN_PASSWORD` from /etc/kanban-mcp.env, and Hermes stores only an scrypt hash
of the dashboard password in config.yaml. Rotating the dashboard password by hand
therefore leaves the bridge holding a dead credential, and the bridge logs in ONCE
at startup with no re-login on 401 -- so it crash-loops (or holds a stale session)
from then on. This script writes both places from ONE value and restarts the three
processes in the order the runtime requires:

  1. stop kanban-mcp        -- a running bridge retries login every 3s, and the login
                               endpoint throttles 10 attempts/60s per IP: an
                               already-running loop guarantees the new credential is
                               rejected with 429 before it can ever succeed.
  2. set the dashboard hash -- `hermes dashboard password --password-stdin`.
  3. sync /etc/kanban-mcp.env -- the plaintext the bridge needs.
  4. restart the gateway     -- the listener reads dashboard.basic_auth ONLY at start.
  5. start kanban-mcp        -- one login attempt into a drained budget.
  6. verify                  -- a real login + GET /api/plugins/kanban/boards.

USAGE
  python3 deploy/rotate-credentials.py             # generate a strong password
  python3 deploy/rotate-credentials.py --prompt    # choose one (hidden prompt)

With --prompt the value is never written anywhere but the env file. Without it the
generated value is also saved to ~/.hermes/kanban-mcp-operator-password (mode 0600)
so the operator can sign in to the web UI; delete that file once stored elsewhere.

THE ORIGIN is the GATEWAY's Webapp listener (`webapp.enabled/host/port` in the
gateway home's config.yaml), not a `hermes dashboard` process: that standalone
backend is the retired "old backend" upstream. This script resolves the bind from
that config so it follows the origin when the port moves; `--origin` overrides it
for a deployment behind a proxy or tunnel.

A LOOPBACK bind (127.0.0.1 / ::1 / localhost) leaves the sign-in gate OFF, and
/api/* then refuses session cookies and wants `X-Hermes-Session-Token`. kanban-mcp
is a cookie client with no session-token path, so it cannot authenticate against a
loopback bind at all. This script therefore REFUSES a loopback bind before it
changes anything: rotating into a bind the bridge can never use produces a
"successful" rotation and a dead bridge (deploy/install.md, "the origin" trap 1).

Never prints or logs the password: every status line carries a length and a short
SHA-256 fingerprint instead.
"""

from __future__ import annotations

import argparse
import getpass
import hashlib
import http.cookiejar
import json
import os
import pathlib
import secrets
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request
from typing import NamedTuple, NoReturn

ENV_FILE = pathlib.Path("/etc/kanban-mcp.env")
HERMES_HOME = pathlib.Path(os.environ.get("HERMES_HOME") or pathlib.Path.home() / ".hermes")
CONFIG_FILE = HERMES_HOME / "config.yaml"
SESSION_TOKEN_FILE = HERMES_HOME / "webapp" / "session-token"
OPERATOR_COPY = HERMES_HOME / "kanban-mcp-operator-password"
# The gateway's Webapp listener defaults (hermes_cli/config_defaults.py); the live
# values are read from CONFIG_FILE, so these are only the fallback and the label.
DEFAULT_WEBAPP_HOST = "127.0.0.1"
DEFAULT_WEBAPP_PORT = 9120
# Binds that leave the sign-in gate OFF (web_server.should_require_dashboard_auth).
LOOPBACK_HOSTS = frozenset({"127.0.0.1", "::1", "localhost"})
BRIDGE_PORT = 9130
GATEWAY_UNIT = "hermes-gateway"
BRIDGE_UNIT = "kanban-mcp"
BOOT_WAIT_SEC = 120.0


class Webapp(NamedTuple):
    """The gateway's Webapp listener as configured (`webapp:` in config.yaml)."""

    enabled: bool
    host: str
    port: int
    public_url: str

    @property
    def gate_off(self) -> bool:
        """True when /api/* refuses cookies: a loopback bind with no public_url.

        The gate is engaged by a non-loopback bind OR by a `webapp.public_url` that
        names a public host, so a loopback bind behind a proxy/tunnel still accepts
        session cookies (gateway/webapp_listener.py).
        """
        return self.host in LOOPBACK_HOSTS and not self.public_url

    @property
    def origin(self) -> str:
        return origin_url(self.host, self.port)


def fingerprint(value: str) -> str:
    return f"{len(value)} chars, sha256:{hashlib.sha256(value.encode()).hexdigest()[:8]}"


def say(msg: str) -> None:
    print(msg, flush=True)


def fail(msg: str, code: int = 1) -> NoReturn:
    print(f"FAILED: {msg}", file=sys.stderr, flush=True)
    raise SystemExit(code)


def webapp_bind(config_file: pathlib.Path | None = None) -> "Webapp":
    """`webapp:` from the gateway home's config.yaml, with the upstream defaults filled in.

    Line-based on purpose: this must not depend on PyYAML being importable where the
    script runs, and the block is four flat keys. A missing/unreadable block reads as
    "off" -- the gateway then keeps a private ephemeral listener, i.e. no origin at all.
    """
    path = config_file or CONFIG_FILE
    enabled, host, port, public_url = False, DEFAULT_WEBAPP_HOST, DEFAULT_WEBAPP_PORT, ""
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError as exc:
        fail(f"cannot read {path} ({exc.strerror}) -- is HERMES_HOME the gateway's home?")
    in_block = False
    for line in lines:
        stripped = line.split("#", 1)[0].rstrip()
        if not stripped.strip():
            continue
        if not stripped[:1].isspace():
            in_block = stripped.strip() == "webapp:"
            continue
        if not in_block:
            continue
        key, _, value = stripped.strip().partition(":")
        value = value.strip().strip("'\"")
        if key == "enabled":
            enabled = value.lower() == "true"
        elif key == "host":
            host = value
        elif key == "port":
            try:
                port = int(value)
            except ValueError:
                fail(f"webapp.port in {path} is not a number: {value!r}")
        elif key == "public_url":
            public_url = value
    return Webapp(enabled=enabled, host=host, port=port, public_url=public_url)


def origin_url(host: str, port: int) -> str:
    return f"http://[{host}]:{port}" if ":" in host else f"http://{host}:{port}"


def env_value(key: str) -> str:
    """One value from the bridge's env file, for the keys that are NOT secrets."""
    for line in ENV_FILE.read_text(encoding="utf-8").splitlines():
        if line.startswith(f"{key}="):
            return line.split("=", 1)[1].strip()
    return ""


def hermes_bin() -> str:
    found = shutil.which("hermes")
    if found:
        return found
    for candidate in (
        pathlib.Path.home() / ".hermes/hermes-agent/.hermes/bin/hermes",
        pathlib.Path.home() / ".local/bin/hermes",
    ):
        if candidate.exists():
            return str(candidate)
    fail("cannot find the hermes CLI")


def run(cmd: list[str], *, stdin_text: str | None = None, check: bool = True) -> subprocess.CompletedProcess:
    """Run a command on the inherited terminal so `sudo` can prompt if it needs to."""
    proc = subprocess.run(cmd, input=stdin_text, text=True)
    if check and proc.returncode != 0:
        fail(f"{' '.join(cmd[:3])} exited {proc.returncode}")
    return proc


def systemctl_user(*args: str) -> None:
    run(["systemctl", "--user", *args])


def sudo_systemctl(*args: str) -> None:
    run(["sudo", "systemctl", *args])


def port_open(port: int, hosts: tuple[str, ...] = ("127.0.0.1",)) -> bool:
    import socket

    for host in hosts:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
            sock.settimeout(1.0)
            if sock.connect_ex((host, port)) == 0:
                return True
    return False


def wait_until(predicate, *, timeout: float, what: str, interval: float = 1.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(interval)
    fail(f"timed out after {int(timeout)}s waiting for {what}")


def origin_ready(origin: str) -> bool:
    """Bound AND past its 'gateway_not_ready' warm-up: 401 or 200 both mean serving."""
    try:
        with urllib.request.urlopen(f"{origin}/api/auth/providers", timeout=3) as resp:
            return resp.status in (200, 401, 403)
    except urllib.error.HTTPError as exc:
        return exc.code in (200, 401, 403)
    except (urllib.error.URLError, OSError):
        return False


def write_env_password(password: str) -> None:
    """Replace the KANBAN_PASSWORD line in place. Line-based, not a regex: a backslash or
    an anchor in the value must not be able to change the shape of the file."""
    text = ENV_FILE.read_text(encoding="utf-8")
    lines = text.splitlines(keepends=True)
    replacement = f"KANBAN_PASSWORD={password}\n"
    hits = 0
    for index, line in enumerate(lines):
        if line.startswith("KANBAN_PASSWORD="):
            lines[index] = replacement
            hits += 1
    if hits != 1:
        fail(f"expected exactly one KANBAN_PASSWORD line in {ENV_FILE}, found {hits}")
    ENV_FILE.write_text("".join(lines), encoding="utf-8")
    os.chmod(ENV_FILE, 0o600)


def env_password() -> str:
    for line in ENV_FILE.read_text(encoding="utf-8").splitlines():
        if line.startswith("KANBAN_PASSWORD="):
            return line.split("=", 1)[1]
    fail(f"no KANBAN_PASSWORD in {ENV_FILE}")


def verify_login(password: str, origin: str, *, username: str, session_token: str | None = None) -> None:
    """Prove the new hash AND that the origin serves /api/* to the bridge's credential.

    Gated bind: the credential is the session cookie the login just minted.
    Loopback bind: the gate is OFF and /api/* wants X-Hermes-Session-Token instead --
    cookies there are ignored rather than rejected, so the failure reads as a bad
    password unless it is named (deploy/install.md, "the origin" trap 1).
    """
    jar = http.cookiejar.CookieJar()
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
    body = json.dumps({"provider": "basic", "username": username, "password": password}).encode()
    req = urllib.request.Request(
        f"{origin}/auth/password-login", data=body,
        headers={"Content-Type": "application/json"}, method="POST")
    try:
        with opener.open(req, timeout=15) as resp:
            if resp.status != 200:
                fail(f"login returned HTTP {resp.status}")
    except urllib.error.HTTPError as exc:
        detail = exc.read()[:200].decode(errors="replace")
        fail(f"login returned HTTP {exc.code}: {detail}")

    headers = {"X-Hermes-Session-Token": session_token} if session_token else {}
    req = urllib.request.Request(f"{origin}/api/plugins/kanban/boards", headers=headers)
    payload: dict = {}
    try:
        with opener.open(req, timeout=20) as resp:
            payload = json.loads(resp.read().decode())
    except urllib.error.HTTPError as exc:
        hint = (" (the gate is OFF on a loopback bind: /api/* wants X-Hermes-Session-Token, "
                "not the cookie this login just set)" if session_token else " with a valid session")
        fail(f"/boards returned HTTP {exc.code}{hint}")
    boards = [b.get("slug") for b in (payload.get("boards") or [])]
    if not boards:
        fail(f"/boards returned no boards: {str(payload)[:200]}")
    via = "session token" if session_token else "session cookie"
    say(f"  verified: login 200, /boards 200 via {via}, {len(boards)} board(s): {', '.join(boards[:6])}")


def main() -> int:
    parser = argparse.ArgumentParser(description="Rotate the dashboard password and resync kanban-mcp.")
    parser.add_argument("--prompt", action="store_true",
                        help="ask for the password instead of generating one (hidden input)")
    parser.add_argument("--origin", default="",
                        help="origin to verify against (default: the gateway's webapp.host/port)")
    args = parser.parse_args()

    if not ENV_FILE.exists():
        fail(f"{ENV_FILE} does not exist -- is this the host that runs kanban-mcp?")
    if os.geteuid() == 0:
        fail("run this as the service account, not root: it calls sudo where it needs to")

    # Resolve the origin BEFORE touching anything: rotating into a bind the bridge can
    # never authenticate against is the failure this check exists to prevent.
    webapp = webapp_bind()
    if not webapp.enabled:
        fail(f"webapp.enabled is not true in {CONFIG_FILE}: the gateway's Webapp listener is "
             f"off, so there is no stable origin to rotate against (deploy/install.md)")
    origin = (args.origin.rstrip("/") or webapp.origin)
    if webapp.gate_off and not args.origin:
        fail(f"webapp.host is {webapp.host} in {CONFIG_FILE} and webapp.public_url is empty: a "
             f"loopback bind leaves /api/* refusing session cookies (it wants "
             f"X-Hermes-Session-Token), and kanban-mcp is a cookie client -- nothing was changed. "
             f"Bind webapp.host to this host's Tailscale/LAN address, or set webapp.public_url, "
             f"then rerun (deploy/install.md, 'the origin', trap 1)")
    session_token = None
    if webapp.gate_off:
        try:
            session_token = SESSION_TOKEN_FILE.read_text(encoding="utf-8").strip()
        except OSError as exc:
            fail(f"cannot read {SESSION_TOKEN_FILE} ({exc.strerror}): an explicit loopback "
                 f"--origin verifies with that token, not with cookies")
    say(f"origin: {origin} (webapp.host={webapp.host} webapp.port={webapp.port}"
        f"{', gate off' if webapp.gate_off else ''})")

    if args.prompt:
        password = getpass.getpass("  new dashboard password: ")
        if len(password) < 12:
            fail("refusing a password under 12 characters")
        if getpass.getpass("  confirm: ") != password:
            fail("the two entries did not match")
    else:
        password = secrets.token_urlsafe(24)
        say(f"generated a password: {fingerprint(password)}")

    say("1/6 stopping the bridge (a live retry loop would starve the login budget)")
    sudo_systemctl("stop", BRIDGE_UNIT)

    say("2/6 writing the dashboard hash")
    run([hermes_bin(), "dashboard", "password", "--password-stdin"], stdin_text=password + "\n")

    say(f"3/6 syncing {ENV_FILE}")
    write_env_password(password)
    say(f"  env now holds {fingerprint(env_password())}")

    say("4/6 restarting the gateway so the listener loads the new hash")
    systemctl_user("restart", GATEWAY_UNIT)
    wait_until(lambda: origin_ready(origin), timeout=BOOT_WAIT_SEC, what=f"{origin} to finish starting")

    say("5/6 starting the bridge into a drained login budget")
    sudo_systemctl("start", BRIDGE_UNIT)
    wait_until(lambda: port_open(BRIDGE_PORT), timeout=30, what=f"port {BRIDGE_PORT}")

    say("6/6 verifying against the live origin")
    verify_login(password, origin, username=env_value("KANBAN_USERNAME") or "admin",
                 session_token=session_token)

    if not args.prompt:
        OPERATOR_COPY.write_text(password + "\n", encoding="utf-8")
        os.chmod(OPERATOR_COPY, 0o600)
        say(f"saved the password for the web UI at {OPERATOR_COPY} (mode 0600)")
        say("  move it into your password manager, then delete that file")

    say("done: the dashboard password and the bridge credential are the same value again")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
