#!/usr/bin/env python3
"""Offline checks for the tool-result error guard (run with system python3).

Regression for the round-3 review finding: ticket_detail() / all_boards() /
blocked_tickets() parsed a tool result with a bare json.loads, so an ERROR
result from the bridge (not JSON) raised json.JSONDecodeError, which is NOT
an McpError and therefore escaped the call site's `except McpError: continue`
— aborting the entire tick as 'review-sweeper: BROKEN' (exit 1). Because the
candidate order is stable, one permanently-failing ticket then blocks every
later candidate on every tick. Reachable exactly for the giant-handoff
tickets this pipeline reviews.

No live services: a FakeMcp stands in for the bridge.
"""
import os
import sys

sys.path.insert(0, os.path.expanduser("~/.hermes/scripts"))  # host fallback (may not exist)
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))  # repo sweeper/ copy wins
from review_sweeper import (McpError, all_boards, blocked_tickets,  # noqa: E402
                            parse_tool_json, ticket_detail)

failures = []


def check(label, got, want):
    ok = got == want
    if not ok:
        failures.append(label)
    print("%s  %s: got=%r want=%r" % ("PASS" if ok else "FAIL", label, got, want))


def check_raises(label, fn, exc=McpError):
    try:
        fn()
    except exc as e:
        print("PASS  %s: raised %s (%s)" % (label, type(e).__name__, str(e)[:70]))
        return
    except Exception as e:  # wrong exception type = still a failure
        failures.append(label)
        print("FAIL  %s: raised %s, want %s (%s)" % (label, type(e).__name__, exc.__name__, e))
        return
    failures.append(label)
    print("FAIL  %s: did not raise" % label)


class FakeMcp:
    def __init__(self, payload):
        self.payload = payload

    def call(self, name, args):
        return self.payload


# The three real non-JSON error texts the bridge returns.
NOT_FOUND = "not found: ticket t_deadbeef on board hermes-agent"
OVERSIZED = ("oversized ticket t_big (status blocked): not representable within the "
             "8192-byte partial budget after the body and comments were reduced to "
             "their 120-rune floors")
DECODE = "unavailable: decode kanban backend response: unexpected end of JSON input"

for label, payload in (("not-found", NOT_FOUND), ("oversized", OVERSIZED), ("decode-cap", DECODE)):
    check_raises("ticket_detail %s -> McpError" % label,
                 lambda p=payload: ticket_detail(FakeMcp(p), "hermes-agent", "t_x"))
    check_raises("all_boards %s -> McpError" % label,
                 lambda p=payload: all_boards(FakeMcp(p)))
    check_raises("blocked_tickets %s -> McpError" % label,
                 lambda p=payload: blocked_tickets(FakeMcp(p), "hermes-agent"))

# Non-dict JSON is also a malformed tool result, not a payload to .get() on.
check_raises("parse_tool_json rejects a JSON list", lambda: parse_tool_json("[1,2]", "x"))
check_raises("parse_tool_json rejects JSON null", lambda: parse_tool_json("null", "x"))

# ...and the happy path still parses, including the empty-queue shape.
check("parse_tool_json dict", parse_tool_json('{"a": 1}', "x"), {"a": 1})
check("ticket_detail OK", ticket_detail(FakeMcp('{"id": "t_x"}'), "hermes-agent", "t_x"),
      {"id": "t_x"})

print("---")
print("FAILURES: %s" % (", ".join(failures) if failures else "none"))
sys.exit(1 if failures else 0)
