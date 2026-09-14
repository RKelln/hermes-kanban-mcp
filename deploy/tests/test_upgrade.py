#!/usr/bin/env python3
"""Tests for deploy/upgrade.py.

Focused on the checks that must never regress: the placeholder detector (the one
that would have prevented the 2026-09-14 outage), the version parser and its
retry (the ones behind "I deployed it" vs "what is running is what I built"),
the env-key reader, the rollback line (which must never offer a binary that does
not exist), and the repo-wide secret-hygiene scan from scripts/smoke.sh — which
deploy/upgrade.py runs as its OWN final verification step, so a source line that
trips it turns every upgrade into a reported failure.

Run:  python3 deploy/tests/test_upgrade.py
"""

from __future__ import annotations

import contextlib
import importlib.util
import io
import os
import re
import shutil
import subprocess
import tempfile
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.normpath(os.path.join(HERE, "..", "upgrade.py"))
REPO_ROOT = os.path.normpath(os.path.join(HERE, "..", ".."))
REPO_UNIT = os.path.normpath(os.path.join(HERE, "..", "kanban-mcp.service"))


def load():
    spec = importlib.util.spec_from_file_location("upgrade", SCRIPT)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


up = load()


# --- the repo-wide secret-hygiene scan (scripts/smoke.sh:207-208) -------------
# Composed from the module's key constants rather than written as a literal:
# a literal NAME=[value] here would itself be a line the scan reports, which is
# exactly the defect these tests exist to pin.
HYGIENE_PATTERN = "(%s|%s)=[^ ]" % (up.TOKEN_KEY, up.PASSWORD_KEY)
HYGIENE_EXCLUDE = r"=<[^>]+>|=[.]{3}"


def hygiene_hits(cwd: str) -> list[str]:
    """The lines that survive the scan scripts/smoke.sh runs over the repo."""
    p = subprocess.run(["git", "grep", "-nE", HYGIENE_PATTERN, "--", ":!*.example"],
                       cwd=cwd, capture_output=True, text=True)
    if p.returncode not in (0, 1):
        raise unittest.SkipTest("git grep unusable here (rc=%d): %s"
                                % (p.returncode, p.stderr.strip()))
    return [line for line in p.stdout.splitlines()
            if not re.search(HYGIENE_EXCLUDE, line)]


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
        with open(REPO_UNIT, encoding="utf-8") as fh:
            text = fh.read()
        live = up.unit_placeholder_lines(text)
        self.assertEqual(live, ["User=__SERVICE_USER__"],
                         "the repo unit is expected to ship exactly one live "
                         "placeholder line; found %r" % (live,))

    def test_a_substituted_unit_passes(self):
        with open(REPO_UNIT, encoding="utf-8") as fh:
            text = fh.read()
        text = text.replace("User=__SERVICE_USER__", "User=experimance")
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
        for text in ("", None, "journal noise without a version", '"msg":"listening"'):
            self.assertIsNone(up.parse_version_line(text))


class TestRunningVersionVerification(unittest.TestCase):
    """The claim that matters, and the retry that keeps it from lying."""

    WANT = "abc1234-abc1234"
    LINE = '{"msg":"startup complete","version":"%s"}'

    def test_the_startup_line_is_waited_for(self):
        # A restart logs its version a moment later; one read would fail a good
        # deploy, which is the same class of wrong verdict as passing a stale one.
        seq = [None, self.LINE % self.WANT]

        def journal():
            return seq.pop(0) if seq else None

        with contextlib.redirect_stdout(io.StringIO()):
            up.wait_for_running_version(self.WANT, "since", journal=journal, timeout_s=5)

    def test_the_default_path_actually_reads_the_journal(self):
        """The retry's journal read must hand TEXT to the parser.

        A seam that returned an already-parsed value would make every version
        check read None — the script's central claim, silently always wrong.
        """
        real_run = up.run

        def fake(cmd, **kw):
            if list(cmd)[0] == "journalctl":
                return subprocess.CompletedProcess(list(cmd), 0, self.LINE % self.WANT, "")
            return real_run(cmd, **kw)

        up.run = fake
        self.addCleanup(lambda: setattr(up, "run", real_run))
        with contextlib.redirect_stdout(io.StringIO()):
            up.wait_for_running_version(self.WANT, "since", timeout_s=0)

    def test_a_version_line_that_is_wrong_fails_immediately(self):
        start = time.monotonic()
        with contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaises(up.Fail) as caught:
                up.wait_for_running_version(self.WANT, "since",
                                            journal=lambda: self.LINE % "bed7889-bed7889",
                                            timeout_s=30)
        self.assertLess(time.monotonic() - start, 5,
                        "a WRONG version is not a slow start — it must not wait out the window")
        self.assertIn("reports version 'bed7889-bed7889'", str(caught.exception))

    def test_no_version_line_at_all_fails_after_the_window(self):
        with contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaises(up.Fail) as caught:
                up.wait_for_running_version(self.WANT, "since", journal=lambda: "", timeout_s=0)
        self.assertIn("reports version None", str(caught.exception))


