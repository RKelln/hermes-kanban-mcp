# Installing kanban-mcp

kanban-mcp is a static Go binary with no runtime dependencies. This page walks
through building it, installing it as a systemd service, configuring it, and
verifying the install. It is written for the target host (the machine that runs
the Hermes kanban backend); MCP clients on other machines only need the
`opencode.json` snippet at the end.

## 0. Prerequisites

- Go 1.25+ on the build machine.
- The target host runs Linux with systemd.
- The gateway's Webapp listener is running and reachable from the target host at
  `KANBAN_BASE_URL` — the plugin-prefixed origin described below.
- A login for that listener (`KANBAN_USERNAME` / `KANBAN_PASSWORD`).

### The origin: the gateway's Webapp listener

The board's REST API is served by **the gateway's own HTTP listener**, not by a
separate `hermes dashboard` process. Upstream mounts the same FastAPI app
(`hermes_cli.web_server`, which carries the kanban plugin router) onto the
gateway, so:

- the process that must be running is the gateway — on this kind of install
  `systemctl --user status hermes-gateway`, not a `hermes-dashboard` unit;
- the knob is `webapp.enabled` / `webapp.host` / `webapp.port` in the gateway
  home's `config.yaml` (upstream defaults: `false`, `127.0.0.1`, `9120`);
  `hermes webapp --status` reports whether the running gateway actually serves it,
  which is a different question from what the config says;
- `KANBAN_BASE_URL` is that listener plus the plugin prefix:
  `http://<webapp.host>:<webapp.port>/api/plugins/kanban/`. It has a compiled-in
  default that predates this move — set it explicitly to the resolved bind instead
  of relying on it (`internal/config/config.go`).

A standalone `hermes dashboard` unit, with its own `--host` / `--port`, is the
**old backend**: upstream prints a retirement notice for it on `hermes update`,
and `hermes gateway migrate --backends --apply` moves it onto the gateway's
listener (`--backends` alone only reports), carrying its host/port into `webapp.*`
so clients never move. That is why this host's `webapp.port` is `9119` (the old
backend's port) rather than the `9120` default. Do not build a dashboard unit for a
new install; on an existing one the migration is the path.

Two traps have cost real mornings. Both are about the *bind*, not the password.

**Trap 1 — a loopback webapp bind refuses session cookies on `/api/*`.**
`web_server.py` derives `app.state.auth_required` from the configured bind: `False`
for `127.0.0.1` / `::1` / `localhost`, `True` for a wildcard or a concrete address
(and for any bind once `webapp.public_url` names a public host). `/api/*` then
branches on it — gated mode accepts the login's session cookie, loopback mode
accepts only `X-Hermes-Session-Token`, the 43-character token in
`<gateway home>/webapp/session-token` that is injected into the SPA and never sent
as a cookie. So with `webapp.host: 127.0.0.1`, `POST /auth/password-login` returns
200 **with cookies** and every `/api/*` read still 401s: the symptom reads as a
wrong password, the cause is a wrong bind. It is not only a browser problem —
`kanban-mcp` is a cookie client with no session-token path, so **it cannot
authenticate against a loopback bind at all**. Point `webapp.host` at the host's
Tailscale/LAN address (or set `webapp.public_url` behind a proxy) and keep
`KANBAN_BASE_URL` on that listener. A non-loopback bind also opens a companion
socket on the loopback address *behind the same gate*, which is why
`http://127.0.0.1:<port>/api/plugins/kanban/` works here even though the bind is
the tailnet IP.

**Trap 2 — a login crash-loop defeats its own throttle.** The password login allows
10 attempts per 60 s per client IP (`dashboard_auth/routes.py`,
`_PW_RATE_MAX_ATTEMPTS`, a process-local sliding window) keyed on the ASGI peer, so
every local client shares one bucket. The bridge logs in **once, at startup**
(`kanban.NewSessionClient`, called from `main`); if that login fails the process
exits 1, and `Restart=on-failure` + `RestartSec=3` retries it ~20 times a minute.
Every restart lands inside the 10-attempt window, the window never drains, and the
bridge cannot recover on its own — and no gate can be tested while it loops. An
already-running bridge does **not** re-login: it keeps its old session and answers
`auth: Unauthorized` per tool call once that session ages out, so a rotated
password reads as a healthy bridge pointing at a broken backend until the next
restart turns it into the loop above. (The 401-refresh in
`internal/kanban/client.go` belongs to the separate `Client` that
`internal/kanban/cmd/probe` uses; the server binary runs on the raw
`*http.Client` from `NewSessionClient`.) The order that works is the one
`deploy/rotate-credentials.py` implements: **stop the bridge first**, then write the
hash, then restart the gateway, verify the new credential while the bridge is still
stopped, then start it into a drained budget.

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
- `MCP_WRITE_BEARER_TOKEN` — OPTIONAL; **leave it unset unless you want the
  read/write split.** When set (≥ 16 chars, and it must differ from
  `MCP_BEARER_TOKEN` — equal values are refused at startup), `MCP_BEARER_TOKEN`
  becomes the READ token: it opens every read tool plus `ticket_comment`, while
  `ticket_create`, `ticket_claim`, `ticket_complete`, `ticket_block` and
  `ticket_request_review` answer with a `capability_denied` tool error naming
  `MCP_WRITE_BEARER_TOKEN`. Generate it with a second `openssl rand -hex 32`.
  Unset or empty is exactly the old one-token behaviour, so nothing breaks
  until you opt in.
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

