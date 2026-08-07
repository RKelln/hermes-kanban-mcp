#!/usr/bin/env python3
"""Diagnostic: reproduce the reviewer session for the E2E test ticket and
dump its raw output, so the verdict parser can be matched to reality."""
import json
import os
import sys

sys.path.insert(0, os.path.expanduser("~/.hermes/scripts"))
from review_sweeper import (McpClient, load_env, build_reviewer_prompt,
                            spawn_reviewer, parse_verdict, ticket_detail,
                            ticket_blob, extract_branch, extract_repo)

env = load_env()
mcp = McpClient(env["MCP_BEARER_TOKEN"])
mcp.initialize()
detail = ticket_detail(mcp, "review-sweeper-e2e", "t_d708e09b")
blob = ticket_blob(detail)
prompt = build_reviewer_prompt("review-sweeper-e2e", "t_d708e09b", detail,
                               extract_repo(blob), extract_branch(blob))
print("=== prompt (first 300 chars) ===", prompt[:300], "...")
print("=== spawning reviewer ===")
ok, out = spawn_reviewer(prompt)
print("=== reviewer ok:", ok, "len:", len(out))
print(out[-3000:])
print("=== parsed verdict:", parse_verdict(out))
