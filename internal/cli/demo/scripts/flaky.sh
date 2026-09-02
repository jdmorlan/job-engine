#!/bin/sh
# This job does the thing every hand-written watermark gets wrong, on purpose,
# to show that the engine makes it safe.

processed=$(printf '%s' "$JE_STATE" | sed -n 's/.*"processed":[[:space:]]*\([0-9][0-9]*\).*/\1/p')
[ -n "$processed" ] || processed=0
next=$((processed + 1))

echo "$processed records handled so far; starting batch $next"

# The cursor is written BEFORE we know whether the work succeeded. Written by
# hand to a file of your own, this is the bug that silently skips records: the
# watermark moves, the job dies, and the next run starts after the data it
# never processed.
#
# Here it costs nothing. The engine holds this until the exit code says the
# work actually happened.
printf '{"processed":%d}' "$next" > "$JOB_STATE_OUT_FILE"

# Fails unpredictably, roughly one run in three, the way a real upstream does.
# Deliberately not "every third batch": the cursor does not advance on a
# failure, so that version would retry the same batch forever and never get
# past it. Which is realistic, and a terrible thing to hand somebody on their
# first five minutes.
if [ $(( $(date +%s) % 3 )) -eq 0 ]; then
  echo "the upstream API fell over" >&2
  exit 1
fi

echo "batch $next committed"
