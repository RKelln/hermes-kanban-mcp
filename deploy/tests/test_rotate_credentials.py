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
      python3 -m unittest discover -s deploy    # both deploy suites (57 tests)
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


if __name__ == "__main__":
    unittest.main(verbosity=2)