class TestHealthRetry(unittest.TestCase):
    def test_a_slow_bind_is_not_a_failed_deploy(self):
        answers = ["kanban-mcp.service is activating; /healthz said ''", "ok"]
        calls = []

        def probe():
            calls.append(1)
            return answers.pop(0)

        with contextlib.redirect_stdout(io.StringIO()):
            up.wait_for_health(probe=probe, timeout_s=5)
        self.assertEqual(len(calls), 2)

    def test_a_service_that_never_answers_fails(self):
        with self.assertRaises(up.Fail) as caught:
            up.wait_for_health(probe=lambda: "kanban-mcp.service is failed; /healthz said ''",
                               timeout_s=0)
        self.assertIn("not healthy", str(caught.exception))


class TestEnvKeys(unittest.TestCase):
    def test_keys_are_read_without_values(self):
        text = "KANBAN_USERNAME=admin\n%s=%s\n\n# comment\nBIND_ADDRS=x\n" % (
            up.TOKEN_KEY, "some-value")
        self.assertEqual(up.env_keys(text),
                         {"KANBAN_USERNAME", up.TOKEN_KEY, "BIND_ADDRS"})

    def test_comments_and_bare_words_are_ignored(self):
        self.assertEqual(up.env_keys("# A=B\nnotakey\n"), set())

    def test_empty_input(self):
        self.assertEqual(up.env_keys(""), set())
        self.assertEqual(up.env_keys(None), set())


class TestSmokeToken(unittest.TestCase):
    def test_the_value_is_read(self):
        self.assertEqual(up.smoke_token("%s=%s\n" % (up.TOKEN_KEY, "0123456789abcdef")),
                         "0123456789abcdef")

    def test_a_missing_or_empty_key_is_a_failure_not_an_empty_token(self):
        # An empty token would make smoke.sh fail with a confusing 401 instead
        # of naming what is missing.
        for text in ("", None, "KANBAN_USERNAME=admin\n", "%s=\n" % up.TOKEN_KEY):
            with self.assertRaises(up.Fail):
                up.smoke_token(text)


class TestVersionStamp(unittest.TestCase):
    def test_describe_and_sha_are_both_kept(self):
        # The stamp must name the commit, because that is what a reviewer and a
        # journal reader compare against.
        self.assertEqual(up.stamp("v1.2.0", "abc1234"), "v1.2.0-abc1234")

    def test_untagged_repo_falls_back_to_the_sha_exactly_once(self):
        self.assertEqual(up.stamp("abc1234", "abc1234"), "abc1234-abc1234")
        self.assertEqual(up.stamp("  ", "abc1234"), "abc1234-abc1234")

    def test_a_dirty_deploy_is_labeled_and_distinguishable(self):
        # --allow-dirty ships unreviewed code; it must not be journal-identical
        # to a clean build of the same commit.
        dirty = up.stamp("abc1234", "abc1234", dirty=True)
        self.assertEqual(dirty, "abc1234-abc1234-dirty")
        self.assertNotEqual(dirty, up.stamp("abc1234", "abc1234"))