Every client speaks the same streamable-HTTP endpoint with a static bearer
token — one token on a default install, or two when the read/write split is
on (see "Which token does a client get?" below). There is no OAuth and no
`/.well-known/*` discovery, so a client that
insists on an OAuth flow cannot connect — it must accept a literal
`Authorization: Bearer ...` header.

The `:9130` endpoint below is the bridge's own face. The board's REST **origin**
behind it is the gateway's Webapp listener (§0, "the origin"), and the two fail
differently: a bridge that cannot reach or authenticate to the origin answers
`auth: Unauthorized` (or `connection refused`) on every tool call while its
bearer auth is working perfectly. Diagnose the origin before re-issuing a client
config.

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

### Which token does a client get?

With `MCP_WRITE_BEARER_TOKEN` unset there is one token and this subsection does
not apply. With it set, the same endpoint accepts two credentials, and the
difference is not cosmetic: `ticket_create` puts a card on this host's kanban
dispatcher, which spawns a real worker process for it within about a minute —
so the write token is closer to a shell credential than to a read key.

- **Read token** (`MCP_BEARER_TOKEN`) opens `board_list`, `ticket_list`,
  `ticket_get`, `ticket_events`, `review_queue`, `kanban_help` and
  `ticket_comment`. Give it to anything that observes or annotates: the review
  sweeper (`deploy/review-sweeper.md` only lists, reads and comments), a
  dashboard, an assistant asked what is on the board.
- **Write token** (`MCP_WRITE_BEARER_TOKEN`) opens all of the above **plus**
  `ticket_create`, `ticket_claim`, `ticket_complete`, `ticket_block` and
  `ticket_request_review`. Give it only to the lane that files and claims
  cards.

A write call with the read token is refused **in band**: the tool result is
`IsError` with a prose arm prefixed `capability_denied:` and a JSON arm
carrying `{"code":"write_token_required",...}` that names
`MCP_WRITE_BEARER_TOKEN` and says where to get it. It is deliberately **not** a
401 — the credential is valid, merely insufficient, and a client that reads a
bare 401 retries with the same token.

One token per client session. The capability comes from the token that opened
the MCP session (the `initialize` request), so a client holding both uses
whichever one it connected with; do not mix them inside one session.

The client blocks below are **unchanged** — they name `MCP_BEARER_TOKEN`
because that is the one credential a default install has. On a split install,
point the same header (opencode, Claude Code) or `bearer_token_env_var` (codex)
at the token that client should use:

- opencode / Claude Code: substitute the chosen token where the block below
  shows the read token. `deploy/client-config.sh` always substitutes the env
  file's `MCP_BEARER_TOKEN` — the **read** token under a split — so its output
  is only right for a read-only client; for a write client, paste the write
  token into the same header by hand
  (`sudo grep '^MCP_WRITE_BEARER_TOKEN=' /etc/kanban-mcp.env`).
- codex: `deploy/codex-kanban.sh` exports the env file's `MCP_BEARER_TOKEN` —
  the **read** token under a split — and it overwrites whatever was already in
  the environment, so a write lane cannot use that wrapper as-is. Keep
  `bearer_token_env_var = "MCP_BEARER_TOKEN"` in the config and launch `codex`
  directly with the same variable set to the write token for that one process
  (never exported globally):
  `MCP_BEARER_TOKEN="$(sudo grep '^MCP_WRITE_BEARER_TOKEN=' /etc/kanban-mcp.env | cut -d= -f2-)" codex exec …`

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
   Back the old file up first; the CLI has no rotate verb. **If the read/write
   split is on, rotate `MCP_WRITE_BEARER_TOKEN` in the same pass** — they are
   independent credentials, and rotating only the read token leaves every write
   lane's access intact, which is exactly the access worth burning.
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

**Rotation breaks every client you forget.** The bridge validates static
tokens for all of them, so a lane on another machine starts returning 401 the
moment the service restarts. Announce it before restarting, and re-emit that
machine's config in the same pass. Under the split, a client keeps the token it
was given: rotating the write token does not disturb read-only clients, and
rotating the read token does not disturb write ones.

### Rotating the dashboard password (and the bridge with it)

The dashboard password lives in two places, and only one of them is readable
back: the gateway stores a hash in `dashboard.basic_auth` (the plaintext is
unrecoverable), and `/etc/kanban-mcp.env` stores `KANBAN_PASSWORD`, which is what
the bridge logs in with. Rotate one alone and the bridge is left holding a dead
credential, so use the script:

```sh
python3 deploy/rotate-credentials.py            # generate a strong password
python3 deploy/rotate-credentials.py --prompt   # or type one (hidden input)
```

