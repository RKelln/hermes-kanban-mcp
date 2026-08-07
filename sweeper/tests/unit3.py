#!/usr/bin/env python3
"""Check findings extraction against the saved review logs."""
import glob
import os
import sys

sys.path.insert(0, os.path.expanduser("~/.hermes/scripts"))
from review_sweeper import extract_findings, parse_verdict  # noqa: E402

ok = True
for p in sorted(glob.glob(os.path.expanduser("~/.hermes/state/review-sweeper/reviews/*.log"))):
    out = open(p, encoding="utf-8").read()
    v = parse_verdict(out)
    f = extract_findings(out)
    stripped = "┌─" not in f
    print(os.path.basename(p), "| verdict:", v, "| reasoning-stripped:", stripped,
          "| findings-len:", len(f))
    if v not in ("APPROVE", "REQUEST_CHANGES", "ESCALATE") or not stripped:
        ok = False
print("ALL GOOD" if ok else "PROBLEM")
sys.exit(0 if ok else 1)
