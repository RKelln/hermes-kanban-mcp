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

import argparse
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
from unittest import mock

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


# --- the repo-wide secret-hygiene scan (scripts/smoke.sh) ---------------------
# The pattern is READ FROM THE SCRIPT rather than re-typed here. Two copies of a
# regex are two chances to drift, and the drift is silent in the direction that
# matters: this suite stays green while the deploy gate turns red.
SMOKE_SH = os.path.join(REPO_ROOT, "scripts", "smoke.sh")


def scan_patterns() -> tuple[str, str]:
    """(pattern, exclude) exactly as scripts/smoke.sh defines them.

    The pattern is double-quoted in the script because it contains a double quote
    itself (`[\"']?` — a value's quote delimiter); the exclude is single-quoted.
    So this reads either shell word and undoes exactly the escapes bash undoes
    inside double quotes. Nothing else is unescaped: a regex `\\d` survives,
    which is what bash would hand git grep too.
    """
    with open(SMOKE_SH, encoding="utf-8") as fh:
        src = fh.read()
    found = {}
    for key in ("SECRET_SCAN_RE", "SECRET_SCAN_EXCLUDE"):
        m = re.search(r"^%s='([^']*)'$" % key, src, re.MULTILINE)
        if m:
            found[key] = m.group(1)
            continue
        m = re.search(r'^%s="((?:[^"\\]|\\.)*)"$' % key, src, re.MULTILINE)
        if not m:
            raise RuntimeError(
                "scripts/smoke.sh no longer defines %s as a quoted assignment: the "
                "hygiene scan and this suite must share ONE pattern, or the gate "
                "proves nothing" % key)
        found[key] = re.sub(r'\\([$`"\\])', r'\1', m.group(1))
    return found["SECRET_SCAN_RE"], found["SECRET_SCAN_EXCLUDE"]


HYGIENE_PATTERN, HYGIENE_EXCLUDE = scan_patterns()

# The command smoke.sh runs, so the suite measures the SAME scan the deploy
# runs. `git grep` rc is 0 for hits, 1 for no hits, and >= 2 when the scan
# itself is broken.
GIT_GREP_ARGV = ("git", "grep", "-nE")


def hygiene_hits(cwd: str, pattern: str | None = None,
                 exclude: str | None = None) -> list[str]:
    """The lines that survive the scan scripts/smoke.sh runs over the repo.

    Raises AssertionError (a test FAILURE, not a skip) when `git grep` refuses
    to run: an unusable pattern matches nothing, so "no hits" over a repo
    holding a credential would otherwise read as a pass. Git's absence is
    already handled by the class decorator; anything else is a real defect.
    """
    pattern = HYGIENE_PATTERN if pattern is None else pattern
    exclude = HYGIENE_EXCLUDE if exclude is None else exclude
    p = subprocess.run([*GIT_GREP_ARGV, pattern, "--", ":!*.example"],
                       cwd=cwd, capture_output=True, text=True)
    if p.returncode not in (0, 1):
        raise AssertionError(
            "the hygiene scan could not run (git grep rc=%d), so it proves nothing — "
            "the scan pattern is unusable: %r\n%s"
            % (p.returncode, pattern, p.stderr.strip()))
    return [line for line in p.stdout.splitlines()
            if not re.search(exclude, line)]


# --- the same scan as scripts/smoke.sh runs it ---------------------------------
# Lifted from the script at TEST time, so the shell half of the gate is measured
# as shipped instead of re-typed here. The section runs to the end of the file.
SCAN_SECTION_MARKER = "# --- secret hygiene: repository scan"


def smoke_scan_block(pattern: str | None = None) -> str:
    """scripts/smoke.sh's repository scan, optionally with a substituted pattern."""
    with open(SMOKE_SH, encoding="utf-8") as fh:
        src = fh.read()
    block = src[src.index(SCAN_SECTION_MARKER):]
    if pattern is not None:
        escaped = (pattern.replace("\\", "\\\\").replace('"', '\\"')
                   .replace("$", "\\$").replace("`", "\\`"))
        block = re.sub(r"^SECRET_SCAN_RE=.*$",
                       lambda _m: 'SECRET_SCAN_RE="%s"' % escaped,
                       block, count=1, flags=re.MULTILINE)
    return block


