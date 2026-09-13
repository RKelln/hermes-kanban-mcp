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
from review_sweeper import (McpClient, McpError, RestClient,  # noqa: E402
                            all_boards, blocked_tickets, detail_truncation_warning,
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


# --- transport layer: the paths that actually bypass the isError mechanism ---
#
# McpClient.call() raises McpError when the result carries isError, which is
# how the bridge's error TEXT ("not found", "oversized ticket") is kept away
# from json.loads. That makes parse_tool_json defence-in-depth. The transport
# itself is the real escape route, so both arms are asserted here.

def check_call_raises_on_iserror():
    c = McpClient("token")
    c._post = lambda payload: {"result": {"isError": True,
                                          "content": [{"type": "text", "text": NOT_FOUND}]}}
    check_raises("call() raises McpError on an isError result", lambda: c.call("ticket_get", {}))


def check_call_raises_on_nonstring_text():
    c = McpClient("token")
    c._post = lambda payload: {"result": {"content": [{"type": "text", "text": {"oops": 1}}]}}
    check_raises("call() raises McpError on a non-string content block",
                 lambda: c.call("ticket_get", {}))


check_call_raises_on_iserror()
check_call_raises_on_nonstring_text()

# A malformed HTTP body must be McpError, not ValueError (it escapes call()).
for label, raw in (("truncated JSON body", '{"jsonrpc":"2.0","id":1,"result":'),
                   ("brace-only body", '{"error"}'),
                   ("HTML body", "<html>proxy login</html>")):
    check_raises("_parse_sse %s -> McpError" % label,
                 lambda r=raw: McpClient._parse_sse(r))

# A 2xx REST body that is not JSON must be McpError, not ValueError.
class _Resp:
    def __init__(self, body):
        self._body = body
        self.status = 200

    def read(self):
        return self._body

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


class _Opener:
    def __init__(self, body):
        self._body = body

    def open(self, req, timeout=None):
        return _Resp(self._body)


def _rest_with_body(body):
    rc = object.__new__(RestClient)          # patch_task only needs base+opener
    rc.base = "http://dashboard.invalid/api/plugins/kanban/"
    rc.opener = _Opener(body)
    return rc


for label, body in (("empty body", b""), ("HTML body", b"<html>login page</html>")):
    check_raises("patch_task %s -> McpError" % label,
                 lambda b=body: _rest_with_body(b).patch_task("hermes-agent", "t_x", {"status": "done"}))

check("patch_task valid JSON still returns a dict",
      _rest_with_body(b'{"ok": true}').patch_task("hermes-agent", "t_x", {"status": "done"}),
      {"ok": True})

# A title-only truncation must be reported specifically, not as the generic
# fallback (a long title is common, so a vague standing warning is noise).
check("warning names a clipped title",
      detail_truncation_warning({"truncated": {"titles": True}}),
      "bridge clipped the ticket detail: title clipped")
check("warning names a clipped ref",
      detail_truncation_warning({"truncated": {"refs": True}}),
      "bridge clipped the ticket detail: branch_name clipped")
check("warning silent when nothing was clipped",
      detail_truncation_warning({"truncated": {}}), "")

print("---")
print("FAILURES: %s" % (", ".join(failures) if failures else "none"))
sys.exit(1 if failures else 0)
