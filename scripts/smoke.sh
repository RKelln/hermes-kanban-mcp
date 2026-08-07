#!/usr/bin/env bash
#
# smoke.sh — end-to-end smoke test for the kanban-mcp MCP server.
#
# Usage:
#   scripts/smoke.sh <URL> <MCP_BEARER_TOKEN>
#   URL=<url> with the MCP_BEARER_TOKEN env var set
#
# Asserts, in order:
#   1. GET /healthz returns 200.
#   2. POST /mcp with no Authorization header returns 401 with
#      Content-Type: application/json and a WWW-Authenticate: Bearer header.
#   3. POST /mcp with a wrong bearer token returns 401.
#   4. GET /mcp with no token returns 401.
#   5. POST /mcp initialize with the correct token returns 200 with an
#      inline-JSON or inline-SSE result.
#   6. tools/list exposes every kanban tool.
#   7. board_list includes the hermes-agent board.
#
# Then secret-hygiene checks: a scan of the kanban-mcp journal (last
# 10 minutes) for leaked credentials, and a git grep of the repository
# for committed secret values. Any failure prints a named FAIL line to
# stderr and exits non-zero.
#
# Notes on secret handling: the real bearer token is never placed on a
# command line (it would be visible in `ps`); it is written to a 0600
# temp file and passed to curl via -H @file. The script never prints
# the token value. The script also never assigns a value to the two
# secret variable names directly in source (the repository hygiene scan
# would flag its own source otherwise).
set -euo pipefail

