.PHONY: test test-sweeper

# Go suite (the MCP server). Run race-enabled as the CI gate does.
test:
	go build ./... && go vet ./... && go test -race ./...

# Python review-sweeper unit suite. Offline-only tests; the e2e setup/repro
# scripts under sweeper/tests (review-sweeper-e2e-*.py, review-sweeper-repro.py)
# need a live kanban board + host creds and are run manually on the host (see
# deploy/review-sweeper.md). The tests now resolve the module from the repo's
# sweeper/ copy (repo-relative insert wins over the ~/.hermes/scripts host
# fallback), so make test-sweeper exercises the pinned in-repo source.
SWEEPER_UNIT_TESTS := sweeper/tests/repo-url-hardening.py \
	sweeper/tests/review-sweeper-unit.py \
	sweeper/tests/review-sweeper-unit2.py \
	sweeper/tests/unit3.py \
	sweeper/tests/unit4.py \
	sweeper/tests/apply-path-test.py

test-sweeper:
	@set -e; for t in $(SWEEPER_UNIT_TESTS); do \
		echo "== $$t"; PYTHONPATH=sweeper python3 $$t; \
	done
