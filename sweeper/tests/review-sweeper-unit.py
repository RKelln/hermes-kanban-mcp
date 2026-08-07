#!/usr/bin/env python3
"""Unit checks for the verdict parser (run with system python3)."""
import os
import sys

sys.path.insert(0, os.path.expanduser("~/.hermes/scripts"))
from review_sweeper import parse_verdict, extract_findings  # noqa: E402

failures = []


def check(label, got, want):
    ok = got == want
    print(("%s  %s: got=%r want=%r" % ("PASS" if ok else "FAIL", label, got, want)))
    if not ok:
        failures.append(label)


rambling = """I investigated the sweeper state dir... The contract says "VERDICT: APPROVE | REQUEST_CHANGES | ESCALATE" so I should quote it. 
Verdict: APPROVE.verdict + complete) more garbage
- bullet: artifact verified
- bullet: contents match exactly
VERDICT: APPROVE
"""
check("rambling-parse", parse_verdict(rambling), "APPROVE")
f = extract_findings(rambling)
check("rambling-findings-has-bullets", "artifact verified" in f, True)
check("rambling-findings-excludes-verdict", f.endswith("VERDICT: APPROVE"), False)

skillstyle = "Findings:\n- branch exists\n- tests pass\nVerdict: VERIFY-ONLY APPROVE"
check("skillstyle-verifyonly", parse_verdict(skillstyle), "APPROVE")

check("contract-quote-only", parse_verdict("VERDICT: APPROVE | REQUEST_CHANGES | ESCALATE"), None)
check("request-changes", parse_verdict("findings...\nVERDICT: REQUEST_CHANGES"), "REQUEST_CHANGES")
check("escalate", parse_verdict("security issue\nVerdict: ESCALATE"), "ESCALATE")
check("block-maps-escalate", parse_verdict("VerDICT: BLOCK"), "ESCALATE")
check("none", parse_verdict("no verdict here"), None)

print("---")
print("FAILURES:", failures if failures else "none")
sys.exit(1 if failures else 0)
