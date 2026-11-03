#!/usr/bin/env bash
# Run a command, keep its output in out/pilot/run.log, and surface the tail as a GitHub
# annotation (readable through the API when job logs are not). Usage: step.sh <title> <cmd...>
set -uo pipefail
title="$1"; shift
mkdir -p out/pilot
log="$(mktemp)"
"$@" > >(tee "$log") 2>&1
rc=$?
wait
{ echo "== $title (exit $rc)"; cat "$log"; } >> out/pilot/run.log
tail_txt="$(tail -n 15 "$log" | sed 's/%/%25/g' | sed ':a;N;$!ba;s/\n/%0A/g')"
if [ $rc -ne 0 ]; then
  echo "::error title=${title}::${tail_txt}"
else
  echo "::notice title=${title}::${tail_txt}"
fi
exit $rc
