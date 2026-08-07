#!/usr/bin/env python3
"""Unit checks for repo/branch URL hardening (run with system python3).

Covers _norm_repo, _branch_api_path, and branch_on_github with mocked
subprocess + urllib so no network is touched. Regression for the 2026-08-07
chain: full-form repos, .git suffixes, and percent-encoded branch slashes
each silently broke the branch gate in turn."""
import json
import os
import sys

sys.path.insert(0, os.path.expanduser("~/.hermes/scripts"))  # host fallback (may not exist)
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))  # repo sweeper/ copy wins
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

# --- extract_branch / extract_repo (2026-08-07 regression) ---
check("branch-structured", rs.extract_branch("repo: x; branch: feat/t_abc-foo; sha: 1234567"), "feat/t_abc-foo")
check("branch-no-false-prose", rs.extract_branch("the branch check failed and a phantom branch ref lingers"), "")
check("branch-created", rs.extract_branch("Created branch feat/xyz done"), "feat/xyz")
check("branch-name-eq", rs.extract_branch("branch_name=feat/abc"), "feat/abc")
repo_raw = rs.extract_repo("Adopted verbatim; repo: github.com/RKelln/hermes-kanban-mcp; branch: feat/t_abc")
check("repo-full-form", repo_raw, "github.com/RKelln/hermes-kanban-mcp")
check("repo-normalized", rs._norm_repo(repo_raw), "RKelln/hermes-kanban-mcp")
check("repo-bare", rs.extract_repo("repository: RKelln/x"), "RKelln/x")
check("repo-https", rs._norm_repo(rs.extract_repo("clone https://github.com/RKelln/x.git now")), "RKelln/x")

# --- ticket_blob ordering (2026-08-07 SCOPE ADD from t_d8dfdc4b) ---
# The MCP detail carries NO block_reason/repo/branch/sha fields; the real
# refs live in latest_summary/last_run_summary. The blob must put those
# summaries FIRST so the real ref beats fixture prose or stale body text
# that mentions an old branch.
detail = {
    "body": "AC: verify branch: feat/t_STALE-foo and repo: github.com/RKelln/stale.git",
    "latest_summary": "review-required: ... | repo: github.com/RKelln/hermes-kanban-mcp; branch: feat/t_real-branch; sha: 0123456789abcdef",
}
blob = rs.ticket_blob(detail)
check("blob-summary-branch-wins", rs.extract_branch(blob), "feat/t_real-branch")
check("blob-summary-repo-wins", rs.extract_repo(blob), "github.com/RKelln/hermes-kanban-mcp")
check("blob-normalized-repo", rs._norm_repo(rs.extract_repo(blob)), "RKelln/hermes-kanban-mcp")

# --- process_one summary-first extraction (t_83954395 regression) ---
# Summary-first extraction must yield the REAL branch from
# latest_summary, never the body fixture prose. Mirrors the t_d8dfdc4b
# case (body carried 'branch: feat/t_abc-foo' while the summary carried
# the real feat/t_d8dfdc4b-extraction-fixes ref).
detail2 = {
    "body": "Implemented on feat/t_abc-foo.\nRepo: github.com/RKelln/stale.git",
    "latest_summary": "review-required: Applied fixes | repo: github.com/RKelln/hermes-kanban-mcp; branch: feat/t_d8dfdc4b-extraction-fixes; sha: ac57f6d",
}
blob2 = rs.ticket_blob(detail2)
reason2 = detail2.get("latest_summary") or detail2.get("last_run_summary") or ""
check("p1-summary-branch-wins", rs.extract_branch(reason2) or rs.extract_branch(blob2),
      "feat/t_d8dfdc4b-extraction-fixes")
check("p1-summary-repo-wins", rs.extract_repo(reason2) or rs.extract_repo(blob2),
      "github.com/RKelln/hermes-kanban-mcp")

print("\n%s" % ("ALL PASS" if not failures else "FAILURES: %s" % failures))
sys.exit(1 if failures else 0)
