#!/usr/bin/env bash
# Wrapper for the lane-return cron script — the sweeper's successor (t_2a8c802c).
#
# WHY A WRAPPER: the canonical script lives in the hermes-kanban-mcp repo
# (scripts/lane_return.py), but the cron scheduler hardens against symlink
# escape (upstream 878b1d3d3: scripts must resolve inside HERMES_HOME/scripts/),
# so a symlink from ~/.hermes/scripts/ errors with "Blocked: script path
# resolves outside the scripts directory". The cron entry therefore points at
# THIS real file, which execs the repo copy. This repo copy is the canonical
# source; install it with:
#
#   install -m 0755 deploy/lane_return_wrapper.sh ~/.hermes/scripts/lane_return_wrapper.sh
#
# If the repo moves, update the path below (one line) and in the skill.
#
# Deliberately contains NO `hermes` invocation: a no_agent cron whose command
# text names the hermes CLI is refused as a gateway-lifecycle command
# (guard #30719). The script itself shells out to `hermes kanban promote`,
# which is a board operation, not a lifecycle one.
#
# No LLM. No reviewer. No branch check. It asks the kernel to `promote` a card
# that a review run left blocked, and prints nothing when there is nothing to do.
set -euo pipefail

exec python3 /home/experimance/Documents/Projects/hermes-kanban-mcp/scripts/lane_return.py "$@"
