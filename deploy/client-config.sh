#!/usr/bin/env bash
# client-config.sh — print a ready-to-paste MCP client config for kanban-mcp.
#
#   deploy/client-config.sh opencode [--host H]
#   deploy/client-config.sh codex    [--host H] [--cli]
#   deploy/client-config.sh claude   [--host H]
#   deploy/client-config.sh curl     [--host H]
#   deploy/client-config.sh all      [--host H]
#
# --host defaults to 127.0.0.1 (a client on the bridge host). For a client on
# another machine pass the host's Tailscale address: `tailscale ip -4` there.
# --cli prints the `codex mcp add ...` command instead of a config.toml block.
#
# The token is read from the service env file so there is exactly one copy of it
# on the box. THE OUTPUT CONTAINS THE SECRET: pipe it to a 0600 file, or paste it
# straight into the client config. Never into a ticket, a prompt, or a repo.
#
# Override the env file with KANBAN_MCP_ENV (used by deploy/tests).

set -euo pipefail

ENV_FILE="${KANBAN_MCP_ENV:-/etc/kanban-mcp.env}"
HOST="127.0.0.1"
CLIENT=""
MODE="config"

die() { printf 'client-config: %s\n' "$*" >&2; exit 1; }

for arg in "$@"; do
  case "$arg" in
    -h|--help)
      awk 'NR>1 && /^#/ {sub(/^# ?/,""); print; next} NR>1 && !/^#/ {exit}' "$0"
      exit 0
      ;;
    --host) NEXT_HOST=1 ;;
    --cli) MODE="cli" ;;
    -*)
      die "unknown option: $arg (see --help)"
      ;;
    *)
      if [ "${NEXT_HOST:-}" = "1" ]; then HOST="$arg"; unset NEXT_HOST
      elif [ -z "$CLIENT" ]; then CLIENT="$arg"
      else die "unexpected argument: $arg"; fi
      ;;
  esac
done

[ -n "$CLIENT" ] || die "usage: client-config.sh <opencode|codex|claude|curl|all> [--host H]"
[ "${NEXT_HOST:-}" != "1" ] || die "--host needs a value"
case "$CLIENT" in
  opencode|codex|claude|curl|all) ;;
  *) die "unknown client: $CLIENT (opencode|codex|claude|curl|all)" ;;
esac
[ -r "$ENV_FILE" ] || die "cannot read env file $ENV_FILE (sudo? it is 0600)"

TOKEN="$(awk -F= '/^MCP_BEARER_TOKEN=/{sub(/^[^=]*=/,""); print; exit}' "$ENV_FILE")"
[ -n "$TOKEN" ] || die "MCP_BEARER_TOKEN not found in $ENV_FILE"

URL="http://${HOST}:9130/mcp"

header_warning() {
  cat >&2 <<'EOF'
client-config: the output below CONTAINS THE BEARER TOKEN.
  Write it to a 0600 config file, or pipe it into one. Not into a ticket,
  a prompt, a commit, or a chat message.
EOF
}

emit_opencode() {
  cat <<EOF
// ~/.config/opencode/opencode.json — merge into the existing "mcp" object
// (it holds other servers), then: chmod 0600 ~/.config/opencode/opencode.json
"kanban": {
  "type": "remote",
  "url": "$URL",
  "enabled": true,
  "oauth": false,
  "headers": { "Authorization": "Bearer $TOKEN" }
}
EOF
}

emit_codex() {
  if [ "$MODE" = "cli" ]; then
    cat <<EOF
# keeps the token out of config.toml, but MCP_BEARER_TOKEN must be in codex's
# environment when it starts:
codex mcp add kanban --url $URL --bearer-token-env-var MCP_BEARER_TOKEN
EOF
    return
  fi
  cat <<EOF
# ~/.codex/config.toml — then: chmod 0600 ~/.codex/config.toml
[mcp_servers.kanban]
url = "$URL"
http_headers = { Authorization = "Bearer $TOKEN" }
EOF
}

emit_claude() {
  cat <<EOF
# writes the server (token included) into Claude Code's own config;
# restrict that file to 0600 too
claude mcp add --transport http kanban $URL \\
  --header "Authorization: Bearer $TOKEN"
EOF
}

emit_curl() {
  cat <<EOF
# handshake + tool list, no MCP client needed
SID=\$(curl -sSi -X POST $URL \\
  -H 'Content-Type: application/json' \\
  -H 'Accept: application/json, text/event-stream' \\
  -H 'Authorization: Bearer $TOKEN' \\
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}' \\
  | tr -d '\\r' | awk -F': ' 'tolower(\$1)=="mcp-session-id"{print \$2}')
curl -s -X POST $URL \\
  -H 'Content-Type: application/json' \\
  -H 'Accept: application/json, text/event-stream' \\
  -H 'Authorization: Bearer $TOKEN' \\
  -H "Mcp-Session-Id: \$SID" \\
  -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'
curl -s -X POST $URL \\
  -H 'Content-Type: application/json' \\
  -H 'Accept: application/json, text/event-stream' \\
  -H 'Authorization: Bearer $TOKEN' \\
  -H "Mcp-Session-Id: \$SID" \\
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
EOF
}

header_warning
case "$CLIENT" in
  opencode) emit_opencode ;;
  codex)    emit_codex ;;
  claude)   emit_claude ;;
  curl)     emit_curl ;;
  all)
    printf '=== opencode ===\n'; emit_opencode
    printf '\n=== codex ===\n'; emit_codex
    printf '\n=== claude ===\n'; emit_claude
    ;;
esac
