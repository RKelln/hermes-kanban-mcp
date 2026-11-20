# Installing kanban-mcp

kanban-mcp is a static Go binary with no runtime dependencies. This page walks
through building it, installing it as a systemd service, configuring it, and
verifying the install. It is written for the target host (the machine that runs
the Hermes kanban backend); MCP clients on other machines only need the
`opencode.json` snippet at the end.

## 0. Prerequisites

- Go 1.25+ on the build machine.
- The target host runs Linux with systemd.
- The kanban dashboard REST API is reachable from the target host at
  `KANBAN_BASE_URL` (default `http://127.0.0.1:9119/api/plugins/kanban/`).
- A kanban dashboard login (`KANBAN_USERNAME` / `KANBAN_PASSWORD`).

## 1. Build the binary

From the repository root, the reproducible build is:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(git describe --tags --always --dirty)" -o kanban-mcp ./cmd/kanban-mcp
```

This produces a static binary named `kanban-mcp` in the current directory.
`-trimpath` and the `-s -w` ldflags keep the build reproducible and small;
`-X main.version=...` stamps the git describe output into the binary (visible
via `./kanban-mcp -version`).

### Cross-compiling for a different host

Build on whichever machine is convenient, but confirm the target architecture
**first** — a wrong GOARCH produces a binary that fails to exec:

```sh
uname -m    # run on the target host
```

Then set GOOS/GOARCH for the target:

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=$(git describe --tags --always --dirty)" \
  -o kanban-mcp ./cmd/kanban-mcp
```

`GOARCH` is `amd64` or `arm64` (aarch64) per `uname -m` — `x86_64` maps to
`amd64`, `aarch64` maps to `arm64`. CGO is always disabled, so the binary is
fully static and runs on the target with no shared-library dependencies.

## 2. Install the binary

```sh
sudo install -m 0755 kanban-mcp /usr/local/bin/
```

Verify it runs and reports its version:

```sh
/usr/local/bin/kanban-mcp -version
```

## 3. Write the environment file

Copy the template from the repo and edit it:

```sh
sudo install -m 0600 -o root -g root deploy/kanban-mcp.env.example /etc/kanban-mcp.env
sudo $EDITOR /etc/kanban-mcp.env
```

The file must be owned by `root:root` with mode `0600` — it holds
`KANBAN_PASSWORD` and `MCP_BEARER_TOKEN`, both secrets. Generate a fresh token
rather than reusing one:

```sh
openssl rand -hex 32
```

Set at minimum:

- `KANBAN_USERNAME` / `KANBAN_PASSWORD` — kanban dashboard login.
- `MCP_BEARER_TOKEN` — the static token MCP clients must present (≥ 16 chars,
  enforced at startup; a 64-hex-char `openssl rand -hex 32` output is ideal).
- `BIND_ADDRS` — e.g. `<tailscale-ip>:9130,127.0.0.1:9130` so both remote
  (Tailscale) and local opencode clients reach the same server. Never bind
  `0.0.0.0`; the server rejects it at startup.

Never commit this file — `.gitignore` covers `*.env`; only the `.example`
template is tracked.

## 4. Install the systemd unit

Copy the unit file:

```sh
sudo install -m 0644 deploy/kanban-mcp.service /etc/systemd/system/kanban-mcp.service
```

**Before enabling, edit `User=` in the unit** — `__SERVICE_USER__` is a
placeholder that MUST be replaced with the account that can execute `hermes`.
That account is the one whose `~/.hermes/` holds the kanban state the service
manages, and it must be able to run the `hermes` CLI for the claim, typed
block and review-request shell-outs (`HERMES_BIN`). It is usually the account
that owns the kanban dashboard (e.g. `experimance`), not `root` and not a
purpose-made system user.

