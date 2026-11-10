#!/usr/bin/env bash
# codex-kanban — launch codex with the kanban bridge's bearer token in its
# environment, so `bearer_token_env_var = "MCP_BEARER_TOKEN"` resolves.
#
#   codex-kanban                      # plain codex, with the token available
#   codex-kanban exec --skip-git-repo-check "…"
#
# Why a wrapper rather than exporting the token in a shell profile: `codex`
# needs the variable at process start, but exporting it globally hands the
# credential to every child process on the box, including any agent that can
# read its own environment. This sets it for the one process it launches and
# keeps the single copy of the secret in the service env file.
#
# The token is reused from /etc/kanban-mcp.env so rotation is one file, not
# one file per client. Override with KANBAN_MCP_ENV.

set -euo pipefail

ENV_FILE="${KANBAN_MCP_ENV:-/etc/kanban-mcp.env}"

die() { printf 'codex-kanban: %s\n' "$*" >&2; exit 1; }

[ -r "$ENV_FILE" ] || die "cannot read $ENV_FILE"
TOKEN="$(awk -F= '/^MCP_BEARER_TOKEN=/{sub(/^[^=]*=/,""); print; exit}' "$ENV_FILE")"
[ -n "$TOKEN" ] || die "MCP_BEARER_TOKEN not found in $ENV_FILE"

MCP_BEARER_TOKEN="$TOKEN" exec codex "$@"