def run_smoke_scan_block(cwd: str, pattern: str | None = None) -> subprocess.CompletedProcess:
    """Run that block the way smoke.sh does: same shell options, same fail()."""
    harness = ("#!/usr/bin/env bash\nset -euo pipefail\n"
               'fail() { echo "FAIL: $*" >&2; exit 1; }\n'
               + smoke_scan_block(pattern))
    return subprocess.run(["bash", "-s"], input=harness, cwd=cwd,
                          capture_output=True, text=True)


@contextlib.contextmanager
def committed_repo(name: str, text: str):
    """A throwaway git repo holding one tracked file."""
    with tempfile.TemporaryDirectory() as tmp:
        subprocess.run(["git", "init", "-q"], cwd=tmp, capture_output=True)
        with open(os.path.join(tmp, name), "w", encoding="utf-8") as fh:
            fh.write(text)
        subprocess.run(["git", "add", name], cwd=tmp, capture_output=True)
        yield tmp


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
    """AC6: printed in both paths, and it must name a binary that EXISTS.

    Three states, three truths. The line must not confuse "no backup was taken"
    with "no binary was there": the first is a failure before the install (the
    host is untouched), the second is a first install with nothing to restore.
    """

    def test_a_real_backup_is_named(self):
        with tempfile.TemporaryDirectory() as tmp:
            bak = os.path.join(tmp, "kanban-mcp.bak-20260914-133000")
            with open(bak, "w", encoding="utf-8") as fh:
                fh.write("old binary\n")
            line = up.rollback_line(bak, installed=True)
        self.assertIn(bak, line)
        self.assertIn("sudo install -m 0755", line)
        self.assertIn("systemctl restart", line)

    def test_a_failure_before_the_install_never_claims_the_binary_was_absent(self):
        line = up.rollback_line(None, installed=False)
        self.assertIn("nothing to undo", line)
        self.assertIn("untouched", line)
        self.assertNotIn("no previous binary", line,
                         "a build failure happens on hosts that DO have a binary "
                         "installed; that sentence would be false there")

    def test_an_install_with_no_previous_binary_says_there_is_nothing_to_restore(self):
        line = up.rollback_line(None, installed=True)
        self.assertIn("no previous binary", line)
        self.assertNotIn("sudo install", line,
                         "a rollback that reinstalls the binary we just installed "
                         "(or a path that does not exist) fails when it is run")

    def test_a_backup_path_that_vanished_is_not_offered(self):
        line = up.rollback_line("/nonexistent/kanban-mcp.bak-20260914", installed=True)
        self.assertNotIn("sudo install", line)


class TestSmokeStep(unittest.TestCase):
    """The smoke step's cwd is load-bearing.

    scripts/smoke.sh's secret-hygiene scan needs a git work tree and FAILS if it
    cannot run, so without cwd=<repo> a perfectly good deploy is reported FAILED
    — after it has already installed and restarted.
    """

    TOKEN_VALUE = "0123456789abcdef"

    def _capture(self, rc=0):
        captured = {}
        real_run = up.run

        def fake(cmd, **kw):
            captured["argv"] = list(cmd)
            captured.update(kw)
            return subprocess.CompletedProcess(list(cmd), rc, "smoke step output\n", "")

        up.run = fake
        self.addCleanup(lambda: setattr(up, "run", real_run))
        with contextlib.redirect_stdout(io.StringIO()):
            if rc == 0:
                up.run_smoke("%s=%s\n" % (up.TOKEN_KEY, self.TOKEN_VALUE))
            else:
                with self.assertRaises(up.Fail):
                    up.run_smoke("%s=%s\n" % (up.TOKEN_KEY, self.TOKEN_VALUE))
        return captured

    def test_smoke_runs_with_the_repo_as_cwd(self):
        self.assertEqual(self._capture().get("cwd"), up.REPO)

    def test_the_token_goes_in_the_environment_not_on_the_command_line(self):
        cap = self._capture()
        self.assertEqual(cap.get("env", {}).get(up.TOKEN_KEY), self.TOKEN_VALUE)
        self.assertNotIn(self.TOKEN_VALUE, " ".join(cap["argv"]))

    def test_a_failing_smoke_is_a_failure_not_a_warning(self):
        self._capture(rc=1)


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


