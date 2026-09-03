#!/bin/sh
# A job emits an event by appending one JSON object per line to the file the
# engine handed it. No SDK, no library, no import -- the filesystem is the
# contract (D6), and this is the whole of it.
#
# The event is committed only if this script exits zero, the same rule the
# cursor follows. A job that dies half way does not get to announce that it
# finished.
set -e

rows=$(( (RANDOM % 40) + 1 ))
echo "ingested $rows rows"

printf '{"type":"demo.ingested","payload":{"rows":%d}}\n' "$rows" >> "$JOB_EVENTS_FILE"