Then:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now kanban-mcp
```

Check it came up:

```sh
systemctl status kanban-mcp --no-pager
```

### ProtectHome and the write shell-outs (`claim`, `block`, `request-review`)

The shipped unit sets `ProtectHome=read-only` deliberately: the bridge only
needs to READ Hermes state under the service user's home while proxying REST
calls, and full write access to `$HOME` would let a compromised bridge rewrite
the operator's entire home directory for no functional gain.

**THREE tools write through `$HERMES_BIN` and therefore need write access to
`~/.hermes/kanban.db` under this unit:** `ticket_claim` (ready→running),
`ticket_block` (typed kinds), and `ticket_request_review` (the native review
lane, the recommended completion path). All three are the same shell-out class;
a carve-out that covers one and not the others fails confusingly, because the
tools that keep working are the ones that never needed the write. The review
path issues TWO writes: `assign`/`reassign` to stamp the implementer label on
the card, then `request-review` to hand it over — a unit that permits
`request-review` but not `assign` fails at the first step, before the handoff.

The known trade-off is **not** silently relaxed: `hermes kanban claim`,
`hermes kanban block --kind`, and `hermes kanban request-review` all shell out
via `$HERMES_BIN` and WRITE to `~/.hermes/kanban.db` under the service user's
home. Under `ProtectHome=read-only` those writes fail. If any of them is
required and the write cannot be avoided:

- Either set `ProtectHome=false` in the unit and document why it is loosened
  here (service user needs write access to `~/.hermes/kanban.db` for the write
  shell-outs), or
- prefer a narrower carve-out once `__SERVICE_USER__` is known: keep
  `ProtectHome=read-only` and add
  `ReadWritePaths=/home/<service-user>/.hermes/kanban.db`.

The decision belongs in this file and in the unit comment — never loosen it
silently. If the claim shell-out is unavailable, `ticket_claim` degrades to
comment-only advisory mode (per `MCP_ALLOW_SKIP_CLAIM`) rather than failing
hard. `ticket_request_review` has no such fallback by design: it refuses and
names the reason, because a review row nobody can dispatch is worse than a
clear error.

## 5. Smoke test

With the service running, from the same host:

```sh
URL=http://127.0.0.1:9130 MCP_BEARER_TOKEN=<token> scripts/smoke.sh
```

The script asserts, in order: `/healthz` → 200; unauthenticated and
wrong-token `/mcp` calls → 401 with a JSON body and `WWW-Authenticate: Bearer`;
an `initialize` with the correct token → 200; `tools/list` → all tool names
(`board_list, ticket_list, ticket_get, ticket_claim, ticket_comment,
ticket_complete, ticket_request_review, ticket_block, ticket_create,
ticket_events, kanban_help, review_queue`); `board_list` → includes
`hermes-kanban-mcp`. It also runs the secret-hygiene checks (no bearer token,
password, or set-cookie in recent journald output; no real secrets in git). It
exits non-zero with the failing step named.

The roster is all that the script pins about `ticket_events` — it does not
exercise the wait, so a binary can pass the smoke test while serving the wrong
wait contract. `ticket_events` ends its wait on a **matching** event, on the
ticket's status CHANGING to a status the caller can act on, or on the timeout,
and reports `ticket_status` on every return. That status-change return is what
a review-lane waiter depends on (a verdict that writes no event still ends the
wait), and a binary predating it polls to the timeout instead: a wait that ends
on the change is the one call worth making by hand after an upgrade.
`wake_on` selects the matcher: `verdicts` (the default) suppresses the kernel's
own bookkeeping (heartbeat/claimed/spawned/owner_admitted/worker_bound/
assigned/reclaimed/created) and does not wake when the card moves into
`running` or `review` — that is the reviewer's own claim landing — while `any`
returns on every event (the pre-filter behaviour). Suppressed events are
reported as a count in `suppressed` — cumulative since `since_event_id` and
distinct within one call, so reusing a cursor reports the same bookkeeping
again; nothing is dropped silently. A bridge older
than this option ignores the field, so a post-upgrade probe should pass
`wake_on: "any"` and confirm a heartbeat still wakes it.

For a remote host (e.g. framework over Tailscale):

```sh
URL=http://<tailscale-ip>:9130 MCP_BEARER_TOKEN=<token> scripts/smoke.sh
```

## 6. Watch the logs

```sh
journalctl -u kanban-mcp -f
```

Structured JSON log lines to journald: `startup complete` (with the redacted
config), `listening` per bind address, and shutdown/errors. **That is all.** The
server writes no per-request access log and no `auth_failed` line at any log
level, so:

- an empty journal is not evidence that a request never arrived, and
- a client that is being rejected with 401 leaves **no trace on this host**.

Diagnose auth from the client side (its error is this bridge's JSON
`{"error":"invalid_token"}` 401) or from the connection table
(`ss -tn '( sport = :9130 )'`), never from the absence of journal lines. Secrets
are never logged — a successful install shows the token value nowhere in the
journal.

## 7. Configure MCP clients

Every client speaks the same streamable-HTTP endpoint with one static bearer
token. There is no OAuth and no `/.well-known/*` discovery, so a client that
insists on an OAuth flow cannot connect — it must accept a literal
`Authorization: Bearer <token>` header.

```
http://<host>:9130/mcp
```

- on the bridge host: `http://127.0.0.1:9130/mcp`
- from another machine: `http://<tailscale-ip>:9130/mcp` (`tailscale ip -4` on the host)

`deploy/client-config.sh` prints the block below for a given client with the real
token already substituted, so nobody hand-edits a 64-char secret:

```sh
deploy/client-config.sh opencode                  # also: codex, claude, curl
deploy/client-config.sh codex --host 100.x.y.z    # snippet for a remote machine
```

The token itself (never paste it into a ticket, a prompt, or a repo — it is
SSH-key-grade: whoever holds it can create tickets that the dispatcher will
spawn as real workers on this host):

```sh
sudo grep '^MCP_BEARER_TOKEN=' /etc/kanban-mcp.env
```

### opencode

One command, and it writes the shape opencode 2.x actually uses
(`mcp.servers.<name>`):

```sh
opencode mcp add kanban --url http://<host>:9130/mcp \
  --header "Authorization=Bearer <MCP_BEARER_TOKEN>" --global
chmod 0600 ~/.config/opencode/opencode.json
```

- The file holds the token once this is written, hence the `chmod`.
- The flat `"mcp": { "kanban": { … } }` layout is the deprecated V1 form. It
  still loads — an existing V1 entry gets hoisted into `servers` — but
  `opencode mcp add` writes the nested form, so write that one.
- **Do not verify with `opencode mcp list`.** Measured on v2.2.5: it prints
  "No MCP servers configured" while the server demonstrably works, which sends
  you hunting a config bug that does not exist. `opencode debug config` shows
  the resolved `mcp.servers` tree; the honest check is a tool call.

Restart opencode. A session should list 12 kanban tools: `board_list`,
`kanban_help`, `review_queue`, `ticket_block`, `ticket_claim`, `ticket_comment`,
`ticket_complete`, `ticket_create`, `ticket_events`, `ticket_get`, `ticket_list`,
`ticket_request_review`.

### codex

`~/.codex/config.toml` — the token comes from the environment, never from the
file:

```toml
[mcp_servers.kanban]
url = "http://<host>:9130/mcp"
bearer_token_env_var = "MCP_BEARER_TOKEN"
default_tools_approval_mode = "approve"
```

```sh
chmod 0600 ~/.codex/config.toml
```

- **`bearer_token_env_var`, NOT an inline header.** codex-cli 0.166.0 accepts
  `http_headers = { Authorization = "Bearer …" }` — `codex mcp get kanban`
  prints it straight back — and then fails to authenticate: the rmcp transport
  quits with `Transport channel closed, when AuthRequired(…)` carrying this
  bridge's `invalid_token` 401. The env-var route authenticates; measured end
  to end with a real `board_list`.
- **`default_tools_approval_mode = "approve"` is required for headless use.**
  `codex exec` cannot answer an approval prompt, so without it every call dies
  with `MCP tool call requires approval, but approval policy is never`. If a
  client should be read-only, prefer `enabled_tools` over a blanket approve.
- Something must put `MCP_BEARER_TOKEN` in codex's environment **at launch**.
  `deploy/codex-kanban.sh` reads the service env file and `exec`s codex with it
  (symlink it as `codex-kanban`). Exporting the token in a shell profile instead
  hands the credential to every child process on the box.
- Verify: `codex mcp get kanban` → `transport: streamable_http`,
  `bearer_token_env_var: MCP_BEARER_TOKEN`; then a `codex-kanban exec …` asking
  for `board_list` should report `mcp: kanban/board_list (completed)`.

### Claude Code

```sh
claude mcp add --transport http kanban http://<host>:9130/mcp \
  --header "Authorization: Bearer <MCP_BEARER_TOKEN>"
```

This writes the server (token included) into Claude Code's own config, so
restrict that file to `0600` too.

### Any other client

Set `Authorization: Bearer <token>` and
`Accept: application/json, text/event-stream`. Streamable HTTP needs a real
session before any tool call: `initialize` → keep the `Mcp-Session-Id` response
header → `notifications/initialized` → `tools/call` with that header. Responses
are SSE frames; parse every `data:` line and keep the frame whose `id` matches
the request. Skipping the handshake returns a bare HTTP 400 that reads like an
auth or payload bug.

### Rotating the token

Rotation is one file, one process restart, then every client that materialised
the token. Do it after any suspicion of exposure — a token pasted into a chat, a
ticket, a commit, or a prompt is burned.

1. Write a fresh `MCP_BEARER_TOKEN` (`openssl rand -hex 32`) into
   `/etc/kanban-mcp.env`, leaving every other key alone and the mode at `0600`.
   Back the old file up first; the CLI has no rotate verb.
2. Reload the process. systemd reads `EnvironmentFile` at start, so
   `sudo systemctl restart kanban-mcp`. Without root, and when you own the
   service account, `kill -9 "$(systemctl show -p MainPID --value kanban-mcp)"`
   works: `Restart=on-failure` brings it straight back with the new environment
   (measured — `NRestarts` increments and the listener returns).
3. Re-emit the clients that store the token **inside a config file**: opencode
   (`opencode mcp add …` again, or edit `mcp.servers.kanban.headers`), and Claude
   Code if it was added with `--header`. Clients using `bearer_token_env_var`
   (codex) need nothing — they read the file at launch.
4. Verify in both directions: the OLD token must now return
   `401 {"error":"invalid_token"}`, and the new one `200` with an
   `Mcp-Session-Id`. A rotation that only shows the new token working has not
   proved the old one is dead.

**Rotation breaks every client you forget.** The bridge validates one static
token for all of them, so a lane on another machine starts returning 401 the
moment the service restarts. Announce it before restarting, and re-emit that
machine's config in the same pass.

### If a client will not connect

| symptom | cause |
| --- | --- |
| **401** `invalid_token` | token missing or wrong — this is our middleware, on both surfaces |
| **403** `Forbidden: invalid Host header` | not auth. The Go SDK's DNS-rebinding guard fires when the connection lands on a loopback socket but the URL carries a name (an SSH tunnel, or a hosts alias). Use the tailnet address, or `127.0.0.1` at the tunnel end |
| **429** | per-IP token bucket (`MCP_RATE_LIMIT`, default 120/min, burst 20). Every local client shares one bucket, so a burst in one starves the others |
| tools listed but every result is `{}` or empty | a pre-envelope-fix build. Check the running version: the startup journal line (`journalctl -u kanban-mcp | grep version`) or `kanban-mcp -version` |

`/healthz` returns a bare `ok`; it is a liveness check, not a version check.

## Upgrading an existing install

**For an existing install, use `deploy/upgrade.py` — it is the supported path.
The manual steps below are the reference for a first install and for changing
the unit file or the env file.**

```sh
# from a worktree at the commit you mean to ship, not from the shared checkout
SHA=<merge-commit>
git -C <shared-clone> worktree add --detach "/tmp/deploy-$SHA" "$SHA"
cd "/tmp/deploy-$SHA"

python3 deploy/upgrade.py --dry-run       # preflight and plan, changes nothing
python3 deploy/upgrade.py --expect "$SHA" # build, back up, install, restart, verify
```

The detached worktree is what makes `--expect` pass on a commit merged after the
clone last checked out `main`: nothing has to move the shared checkout, and the
shared checkout stays where it belongs.

`--expect <ref>` is the flag to reach for whenever you are upgrading to ship a
particular merged commit. It asks two questions and refuses on either:

1. **Has a merge reviewed this?** `main` must already contain HEAD
   (`git merge-base --is-ancestor HEAD main`). A `main` checkout passes, and so
   does a detached checkout of any commit main contains — which is why the
   worktree above works. An already-merged feature branch passes too. A feature
   branch main has never merged does **not** pass, even though HEAD *contains*
   the named commit: every feature branch contains the commits it grew from. On
   2026-10-18 the shared checkout sat clean on `fix/t_cb0d23c0-claim-ttl` at
   6688163, which contains the merge commit e7783f8, so `--expect e7783f8` would
   have built and installed the unreviewed TTL branch and stamped it
   6688163-6688163 (t_1243c532). The dirty tree that stopped the earlier run was
   transient, so it was never the guard. The refusal names the branch and the
   commit main is missing (or says it is a detached HEAD at that commit), and
   prints the worktree command that deploys the commit you meant.
2. **Does the build include the fix?** HEAD must contain `--expect`, so a
   checkout that is behind cannot land a build predating the fix you meant to
   ship. It exists because that happened on 2026-09-14 — a redeploy served a
   pre-fix binary and reported the old version.

Local `main` is used when it exists, else `origin/main` (a fetch-only clone or a
linked worktree can lack the local branch). With neither, the script refuses and
says to fetch, rather than skipping the check.

Both questions live under `--expect`. Without it the script says so in its
output and builds whatever HEAD is, so name the commit you mean — the gate is
not a property of the tree, it is a property of the run that names a commit.

It is binary-only on purpose: it never writes the unit file (that is how the
`__SERVICE_USER__` placeholder took the service down — systemd reports the
result as `217/USER` inside an `activating (auto-restart)` loop, which reads
like a slow start) and never writes `/etc/kanban-mcp.env`. It refuses a dirty
working tree (unless `--allow-dirty`, which labels the build `…-dirty` so it is
not journal-identical to a clean build), and refuses an installed unit that
still carries a live placeholder. After the restart it reads the version the
RUNNING process logged and fails if that is not the version it just built —
because "I deployed it" and "what is running is what I built" are different
claims, and the difference is how a stale binary gets served for hours. Both
post-restart reads are bounded retries, so a slow bind is not reported as a
failed deploy. A failing smoke test fails the upgrade.

What it touches, and what it needs:

- **Writes:** `/usr/local/bin/kanban-mcp` (the install) and the service restart.
  Those are the only two privileged writes. The previous binary is copied to
  `~/.local/state/kanban-mcp/backups/kanban-mcp.bak-<timestamp>` as the operator
  (no sudo — the installed binary is world-readable), and the rollback command
  is printed on success and on failure.
- **Reads:** the unit file and the env file, to check them for the two traps.
  The env file is only read, never replaced, so new keys still belong appended
  by hand (never re-run §3 on a live host). On a host where it is `root:root
  0600` — what §3 installs — the read uses `sudo -n` (read-only, and it never
  prompts): run `sudo -v` once before a non-interactive run, and `--dry-run`
  stays prompt-free.
- **First install:** not this script. Follow sections 1–5 above.

**Do not re-run §3 on a live host.** `install -m 0600 deploy/kanban-mcp.env.example
/etc/kanban-mcp.env` OVERWRITES the target, and the example contains placeholders —
it would destroy the real `KANBAN_PASSWORD` and `MCP_BEARER_TOKEN`. Add new keys
to the live file instead.

The manual equivalent of what the script does, if you need to do it by hand
from the repo, on the reviewed commit:

```sh
# 1. build a static binary from the branch under review
git rev-parse --short HEAD          # record the SHA you are shipping
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=$(git describe --tags --always --dirty)" \
  -o kanban-mcp ./cmd/kanban-mcp

# 2. back up the live env BEFORE touching it
sudo cp -a /etc/kanban-mcp.env /etc/kanban-mcp.env.bak-$(date +%Y%m%d-%H%M)

# 3. append only the NEW key(s) — never replace the file
sudo grep -q '^MCP_REVIEWER_PROFILE=' /etc/kanban-mcp.env || \
  printf '\nMCP_REVIEWER_PROFILE=default\n' | sudo tee -a /etc/kanban-mcp.env

# 4. install the binary WITHOUT the unit, then restart
sudo install -m 0755 kanban-mcp /usr/local/bin/kanban-mcp
sudo systemctl restart kanban-mcp.service
systemctl is-active kanban-mcp.service

# 5. verify the SERVED tool roster and the running version
URL=http://127.0.0.1:9130 MCP_BEARER_TOKEN=<token> scripts/smoke.sh
journalctl -u kanban-mcp --since "-2 min" --no-pager | grep -o '"version":"[^"]*"' | tail -1
```

After the restart, confirm the redacted startup config names the reviewer profile
you expect — `MCPReviewerProfile` appears in the log line printed at boot. Then
check the two env values the review handoff needs, because it refuses without
them:

```sh
sudo grep -E '^(MCP_REVIEWER_PROFILE|MCP_CLAIM_WORKER)=' /etc/kanban-mcp.env
# MCP_REVIEWER_PROFILE=default     <- an installed profile the dispatcher can spawn
# MCP_CLAIM_WORKER=remote-mcp <- the lane label it must NEVER spawn
```

One more value to confirm on an external-lane install: `MCP_ALLOW_DEFAULT_ASSIGNEE`
must be absent or `false`. `ticket_create` REFUSES an `assignee` of `default` by
default, because that names THIS host's worker profile — an external lane's card
would be dispatched here within a tick, on a box that usually cannot push to the
project repo the ticket is about. Setting it `true` is only for a single-profile
install that deliberately wants MCP-created cards to run locally.

`MCP_CLAIM_WORKER` is not optional on this path any more (t_b917c3c6): it is the
implementer recorded on the review handoff, and a handoff with no implementer
can only ever be BLOCKED by a request-changes verdict. If it is absent, append
it — never re-run §3:

```sh
sudo grep -q '^MCP_CLAIM_WORKER=' /etc/kanban-mcp.env || \
  printf '\nMCP_CLAIM_WORKER=remote-mcp\n' | sudo tee -a /etc/kanban-mcp.env
sudo systemctl restart kanban-mcp.service
```

Then make one real `ticket_request_review` call and check the ticket reaches
`review` with a spawnable assignee, and that the event records the IMPLEMENTER
(`hermes kanban --board <slug> show <id>`). An `implementer: null` on that
event means the call returned an error and you are looking at a handoff from an
older build: the tool now refuses rather than recording null.

The reviewer is settled BEFORE the implementer stamp, because that stamp moves
the ticket's assignee: an explicit `reviewer`, else `MCP_REVIEWER_PROFILE`, else
the reviewer recorded on the ticket's last changes-requested round. With no
source at all the call is refused before anything is written: with the flag
omitted the kernel keeps whatever assignee the card carries, and the stamp has
just set that to the lane label. So the review row's assignee is always a value
verified against the installed roster, and the handoff always passes it
explicitly.

## Rollback

```sh
sudo systemctl disable --now kanban-mcp
sudo rm /usr/local/bin/kanban-mcp /etc/systemd/system/kanban-mcp.service /etc/kanban-mcp.env
sudo systemctl daemon-reload
```

The kanban backend and Hermes kernel are untouched by this service, so
disabling it fully restores the prior state.
