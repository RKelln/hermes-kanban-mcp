#!/usr/bin/env python3
"""Tests for deploy/upgrade.py.

Focused on the checks that must never regress: the placeholder detector (the
one that would have prevented the 2026-09-14 outage), the version parser (the
one that catches "I deployed it" vs "what is running is what I built"), and the
env-key reader.

Run:  python3 deploy/tests/test_upgrade.py
"""

from __future__ import annotations

import importlib.util
import os
import sqlite3  # noqa: F401  (kept out of the way; no DB touched here)
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.normpath(os.path.join(HERE, "..", "upgrade.py"))
REPO_UNIT = os.path.normpath(os.path.join(HERE, "..", "kanban-mcp.service"))


def load():
    spec = importlib.util.spec_from_file_location("upgrade", SCRIPT)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


up = load()


class TestPlaceholderDetector(unittest.TestCase):
    """The check that stops an upgrade from restarting into 217/USER."""

    def test_a_live_directive_is_caught(self):
        self.assertEqual(up.unit_placeholder_lines("User=__SERVICE_USER__"),
                         ["User=__SERVICE_USER__"])

    def test_comments_are_not_treated_as_live(self):
        # The repo's unit documents the placeholder in COMMENTS on purpose;
        # flagging those would block every legitimate upgrade.
        text = (
            "# is possible once __SERVICE_USER__ is known: keep ProtectHome=read-only\n"
            "# __SERVICE_USER__ placeholder: MUST be the account that can execute hermes\n"
            "User=experimance\n"
        )
        self.assertEqual(up.unit_placeholder_lines(text), [])

    def test_a_group_or_path_placeholder_is_also_caught(self):
        self.assertEqual(up.unit_placeholder_lines("ReadWritePaths=/home/__SERVICE_USER__/.hermes"),
                         ["ReadWritePaths=/home/__SERVICE_USER__/.hermes"])

    def test_empty_and_missing_input_is_safe(self):
        for text in ("", None, "# nothing here\n"):
            self.assertEqual(up.unit_placeholder_lines(text), [])

    def test_the_repo_unit_really_does_carry_the_trap(self):
        """Pin the actual artifact: this is why the script refuses.

        If someone substitutes the placeholder in the repo copy, this test
        fails and the docstring's warning should be revisited — that is
        intended, not incidental.
        """
        text = open(REPO_UNIT, encoding="utf-8").read()
        live = up.unit_placeholder_lines(text)
        self.assertEqual(live, ["User=__SERVICE_USER__"],
                         "the repo unit is expected to ship exactly one live "
                         "placeholder line; found %r" % (live,))

    def test_a_substituted_unit_passes(self):
        text = open(REPO_UNIT, encoding="utf-8").read().replace(
            "User=__SERVICE_USER__", "User=experimance")
        self.assertEqual(up.unit_placeholder_lines(text), [])


class TestVersionParser(unittest.TestCase):
    """What is ACTUALLY running, read back from the journal."""

    ONE = ('{"time":"2026-09-14T12:07:52Z","level":"INFO","msg":"startup complete",'
           '"version":"bed7889-bed7889","config":"..."}')

    def test_a_startup_line_yields_its_version(self):
        self.assertEqual(up.parse_version_line(self.ONE), "bed7889-bed7889")

    def test_the_LAST_startup_line_wins(self):
        older = self.ONE.replace("bed7889-bed7889", "aaa1111-aaa1111")
        self.assertEqual(up.parse_version_line(older + "\n" + self.ONE),
                         "bed7889-bed7889")

    def test_no_version_line_is_none_not_a_guess(self):
        for text in ("", "journal noise without a version", '"msg":"listening"'):
            self.assertIsNone(up.parse_version_line(text))


class TestEnvKeys(unittest.TestCase):
    def test_keys_are_read_without_values(self):
        text = "KANBAN_USERNAME=admin\nMCP_BEARER_TOKEN=secret\n\n# comment\nBIND_ADDRS=x\n"
        self.assertEqual(up.env_keys(text), {"KANBAN_USERNAME", "MCP_BEARER_TOKEN", "BIND_ADDRS"})

    def test_comments_and_bare_words_are_ignored(self):
        self.assertEqual(up.env_keys("# A=B\nnotakey\n"), set())

    def test_empty_input(self):
        self.assertEqual(up.env_keys(""), set())
        self.assertEqual(up.env_keys(None), set())


class TestVersionStamp(unittest.TestCase):
    def test_describe_and_sha_are_both_kept(self):
        # The stamp must name the commit, because that is what a reviewer and a
        # journal reader compare against.
        self.assertEqual(up.stamp("v1.2.0", "abc1234"), "v1.2.0-abc1234")

    def test_untagged_repo_falls_back_to_the_sha_exactly_once(self):
        self.assertEqual(up.stamp("abc1234", "abc1234"), "abc1234-abc1234")
        self.assertEqual(up.stamp("  ", "abc1234"), "abc1234-abc1234")


class TestContainsCommit(unittest.TestCase):
    """`--expect` is what makes the intended commit explicit.

    The failure it exists for: a deploy landed a build predating the fix,
    because nothing anywhere compared "the SHA I meant to ship" with "the SHA
    that got built".
    """

    def setUp(self):
        import subprocess
        import tempfile
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.repo = self.tmp.name
        g = lambda *a: subprocess.run(["git", "-C", self.repo, *a], capture_output=True, text=True)
        g("init", "-q")
        g("config", "user.email", "t@example.invalid")
        g("config", "user.name", "test")
        g("commit", "-q", "--allow-empty", "-m", "one")
        self.first = g("rev-parse", "--short", "HEAD").stdout.strip()
        g("commit", "-q", "--allow-empty", "-m", "two")
        self.second = g("rev-parse", "--short", "HEAD").stdout.strip()

    def test_equal_commits_pass(self):
        self.assertTrue(up.contains_commit(self.second, self.second, cwd=self.repo))

    def test_an_ancestor_passes(self):
        self.assertTrue(up.contains_commit(self.first, self.second, cwd=self.repo))

    def test_a_commit_the_head_lacks_is_refused(self):
        # HEAD is older: deploying it would land a build without the newer fix.
        self.assertFalse(up.contains_commit(self.second, self.first, cwd=self.repo))

    def test_an_unknown_ref_is_not_silently_true(self):
        self.assertFalse(up.contains_commit("deadbee", self.second, cwd=self.repo))


if __name__ == "__main__":
    unittest.main(verbosity=2)
