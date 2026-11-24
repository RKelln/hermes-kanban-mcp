#!/usr/bin/env python3
"""Tests for deploy/rotate-credentials.py — the env writer and the origin resolution.

WHY these exist. On 2026-11-24 a hand-run rotation replaced the KANBAN_PASSWORD
value with a regex, so the stored value was treated as a replacement TEMPLATE:
`\\1` and `\\g<0>` were expanded and a lone backslash raised. The dashboard got the
new hash, the env file got a mangled one, and the bridge then held a dead credential.
The writer is line-based (split, match, replace the line, join) for exactly that
reason, and the first test FAILS if anyone rewrites it as a regex — not just
"today's regex happens to work", because the next value someone picks may not.

The origin tests pin the other half of the same morning: the script must follow
`webapp.host/port` instead of a hard-coded port, and it must refuse a loopback bind
(where /api/* ignores session cookies) before it changes anything.

Run:  python3 deploy/tests/test_rotate_credentials.py
      python3 -m unittest discover -s deploy    # both deploy suites (68 tests)
"""

from __future__ import annotations

import ast
import contextlib
import importlib.util
import io
import os
import pathlib
import re
import shutil
import stat
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.normpath(os.path.join(HERE, "..", "rotate-credentials.py"))


def load():
    spec = importlib.util.spec_from_file_location("rotate_credentials", SCRIPT)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


rot = load()

# A hostile-but-legal value: every character class that has broken a credential
# writer somewhere. A dash password is not what finds these bugs.
HOSTILE = (
    r"back\slash \1 \g<0> \n"        # literal backslashes: regex replacement semantics eat them
    + '"double" ' + "'single' "      # quotes: the value must not need quoting to survive
    + "two words "                   # a space must not split or truncate the line
    + "ünïcode 日本語 "              # non-ASCII round-trips, no re-encoding
    + "$dollar &amp%"
)

ORIGINAL = """\
# bridge environment (fixture, no real values)
BIND_ADDRS=<tailscale-ip>:9130,127.0.0.1:9130
KANBAN_BASE_URL=http://127.0.0.1:9119/api/plugins/kanban/
KANBAN_USERNAME=admin
KANBAN_PASSWORD=<old-secret>
MCP_BEARER_TOKEN=<token>
MCP_CLAIM_WORKER=remote-mcp
"""

PASSWORD_LINE = "KANBAN_PASSWORD=<old-secret>"


