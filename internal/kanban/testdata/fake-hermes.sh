#!/bin/sh
# Fake hermes CLI for the kanban shell-out tests (claim_test.go).
#
# The production code never uses a shell; this fixture is executed
# directly via execve (the kernel honors the shebang). Behavior is
# selected by the task id, which is the first BARE argument after the
# verb: flags and their values (--ttl 14400, --kind needs_input) are
# skipped, so the layout the code emits does not have to be re-learned
# by this fixture every time a flag is added.
#   task-ok    -> print "claimed <id>" to stdout, exit 0
#   task-fail  -> print an error to stderr, exit 1
#   task-hang  -> spin in place until killed (timeout test)
#   task-echo  -> echo the full argv back (argv-layout tests)
#   task-big   -> write >4 KiB to stdout and stderr (cap test)

verb=""
id=""
skip_value=0
for arg in "$@"; do
  if [ "$skip_value" = "1" ]; then
    skip_value=0
    continue
  fi
  case "$arg" in
    claim|block|request-review) verb="$arg" ;;
    --ttl|--kind) skip_value=1 ;;
    -*) ;;
    *)
      if [ -n "$verb" ] && [ -z "$id" ]; then
        id="$arg"
      fi
      ;;
  esac
done

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
