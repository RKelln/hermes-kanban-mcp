#!/usr/bin/env python3
"""review_sweeper.py — SILENT kanban review-required sweeper (no_agent cron, every 15m).

Resurrected 2026-08-07 (t_63327112) from the 2026-08-03 design, with three fixes:
  1. Guard-safe: NO kanban CLI invocations anywhere in this script or the cron
     config (guard false positive #30719). Board reads + comments go through
     the kanban MCP HTTP API (127.0.0.1:9130, streamable-HTTP JSON-RPC, bearer
     token from /etc/kanban-mcp.env). The two status transitions the MCP
     bridge cannot express (blocked -> done, blocked -> ready) go through the
     dashboard REST API (:9119) that the bridge itself proxies to.
  2. Board-agnostic: scans every board the MCP board_list reports, not just one.
  3. Verdict-driven: spawns a real reviewer session (`hermes chat -q -s
     sdlc-review`) per ticket and applies its verdict:
       APPROVE         -> comment verdict, then PATCH status=done
       REQUEST CHANGES -> comment findings, then PATCH status=ready (external
                          lanes re-claim)
       ESCALATE        -> leave blocked (human inbox); comment once

Idempotency: per-ticket lock files (state dir) prevent concurrent reviews;
a ledger line is written only after a verdict is applied, so a crashed run
re-reviews rather than silently forgetting. Verdict comments are
informational; the ledger is authoritative.

Silence contract (watchdog): print NOTHING on empty runs; a one-line summary
per processed ticket; loud "BROKEN:" + exit 1 only on structural failure
(missing config, bridge unreachable, auth failure). Transient per-ticket
failures print a warning and are retried next tick (exit 0).

DeepSeek peak pricing guard: skip the whole pass during UTC 1-4 and 6-10
(the spawned reviewers are DS-paid).

v3 (t_21b38b58, 2026-08-07): per-board repo map (review-sweeper.conf ->
repo + default branch), ONE shared clone per board under checkouts/<board>
(refetched before every review so reviewers work inside a fresh tree and
fetch by SHA/branch), reviewers ALWAYS spawned with terminal+file (the
file-only spawn could not verify branches -- t_ccc76018 ESCALATE), and a
stall alert: N consecutive empty ticks with scan errors print BROKEN
instead of the silent 429 blindness of the first live day.
"""
import hashlib
import http.cookiejar
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone

# --------------------------------------------------------------------------
# Configuration
# --------------------------------------------------------------------------
ENV_FILE = "/etc/kanban-mcp.env"
MCP_URL = "http://127.0.0.1:9130/mcp"
REST_LOGIN_URL = "http://127.0.0.1:9119/auth/password-login"
STATE_DIR = os.path.expanduser("~/.hermes/state/review-sweeper")
CONF_FILE = os.path.expanduser("~/.config/review-sweeper.conf")  # <board> <https-repo> [default-branch]
CHECKOUT_ROOT = os.path.join(STATE_DIR, "checkouts")  # one shared clone per board
STALL_FILE = os.path.join(STATE_DIR, "stall.json")    # consecutive-empty-tick counter
STALL_THRESHOLD = 3  # N empty ticks WITH scan errors -> BROKEN (silent-blindness alert)

HERMES_BIN = "/home/experimance/.local/bin/hermes"
GH_BIN = "/home/linuxbrew/.linuxbrew/bin/gh"
GIT_BIN = shutil.which("git") or "/usr/bin/git"

MAX_TICKETS_PER_RUN = 2        # bound the tick; typical queue is 0-1
REVIEW_TIMEOUT_SECONDS = 1800  # hard cap on one reviewer session
LOCK_TTL_SECONDS = 4 * 3600    # stale lock takeover (covers a hung reviewer)
PEAK_UTC_HOURS = set(range(1, 4)) | set(range(6, 10))  # DS double-price windows

REVIEW_REQUIRED_RE = re.compile(r"review-required", re.IGNORECASE)
BRANCH_RE = re.compile(
    # A real ref contains a slash (feat/t_xxx); the loose 'branch <word>' form
    # matched arbitrary prose ('branch check', 'branch ref') and silently broke
    # the branch gate. Require the slash-form, prefer the structured field.
    r"(?:branch_name[=:]\s*|branch[=:]\s*|Created branch\s+|branch\s+)"
    r"([A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-]+)"
)
REPO_RE = re.compile(
    # Multi-segment capture (github.com/owner/repo full path, or owner/repo);
    # _norm_repo() strips prefixes/.git afterwards.
    r"(?:https?://github\.com/|git@github\.com:|ssh://git@github\.com/|github\.com/|repo[=:]\s*|repository[=:]\s*)"
    r"([A-Za-z0-9_.\-]+(?:/[A-Za-z0-9_.\-]+)+)"
)
SHA_RE = re.compile(
    r"(?:commit|sha|sha1|revision)[\s:=]+([0-9a-fA-F]{7,40})(?=[\s,.);]|$)"
)
VERDICT_RE = re.compile(r"^\s*VERDICT\s*:\s*([A-Z_\- ]+?)\s*$", re.IGNORECASE | re.MULTILINE)
# Anchored whole-line marker: a comment counts as "already has a sweeper
# verdict" ONLY when it begins with a clean, self-contained verdict line.
# The v1 malformed comment ("review-sweeper: ESCALATED (Verdict: APPROVE...")
# has trailing text, so it no longer satisfies this -> no dedup trap.
VERDICT_COMMENT_RE = re.compile(
    r"^\s*review-sweeper:\s*(approved|request changes|escalated)\s*$",
    re.IGNORECASE | re.MULTILINE,
)


