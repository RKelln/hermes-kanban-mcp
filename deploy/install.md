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

`deploy/kanban-mcp.service` is a **template, not an installable unit**. It ships
`User=__SERVICE_USER__` and `ReadWritePaths=__SERVICE_HOME__/.hermes`, because
the repo cannot know which account on the target host owns `~/.hermes`. An
`install` of that file verbatim hands systemd a user that does not exist, and the
service then fails with `status=217/USER` while `systemctl status` reports only
`activating (auto-restart)` — a credential failure that reads like a slow start.
That is not hypothetical: it took the bridge down on 2026-09-14, from this page's
upgrade path, which installed the template and had no equivalent of the "edit
`User=`" warning the fresh-install path carried.

Run this block from the repo root. It is the only supported way to install or
refresh the unit, and **the fresh install and the upgrade path below use the same
block on purpose** — two copies of this step is precisely how the two paths
drifted apart the last time.

```sh
# SERVICE_USER: the account that owns ~/.hermes (the kanban state this service
# manages) and can execute `hermes` for the claim, typed-block and
# review-request shell-outs via $HERMES_BIN. It is usually the account that owns
# the kanban dashboard (e.g. experimance) — not root and not a purpose-made
# system user.
SERVICE_USER=experimance

# --- pre-flight: a wrong SERVICE_USER must fail HERE, not in a restart loop ---
id -u "$SERVICE_USER" >/dev/null 2>&1 || { echo "FATAL: no such user: $SERVICE_USER" >&2; exit 1; }
SERVICE_HOME=$(getent passwd "$SERVICE_USER" | cut -d: -f6)
[ -d "$SERVICE_HOME/.hermes" ] || { echo "FATAL: $SERVICE_HOME/.hermes missing — wrong SERVICE_USER?" >&2; exit 1; }
[ -x /usr/local/bin/kanban-mcp ] || { echo "FATAL: /usr/local/bin/kanban-mcp missing — do §2 first" >&2; exit 1; }

# --- generate the unit: substitute both placeholders, then refuse to install
# an unresolved copy (a placeholder that reaches /etc/systemd IS the 217/USER outage)
UNIT=$(mktemp)
sed -e "s|^User=__SERVICE_USER__$|User=$SERVICE_USER|" \
    -e "s|^ReadWritePaths=__SERVICE_HOME__/.hermes$|ReadWritePaths=$SERVICE_HOME/.hermes|" \
    deploy/kanban-mcp.service >"$UNIT"
if grep -n '__SERVICE' "$UNIT"; then
  echo "FATAL: a placeholder survived substitution (lines above) — template changed shape?" >&2
  rm -f "$UNIT"; exit 1
fi

# --- install only when it changed; never bounce an unchanged unit ------------
if sudo test -f /etc/systemd/system/kanban-mcp.service &&
   sudo cmp -s "$UNIT" /etc/systemd/system/kanban-mcp.service; then
  CHANGED=0
  echo "unit unchanged — no reload, no restart"
else
  sudo install -m 0644 "$UNIT" /etc/systemd/system/kanban-mcp.service
  CHANGED=1
  echo "unit installed → reload + restart"
fi
rm -f "$UNIT"

# enable is idempotent, and --now starts the unit only if it is not running, so
# re-running this block against an unchanged unit does not interrupt the bridge.
[ "$CHANGED" = 1 ] && sudo systemctl daemon-reload
sudo systemctl enable --now kanban-mcp.service
[ "$CHANGED" = 1 ] && sudo systemctl restart kanban-mcp.service   # pick up the new unit file

# --- post-restart assertion: `activating (auto-restart)` must not pass -------
for _ in $(seq 1 10); do
  systemctl is-active --quiet kanban-mcp.service && break
  sleep 1
done
systemctl is-active --quiet kanban-mcp.service || {
  echo "FATAL: kanban-mcp.service is not active. Diagnose before continuing:" >&2
  systemctl status kanban-mcp.service --no-pager --lines=0 >&2
  echo "  User=            $(systemctl show -p User --value kanban-mcp.service)" >&2
  echo "  ExecMainStatus=  $(systemctl show -p ExecMainStatus --value kanban-mcp.service)   # 217 = that User= does not exist" >&2
  echo "  ReadWritePaths=  $(systemctl show -p ReadWritePaths --value kanban-mcp.service)" >&2
  exit 1
}
```

Notes on the block:

- **`SERVICE_USER` is the only knob.** Everything else is derived (`SERVICE_HOME`
  comes from `getent passwd`, not from typing the path twice).