class TestExpectRequiresAReviewedHead(unittest.TestCase):
    """t_1243c532: `--expect` is an exactness gate, not a "related work is present" gate.

    Functional on purpose, through `preflight` and the real git plumbing. The
    unit tests around `contains_commit` could not catch this: that function is
    correct at what it claims, and what it was being asked ("does the build
    include the fix") is not the question a deploy gate needs answered ("has a
    merge reviewed the code that is about to be built"). The failure was a clean
    tree on a feature branch, so these tests are stated as tree states.
    """

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.repo = os.path.join(self.tmp.name, "repo")
        os.makedirs(os.path.join(self.repo, "cmd", "kanban-mcp"))
        with open(os.path.join(self.repo, "go.mod"), "w", encoding="utf-8"):
            pass
        self.git("init", "-q")
        self.git("symbolic-ref", "HEAD", "refs/heads/main")
        self.git("config", "user.email", "t@example.invalid")
        self.git("config", "user.name", "test")
        self.first = self.commit("one")
        self.merged = self.commit("two")          # stand-in for the merge commit
        self.git("checkout", "-q", "-b", "fix/unreviewed")
        self.feature = self.commit("three")       # ahead of main, unreviewed
        self.git("checkout", "-q", "main")

    # --- fixture helpers -----------------------------------------------------

    def git(self, *args):
        p = subprocess.run(["git", "-C", self.repo, *args],
                           capture_output=True, text=True)
        if p.returncode != 0:
            raise AssertionError("git %s failed: %s" % (" ".join(args), p.stderr.strip()))
        return p.stdout.strip()

    def commit(self, msg):
        self.git("add", "-A")
        p = subprocess.run(["git", "-C", self.repo, "commit", "-q", "--allow-empty",
                            "-m", msg], capture_output=True, text=True)
        if p.returncode != 0:
            raise AssertionError("git commit failed: %s" % p.stderr.strip())
        return self.git("rev-parse", "--short", "HEAD")

    def preflight(self, expect):
        """Run the real preflight against the fixture repo, with --expect."""
        real_repo, real_read = up.REPO, up.read_installed
        up.REPO = self.repo

        def fake_read_installed(path):
            if path == up.UNIT:
                return "User=experimance\n"
            return "KANBAN_USERNAME=admin\n%s=%s\n%s=%s\n" % (
                up.TOKEN_KEY, "0123456789abcdef", up.PASSWORD_KEY, "not-a-real-secret")

        up.read_installed = fake_read_installed
        self.addCleanup(lambda: setattr(up, "REPO", real_repo))
        self.addCleanup(lambda: setattr(up, "read_installed", real_read))
        with mock.patch("shutil.which", return_value="/usr/bin/go"):
            return up.preflight(argparse.Namespace(expect=expect, allow_dirty=False))

    # --- the three tree states the card names ---------------------------------

    def test_a_branch_main_has_never_seen_is_refused(self):
        self.git("checkout", "-q", "fix/unreviewed")
        with self.assertRaises(up.Fail) as caught:
            self.preflight(self.merged)
        msg = str(caught.exception)
        self.assertIn("does not contain %s" % self.feature, msg)
        self.assertIn("fix/unreviewed", msg,
                      "naming the branch is the whole diagnosis, and the message "
                      "must carry it")
        self.assertIn(self.repo, msg,
                      "the refusal must print a runnable worktree command, or the "
                      "operator is left to invent the workaround")

    def test_a_detached_unmerged_commit_is_refused_and_named_as_such(self):
        # A detached checkout is the SUPPORTED deploy shape, so the refusal must
        # say "detached HEAD at <sha>" rather than invent a branch name — and it
        # must still refuse, because a detached HEAD is not a merge.
        self.git("checkout", "-q", "--detach", self.feature)
        with self.assertRaises(up.Fail) as caught:
            self.preflight(self.feature)
        msg = str(caught.exception)
        self.assertIn("a detached HEAD at %s" % self.feature, msg)
        self.assertIn("does not contain %s" % self.feature, msg)

    def test_the_merge_commit_on_main_passes(self):
        info = self.preflight(self.merged)
        self.assertEqual(info["branch"], "main")
        self.assertEqual(info["sha"], self.merged)

    def test_a_detached_checkout_of_a_commit_on_main_passes(self):
        # The documented deploy shape: a detached worktree at the merge commit,
        # so the shared checkout never has to move.
        self.git("checkout", "-q", "--detach", self.first)
        info = self.preflight(self.first)
        self.assertEqual(info["branch"], "HEAD")
        self.assertEqual(info["sha"], self.first)

    # --- the refusal that was already there, still there ----------------------

    def test_a_detached_commit_that_predates_expect_is_refused(self):
        # The 2026-09-14 defect: HEAD is on main's history but is OLDER than the
        # fix --expect names. This is the half contains_commit keeps.
        self.git("checkout", "-q", "--detach", self.first)
        with self.assertRaises(up.Fail) as caught:
            self.preflight(self.merged)
        self.assertIn("does NOT contain --expect", str(caught.exception))

    # --- negative controls: remove the check, the assertion must fail ---------

    def test_negative_control_the_branch_refusal_is_the_new_gates(self):
        """With the containment check removed the same tree passes.

        Without this, a refusal could be coming from anything else in preflight
        (a dirty tree, a missing unit) and the test above would prove nothing.
        """
        self.git("checkout", "-q", "fix/unreviewed")
        real = up.unreviewed_head
        up.unreviewed_head = lambda *a, **k: None
        self.addCleanup(lambda: setattr(up, "unreviewed_head", real))
        info = self.preflight(self.merged)
        self.assertEqual(info["sha"], self.feature)

    def test_negative_control_the_expect_refusal_is_contains_commit(self):
        self.git("checkout", "-q", "--detach", self.first)
        real = up.contains_commit
        up.contains_commit = lambda *a, **k: True
        self.addCleanup(lambda: setattr(up, "contains_commit", real))
        info = self.preflight(self.merged)
        self.assertEqual(info["sha"], self.first)

    # --- the reviewed branch is looked up, never assumed ----------------------

    def test_origin_main_answers_when_there_is_no_local_main(self):
        self.git("checkout", "-q", "--detach", self.merged)
        self.git("update-ref", "refs/remotes/origin/main", self.merged)
        self.git("branch", "-D", "main")
        self.assertEqual(up.resolve_main_ref(self.repo), "origin/main")
        info = self.preflight(self.merged)
        self.assertEqual(info["sha"], self.merged)

    def test_no_main_branch_at_all_refuses_and_says_which_refs_it_needs(self):
        self.git("checkout", "-q", "--detach", self.merged)
        self.git("branch", "-D", "main")
        with self.assertRaises(up.Fail) as caught:
            self.preflight(self.merged)
        msg = str(caught.exception)
        self.assertIn("neither", msg)
        self.assertIn("origin/main", msg)


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
        with open(SMOKE_SH, encoding="utf-8") as fh:
            smoke = fh.read()
        self.assertIn('git grep -nE "$SECRET_SCAN_RE"', smoke,
                      "smoke.sh must run the shared variable, not a private copy")
        self.assertIn('grep -vE "$SECRET_SCAN_EXCLUDE"', smoke)
        self.assertIn("{16,}", HYGIENE_PATTERN,
                      "the value floor is the fix: `=[^ ]` reported every shell idiom "
                      "that mentions the variable name as a committed credential")

    def test_source_idioms_are_not_credentials(self):
        """The four lines that made a healthy deploy print FAILED (2026-11-16).

        Each mentions a secret VARIABLE NAME and continues into source syntax —
        an awk program, a shell expansion, a grep pattern — rather than a value.
        The scan's subject is a pasted credential; a false FAIL here teaches the
        operator to ignore the deploy's own final verdict.
        """
        with tempfile.TemporaryDirectory() as tmp:
            subprocess.run(["git", "init", "-q"], cwd=tmp, capture_output=True)
            idioms = os.path.join(tmp, "idioms.sh")
            with open(idioms, "w", encoding="utf-8") as fh:
                fh.write("TOKEN=\"$(awk -F= '/^%s=/{sub(/^[^=]*=/,\"\"); print; exit}' \"$ENV_FILE\")\"\n"
                         % up.TOKEN_KEY)
                fh.write('%s="$TOKEN" exec codex "$@"\n' % up.TOKEN_KEY)
                fh.write("sudo grep '^%s=' /etc/kanban-mcp.env\n" % up.TOKEN_KEY)
            subprocess.run(["git", "add", "idioms.sh"], cwd=tmp, capture_output=True)
            self.assertEqual(hygiene_hits(tmp), [],
                             "source that READS the variable is not a committed secret")

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

    def test_a_quoted_literal_is_still_a_credential(self):
        """A quote is the value's DELIMITER, not evidence of a reference.

        Round 1's class started at the first character after `=`, so a pasted
        value wrapped in shell/dotenv quotes became invisible to the scan while
        the old `=[^ ]` pattern caught it. Each of these is the same leak a bare
        value would be, and the constraint on this change is that nothing the
        base pattern caught may slip through.
        """
        token = "0123456789abcdef0123456789abcdef"
        password = "S3cret-Passw0rd-Value"
        shapes = {
            "double-quoted token": '%s="%s"\n' % (up.TOKEN_KEY, token),
            "single-quoted token": "%s='%s'\n" % (up.TOKEN_KEY, token),
            "export + double-quoted token": 'export %s="%s"\n' % (up.TOKEN_KEY, token),
            "double-quoted password": '%s="%s"\n' % (up.PASSWORD_KEY, password),
        }
        for label, line in shapes.items():
            with self.subTest(label):
                with committed_repo("leak.txt", line) as repo:
                    self.assertEqual(len(hygiene_hits(repo)), 1,
                                     "a pasted credential wrapped in a quote is "
                                     "still a committed credential: " + line.strip())

    def test_an_unusable_pattern_is_a_failure_not_a_skip(self):
        """A typo in the one shared pattern would disable BOTH gates at once.

        `git grep` exits >= 2 when it cannot compile the pattern; treating that
        as "nothing to check" is how a clean-looking suite sits over a repo that
        holds a credential. The suite must go red instead.
        """
        broken = HYGIENE_PATTERN + "["
        self.assertIn("{16,}", broken,
                      "the fixture must keep the floor so only a validity check "
                      "can catch it")
        with committed_repo("leak.txt",
                            "%s=%s\n" % (up.TOKEN_KEY, "0123456789abcdef0123456789abcdef")) as repo:
            try:
                hits = hygiene_hits(repo, pattern=broken)
            except AssertionError as exc:
                self.assertIn("could not run", str(exc))
            except unittest.SkipTest as exc:
                self.fail("an unusable pattern must FAIL the scan, not skip it: "
                          "a skip reads as 'all fine' and the gate is green over an "
                          "unscanned repo (%s)" % exc)
            else:
                self.fail("the scan reported %r for a pattern git grep cannot compile"
                          % (hits,))

    def test_the_shell_scan_fails_closed_when_the_pattern_is_unusable(self):
        """The shell half of the same gate: same verdict for a broken pattern."""
        with committed_repo("leak.txt",
                            "%s=%s\n" % (up.TOKEN_KEY, "0123456789abcdef0123456789abcdef")) as repo:
            p = run_smoke_scan_block(repo, pattern=HYGIENE_PATTERN + "[")
        self.assertNotEqual(p.returncode, 0,
                            "a scan that cannot compile must fail, not pass silently")
        self.assertIn("scan pattern is unusable", p.stderr)
        self.assertIn("rc=", p.stderr)

    def test_the_shell_scan_prints_the_offending_lines(self):
        """AC3: the operator judges a report without re-running grep by hand."""
        with committed_repo("leak.txt",
                            "%s=%s\n" % (up.TOKEN_KEY, "0123456789abcdef0123456789abcdef")) as repo:
            p = run_smoke_scan_block(repo)
        self.assertEqual(p.returncode, 1)
        self.assertIn("committed secret value", p.stderr)
        self.assertIn("leak.txt:1:", p.stderr)

    def test_the_shell_scan_passes_a_clean_tree(self):
        with committed_repo("clean.txt", "nothing to see here\n") as repo:
            p = run_smoke_scan_block(repo)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn("no committed secret values", p.stdout)

    def test_the_repo_passes(self):
        hits = hygiene_hits(REPO_ROOT)
        self.assertEqual(hits, [],
                         "these lines trip scripts/smoke.sh's hygiene scan — the "
                         "final verification step of deploy/upgrade.py itself:\n  "
                         + "\n  ".join(hits))


if __name__ == "__main__":
    unittest.main(verbosity=2)
