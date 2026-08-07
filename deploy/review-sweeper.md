# Installing the review-sweeper

The review-sweeper is a silent cron job that reviews `review-required` kanban
tickets end-to-end (scan → branch-verify → reviewer spawn → verdict). It lives
in the repo at `sweeper/review_sweeper.py`; this page covers installing it on
the Hermes host and keeping the ops invariants that keep it from going blind.

## 1. Install the script

```sh
install -m 0755 sweeper/review_sweeper.py ~/.hermes/scripts/review_sweeper.py
```

It resolves on the host under `~/.hermes/scripts/` (the cron `no_agent` script
mode resolves it there).

## 2. Env file — MCP_RATE_LIMIT is a hard requirement

The sweeper reads `/etc/kanban-mcp.env` (the same file the MCP server uses):

- `MCP_BEARER_TOKEN` — REQUIRED. Without it the script prints BROKEN and exits 1.
- `MCP_RATE_LIMIT` — **must be generous** (this host: 120). The MCP bridge
  rate-limits per IP, and the sweeper runs as 127.0.0.1. When the bridge 429s,
  every tick finds zero tickets while exiting 0 — silent blindness. The
  `stall.json` counter (3+ consecutive empty ticks WITH scan errors → BROKEN)
  exists exactly for this; do not raise MCP_RATE_LIMIT beyond what remote
  clients tolerate without accounting for the sweeper's per-minute scans.
  (Note: the checked-in `deploy/kanban-mcp.env.example` still ships the older
  default `MCP_RATE_LIMIT=60` — this host runs 120.)
- `KANBAN_BASE_URL` / `KANBAN_USERNAME` / `KANBAN_PASSWORD` — optional, used
  only for the dashboard REST transitions (lazy).
- Optional `GH_TOKEN` — set it in the cron process env (or `gh auth`) to make
  the branch check authenticated. The script itself reads no secrets file; only
  the `gh` CLI honors `GH_TOKEN`. Without it the check falls back to
  unauthenticated api.github.com (public repos work; orgs where the PAT 403s
  are covered by the fallback).

## 3. Conf file

`~/.config/review-sweeper.conf` — one line per board:

```
<board-slug> <https-repo-url> [default-branch]
```

2-column form defaults the branch to `main`; `#` comments ignored; HTTPS
read-only only. See `sweeper/README.md` for the live host content.

## 4. Cron entry (PINNED schedule)

`no_agent` script mode, `deliver: local`, enabled_toolsets
`[terminal, file, delegation]`, schedule **PINNED** to:

```
* 0-1,6-20 * * *
```

Every 1 minute, skipping the DeepSeek-peak hours (UTC 1–3 and 6–9). The cron
still fires those minutes; the script itself no-ops via `is_peak_hour()`. The
script self-checks the schedule in `~/.hermes/cron/jobs.json` and exits BROKEN
on drift — **keep that guard** (a worker once changed it to */15 post-approval,
silently slowing pickup from 1m to 15m).

Reviewer spawn: `hermes chat -q -s sdlc-review -t terminal,file` (the script
invokes it with `-Q --max-turns 40 --reasoning low`, 30-min cap). Requires the
`hermes` CLI at its configured absolute path (`HERMES_BIN`) and the
`sdlc-review` skill installed on the host. The spawn command is intended to
become env-configurable (default unchanged) as a follow-up.

## 5. State dir

`~/.hermes/state/review-sweeper/` — created on first run:

- `sweeper.pid` — single-instance guard (one sweeper per host)
- `ledger.jsonl` — dedup ledger (one line per applied verdict, keyed by block
  fingerprint)
- `locks/` — per-ticket review locks (stale takeover + orphaned-reviewer kill)
- `checkouts/<board>` — one shared clone per mapped board
- `stall.json` — consecutive-empty-tick counter
- `reviews/*.log` — saved reviewer output

## 6. Branch-check semantics (never silent)

`branch_on_github` normalizes full-form repos (owner/repo, .git suffixes) and
quotes only the branch segment (slash-branches). 404 → False silently (legit
push-pending); 403/429/5xx → False **with a printed warn** so transient API
failures can never silently skip tickets.

## 7. Tests

```sh
make test-sweeper
```

Offline unit suite (verdict parser, repo/branch hardening, ledger/locks,
conf/SHA/stall, apply-path). The `review-sweeper-e2e-*` and `-repro` scripts
need a live board + creds and are run manually on the host.

## Retirement

Disposable by design: the native review lane (upstream #49368) supersedes it
when merged. Do not gold-plate it; keep the code faithful to the live host.