class EnvWriter(unittest.TestCase):
    """write_env_password: exact round-trip, one line touched, mode 0600."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="rotate-creds-")
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.env = pathlib.Path(self.tmp) / "kanban-mcp.env"
        self.env.write_text(ORIGINAL, encoding="utf-8")
        # Deliberately loose: a passing chmod test must not be a no-op.
        os.chmod(self.env, 0o644)
        self._saved_env_file = rot.ENV_FILE
        rot.ENV_FILE = self.env
        self.addCleanup(setattr, rot, "ENV_FILE", self._saved_env_file)

    def test_hostile_value_round_trips_exactly(self):
        rot.write_env_password(HOSTILE)
        self.assertEqual(rot.env_password(), HOSTILE)
        expected = ORIGINAL.replace(PASSWORD_LINE, "KANBAN_PASSWORD=" + HOSTILE)
        self.assertEqual(self.env.read_text(encoding="utf-8"), expected)

    def test_only_the_password_line_changes(self):
        rot.write_env_password(HOSTILE)
        before = ORIGINAL.splitlines()
        after = self.env.read_text(encoding="utf-8").splitlines()
        self.assertEqual(len(before), len(after), "the writer changed the line count")
        changed = [i for i, (a, b) in enumerate(zip(before, after)) if a != b]
        self.assertEqual(len(changed), 1, f"more than one line changed: {changed}")
        self.assertTrue(after[changed[0]].startswith("KANBAN_PASSWORD="))

    def test_mode_is_0600_after_the_write(self):
        rot.write_env_password(HOSTILE)
        self.assertEqual(stat.S_IMODE(self.env.stat().st_mode), 0o600)

    def test_no_credential_is_printed(self):
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf), contextlib.redirect_stderr(buf):
            rot.write_env_password(HOSTILE)
            printed = rot.fingerprint(HOSTILE)
        out = buf.getvalue()
        self.assertNotIn(HOSTILE, out)
        self.assertNotIn("back", out)  # any fragment of the value, not just the whole
        self.assertNotIn(HOSTILE, printed)
        self.assertIn(str(len(HOSTILE)), printed)
        self.assertIn("sha256:", printed)

    def test_refuses_zero_or_two_password_lines(self):
        noise = io.StringIO()
        self.env.write_text(ORIGINAL.replace(PASSWORD_LINE + "\n", ""), encoding="utf-8")
        with contextlib.redirect_stderr(noise):
            with self.assertRaises(SystemExit):
                rot.write_env_password(HOSTILE)
        self.env.write_text(ORIGINAL + PASSWORD_LINE + "\n", encoding="utf-8")
        with contextlib.redirect_stderr(noise):
            with self.assertRaises(SystemExit):
                rot.write_env_password(HOSTILE)
        # A failed write must not have touched the file.
        self.assertEqual(self.env.read_text(encoding="utf-8"),
                         ORIGINAL + PASSWORD_LINE + "\n")

    def test_writer_is_line_based_not_a_regex(self):
        """The mechanical half of the pin: no regex substitution in the writer.

        The payload test below proves a regex writer would corrupt THIS value; this
        test refuses the rewrite outright, because a future value may be benign
        enough that the corruption goes unnoticed until it is live.
        """
        tree = ast.parse(pathlib.Path(SCRIPT).read_text(encoding="utf-8"))
        writers = [n for n in ast.walk(tree)
                   if isinstance(n, ast.FunctionDef) and n.name == "write_env_password"]
        self.assertEqual(len(writers), 1, "write_env_password is gone or duplicated")
        forbidden = {("re", "sub"), ("re", "subn"), ("re", "compile"), ("re", "match")}
        called = set()
        for call in (n for n in ast.walk(writers[0]) if isinstance(n, ast.Call)):
            func = call.func
            if isinstance(func, ast.Attribute) and isinstance(func.value, ast.Name):
                called.add((func.value.id, func.attr))
        hits = forbidden & called
        self.assertFalse(
            hits,
            "write_env_password calls a regex API " + repr(sorted(hits))
            + ": a regex replacement treats the password as a template (\\1, \\g<0>, "
              "escapes), which is the 2026-11-24 regression. Replace the line positionally.",
        )

    def test_the_payload_discriminates_a_regex_writer(self):
        """Negative control: a regex writer must actually fail on this payload.

        Without this, `test_hostile_value_round_trips_exactly` could be decorative —
        passing because the payload happens to be harmless rather than because the
        writer is line-based. If this stops failing, harden HOSTILE.
        """

        def regex_writer(path, value):
            text = path.read_text(encoding="utf-8")
            path.write_text(
                re.sub(r"(?m)^KANBAN_PASSWORD=.*$", f"KANBAN_PASSWORD={value}", text, count=1),
                encoding="utf-8",
            )

        try:
            regex_writer(self.env, HOSTILE)
        except re.error:
            return  # the template expansion blew up: the payload discriminates
        self.assertNotEqual(
            rot.env_password(), HOSTILE,
            "a regex writer round-tripped the hostile payload unchanged — the fixture "
            "no longer proves the writer is line-based; add a backslash escape a regex "
            "replacement treats specially (\\1, \\g<0>, \\s) or a real quote",
        )


class OriginResolution(unittest.TestCase):
    """webapp.host/port drives the origin, and a loopback bind is the trap."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="rotate-config-")
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.config = pathlib.Path(self.tmp) / "config.yaml"

    def write(self, block):
        self.config.write_text(
            "model: something\n"
            "dashboard:\n"
            "  theme: default\n"
            + block
            + "update:\n  channel: main\n",
            encoding="utf-8",
        )

    def test_reads_the_gateway_webapp_block(self):
        self.write("webapp:\n  enabled: true\n  host: 100.126.212.105\n  port: 9119\n")
        webapp = rot.webapp_bind(self.config)
        self.assertTrue(webapp.enabled)
        self.assertEqual((webapp.host, webapp.port), ("100.126.212.105", 9119))
        self.assertEqual(webapp.origin, "http://100.126.212.105:9119")
        self.assertFalse(webapp.gate_off, "a tailnet bind engages the sign-in gate")

    def test_loopback_bind_leaves_the_gate_off(self):
        self.write("webapp:\n  enabled: true\n  host: 127.0.0.1\n  port: 9120\n")
        webapp = rot.webapp_bind(self.config)
        self.assertTrue(webapp.gate_off)
        self.assertEqual(webapp.origin, "http://127.0.0.1:9120")

    def test_public_url_engages_the_gate_on_a_loopback_bind(self):
        self.write("webapp:\n  enabled: true\n  host: 127.0.0.1\n  port: 9120\n"
                   "  public_url: https://board.example.org\n")
        webapp = rot.webapp_bind(self.config)
        self.assertFalse(webapp.gate_off,
                         "a reverse proxy on a loopback bind keeps the gate on")

    def test_missing_block_reads_as_off_with_the_upstream_defaults(self):
        self.write("")
        webapp = rot.webapp_bind(self.config)
        self.assertFalse(webapp.enabled)
        self.assertEqual((webapp.host, webapp.port), ("127.0.0.1", 9120))

    def test_quoted_and_commented_values_parse(self):
        self.write("webapp:\n  enabled: true   # the gateway's own listener\n"
                   "  host: '100.126.212.105'\n  port: 9119\n")
        webapp = rot.webapp_bind(self.config)
        self.assertEqual((webapp.host, webapp.port), ("100.126.212.105", 9119))


