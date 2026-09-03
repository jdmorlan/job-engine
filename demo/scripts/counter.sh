#!/bin/sh
# The entire job protocol. Nothing is imported and nothing is installed: the
# engine hands you JSON in the environment and paths to write, so any language
# that can read an environment variable and write a file can be a job.

# JE_STATE is your cursor. The engine seeds it on a first run, so it is never
# empty -- on run one it holds a timestamp, and after that whatever you wrote.
count=$(printf '%s' "$JE_STATE" | sed -n 's/.*"count":[[:space:]]*\([0-9][0-9]*\).*/\1/p')
[ -n "$count" ] || count=0
next=$((count + 1))

echo "the cursor said $count, so this is run number $next"

# JOB_STATE_OUT_FILE is where a new cursor goes. The engine commits it only if
# this script exits zero. Not writing it at all means "no change".
printf '{"count":%d}' "$next" > "$JOB_STATE_OUT_FILE"
