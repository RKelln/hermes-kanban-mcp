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

verb=""
id=""
for arg in "$@"; do
  case "$arg" in
    claim|block) verb="$arg" ;;
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