# --- argument handling: two positional args, or URL + MCP_BEARER_TOKEN env ---
if [[ $# -ge 2 ]]; then
    URL=$1
    BEARER_TOKEN=$2
else
    URL=${URL:-}
    BEARER_TOKEN=${MCP_BEARER_TOKEN:-}
fi

if [[ -z $URL ]]; then
    echo "FAIL: usage: $0 <URL> <MCP_BEARER_TOKEN>, or set the URL and MCP_BEARER_TOKEN environment variables" >&2
    exit 1
fi
if [[ -z $BEARER_TOKEN ]]; then
    echo "FAIL: usage: $0 <URL> <MCP_BEARER_TOKEN>, or set the URL and MCP_BEARER_TOKEN environment variables" >&2
    exit 1
fi

URL=${URL%/}                 # tolerate a trailing slash
MCP_URL="$URL/mcp"

# --- scratch dir + auth header file, cleaned up on exit ----------------------
WORKDIR=$(mktemp -d "${TMPDIR:-/tmp}/kanban-smoke.XXXXXX")
AUTH_HDR="$WORKDIR/auth.hdr"
trap 'rm -rf "$WORKDIR"' EXIT
printf 'Authorization: Bearer %s\n' "$BEARER_TOKEN" > "$AUTH_HDR"
chmod 600 "$AUTH_HDR"

# fail <named-step-message> — print FAIL line to stderr and exit non-zero
fail() {
    echo "FAIL: $*" >&2
    exit 1
}

SESSION_ID=""                # set from initialize response when the server issues one

# mcp_req <auth: none|wrong|good> <json-body> <out-body-file> <out-headers-file>
# POSTs a JSON-RPC message to /mcp and echoes the HTTP status code.
# With auth=good the real token is used (via the 0600 header file); with
# auth=wrong a deliberately bogus token is sent; with auth=none no
# Authorization header is sent at all.
mcp_req() {
    local auth=$1 json=$2 out_body=$3 out_hdr=$4
    local -a hdr=()
    if [[ $auth == good ]]; then
        hdr+=(-H "@$AUTH_HDR")
    elif [[ $auth == wrong ]]; then
        hdr+=(-H "Authorization: Bearer smoke-wrong-token")
    fi
    if [[ -n $SESSION_ID ]]; then
        hdr+=(-H "Mcp-Session-Id: $SESSION_ID")
    fi
    curl -sS --max-time 10 -o "$out_body" -D "$out_hdr" -w '%{http_code}' \
        -X POST "$MCP_URL" "${hdr[@]}" \
        -H "Content-Type: application/json" \
        -H "Accept: application/json, text/event-stream" \
        --data "$json"
}

# extract_json <file> — prints the JSON-RPC payload from a response body
# that is either inline JSON (application/json) or an SSE stream
# (text/event-stream with `data:` lines).
extract_json() {
    local f=$1
    if grep -q '^data:' "$f"; then
        grep '^data:' "$f" | sed 's/^data:[[:space:]]*//'
    else
        cat "$f"
    fi
}

# --- 1. healthz ---------------------------------------------------------------
echo "check 1/7: GET /healthz -> 200"
code=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' "$URL/healthz") \
    || fail "step 1 curl to $URL/healthz failed (is the server up?)"
[[ $code == 200 ]] || fail "step 1 GET /healthz should return 200, got $code"
echo "  ok: /healthz returned 200"

# --- 2. POST /mcp with no auth -> 401 + JSON + WWW-Authenticate ---------------
echo "check 2/7: POST /mcp with no Authorization -> 401"
BODY="$WORKDIR/s2-body"; HDR="$WORKDIR/s2-hdr"
INIT='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'
code=$(mcp_req none "$INIT" "$BODY" "$HDR") \
    || fail "step 2 curl to $MCP_URL failed (is the server up?)"
[[ $code == 401 ]] || fail "step 2 POST /mcp with no Authorization should return 401, got $code"
grep -qi '^content-type: application/json' "$HDR" \
    || fail "step 2 401 response is missing Content-Type: application/json"
grep -qi '^www-authenticate: *Bearer' "$HDR" \
    || fail "step 2 401 response is missing a WWW-Authenticate: Bearer header"
echo "  ok: 401 with JSON content-type and WWW-Authenticate: Bearer"

# --- 3. POST /mcp with a wrong token -> 401 ------------------------------------
echo "check 3/7: POST /mcp with a wrong bearer token -> 401"
BODY="$WORKDIR/s3-body"; HDR="$WORKDIR/s3-hdr"
code=$(mcp_req wrong "$INIT" "$BODY" "$HDR") \
    || fail "step 3 curl to $MCP_URL failed (is the server up?)"
[[ $code == 401 ]] || fail "step 3 POST /mcp with a wrong bearer token should return 401, got $code"
echo "  ok: wrong token rejected with 401"

# --- 4. GET /mcp with no token -> 401 -------------------------------------------
echo "check 4/7: GET /mcp with no token -> 401"
code=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' "$MCP_URL") \
    || fail "step 4 curl GET to $MCP_URL failed (is the server up?)"
[[ $code == 401 ]] || fail "step 4 GET /mcp with no token should return 401, got $code"
echo "  ok: unauthenticated GET rejected with 401"

# --- 5. initialize with the correct token -> 200, inline-JSON or inline-SSE ----
echo "check 5/7: POST /mcp initialize with correct token -> 200"
BODY="$WORKDIR/s5-body"; HDR="$WORKDIR/s5-hdr"
code=$(mcp_req good "$INIT" "$BODY" "$HDR") \
    || fail "step 5 curl to $MCP_URL failed (is the server up?)"
[[ $code == 200 ]] || fail "step 5 POST /mcp initialize should return 200, got $code"
if grep -qi '^content-type: application/json' "$HDR"; then
    ct=application/json
elif grep -qi '^content-type: text/event-stream' "$HDR"; then
    ct=text/event-stream
else
    fail "step 5 initialize returned an unexpected Content-Type (expected application/json or text/event-stream)"
fi
RESP=$(extract_json "$BODY")
grep -q '"result"' <<<"$RESP" || fail "step 5 initialize response has no result member"
grep -q '"serverInfo"' <<<"$RESP" || fail "step 5 initialize result is missing serverInfo"
# Capture a session id if the server issued one (Streamable HTTP sessions);
# subsequent requests carry it when present.
SESSION_ID=$(awk -F': ' 'tolower($1)=="mcp-session-id" {sub("\r$","",$2); print $2}' "$HDR")
echo "  ok: initialize returned 200 ($ct, serverInfo present)"

# spec-correct: notify initialized before issuing requests
BODY="$WORKDIR/notif-body"; HDR="$WORKDIR/notif-hdr"
mcp_req good '{"jsonrpc":"2.0","method":"notifications/initialized"}' "$BODY" "$HDR" >/dev/null || true

# --- 6. tools/list exposes every kanban tool -----------------------------------
echo "check 6/7: tools/list exposes every kanban tool"
BODY="$WORKDIR/s6-body"; HDR="$WORKDIR/s6-hdr"
LIST='{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
code=$(mcp_req good "$LIST" "$BODY" "$HDR") \
    || fail "step 6 curl to $MCP_URL failed (is the server up?)"
[[ $code == 200 ]] || fail "step 6 tools/list should return 200, got $code"
RESP=$(extract_json "$BODY")
missing=""
for tool in board_list ticket_list ticket_get ticket_events ticket_claim \
            ticket_comment ticket_complete ticket_block ticket_create \
            kanban_help; do
    grep -Fq "\"name\":\"$tool\"" <<<"$RESP" || missing="$missing $tool"
done
[[ -z $missing ]] || fail "step 6 tools/list is missing tool(s):$missing"
echo "  ok: all tools present (board_list, ticket_list, ticket_get, ticket_events, ticket_claim, ticket_comment, ticket_complete, ticket_block, ticket_create, kanban_help)"

# --- 7. board_list includes hermes-agent -----------------------------------------
echo "check 7/7: board_list includes hermes-agent"
BODY="$WORKDIR/s7-body"; HDR="$WORKDIR/s7-hdr"
CALL='{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"board_list","arguments":{}}}'
code=$(mcp_req good "$CALL" "$BODY" "$HDR") \
    || fail "step 7 curl to $MCP_URL failed (is the server up?)"
[[ $code == 200 ]] || fail "step 7 board_list should return 200, got $code"
RESP=$(extract_json "$BODY")
grep -Fq 'hermes-agent' <<<"$RESP" || fail "step 7 board_list result does not include the hermes-agent board"
echo "  ok: board_list includes hermes-agent"

# --- secret hygiene: journald log scan -------------------------------------------
echo "secret hygiene: scan kanban-mcp journal (last 10 min) for leaked credentials"
if journalctl -u kanban-mcp --since "10 min ago" 2>/dev/null | grep -qiE 'bearer [a-f0-9]{16}|password|set-cookie'; then
    fail "secret hygiene journalctl -u kanban-mcp --since \"10 min ago\" matched a leaked credential (bearer token / password / set-cookie)"
fi
echo "  ok: no leaked credentials in the journal"

# --- secret hygiene: repository scan ----------------------------------------------
echo "secret hygiene: git grep for committed secret values"
git rev-parse --is-inside-work-tree >/dev/null 2>&1 \
    || fail "secret hygiene cannot run — not inside a git repository (git grep needs a work tree)"
if git grep -nE '(MCP_BEARER_TOKEN|KANBAN_PASSWORD)=[^ ]' -- ':!*.example' \
    | grep -vE '=<[^>]+>' | grep -q .; then
    fail "secret hygiene git grep found a committed secret value (a secret variable name followed by an equals sign and a value)"
fi
echo "  ok: no committed secret values in the repository"
echo "  note: doc placeholders like MCP_BEARER_TOKEN=<token> are excluded from the scan"

echo
echo "smoke test passed: steps 1-7 and secret hygiene checks all ok"
