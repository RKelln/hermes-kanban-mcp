#!/bin/sh
# Fake hermes CLI for the kanban shell-out tests (claim_test.go).
#
# The production code never uses a shell; this fixture is executed
# directly via execve (the kernel honors the shebang). Behavior is
# selected by the task id, which the real CLI always places directly
# after the verb:
#   task-ok    -> print "claimed <id>" to stdout, exit 0
#   task-fail  -> print an error to stderr, exit 1
#   task-hang  -> spin in place until killed (timeout test)
#   task-echo  -> echo the full argv back (argv-layout tests)
#   task-big   -> write >4 KiB to stdout and stderr (cap test)
#   task-reviewfail -> succeed on every verb EXCEPT request-review, which
#                exits 1 (the handoff fails after a successful stamp)

verb=""
id=""
skip_value=0
for arg in "$@"; do
  if [ "$skip_value" = "1" ]; then
    skip_value=0
    continue
  fi
  case "$arg" in
    claim|block|request-review|assign|reassign) verb="$arg" ;;
    --ttl|--kind) skip_value=1 ;;
    -*) ;;
    *)
      if [ -n "$verb" ] && [ -z "$id" ]; then
        id="$arg"
      fi
      ;;
  esac
done

# Optional argv capture, for tests that must assert what the tool layer
# actually passed (the --force decision). The production child env is
# scrubbed down to PATH/HOME/LANG/HERMES_*, so the path has to arrive
# under a HERMES_-prefixed name to survive the scrub.
if [ -n "$HERMES_FAKE_ARGV_LOG" ]; then
  printf '%s\n' "$*" >> "$HERMES_FAKE_ARGV_LOG"
fi

emit_big() {
  i=0
  while [ "$i" -lt 1000 ]; do
    printf '0123456789'
    i=$((i + 1))
  done
}

case "$id" in
  task-fail)
    echo "error: task already claimed" >&2
    exit 1
    ;;
  task-hang)
    while :; do :; done
    ;;
  task-echo)
    echo "argv: $*"
    ;;
  task-reviewfail)
    if [ "$verb" = "request-review" ]; then
      echo "error: re-review has no durable reviewer provenance" >&2
      exit 1
    fi
    echo "assigned $id"
    ;;
  task-big)
    emit_big
    emit_big >&2
    exit 0
    ;;
  "")
    echo "no task id" >&2
    exit 2
    ;;
  *)
    echo "claimed $id"
    ;;
esac
exit 0