def load_env() -> dict:
    """Parse the kanban-mcp env file (0600, owned by the cron user)."""
    env = {}
    with open(ENV_FILE, encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, _, val = line.partition("=")
            env[key.strip()] = val.strip().strip('"').strip("'")
    return env


def load_conf(path: str = CONF_FILE) -> dict:
    """Parse review-sweeper.conf: '<board> <https-repo-url> [default-branch]'.

    '#' comments and blank lines are ignored; the 2-column legacy form
    defaults the branch to 'main'. Returns {board: (repo, default_branch)}.
    """
    conf = {}
    if not os.path.isfile(path):
        return conf
    with open(path, encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            parts = line.split()
            if len(parts) < 2:
                continue
            conf[parts[0]] = (parts[1], parts[2] if len(parts) >= 3 else "main")
    return conf


def repo_for_board(board: str, conf: dict):
    """Board -> (repo_url, default_branch) from review-sweeper.conf."""
    return conf.get(board, (None, None))


def is_peak_hour() -> bool:
    return datetime.now(timezone.utc).hour in PEAK_UTC_HOURS


# --------------------------------------------------------------------------
# MCP JSON-RPC client (streamable HTTP, bearer auth, SSE responses)
# --------------------------------------------------------------------------
class McpError(Exception):
    pass


class McpClient:
    """Minimal MCP streamable-HTTP client: initialize -> initialized ->
    tools/call, reusing one session."""

    def __init__(self, token: str, url: str = MCP_URL):
        self.token = token
        self.url = url
        self.session_id = None
        self._req_id = 0

    def _post(self, payload: dict) -> dict:
        data = json.dumps(payload).encode("utf-8")
        headers = {
            "Authorization": "Bearer " + self.token,
            "Content-Type": "application/json",
            "Accept": "application/json, text/event-stream",
        }
        if self.session_id:
            headers["Mcp-Session-Id"] = self.session_id
        req = urllib.request.Request(self.url, data=data, headers=headers, method="POST")
        is_notification = "id" not in payload
        try:
            with urllib.request.urlopen(req, timeout=30) as resp:
                self.session_id = self.session_id or resp.headers.get("Mcp-Session-Id")
                raw = resp.read().decode("utf-8", errors="replace")
        except urllib.error.HTTPError as exc:
            raise McpError("mcp http %s: %s" % (exc.code, exc.read()[:200]))
        except urllib.error.URLError as exc:
            raise McpError("mcp unreachable: %s" % exc.reason)
        if is_notification:
            return {}  # notifications get a 2xx with an empty body
        return self._parse_sse(raw)

    @staticmethod
    def _parse_sse(raw: str) -> dict:
        """Extract the first JSON payload from an SSE (or plain JSON) body."""
        for line in raw.splitlines():
            line = line.strip()
            if line.startswith("data:"):
                try:
                    return json.loads(line[5:].strip())
                except ValueError:
                    continue
        if raw.strip().startswith("{"):
            return json.loads(raw)
        raise McpError("unexpected mcp response: %r" % raw[:200])

    def initialize(self) -> None:
        self._post({
            "jsonrpc": "2.0", "id": 1, "method": "initialize",
            "params": {
                "protocolVersion": "2025-03-26", "capabilities": {},
                "clientInfo": {"name": "review-sweeper", "version": "1.0"},
            },
        })
        self._post({"jsonrpc": "2.0", "method": "notifications/initialized"})

    def call(self, name: str, arguments: dict) -> str:
        """Call a tool; returns the text content of its result. Raises on
        error results so callers can't mistake a failure for data."""
        self._req_id += 1
        msg = self._post({
            "jsonrpc": "2.0", "id": self._req_id, "method": "tools/call",
            "params": {"name": name, "arguments": arguments},
        })
        if "error" in msg:
            raise McpError("mcp tool %s: %s" % (name, msg["error"]))
        result = msg.get("result", {})
        if result.get("isError"):
            raise McpError("mcp tool %s: %s" % (name, result.get("content")))
        parts = result.get("content") or []
        return "".join(p.get("text", "") for p in parts if p.get("type") == "text")


# --------------------------------------------------------------------------
# Dashboard REST client (only for the transitions MCP cannot express)
# --------------------------------------------------------------------------
class RestClient:
    """Cookie-session REST client for the two PATCH transitions the MCP bridge
    lacks: blocked -> done (approve) and blocked -> ready (request changes)."""

    def __init__(self, base_url: str, username: str, password: str):
        jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
        self.base = base_url.rstrip("/") + "/"
        self.username = username
        self.password = password
        self._login()

    def _login(self) -> None:
        body = json.dumps({"provider": "basic", "username": self.username,
                           "password": self.password}).encode()
        req = urllib.request.Request(REST_LOGIN_URL, data=body,
                                     headers={"Content-Type": "application/json"})
        try:
            with self.opener.open(req, timeout=30) as resp:
                if resp.status != 200:
                    raise McpError("dashboard login failed: http %s" % resp.status)
        except urllib.error.URLError as exc:
            raise McpError("dashboard unreachable: %s" % exc.reason)

    def patch_task(self, board: str, tid: str, body: dict) -> dict:
        url = self.base + "tasks/" + urllib.parse.quote(tid) + "?board=" + urllib.parse.quote(board)
        req = urllib.request.Request(
            url, data=json.dumps(body).encode(),
            headers={"Content-Type": "application/json"}, method="PATCH")
        try:
            with self.opener.open(req, timeout=30) as resp:
                return json.loads(resp.read().decode("utf-8", errors="replace"))
        except urllib.error.HTTPError as exc:
            detail = exc.read().decode("utf-8", errors="replace")[:300]
            raise McpError("rest patch %s -> %s failed: http %s %s" % (tid, body.get("status"), exc.code, detail))
        except urllib.error.URLError as exc:
            raise McpError("dashboard unreachable: %s" % exc.reason)


# --------------------------------------------------------------------------
# State (locks + ledger) — the anti-duplicate machinery
# --------------------------------------------------------------------------
SINGLETON_LOCK = None  # set by _singleton_paths()


def _state_paths():
    locks = os.path.join(STATE_DIR, "locks")
    os.makedirs(locks, exist_ok=True)
    return locks, os.path.join(STATE_DIR, "ledger.jsonl")


def _singleton_paths():
    os.makedirs(STATE_DIR, exist_ok=True)
    return os.path.join(STATE_DIR, "sweeper.pid")


def singleton_acquire(pid: int) -> bool:
    """One sweeper process per host: cron tick + manual runs must never
    overlap (the round-1 leak re-spawned reviewers every few minutes). A live
    pid holds the lock; a dead pid or one older than LOCK_TTL is taken over.
    Returns False when another sweeper is alive."""
    path = _singleton_paths()
    payload = json.dumps({"pid": pid, "started_at": int(time.time())})
    try:
        fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    except FileExistsError:
        try:
            with open(path, encoding="utf-8") as fh:
                meta = json.load(fh)
        except Exception:
            meta = {}
        old_pid = meta.get("pid")
        alive = False
        if old_pid:
            try:
                os.kill(int(old_pid), 0)
                alive = True
            except (OSError, ValueError):
                alive = False
        stale = (not alive) or (time.time() - meta.get("started_at", 0) > LOCK_TTL_SECONDS)
        if not stale:
            return False
        try:
            os.remove(path)  # steal the stale singleton
        except OSError:
            return False
        fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    with os.fdopen(fd, "w") as fh:
        fh.write(payload)
    return True


def singleton_release(pid: int) -> None:
    path = _singleton_paths()
    try:
        with open(path, encoding="utf-8") as fh:
            meta = json.load(fh)
        if meta.get("pid") == pid:
            os.remove(path)
    except (OSError, ValueError):
        pass


def block_fingerprint(detail: dict) -> str:
    """Stable id for ONE block event. The ledger is keyed by (board, tid,
    fingerprint) so a RE-BLOCKED ticket (REQUEST CHANGES -> ready -> external
    lane re-claims -> blocks again with a new reason) gets re-reviewed instead
    of being skipped forever by its old ledger line."""
    key = "|".join([
        detail.get("block_reason") or "",
        detail.get("last_run_summary") or "",
        detail.get("latest_summary") or "",
    ])
    return hashlib.sha1(key.encode("utf-8")).hexdigest()[:16]


def lock_acquire(board: str, tid: str, pid: int) -> bool:
    """Take the review lock for a ticket. Returns False if another review is
    live (fresh lock whose pid is alive). Stale locks (dead pid or older than
    LOCK_TTL) are taken over, and any reviewer subagent the dead owner left
    behind is killed (round-1 leak: orphaned reviewers kept re-spawning)."""
    locks, _ = _state_paths()
    path = os.path.join(locks, "%s__%s.lock" % (board, tid))
    payload = json.dumps({"pid": pid, "board": board, "tid": tid,
                          "reviewer_pid": None, "started_at": int(time.time())})
    try:
        fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    except FileExistsError:
        try:
            with open(path, encoding="utf-8") as fh:
                meta = json.load(fh)
        except Exception:
            meta = {}
        old_pid = meta.get("pid")
        alive = False
        if old_pid:
            try:
                os.kill(int(old_pid), 0)
                alive = True
            except (OSError, ValueError):
                alive = False
        stale = (not alive) or (time.time() - meta.get("started_at", 0) > LOCK_TTL_SECONDS)
        if not stale:
            return False
        # the old owner is gone — reap any reviewer it spawned before taking over
        old_reviewer = meta.get("reviewer_pid")
        if old_reviewer:
            _kill_reviewer(old_reviewer)
        try:
            os.remove(path)  # steal the stale lock
        except OSError:
            return False
        fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    with os.fdopen(fd, "w") as fh:
        fh.write(payload)
    return True


def lock_set_reviewer(board: str, tid: str, reviewer_pid: int) -> None:
    """Record the spawned reviewer subagent's pid in the lock so a later
    stale-lock takeover can kill it (straggler reaping)."""
    locks, _ = _state_paths()
    path = os.path.join(locks, "%s__%s.lock" % (board, tid))
    try:
        with open(path, encoding="utf-8") as fh:
            meta = json.load(fh)
        meta["reviewer_pid"] = reviewer_pid
        with open(path, "w", encoding="utf-8") as fh:
            json.dump(meta, fh)
    except (OSError, ValueError):
        pass


def _kill_reviewer(pid) -> None:
    """Terminate a reviewer subagent (SIGTERM, then SIGKILL after a grace
    period). Uses the process group when the reviewer created one."""
    try:
        pid = int(pid)
    except (TypeError, ValueError):
        return
    for sig, grace in ((signal.SIGTERM, 3), (signal.SIGKILL, 0)):
        try:
            os.killpg(pid, sig)  # start_new_session=True in spawn_reviewer
        except ProcessLookupError:
            return
        except OSError:
            try:
                os.kill(pid, sig)
            except OSError:
                return
        if grace:
            time.sleep(grace)
        else:
            return
    try:
        os.kill(pid, 0)
    except OSError:
        pass  # gone


def lock_release(board: str, tid: str) -> None:
    locks, _ = _state_paths()
    try:
        os.remove(os.path.join(locks, "%s__%s.lock" % (board, tid)))
    except OSError:
        pass


def ledger_has(board: str, tid: str, fp: str = "") -> bool:
    """True when this exact block event (board+tid+block fingerprint) already
    has a ledger line. fp='' matches any fingerprint (legacy calls)."""
    _, ledger = _state_paths()
    if not os.path.isfile(ledger):
        return False
    with open(ledger, encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            try:
                entry = json.loads(line)
            except ValueError:
                continue
            if entry.get("board") != board or entry.get("tid") != tid:
                continue
            if not fp or entry.get("fp") == fp:
                return True
    return False


def ledger_append(board: str, tid: str, verdict: str, status_after: str, fp: str = "") -> None:
    _, ledger = _state_paths()
    entry = {"ts": int(time.time()), "board": board, "tid": tid,
             "verdict": verdict, "status_after": status_after, "fp": fp}
    with open(ledger, "a", encoding="utf-8") as fh:
        fh.write(json.dumps(entry) + "\n")


# --------------------------------------------------------------------------
# Board scanning
# --------------------------------------------------------------------------
def all_boards(mcp: McpClient) -> list:
    text = mcp.call("board_list", {"include_archived": False})
    data = json.loads(text)
    return [b.get("slug", "") for b in data.get("boards", []) if b.get("slug")]


def blocked_tickets(mcp: McpClient, board: str) -> list:
    text = mcp.call("ticket_list", {"board": board, "status": ["blocked"], "limit": 50})
    data = json.loads(text)
    return data.get("tickets", [])


def ticket_detail(mcp: McpClient, board: str, tid: str) -> dict:
    text = mcp.call("ticket_get", {"board": board, "id": tid})
    return json.loads(text)


def is_review_required(detail: dict) -> bool:
    haystack = " ".join([
        detail.get("block_reason") or "",
        detail.get("last_run_summary") or "",
        detail.get("latest_summary") or "",
        detail.get("body") or "",
    ])
    return bool(REVIEW_REQUIRED_RE.search(haystack))


def ticket_blob(detail: dict) -> str:
    """Everything the branch/repo extractors search.

    Order matters: latest_summary/last_run_summary come FIRST. The MCP
    detail carries NO block_reason/repo/branch/sha fields (bridge gap) —
    the real refs live in the kernel's run summaries. Reading them first
    means a real 'branch: feat/t_xxx' in the summary beats fixture prose
    or stale body text that mentions an old branch."""
    bits = [detail.get("latest_summary") or "", detail.get("last_run_summary") or "",
            detail.get("block_reason") or "", detail.get("body") or ""]
    for c in detail.get("comments") or []:
        bits.append(c.get("body") or "")
    return "\n".join(bits)


def extract_branch(blob: str) -> str:
    m = BRANCH_RE.search(blob)
    return m.group(1) if m else ""


def extract_repo(blob: str) -> str:
    m = REPO_RE.search(blob)
    return m.group(1) if m else ""


def extract_sha(blob: str) -> str:
    """A commit SHA named as commit/sha/sha1/revision <sha> in ticket text."""
    m = SHA_RE.search(blob)
    return m.group(1) if m else ""


def _norm_repo(repo: str) -> str:
    """Normalize a repo ref to owner/repo form. Tickets may carry the full
    'github.com/owner/repo' form (the MCP structured fields) or plain
    'owner/repo' (conf/older bodies); API URLs and clone paths need owner/repo.
    """
    repo = (repo or "").strip().rstrip("/")
    for prefix in ("https://github.com/", "http://github.com/", "git@github.com:",
                   "ssh://git@github.com/", "github.com/"):
        if repo.startswith(prefix):
            repo = repo[len(prefix):]
            break
    if repo.endswith(".git"):
        repo = repo[:-4]  # clone URLs from the board conf carry the .git suffix
    return repo


def _branch_api_path(repo: str, branch: str) -> str:
    """Canonical GitHub API path for a branch check:
    repos/<owner>/<repo>/branches/<branch>. Single construction point so the
    gh path and the urllib fallback can never diverge again (2026-08-07:
    full-form repos + .git suffixes + percent-encoded slashes each broke the
    check in turn)."""
    repo = _norm_repo(repo)
    return "repos/%s/branches/%s" % (repo, branch)


def branch_on_github(repo: str, branch: str) -> bool:
    """Public GitHub API check that the implementation branch was pushed.
    gh (authenticated as RKelln) first; unauthenticated api.github.com as
    fallback for orgs where the PAT 403s.

    False = branch absent (404, silent — legit 'push pending'). Transient API
    failures (403/429/5xx) print a warn so they can never silently skip
    tickets again (the 2026-08-07 blindness lesson)."""
    if not _norm_repo(repo) or not branch:
        return False
    path = _branch_api_path(repo, branch)
    try:
        r = subprocess.run([GH_BIN, "api", path, "--jq", ".name"],
                           capture_output=True, text=True, timeout=30)
        if r.returncode == 0 and r.stdout.strip():
            return True
    except Exception:
        pass
    try:
        # branch names carry '/' (feat/t_xxx): quote ONLY the branch segment,
        # never the path separators (urllib.parse.quote on the whole path
        # percent-encoded the slashes -> 404 on every slash-branch).
        head, _, tail = path.rpartition("/branches/")
        if head and tail:
            full = "https://api.github.com/%s/branches/%s" % (head, urllib.parse.quote(tail, safe="/"))
        else:
            full = "https://api.github.com/" + path
        req = urllib.request.Request(full, headers={"Accept": "application/vnd.github+json",
                                                    "User-Agent": "review-sweeper"})
        with urllib.request.urlopen(req, timeout=20) as resp:
            data = json.loads(resp.read().decode("utf-8", errors="replace"))
            return bool(data.get("name"))
    except Exception as exc:
        code = getattr(exc, "code", None)
        if code and code != 404:
            print("review-sweeper: warn: branch check %s: HTTP %s (transient; retried next tick)" % (path, code))
        return False


def ensure_checkout(board: str, repo: str, default_branch: str = "main") -> str:
    """One shared clone per board: CHECKOUT_ROOT/<board>.

    Clones on first use, then `git fetch origin` (all branches) before every
    review so ticket->repo resolution is consistent and reviewers work inside
    a fresh, already-fetched tree instead of cloning or hunting for a local
    checkout. Returns the checkout path, or '' when the repo is unset or the
    clone/fetch failed (callers fall back to repo-only instructions).
    Errors are swallowed on purpose: a checkout failure must not take the
    whole tick down -- the reviewer can still fetch via HTTPS itself.
    """
    if not repo:
        return ""
    repo = _norm_repo(repo)
    clone_url = "https://github.com/%s" % repo
    path = os.path.join(CHECKOUT_ROOT, board)
    try:
        if os.path.isdir(os.path.join(path, ".git")):
            subprocess.run([GIT_BIN, "-C", path, "fetch", "origin",
                            "+refs/heads/*:refs/remotes/origin/*"],
                           capture_output=True, text=True, timeout=300)
            return path
        if os.path.lexists(path):
            shutil.rmtree(path)  # stale partial clone
        os.makedirs(os.path.dirname(path), exist_ok=True)
        subprocess.run([GIT_BIN, "clone", "--no-tags", clone_url, path],
                       capture_output=True, text=True, timeout=600)
        if os.path.isdir(os.path.join(path, ".git")):
            return path
    except Exception:
        pass
    return ""


def comments_have_sweeper_verdict(detail: dict) -> bool:
    for c in detail.get("comments") or []:
        if VERDICT_COMMENT_RE.search(c.get("body") or ""):
            return True
    return False


# --------------------------------------------------------------------------
# Reviewer spawn + verdict
# --------------------------------------------------------------------------
def build_reviewer_prompt(board: str, tid: str, detail: dict, repo: str, branch: str,
                          sha: str = "", checkout: str = "", base: str = "main") -> str:
    comments = "\n".join(
        "- %s: %s" % (c.get("author") or "?", (c.get("body") or "")[:500])
        for c in (detail.get("comments") or [])[-10:]
    )
    body = (detail.get("body") or "")[:4000]
    block_reason = (detail.get("block_reason") or detail.get("last_run_summary") or "")[:300]
    if repo:
        ref_note = "REPO: %s\nBASE: %s" % (repo, base or "main")
        if branch:
            ref_note += ("\nBRANCH: %s (verified present on origin via the public "
                         "GitHub API)" % branch)
        if sha:
            ref_note += "\nCOMMIT: %s (fetch by SHA)" % sha
        if checkout:
            ref_note += ("\nCHECKOUT: %s (shared per-board clone, fetched before this "
                         "review — work inside it only)" % checkout)
            if branch:
                work = ("cd %s && git fetch origin && git checkout -q origin/%s; "
                        "diff with `git diff origin/%s...origin/%s`; secret-scan the diff "
                        "(tokens, keys, .env, credentials, private URLs); run any "
                        "verification commands the ticket body names (tests/build) inside "
                        "the checkout and record actual output."
                        % (checkout, branch, base or "main", branch))
            else:
                work = ("cd %s && git fetch origin %s && git checkout -q %s; "
                        "inspect `git show --stat %s` and read the files it names; "
                        "secret-scan the patch; run any verification commands the ticket "
                        "body names inside the checkout and record actual output."
                        % (checkout, sha, sha, sha))
        else:
            work = ("clone/fetch the branch via HTTPS read-only (never push); "
                    "`git diff origin/%s...origin/<branch>` or fetch by SHA; secret-scan "
                    "the diff; run any verification commands the ticket body names and "
                    "record actual output." % (base or "main"))
        verify_instructions = work + " Compare the ticket's acceptance criteria one by one."
    elif branch:
        ref_note = ("BRANCH: %s (the repo could not be determined from the conf/ticket; you "
                    "HAVE terminal — probe read-only with `git ls-remote`/`gh api` ONLY if "
                    "the ticket names an org/repo; otherwise verdict ESCALATE)" % branch)
        verify_instructions = (
            "Locate the code from the ticket context (branch ref or commit SHA). If you "
            "cannot find it, verdict ESCALATE. If found, verify as above."
        )
    else:
        ref_note = "No repo/branch ref — this is a docs/artifact ticket."
        verify_instructions = (
            "Read the artifact or file the ticket body names (use the read_file tool) and "
            "check it against the acceptance criteria. You have terminal if a verification "
            "command is needed. If nothing verifiable exists, verdict ESCALATE."
        )
    return (
        "You are a code reviewer. Review ONE review-required kanban ticket and return a "
        "verdict. The ticket: board %s, id %s, title %s.\n\n"
        "BLOCK REASON: %s\n"
        "%s\n\n"
        "ACCEPTANCE CRITERIA (ticket body):\n%s\n\n"
        "IMPLEMENTER COMMENTS (context only):\n%s\n\n"
        "VERIFY:\n%s\n\n"
        "HARD RULES:\n"
        "- This host is READ-ONLY against GitHub by design: NEVER push, merge, or write to "
        "any git remote, repo, database, kanban board, or API. Fetch and read only.\n"
        "- DO NOT investigate the review system, the sweeper, lock files, ledgers, the "
        "kanban board, this ticket's status, or any process on this host. If a shared "
        "CHECKOUT was provided, restrict ALL filesystem work to that directory and the "
        "ticket's artifact (no directory listings or searches elsewhere). Run ONLY the "
        "commands needed to verify the deliverable named above.\n"
        "- If a merge is genuinely required, that is an ESCALATE, not something you do.\n"
        "- REQUEST CHANGES returns the ticket to ready for the external lane to re-claim.\n"
        "- CI-red rule: if CI looks red, compare the previous commit's check-runs on the "
        "same repo; pre-existing failures are a note, not a block.\n"
        "- Do NOT use extended reasoning mode; produce the review directly.\n\n"
        "OUTPUT CONTRACT — your ENTIRE final response must be exactly this shape, nothing "
        "else, no preamble, no commentary about the process:\n"
        "1. 2-8 short findings bullets.\n"
        "2. One final line, with NOTHING after it: VERDICT: APPROVE | REQUEST_CHANGES | "
        "ESCALATE\n"
        "Do not write the word VERDICT anywhere else in your response. APPROVE = verified "
        "against the criteria; REQUEST_CHANGES = concrete findings need fixing; ESCALATE = "
        "unverifiable, security issue, merge required, or human decision needed."
    ) % (board, tid, (detail.get("title") or "")[:120], block_reason, ref_note,
         body, comments or "(none)", verify_instructions)


def spawn_reviewer(prompt: str, toolsets: str = "terminal,file") -> tuple:
    """Run one reviewer session. Returns (ok: bool, output: str, pid: int).

    The reviewer runs with a scrubbed environment: it must be a standalone
    hermes session, NOT a nested one. Inheriting the caller's HERMES_* vars
    (e.g. HERMES_KANBAN_TASK, _HERMES_GATEWAY when the sweeper runs inside a
    gateway session) makes the child attach to the parent session machinery
    and can get the whole tree SIGTERMed. The cron scheduler already provides
    a clean env; the scrub makes interactive runs behave identically.

    start_new_session=True gives the reviewer its own process group so a
    timeout (or a stale-lock takeover) can killpg the WHOLE tree — the
    round-1 leak left orphaned reviewers re-spawning behind dead owners.
    The pid is returned for the per-ticket lock's reviewer_pid field."""
    cmd = [HERMES_BIN, "chat", "-q", prompt, "-s", "sdlc-review", "-Q",
           "-t", toolsets, "--max-turns", "40", "--reasoning", "low"]
    env = {k: v for k, v in os.environ.items()
           if k != "_HERMES_GATEWAY" and not k.startswith("HERMES_")}
    proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            text=True, env=env, start_new_session=True)
    try:
        out, err = proc.communicate(timeout=REVIEW_TIMEOUT_SECONDS)
        output = (out or "") + "\n" + (err or "")
        return proc.returncode == 0, output, proc.pid
    except subprocess.TimeoutExpired:
        _kill_reviewer(proc.pid)
        proc.wait(timeout=10)
        return False, "reviewer timed out after %ds" % REVIEW_TIMEOUT_SECONDS, proc.pid


def save_review_output(board: str, tid: str, output: str) -> str:
    """Persist one reviewer session's raw output for audit/debugging."""
    reviews_dir = os.path.join(STATE_DIR, "reviews")
    os.makedirs(reviews_dir, exist_ok=True)
    path = os.path.join(reviews_dir, "%s__%s_%d.log" % (board, tid, int(time.time())))
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(output)
    return path


def parse_verdict(output: str):
    """Returns APPROVE | REQUEST_CHANGES | ESCALATE | None.

    Tolerant on vocabulary (the skill says VERIFY-ONLY APPROVE / REQUEST
    CHANGES / ESCALATE; the contract says APPROVE / REQUEST_CHANGES /
    ESCALATE). Lines that quote the contract itself ('APPROVE | REQUEST_CHANGES
    | ESCALATE') are skipped. The LAST valid line wins."""
    verdict = None
    for m in VERDICT_RE.finditer(output):
        value = m.group(1).strip()
        if "|" in value or "/" in value:
            continue  # contract quotation, not a real verdict
        norm = re.sub(r"[^a-z]", "", value.lower())
        if norm in ("approve", "approved", "approvewithnits", "approvedwithnits",
                    "verifyonlyapprove", "verifyonly", "verifyapprove"):
            verdict = "APPROVE"
        elif norm in ("requestchanges", "requestedchanges", "changesrequested", "changes"):
            verdict = "REQUEST_CHANGES"
        elif norm in ("escalate", "escalated", "block", "human", "needsreview", "humanreview"):
            verdict = "ESCALATE"
    return verdict


# --------------------------------------------------------------------------
# Verdict application
# --------------------------------------------------------------------------
def apply_verdict(mcp: McpClient, rest: RestClient, board: str, tid: str,
                  detail: dict, verdict: str, reviewer_output: str) -> str:
    """Apply a verdict. Returns the ticket's status after the attempt."""
    findings = extract_findings(reviewer_output)
    if verdict == "APPROVE":
        body = "review-sweeper: APPROVED\n%s" % findings
        mcp.call("ticket_comment", {"board": board, "id": tid, "body": body,
                                    "author": "review-sweeper"})
        rest.patch_task(board, tid, {"status": "done",
                                     "summary": "review-sweeper: APPROVED"})
        return "done"
    if verdict == "REQUEST_CHANGES":
        body = "review-sweeper: REQUEST CHANGES\n%s" % findings
        mcp.call("ticket_comment", {"board": board, "id": tid, "body": body,
                                    "author": "review-sweeper"})
        rest.patch_task(board, tid, {"status": "ready"})
        return "ready"
    # ESCALATE (or anything unknown) — leave blocked, human inbox. Comment once.
    if not comments_have_sweeper_verdict(detail):
        body = "review-sweeper: ESCALATED\n%s" % (findings or "human review required")
        mcp.call("ticket_comment", {"board": board, "id": tid, "body": body,
                                    "author": "review-sweeper"})
    return "blocked"


def _strip_reasoning(text: str) -> str:
    """Remove the '┌─ Reasoning ─' display box (which has no reliable closing
    border) by dropping from the box header to the first findings bullet."""
    m = re.search(r"┌[─\-—]+ Reasoning", text)
    if not m:
        return text
    start = m.start()
    rest = text[start:]
    m2 = re.search(r"(?m)^\s*[-*•] ", rest)
    if m2:
        return text[:start] + rest[m2.start():]
    return text[:start]


def extract_findings(output: str) -> str:
    """The reviewer's findings, as CLEAN BULLETS ONLY — never raw output.

    Everything before the final valid VERDICT line is considered; reasoning
    display boxes are stripped; then only bullet lines (leading '-'/'•'/'*')
    are kept, collapsed to <=1200 chars. This is the dedup-trap fix: the v1
    malformed comment leaked the raw pre-verdict blob (reasoning box + echoed
    contract text) into the ticket. If the reviewer produced no bullets, an
    empty string is returned and the caller substitutes a clean fallback."""
    last = None
    for m in VERDICT_RE.finditer(output):
        value = m.group(1).strip()
        if "|" in value or "/" in value:
            continue
        last = m
    if last is None:
        return ""
    excerpt = output[:last.start()]
    excerpt = _strip_reasoning(excerpt)
    bullets = [ln.strip() for ln in excerpt.splitlines()
               if ln.strip().startswith(("- ", "• ", "* "))]
    text = "\n".join(bullets).strip()
    if len(text) > 1200:
        text = text[-1200:]
    return text


# --------------------------------------------------------------------------
# Main
# --------------------------------------------------------------------------
def _install_signal_handlers() -> None:
    """SIGTERM/SIGINT must release locks: raise SystemExit so try/finally
    blocks run instead of the default abrupt termination."""

    def handler(signum, _frame):
        raise SystemExit(128 + signum)

    signal.signal(signal.SIGTERM, handler)
    signal.signal(signal.SIGINT, handler)


def main(argv) -> int:
    _install_signal_handlers()
    args = parse_args(argv)
    if is_peak_hour():
        return 0
    if not os.path.isfile(ENV_FILE):
        print("review-sweeper: BROKEN: missing %s" % ENV_FILE)
        return 1
    env = load_env()
    token = env.get("MCP_BEARER_TOKEN") or ""
    if not token:
        print("review-sweeper: BROKEN: MCP_BEARER_TOKEN not in %s" % ENV_FILE)
        return 1

    # cadence guard (2026-08-07): the cron schedule is PINNED to every-1-minute
    # with the DeepSeek-peak skip (Ryan's spec). A worker once changed it to
    # */15 post-approval, silently slowing pickup from 1m to 15m. If the job's
    # schedule drifts, scream BROKEN so the cron's last_status turns red
    # instead of silently missing tickets.
    _EXPECTED_SCHEDULE = "* 0-1,6-20 * * *"
    try:
        with open(os.path.expanduser("~/.hermes/cron/jobs.json"), encoding="utf-8") as fh:
            _jobs = json.load(fh)
        _jobs = _jobs if isinstance(_jobs, list) else _jobs.get("jobs", [])
        for _j in _jobs:
            if str(_j.get("id", "")).startswith("8305dcd5bfbd"):
                _expr = (_j.get("schedule") or {}).get("expr", "")
                if _expr != _EXPECTED_SCHEDULE:
                    _msg = ("cron schedule drifted: %r (expected %r) — fix with "
                            "`hermes cron update 8305dcd5bfbd --schedule '%s'`"
                            % (_expr, _EXPECTED_SCHEDULE, _EXPECTED_SCHEDULE))
                    if args.scan:
                        print("review-sweeper: warn: %s" % _msg)
                    else:
                        print("review-sweeper: BROKEN: %s" % _msg)
                        return 1
                break
    except (OSError, ValueError, TypeError):
        pass  # jobs.json unreadable mid-update — don't block sweeps

    # single-instance guard: cron tick + manual runs must never overlap
    # (round-1 leak: overlapping instances re-spawned reviewers every few
    # minutes). --scan is read-only and lock-scoped, so it may run alongside.
    if not args.scan and not singleton_acquire(os.getpid()):
        return 0  # another sweeper is live; stay silent

    conf = load_conf()  # review-sweeper.conf: board -> (repo, default_branch)

    mcp = McpClient(token)
    try:
        mcp.initialize()
    except McpError as exc:
        if not args.scan:
            singleton_release(os.getpid())
        print("review-sweeper: BROKEN: %s" % exc)
        return 1

    prev_stall = 0
    try:
        if os.path.isfile(STALL_FILE):
            prev_stall = int(json.load(open(STALL_FILE, encoding="utf-8"))
                             .get("empty_ticks", 0))
    except (OSError, ValueError, TypeError):
        prev_stall = 0

    rest = None  # lazily built (only needed when a verdict is applied)
    def rest_client():
        nonlocal rest
        if rest is None:
            rest = RestClient(env.get("KANBAN_BASE_URL", "http://127.0.0.1:9119/api/plugins/kanban/"),
                              env.get("KANBAN_USERNAME", ""), env.get("KANBAN_PASSWORD", ""))
        return rest

    boards = args.boards or all_boards(mcp)
    processed = 0
    warnings = []
    seen_review_required = False  # stall alert: any review-required ticket seen this tick
    try:
        for board in boards:
            if processed >= args.max_tickets:
                break
            try:
                blocked = blocked_tickets(mcp, board)
            except McpError as exc:
                warnings.append("board %s: %s" % (board, exc))
                continue
            for item in blocked:
                if processed >= args.max_tickets:
                    break
                tid = item.get("id", "")
                if not tid:
                    continue
                try:
                    detail = ticket_detail(mcp, board, tid)
                except McpError as exc:
                    warnings.append("%s/%s: %s" % (board, tid, exc))
                    continue
                if not is_review_required(detail):
                    continue
                seen_review_required = True
                fp = block_fingerprint(detail)
                if ledger_has(board, tid, fp):
                    continue  # this block event already reviewed
                if not lock_acquire(board, tid, os.getpid()):
                    continue  # already being reviewed
                try:
                    if args.scan:
                        print("SCAN: %s/%s %s (%s)" % (board, tid, detail.get("title", ""),
                                                       detail.get("block_reason") or detail.get("last_run_summary") or ""))
                        lock_release(board, tid)
                        continue
                    ok = process_one(mcp, rest_client, board, tid, detail, fp, conf, args)
                    processed += 1
                    if ok is not True:
                        warnings.append(str(ok))
                finally:
                    lock_release(board, tid)
    finally:
        if not args.scan:
            singleton_release(os.getpid())

    for w in warnings:
        print("review-sweeper: warn: %s" % w)

    # silent-blindness alert (2026-08-07): when the MCP bridge rate-limited,
    # every tick found zero tickets while exiting 0 -- blindness with no
    # signal. N consecutive empty ticks WITH scan errors => BROKEN loudly.
    stall = stall_update(prev_stall, seen_review_required, bool(warnings))
    try:
        with open(STALL_FILE, "w", encoding="utf-8") as fh:
            json.dump({"empty_ticks": stall, "ts": int(time.time())}, fh)
    except OSError:
        pass
    if stall >= STALL_THRESHOLD:
        print("review-sweeper: BROKEN: %d consecutive ticks found zero review-required "
              "tickets while MCP/dashboard calls errored (rate-limited or blind?) — "
              "last errors: %s" % (stall, warnings[-3:]))
        return 1
    return 0


def process_one(mcp, rest_client, board, tid, detail, fp, conf, args):
    """Review one ticket. Returns True on success or a warning string."""
    blob = ticket_blob(detail)
    branch = extract_branch(blob)
    sha = extract_sha(blob)
    # conf (per-board repo map) is authoritative; ticket text is the fallback
    # so a repo spelled out in the body still wins when the board is unmapped.
    repo, base = repo_for_board(board, conf)
    if not repo:
        repo = extract_repo(blob)
    base = base or "main"

    if branch and repo and not branch_on_github(repo, branch):
        print("review-sweeper: warn: %s/%s: branch %r not on origin for %r — push pending? "
              "retry next tick (stale summary refs surface here, not as a silent skip)"
              % (board, tid, branch, repo))
        return True  # push pending; silent retry next tick
    # branch without a repo: the reviewer (now ALWAYS spawned with terminal)
    # tries to locate the code from the ticket context and ESCALATEs if it
    # cannot (never silently skipped). t_ccc76018 root cause: repo-less
    # tickets were spawned file-only, so branch verification was impossible.

    # one shared clone per board, fetched before the review — reviewers work
    # inside it (fetch by SHA/branch) instead of cloning or hunting.
    checkout = ensure_checkout(board, repo, base) if repo else ""
    prompt = build_reviewer_prompt(board, tid, detail, repo, branch, sha, checkout, base)
    if args.dry_run:
        print("DRY-RUN: would review %s/%s (%s)" % (board, tid, detail.get("title", "")))
        return True
    # ALWAYS terminal+file: reviewers need git/gh even on docs tickets (CI
    # check-runs, commit lookup), and a file-only spawn cannot verify a branch
    # ref (the 2026-08-07 t_ccc76018 ESCALATE). Read-only discipline is
    # enforced by the prompt's HARD RULES, not by toolset removal.
    toolsets = "terminal,file"
    ok, output, reviewer_pid = spawn_reviewer(prompt, toolsets)
    lock_set_reviewer(board, tid, reviewer_pid)  # straggler reaping handle
    save_review_output(board, tid, output)
    verdict = parse_verdict(output) if ok else None
    if not verdict:
        if not ok:
            print("review-sweeper: warn: %s/%s: reviewer failed — left blocked, retry next tick" % (board, tid))
            print("review-sweeper: reviewer output tail:\n%s" % output[-800:])
        else:
            apply_verdict(mcp, rest_client(), board, tid, detail, "ESCALATE", output)
            ledger_append(board, tid, "ESCALATE", "blocked", fp)
            print("review-sweeper: %s/%s: verdict missing/unparseable -> ESCALATE (left blocked)" % (board, tid))
        return True
    try:
        status_after = apply_verdict(mcp, rest_client(), board, tid, detail, verdict, output)
        ledger_append(board, tid, verdict, status_after, fp)
        print("review-sweeper: %s/%s -> %s (%s)" % (board, tid, verdict, status_after))
        return True
    except McpError as exc:
        return "%s/%s: apply failed (%s) — ticket left as-is; retry next tick" % (board, tid, exc)


def stall_update(prev: int, seen_review_required: bool, has_warnings: bool) -> int:
    """Tick the consecutive-empty counter for the silent-blindness alert.

    Resets when any review-required ticket was seen or the scan was clean;
    increments when the scan found nothing AND errored. Pure — unit-testable.
    """
    if seen_review_required or not has_warnings:
        return 0
    return prev + 1


def parse_args(argv):
    import argparse
    p = argparse.ArgumentParser(description="kanban review-required sweeper")
    p.add_argument("--scan", action="store_true", help="inventory review-required tickets, no review")
    p.add_argument("--dry-run", action="store_true", help="report what would be reviewed, spawn nothing")
    p.add_argument("--board", help="restrict to one board")
    p.add_argument("--max-tickets", type=int, default=MAX_TICKETS_PER_RUN)
    a = p.parse_args(argv)
    return argparse_namespace(scan=a.scan, dry_run=a.dry_run,
                              boards=[a.board] if a.board else None,
                              max_tickets=a.max_tickets)


class argparse_namespace:
    def __init__(self, **kw):
        self.__dict__.update(kw)


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except Exception as exc:
        print("review-sweeper: BROKEN: %s" % exc)
        sys.exit(1)