class BridgeTarget(unittest.TestCase):
    """KANBAN_BASE_URL decides the URL verified, and a mismatch with webapp.* is refused.

    Review F2, 2026-11-24: the script used to verify `webapp.host:port`, not the URL
    the started bridge dials. A moved `webapp.port` with a stale env then verified
    green on the new listener while the bridge crash-looped on the old URL.
    """

    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="rotate-target-")
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.env = pathlib.Path(self.tmp) / "kanban-mcp.env"
        self.env.write_text(ORIGINAL, encoding="utf-8")
        self._saved_env_file = rot.ENV_FILE
        setattr(rot, "ENV_FILE", self.env)
        self.addCleanup(setattr, rot, "ENV_FILE", self._saved_env_file)

    def test_origin_is_derived_from_the_base_url(self):
        self.assertEqual(rot.bridge_origin("http://127.0.0.1:9119/api/plugins/kanban/"),
                         "http://127.0.0.1:9119")
        self.assertEqual(rot.bridge_origin("https://board.example.org/api/plugins/kanban/"),
                         "https://board.example.org")

    def test_a_stale_env_port_is_a_problem(self):
        webapp = rot.Webapp(True, "100.126.212.105", 9119, "")
        problem = rot.bridge_target_problem(webapp, "http://127.0.0.1:9120")
        self.assertIn("9120", problem)
        self.assertIn("9119", problem)

    def test_the_live_host_shape_is_accepted(self):
        # This host: the env dials loopback:9119 while webapp.host is the tailnet
        # address on the same port -- the listener's loopback companion socket.
        webapp = rot.Webapp(True, "100.126.212.105", 9119, "")
        self.assertEqual(rot.bridge_target_problem(webapp, "http://127.0.0.1:9119"), "")

    def test_a_different_non_loopback_host_is_a_problem(self):
        webapp = rot.Webapp(True, "100.126.212.105", 9119, "")
        self.assertNotEqual(rot.bridge_target_problem(webapp, "http://100.9.9.9:9119"), "")

    def test_a_public_url_is_accepted_behind_a_proxy(self):
        webapp = rot.Webapp(True, "127.0.0.1", 9120, "https://board.example.org")
        self.assertEqual(rot.bridge_target_problem(webapp, "https://board.example.org"), "")

    def test_bind_addrs_come_from_the_env_file_not_a_constant(self):
        """Review F5: the post-start probe must use BIND_ADDRS, not a hard-coded port."""
        self.assertEqual(rot.bridge_bind_addrs(),
                         (("<tailscale-ip>", 9130), ("127.0.0.1", 9130)))

    def test_bind_addrs_parses_ipv6(self):
        self.assertEqual(rot.parse_hostport("[::1]:9130"), ("::1", 9130))