- **The template is never installed as-is.** The placeholder assertion runs on the
  generated file, so an unresolved `User=`/`ReadWritePaths=` cannot reach
  `/etc/systemd/system/` — the failure mode becomes a loud message at the install
  step instead of a restart loop at 03:00. The assertion greps for the token
  anywhere in the file, so the template's comments deliberately do **not** spell
  it out (only the two directive lines carry it). That keeps the post-install
  check — `grep -c __SERVICE /etc/systemd/system/kanban-mcp.service` returning
  `0` — a valid end-to-end test of the substitution.
- **Idempotent by construction.** Unchanged unit → no `install`, no
  `daemon-reload`, no `restart`; a healthy bridge is not touched. A changed unit →
  replaced, reloaded, restarted once.
- **The assertion is not `systemctl status`.** `is-active` refuses
  `activating (auto-restart)`, which is the state a 217/USER loop sits in.

If you hand-edited `/etc/systemd/system/kanban-mcp.service` to something this
block does not generate, fold that edit into `deploy/kanban-mcp.service` before
running it: the block regenerates the installed unit from the template and will
replace your edit.

### ProtectHome and the write shell-outs (`claim`, `block`, `request-review`)

The unit keeps `ProtectHome=read-only`: nothing under `$HOME` outside the Hermes
state directory is writable by the bridge, so a compromised bridge cannot rewrite
the operator's home directory. The generated unit allow-lists the one tree the
shell-outs write:

    ProtectHome=read-only
    ReadWritePaths=/home/<service-user>/.hermes    # ReadWritePaths=__SERVICE_HOME__/.hermes in the template

**Why this is part of the install and not an option left in a comment.** THREE
tools shell out through `$HERMES_BIN` and WRITE the board database:
`ticket_claim` (ready→running), `ticket_block` (typed kinds) and
`ticket_request_review` (the native review lane, the recommended completion
path). All three are the same shell-out class. Under a read-only `/home` those
writes fail at runtime — a bridge that "installed successfully" and then refuses
the tools that make it useful, while the REST-only tools keep working and make
the install look healthy.

**Why `~/.hermes` and not `~/.hermes/kanban.db`.** SQLite writes `-wal` and
`-shm` siblings next to each database, and board databases live one level deeper
(`~/.hermes/kanban/boards/<slug>/kanban.db`), so what the shell-outs need is
write access to the database *directory*, not to one file. A carve-out on
`kanban.db` alone fails the first time SQLite has to recreate the WAL. Nesting a
writable path inside a read-only one is the supported pattern — `systemd.exec`:
*"Nest ReadWritePaths= inside of ReadOnlyPaths= in order to provide writable
subdirectories within read-only directories"* — and `ProtectHome=read-only` is
documented as equivalent to read-only paths on `/home`, `/root` and `/run/user`.

`ProtectHome=false` is the other way to unblock the shell-outs, and it is not
needed: it would hand the bridge the whole home directory for a write it needs in
exactly one tree.

Verify the carve-out with a WRITE; §5's smoke test only reads. Run (a) always,
and both (a) and (b) before calling an upgrade done:

```sh
# (a) the unit's own sandbox, with the unit's own settings (no drift, no board
#     effect): a write inside ~/.hermes as the service user must succeed.
SVC_USER=$(systemctl show -p User --value kanban-mcp.service)
sudo systemd-run --unit=kanban-mcp-write-probe --collect \
  --property=User="$SVC_USER" \
  --property=ProtectHome="$(systemctl show -p ProtectHome --value kanban-mcp.service)" \
  --property=ReadWritePaths="$(systemctl show -p ReadWritePaths --value kanban-mcp.service)" \
  /bin/sh -c 'touch "$HOME/.hermes/.write-probe" && rm -f "$HOME/.hermes/.write-probe" && echo "WRITE OK"'
```

```sh
# (b) the real path: one WRITE through the bridge, on a ticket you were about to
#     move anyway. ticket_claim and ticket_request_review shell out to
#     `hermes kanban …` and rewrite the board DB; ticket_list and the other read
#     tools never touch it, which is why they are not the check.
#       ticket_claim          on a READY ticket    -> status: running
#       ticket_request_review on a RUNNING ticket  -> status: review, with a
#                                                     spawnable assignee
```

A read-only failure names itself: the bridge returns the CLI's stderr. If it
appears, `systemctl show -p ReadWritePaths --value kanban-mcp.service` shows
whether the carve-out reached the installed unit. If the claim shell-out is
unavailable, `ticket_claim` degrades to comment-only advisory mode (per
`MCP_ALLOW_SKIP_CLAIM`) rather than failing hard. `ticket_request_review` has no
such fallback by design: it refuses and names the reason, because a review row
nobody can dispatch is worse than a clear error.

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

For a remote host (e.g. framework over Tailscale):

```sh
URL=http://<tailscale-ip>:9130 MCP_BEARER_TOKEN=<token> scripts/smoke.sh
```

## 6. Watch the logs

