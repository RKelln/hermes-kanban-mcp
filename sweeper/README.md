# review_sweeper — kanban review-required sweeper

A silent cron job that reviews review-required kanban tickets end-to-end: it
scans every board, spawns a real reviewer session per ticket, and applies the
verdict (done / ready / leave-blocked). Adopted into this repo from the live
Hermes host (2026-08-07) so every future change goes through the normal branch
→ review → merge lane discipline instead of unversioned host edits.

> Note: `review_sweeper.py` is byte-identical to the live host source
> (sha256-pinned) and its header docstring still says "every 15m" — the actual
> pinned cadence is every 1 minute (see Cadence). The stale header is kept for
> fidelity; trust the Cadence section.

## Flow

1. **Scan** — `board_list` for every board, then `ticket_list` (blocked) per
   board, then `ticket_get` per candidate. A ticket is review-required when its
   block reason / summaries carry `review-required`.
2. **Branch-verify** — if the ticket names a branch and the board is mapped to
   a repo, confirm the branch exists on origin (gh API first, public
   api.github.com fallback). A missing branch is a silent skip (push pending);
   a 403/429/5xx prints a warn (never silent blindness).
3. **Reviewer spawn** — one `hermes chat -q -s sdlc-review` session per ticket
   (terminal+file toolsets, 40-turn / 30-min caps), working inside a shared
   per-board clone fetched before the review.
4. **Verdict** — `APPROVE` → comment + PATCH done; `REQUEST_CHANGES` → comment
   + PATCH ready (external lane re-claims); `ESCALATE` → leave blocked (human
   inbox), comment once.

## Runtime state

State dir: `~/.hermes/state/review-sweeper/`

- `sweeper.pid` — single-instance guard (one sweeper per host)
- `ledger.jsonl` — dedup: one line per applied verdict, keyed by block
  fingerprint so a re-blocked ticket gets re-reviewed
- `locks/` — per-ticket review locks (stale takeover kills orphaned reviewers)
- `checkouts/<board>` — one shared clone per mapped board
- `stall.json` — consecutive-empty-tick counter (silent-blindness alert)
- `reviews/*.log` — saved reviewer output per review (audit/debug)

## Environment

Read from `/etc/kanban-mcp.env` (the same file the MCP server uses):

- `MCP_BEARER_TOKEN` — **required**; script prints BROKEN and exits 1 without it
- `MCP_RATE_LIMIT` — **must be generous** (this host: 120). If the bridge
  429s the sweeper, every tick finds zero tickets while exiting 0 — silent
  blindness (the stall.json alert exists for exactly this).
- `KANBAN_BASE_URL` / `KANBAN_USERNAME` / `KANBAN_PASSWORD` — optional; only
  used for the dashboard REST transitions when a verdict is applied (lazy).
- Optional `GH_TOKEN` — makes the gh branch-check authenticated.

## Conf file

`~/.config/review-sweeper.conf`, one line per board:

```
<board-slug> <https-repo-url> [default-branch]
```

`#` comments ignored; the 2-column form defaults the branch to `main`. Repos
MUST be HTTPS read-only. Actual host content (public URLs, safe to reuse):

```
hermes-agent https://github.com/RKelln/hermes-kanban-mcp.git main
togather https://github.com/Togather-Foundation/server.git main
bard https://github.com/RKelln/bard.git main
covenant https://github.com/RKelln/covenant.git main
review-sweeper-e2e https://github.com/RKelln/hermes-kanban-mcp.git main
```

## Cadence (PINNED)

`* 0-1,6-20 * * *` — every 1 minute, skipping the DeepSeek-peak hours
(UTC 1–3 and 6–9) when spawned reviewers are double-priced. The cron still
fires those minutes; the script itself no-ops via `is_peak_hour()`. The script
self-checks the cron schedule in `~/.hermes/cron/jobs.json` and exits BROKEN if
it drifts — keep that guard.

## Tests

```sh
make test-sweeper
```

Runs the offline unit suite (verdict parser, repo/branch hardening, ledger +
locks, conf/SHA/stall, apply-path). The tests resolve the module from the
repo's `sweeper/` copy first (repo-relative sys.path insert), so `make
test-sweeper` always exercises the pinned in-repo source even on the host
where `~/.hermes/scripts/review_sweeper.py` still exists. `unit3.py` globs the
host runtime reviews dir, so it is vacuous (exit 0) in-repo — not real coverage
there. The `sweeper/tests/review-sweeper-e2e-*.py` and `*-repro.py` scripts
need a live kanban board + host creds and are run manually on the host — see
deploy/review-sweeper.md.

Per-tick bound: the run processes at most `MAX_TICKETS_PER_RUN` (2) tickets, so
a queue larger than that drains over successive ticks by design.

## Hardcoded paths → env (documented follow-up)

Runtime paths are hardcoded today (ENV_FILE, STATE_DIR, CONF_FILE, GH_BIN,
GIT_BIN). Making them env-overridable is a tracked follow-up, not part of the
adoption ticket — do not change runtime paths in this move.
