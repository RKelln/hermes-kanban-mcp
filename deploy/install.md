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
manages, and it must be able to run the `hermes` CLI for the claim and typed
block shell-outs (`HERMES_BIN`). It is usually the account that owns the kanban
dashboard (e.g. `experimance`), not `root` and not a purpose-made system user.

Then:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now kanban-mcp
```

Check it came up:

```sh
systemctl status kanban-mcp --no-pager
```

### ProtectHome and `hermes kanban claim`

The shipped unit sets `ProtectHome=read-only` deliberately: the bridge only
needs to READ Hermes state under the service user's home while proxying REST
calls, and full write access to `$HOME` would let a compromised bridge rewrite
the operator's entire home directory for no functional gain.

The known trade-off is **not** silently relaxed: `hermes kanban claim` (and
typed `hermes kanban block`) shell out via `$HERMES_BIN` and WRITE to
`~/.hermes/kanban.db` under the service user's home. Under
`ProtectHome=read-only` those writes fail. If claim support is required and the
write cannot be avoided:

- Either set `ProtectHome=false` in the unit and document why it is loosened
  here (service user needs write access to `~/.hermes/kanban.db` for claim
  shell-outs), or
- prefer a narrower carve-out once `__SERVICE_USER__` is known: keep
  `ProtectHome=read-only` and add
  `ReadWritePaths=/home/<service-user>/.hermes/kanban.db`.

The decision belongs in this file and in the unit comment — never loosen it
silently. If the claim shell-out is unavailable, `ticket_claim` degrades to
comment-only advisory mode (per `MCP_ALLOW_SKIP_CLAIM`) rather than failing
hard.

## 5. Smoke test

With the service running, from the same host:

```sh
URL=http://127.0.0.1:9130 MCP_BEARER_TOKEN=<token> scripts/smoke.sh
```

The script asserts, in order: `/healthz` → 200; unauthenticated and
wrong-token `/mcp` calls → 401 with a JSON body and `WWW-Authenticate: Bearer`;
an `initialize` with the correct token → 200; `tools/list` → all 8 tool names
(`board_list, ticket_list, ticket_get, ticket_claim, ticket_comment,
ticket_complete, ticket_block, ticket_create`); `board_list` → includes
`hermes-agent`. It also runs the secret-hygiene checks (no bearer token,
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

After editing, restart opencode and confirm the 8 `kanban_*` tools are listed
in a session.

## Rollback

```sh
sudo systemctl disable --now kanban-mcp
sudo rm /usr/local/bin/kanban-mcp /etc/systemd/system/kanban-mcp.service /etc/kanban-mcp.env
sudo systemctl daemon-reload
```

The kanban backend and Hermes kernel are untouched by this service, so
disabling it fully restores the prior state.