```sh
journalctl -u kanban-mcp -f
```

Structured JSON log lines to journald: startup (with the redacted config),
per-request access logs (`method`, `path`, `status`, `duration_ms`), and
`outcome=auth_failed` on bad tokens. Secrets are never logged — a successful
install shows the token value nowhere in the journal.

## 7. Configure the MCP client (opencode)

On the machine running opencode, add the server to
`~/.config/opencode/opencode.json`:

```json
{ "mcp": { "kanban": { "type": "remote", "url": "http://<tailscale-ip>:9130/mcp", "enabled": true, "oauth": false, "headers": { "Authorization": "Bearer <MCP_BEARER_TOKEN>" } } } }
```

- `oauth: false` means the client uses the **static bearer token** in `headers`
  as-is — it does not attempt dynamic OAuth registration with the server
  (kanban-mcp does not implement OAuth or `/.well-known/*` endpoints).
- The client config file contains the token, so restrict it:

```sh
chmod 0600 ~/.config/opencode/opencode.json
```

After editing, restart opencode and confirm the 12 `hermes-kanban-*` tools are listed
in a session.

## Upgrading an existing install

**Do not re-run §3 on a live host.** `install -m 0600 deploy/kanban-mcp.env.example
/etc/kanban-mcp.env` OVERWRITES the target, and the example contains placeholders —
it would destroy the real `KANBAN_PASSWORD` and `MCP_BEARER_TOKEN`. Add new keys
to the live file instead.

From the repo, on the reviewed commit:

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

# 4. install the binary
sudo install -m 0755 kanban-mcp /usr/local/bin/kanban-mcp
```

**5. Refresh the unit with §4's block** — do NOT `sudo install -m 0644
deploy/kanban-mcp.service /etc/systemd/system/kanban-mcp.service`. The tracked
file is a *template*: `User=__SERVICE_USER__` is not an account, and installing it
verbatim is exactly what took the bridge down on 2026-09-14. systemd could not
resolve the user, the unit exited `status=217/USER`, and 100+ restarts passed as
`activating (auto-restart)` — which reads like a slow start, not a credential
failure.

Remove the placeholder the old upgrade path may have left in the installed unit
before you start — check with:

```sh
sudo grep -n '__SERVICE' /etc/systemd/system/kanban-mcp.service   # must print nothing
```

§4's block is deliberately the same code path a fresh install uses. What it gives
this step:

- the **pre-flight**: `id -u "$SERVICE_USER"` and a `~/.hermes` check fail loudly
  *before* anything is installed or restarted, so a typo cannot become a restart
  loop;
- the **placeholder assertion**: a unit whose `User=`/`ReadWritePaths=` were not
  substituted never reaches `/etc/systemd/system/`;
- the **change gate**: the unit is installed only when the generated file differs
  from the installed one, and `enable --now` leaves a running service alone — so
  re-running this section after a binary-only or docs-only change does not
  interrupt the bridge;
- the **post-restart assertion**: `systemctl is-active` (which refuses
  `activating (auto-restart)`) plus the 217/USER diagnostics
  (`systemctl show -p User -p ExecMainStatus`) if it fails.

If the block does report a failure, those three reads name the cause without
digging through the journal:

```sh
systemctl is-active kanban-mcp.service                        # activating = still in the restart loop
systemctl show -p User --value kanban-mcp.service             # __SERVICE_USER__ = the placeholder is installed
systemctl show -p ExecMainStatus --value kanban-mcp.service   # 217 = no such user
```

**6. Prove the write shell-outs work — with a WRITE, not a read.** `ticket_claim`,
`ticket_block` and `ticket_request_review` shell out through `$HERMES_BIN` and
rewrite the board database under the service user's home; a read-only `/home`
breaks them after a "successful" install (see §4's ProtectHome section). Run
§4's sandbox probe, then one real call on a ticket you were about to move anyway:
`ticket_claim` on a READY ticket (expect `status: running`), or
`ticket_request_review` on a RUNNING one (expect the ticket in `review` with a
spawnable assignee — confirm with `hermes kanban --board <slug> show <id>`).

**7. Verify the SERVED tool roster** (now 12 names, including
`ticket_request_review`):

```sh
URL=http://127.0.0.1:9130 MCP_BEARER_TOKEN=<token> scripts/smoke.sh
```

After the restart, confirm the redacted startup config names the reviewer profile
you expect — `MCPReviewerProfile` appears in the log line printed at boot.

## Rollback

```sh
sudo systemctl disable --now kanban-mcp
sudo rm /usr/local/bin/kanban-mcp /etc/systemd/system/kanban-mcp.service /etc/kanban-mcp.env
sudo systemctl daemon-reload
```

The kanban backend and Hermes kernel are untouched by this service, so
disabling it fully restores the prior state.
