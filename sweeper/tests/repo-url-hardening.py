#!/usr/bin/env python3
"""Unit checks for repo/branch URL hardening (run with system python3).

Covers _norm_repo, _branch_api_path, and branch_on_github with mocked
subprocess + urllib so no network is touched. Regression for the 2026-08-07
chain: full-form repos, .git suffixes, and percent-encoded branch slashes
each silently broke the branch gate in turn."""
import json
import os
import sys

sys.path.insert(0, os.path.expanduser("~/.hermes/scripts"))
import review_sweeper as rs  # noqa: E402

failures = []


def check(label, got, want):
    ok = got == want
    print(("%s  %s: got=%r want=%r" % ("PASS" if ok else "FAIL", label, got, want)))
    if not ok:
        failures.append(label)


# --- _norm_repo -----------------------------------------------------------
NORM = [
    ("github.com/RKelln/hermes-kanban-mcp", "RKelln/hermes-kanban-mcp"),
    ("https://github.com/RKelln/hermes-kanban-mcp.git", "RKelln/hermes-kanban-mcp"),
    ("http://github.com/RKelln/x", "RKelln/x"),
    ("git@github.com:RKelln/x.git", "RKelln/x"),
    ("ssh://git@github.com/RKelln/x", "RKelln/x"),
    ("RKelln/x", "RKelln/x"),
    ("github.com/RKelln/x/", "RKelln/x"),
    ("RKelln/x.git", "RKelln/x"),
    ("", ""),
    (None, ""),
]
for inp, want in NORM:
    check("norm(%r)" % inp, rs._norm_repo(inp), want)

# --- _branch_api_path -----------------------------------------------------
check("path-plain", rs._branch_api_path("github.com/RKelln/x.git", "main"),
      "repos/RKelln/x/branches/main")
check("path-slash-branch", rs._branch_api_path("RKelln/x", "feat/t_abc-foo"),
      "repos/RKelln/x/branches/feat/t_abc-foo")

# --- branch_on_github (mocked) --------------------------------------------
class FakeResp:
    def __init__(self, status, data):
        self._status, self._data = status, data

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    def read(self):
        return json.dumps(self._data).encode()

    @property
    def code(self):
        return self._status


class FakeHTTPError(Exception):
    def __init__(self, code):
        self.code = code


def mock_branch_check(urlopen_behavior, branch="feat/t_abc-foo", repo="https://github.com/RKelln/x.git"):
    """Force the urllib fallback (gh missing) and capture the URL it asks for."""
    captured = {}

    def fake_run(*a, **k):
        raise FileNotFoundError("gh not on path")

    class FakeUrlopen:
        def __init__(self, behavior):
            self._behavior = behavior

        def __call__(self, req, timeout=None):
            captured["url"] = req.full_url
            if callable(self._behavior):
                return self._behavior(req)
            raise self._behavior

    rs.subprocess.run = fake_run
    rs.urllib.request.urlopen = FakeUrlopen(urlopen_behavior)
    try:
        return rs.branch_on_github(repo, branch), captured.get("url")
    finally:
        import subprocess, urllib.request
        rs.subprocess.run = subprocess.run
        rs.urllib.request.urlopen = urllib.request.urlopen


# 200 -> True; URL must be canonical (no prefix, no .git, slashes intact)
got, url = mock_branch_check(lambda req: FakeResp(200, {"name": "feat/t_abc-foo"}))
check("bog-200-true", got, True)
check("bog-url-canonical", url, "https://api.github.com/repos/RKelln/x/branches/feat/t_abc-foo")

# 404 -> False (silent, legit push-pending)
got, _ = mock_branch_check(FakeHTTPError(404))
check("bog-404-false", got, False)

# 403 -> False + warn (transient must be visible, not silent)
got, _ = mock_branch_check(FakeHTTPError(403))
check("bog-403-false", got, False)

# empty repo/branch -> False without any call
rs.subprocess.run = lambda *a, **k: (_ for _ in ()).throw(AssertionError("should not run"))
check("bog-empty", rs.branch_on_github("", ""), False)
check("bog-none", rs.branch_on_github(None, None), False)

print("\n%s" % ("ALL PASS" if not failures else "FAILURES: %s" % failures))
sys.exit(1 if failures else 0)