class MainFlow(unittest.TestCase):
    """The step order, driven through main() with every side effect stubbed.

    Review F2/F3 of 2026-11-24 were order properties: what URL is verified, whether
    the bridge is already started when it is verified, and what happens when the
    started bridge never comes up. Each is asserted on the recorded calls, so a
    reorder fails here rather than at 03:00 on a live host.
    """

    CONFIG = "webapp:\n  enabled: true\n  host: 100.126.212.105\n  port: 9119\n"

    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="rotate-main-")
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.env = pathlib.Path(self.tmp) / "kanban-mcp.env"
        self.env.write_text(ORIGINAL, encoding="utf-8")
        os.chmod(self.env, 0o600)
        self.config = pathlib.Path(self.tmp) / "config.yaml"
        self.config.write_text(self.CONFIG, encoding="utf-8")
        self.commands: list[tuple[str, ...]] = []
        self._patch("ENV_FILE", self.env)
        self._patch("CONFIG_FILE", self.config)
        self._patch("OPERATOR_COPY", pathlib.Path(self.tmp) / "operator-password")
        self._patch("sudo_systemctl", lambda *a: self.commands.append(("sudo",) + a))
        self._patch("systemctl_user", lambda *a: self.commands.append(("user",) + a))
        self._patch("run", lambda cmd, **kw: self.commands.append(tuple(cmd)))
        self._patch("hermes_bin", lambda: "/fake/hermes")
        self._patch("origin_ready", lambda origin: True)
        self._patch("verify_login", lambda password, origin, **kw: self.commands.append(("verify", origin)))
        self._patch("bridge_healthz", lambda host, port: True)
        self._patch("bridge_active", lambda: "active")

    def _patch(self, name, value):
        self.addCleanup(setattr, rot, name, getattr(rot, name))
        setattr(rot, name, value)

    def run_main(self, *argv):
        saved = rot.sys.argv
        rot.sys.argv = ["rotate-credentials.py", *argv]
        self.addCleanup(setattr, rot.sys, "argv", saved)
        buf = io.StringIO()
        try:
            with contextlib.redirect_stdout(buf), contextlib.redirect_stderr(buf):
                rc = rot.main()
        except SystemExit as exc:
            rc = exc.code
        return rc, buf.getvalue()

    def test_verification_precedes_the_bridge_start(self):
        rc, out = self.run_main()
        self.assertEqual(rc, 0, out)
        verify = next(i for i, c in enumerate(self.commands) if c[0] == "verify")
        start = self.commands.index(("sudo", "start", "kanban-mcp"))
        self.assertLess(verify, start, "the credential was verified only after the bridge started")
        self.assertEqual(self.commands[verify], ("verify", "http://127.0.0.1:9119"),
                         "verified something other than the URL KANBAN_BASE_URL gives the bridge")

    def test_refuses_a_stale_env_port_before_changing_anything(self):
        self.env.write_text(ORIGINAL.replace(":9119/api", ":9120/api"), encoding="utf-8")
        before = self.env.read_bytes()
        rc, out = self.run_main()
        self.assertEqual(rc, 1)
        self.assertEqual(self.commands, [], "a refused run must not touch the host")
        self.assertEqual(self.env.read_bytes(), before)
        self.assertIn("webapp.port is 9119", out)

    def test_loopback_bind_refuses_before_changing_anything(self):
        self.config.write_text("webapp:\n  enabled: true\n  host: 127.0.0.1\n  port: 9119\n",
                               encoding="utf-8")
        before = self.env.read_bytes()
        rc, out = self.run_main()
        self.assertEqual(rc, 1)
        self.assertEqual(self.commands, [])
        self.assertEqual(self.env.read_bytes(), before)
        self.assertIn("loopback bind", out)

    def test_a_bridge_that_does_not_come_up_is_stopped_again(self):
        self._patch("bridge_active", lambda: "failed")
        rc, out = self.run_main()
        self.assertEqual(rc, 1, out)
        self.assertEqual(self.commands[1:], [
            ("/fake/hermes", "dashboard", "password", "--password-stdin"),
            ("user", "restart", "hermes-gateway"),
            ("verify", "http://127.0.0.1:9119"),
            ("sudo", "start", "kanban-mcp"),
            ("sudo", "stop", "kanban-mcp"),
        ], "a bridge that did not come up was left running")


if __name__ == "__main__":
    unittest.main(verbosity=2)