class TestRollbackLine(unittest.TestCase):
    """AC6: printed in both paths, and it must name a binary that EXISTS."""

    def test_a_real_backup_is_named(self):
        with tempfile.TemporaryDirectory() as tmp:
            bak = os.path.join(tmp, "kanban-mcp.bak-20260914-133000")
            with open(bak, "w", encoding="utf-8") as fh:
                fh.write("old binary\n")
            line = up.rollback_line(bak)
        self.assertIn(bak, line)
        self.assertIn("sudo install -m 0755", line)
        self.assertIn("systemctl restart", line)

    def test_no_backup_says_so_and_never_offers_a_self_copy(self):
        line = up.rollback_line(None)
        self.assertIn("no previous binary", line)
        self.assertNotIn("sudo install", line,
                         "a rollback that reinstalls the binary we just installed "
                         "(or a path that does not exist) fails when it is run")

    def test_a_backup_path_that_vanished_is_not_offered(self):
        self.assertIn("no previous binary",
                      up.rollback_line("/nonexistent/kanban-mcp.bak-20260914"))


class TestContainsCommit(unittest.TestCase):
    """`--expect` is what makes the intended commit explicit.

    The failure it exists for: a deploy landed a build predating the fix,
    because nothing anywhere compared "the SHA I meant to ship" with "the SHA
    that got built".
    """

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.repo = self.tmp.name
        g = lambda *a: subprocess.run(["git", "-C", self.repo, *a],
                                      capture_output=True, text=True)
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


class TestNoBuildRefFlag(unittest.TestCase):
    """`go build` compiles the WORKING TREE, and nothing else.

    A flag that pointed the version stamp or the `--expect` guard at a DIFFERENT
    commit would describe an artifact that does not exist: the label would name
    a commit the build never read. No flag is better than a lying one, so the
    only refs this script knows are HEAD (what it builds) and --expect (what it
    meant to build).
    """

    def test_sha_is_not_a_flag(self):
        with contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit):
                up.main(["--dry-run", "--sha", "deadbeef"])


@unittest.skipUnless(shutil.which("git"), "git is required")
class TestSecretHygieneScan(unittest.TestCase):
    """deploy/upgrade.py RUNS scripts/smoke.sh, so the repo must pass its scan.

    A source line that survives this pipeline makes the smoke step fail, which
    makes every upgrade report FAILED after it has already installed and
    restarted — the worst possible moment to be told the deploy is bad.
    """

    def test_the_scan_is_the_one_smoke_sh_runs(self):
        with open(os.path.join(REPO_ROOT, "scripts", "smoke.sh"), encoding="utf-8") as fh:
            smoke = fh.read()
        self.assertIn("git grep -nE", smoke)
        self.assertIn(HYGIENE_EXCLUDE, smoke,
                      "the exclude pattern must stay in step with scripts/smoke.sh")

    def test_the_pipeline_is_not_vacuous(self):
        with tempfile.TemporaryDirectory() as tmp:
            subprocess.run(["git", "init", "-q"], cwd=tmp, capture_output=True)
            planted = os.path.join(tmp, "planted.py")
            with open(planted, "w", encoding="utf-8") as fh:
                fh.write("%s=%s\n" % (up.TOKEN_KEY, "not-a-real-secret"))
            subprocess.run(["git", "add", "planted.py"], cwd=tmp, capture_output=True)
            self.assertEqual(len(hygiene_hits(tmp)), 1,
                             "the pipeline must catch a planted value, or this "
                             "test proves nothing")

    def test_placeholder_forms_are_still_excluded(self):
        with tempfile.TemporaryDirectory() as tmp:
            subprocess.run(["git", "init", "-q"], cwd=tmp, capture_output=True)
            doc = os.path.join(tmp, "README.md")
            with open(doc, "w", encoding="utf-8") as fh:
                fh.write("%s=%s\n" % (up.TOKEN_KEY, "<token>"))
                fh.write("%s=%s\n" % (up.PASSWORD_KEY, "..."))
            subprocess.run(["git", "add", "README.md"], cwd=tmp, capture_output=True)
            self.assertEqual(hygiene_hits(tmp), [])

    def test_the_repo_passes(self):
        hits = hygiene_hits(REPO_ROOT)
        self.assertEqual(hits, [],
                         "these lines trip scripts/smoke.sh's hygiene scan — the "
                         "final verification step of deploy/upgrade.py itself:\n  "
                         + "\n  ".join(hits))


if __name__ == "__main__":
    unittest.main(verbosity=2)