It verifies against the origin behind `KANBAN_BASE_URL` — the URL the bridge
actually dials — and refuses before changing anything when that URL disagrees with
`webapp.host` / `webapp.port`, because a moved listener with a stale env would
otherwise verify green on the new port while the bridge crash-loops on the old one
(`--origin` replaces the verified URL for a proxy or tunnel). It then runs stop →
hash → env → gateway restart → **verify** → bridge start, because a running bridge
burns the login budget before the new credential can land (§0, trap 2) and
verifying before the start means a bad value leaves nothing running to crash-loop.
It **refuses to run at all** when the Webapp is bound to loopback, since that bind
cannot authenticate the bridge (§0, trap 1) — nothing is changed in that case.

Nothing it prints is a credential: status lines carry a length and a SHA-256
prefix, so a pasted log or a ticket comment is not a leak. Without `--prompt` the
generated value is also written to `~/.hermes/kanban-mcp-operator-password` (mode
0600) so you can sign in to the Web UI — move it into your password manager, then
delete that file.

Reading the env file to confirm the sync is an explicit `sudo` read, by design.
Do not add a `NOPASSWD` helper for it: that turns "the operator can read this
file" into "anything running as the service account can", which is the whole
capability the file exists to gate.

If you rotate by hand anyway, the check is a real login **and** a
`GET /api/plugins/kanban/boards` with the resulting cookie — a 200 from the login
proves the hash, not the reach (trap 1).

### If a client will not connect

| symptom | cause |
| --- | --- |
| **401** `invalid_token` | token missing or wrong — this is our middleware, on both surfaces |
| a tool error starting `capability_denied:` on a write call | the client holds the read token — valid, but insufficient; it is refused in band, not with a 401. Give that client `MCP_WRITE_BEARER_TOKEN` (§7) |
| **403** `Forbidden: invalid Host header` | not auth. The Go SDK's DNS-rebinding guard fires when the connection lands on a loopback socket but the URL carries a name (an SSH tunnel, or a hosts alias). Use the tailnet address, or `127.0.0.1` at the tunnel end |
| **429** | per-IP token bucket (`MCP_RATE_LIMIT`, default 120/min, burst 20). Every local client shares one bucket, so a burst in one starves the others |
| tools listed but every result is `{}` or empty | a pre-envelope-fix build. Check the running version: the startup journal line (`journalctl -u kanban-mcp | grep version`) or `kanban-mcp -version` |

`/healthz` returns a bare `ok`; it is a liveness check, not a version check.

## Upgrading an existing install

**For an existing install, use `deploy/upgrade.py` — it is the supported path.
The manual steps below are the reference for a first install and for changing
the unit file or the env file.**

```sh
python3 deploy/upgrade.py --dry-run                  # preflight and plan, changes nothing
python3 deploy/upgrade.py --expect <sha-of-the-fix>  # build, back up, install, restart, verify
```

`--expect <ref>` is the flag to reach for whenever you are upgrading to ship a
particular merged commit: the script refuses unless the commit it is about to
deploy CONTAINS that ref, so a checkout that is behind cannot land a build
predating the fix you meant to ship. It exists because that happened on
2026-09-14 — a redeploy served a pre-fix binary and reported the old version.

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

### Turning the read/write split on (optional, live install)

`MCP_WRITE_BEARER_TOKEN` is optional and this section is only for opting in.
The upgrade script and §3 never rewrite an existing env file, so a live install
gains the second token by hand — back up, append, restart:

```sh
sudo cp -a /etc/kanban-mcp.env /etc/kanban-mcp.env.bak-$(date +%Y%m%d-%H%M)
sudo sh -c 'printf "\nMCP_WRITE_BEARER_TOKEN=%s\n" "$(openssl rand -hex 32)" >> /etc/kanban-mcp.env'
sudo systemctl restart kanban-mcp.service
```

- `sh -c` runs as root under `sudo`, so the generated secret never passes
  through your shell history and never appears in the command line.
- If the appended value ever equals `MCP_BEARER_TOKEN`, the service REFUSES to
  start (equal values mean no split). That failure is in the journal:
  `journalctl -u kanban-mcp --since "-2 min"`.
- Confirm it took effect without reading a secret: the boot banner now prints
  `MCPTokenMode=split` (`journalctl -u kanban-mcp --since "-2 min" | grep
  MCPTokenMode`). `MCPTokenMode=single` means the variable did not land.
- Nothing changes for a read-only client — the old token still opens every read
  tool and `ticket_comment`. Every existing client that files or claims cards
  must be given the new token (see §7); until it is, its
  `ticket_create`/`claim`/`complete`/`block`/`request_review` calls return
  `capability_denied: ... MCP_WRITE_BEARER_TOKEN`. Plan the re-issue in the
  same change window, or the lanes go read-only the moment the service
  restarts.
- Undo is one line: delete `MCP_WRITE_BEARER_TOKEN` from the env file and
  restart. Every client is immediately back to the single-token surface.

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
