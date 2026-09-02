# job-engine

**Cron you can debug.**

`je` is a single static binary with an embedded database. It runs your scheduled
and event-driven work, and it can always answer three questions: **what ran, why
it ran, and what it actually processed.**

See [`PITCH.md`](PITCH.md) for why, and [`PROPOSAL.md`](PROPOSAL.md) for the
design decisions. Comments in the code cite those decisions by number (`D14`,
`P1`) rather than restating them.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/jdmorlan/job-engine/main/install.sh | sh
```

Downloads the build for your platform, verifies its SHA-256 against the
checksums published with the release, and puts it in `~/.local/bin` (no sudo).
It starts nothing and edits no shell config; starting the engine is a separate,
explicit step.

```sh
JE_INSTALL_DIR=/usr/local/bin  # where to put it
JE_VERSION=v0.2.0              # pin a release instead of taking the latest
```

Updating is `je upgrade`, which verifies the same checksum before replacing
itself in place:

```console
$ je upgrade
  current  v0.1.0
  latest   v0.2.0

downloading je_0.2.0_darwin_arm64.tar.gz (4.8 MB)
verified sha256 194902ec097ce0a4

upgraded to v0.2.0 at /Users/you/.local/bin/je

note: a daemon is still running v0.1.0 (pid 4821).
      Restart it to pick up v0.2.0.
```

A running daemon keeps executing the old binary until it restarts — replacing a
file does not replace a process — so both `je upgrade` and `je status` say so
rather than leaving you to wonder why nothing changed.

`je upgrade --check` reports without installing. Building from source is
`go build ./cmd/je`.

## Two components

The engine is a **control plane** and one or more **workers**.

The control plane owns the database, fires schedules, serves the API, and holds
every fact about what happened. It never runs a job. Workers run jobs and hold
nothing: they dial the control plane, ask for work, and report back.

That split is not deployment trivia — it is why a job that has to touch *your
Mac* can, while the thing that remembers what happened lives in a cluster. A
worker goes where the work is; the control plane goes where a database is happy.

```
                    ┌──────────────────┐
   je ─────────────▶│  control plane   │  schedules, history, cursors, API
                    │  (one container) │  never runs your code
                    └────────┬─────────┘
                       ▲     │ dispatch
                dials  │     ▼
              ┌────────┴──┐  ┌───────────┐
              │  worker   │  │  worker   │   runs_on: default / macos / ...
              │ (default) │  │  (macos)  │   no state, no open ports
              └───────────┘  └───────────┘
```

A control plane with no worker attached runs **nothing**, and says so:

```console
$ je status
control plane  running (v0.3.0)
workers        NONE -- nothing will run
               start one:  je worker
```

### Running it

For trying it out, one command runs both halves in your terminal:

```console
$ je quickstart
je: control plane on 127.0.0.1:7620, one worker attached (default)
    try:  je jobs        in another terminal
          je run <job>
```

For anything unattended, Docker — the same image that runs in a cluster:

```console
$ docker compose up -d      # control plane and its system worker
```

Two rules in `compose.yaml` that are not optional. The data directory must be a
**named volume, never a bind mount** — SQLite over macOS bind mounts (and over
NFS) has silent locking pathologies that surface weeks later. And `TZ` must be
set explicitly, because containers default to UTC and schedules mean local time
to a human.

### Workers, and where jobs run

```console
$ je workers
NAME     ROLES    LABELS   SESSION
system   execute  default  online just now
macbook  execute  macos    online 4d
```

A job picks a worker by capability, not by address:

```yaml
runs_on: macos        # default: "default"
command: ["shortcuts", "run", "Water the plants"]
```

Jobs are **pinned, not placed**: a run goes to a worker advertising its label,
or it waits — visibly. Nothing balances load, steals work, or reschedules, and
none of that machinery is needed, because the whole point of a label is that the
work is *not* interchangeable.

```console
$ je waiting
WAITING FOR A WORKER  (queued for a label nothing is serving)
  runs_on: macos
    3 run(s), jobs: water-plants
    start one:  je worker --labels macos
```

To run a worker on your Mac:

```console
$ je worker --labels macos
```

It opens no ports and dials out, so it works from a laptop behind NAT. It holds
no state, so killing it costs its in-flight runs and nothing else.

### When a worker disappears

If a worker stops answering, the control plane cannot tell "it died" from "it is
partitioned and still running your job". Nothing can. So it stops guessing and
says what it actually knows:

```console
$ je runs
RUN  JOB      STATUS  STARTED              DURATION  ATTEMPTS
41   sleeper  lost    2026-09-02 17:57:30  -         1
```

`lost`, not `failed` — we do not know that it failed. The run appears in
`je waiting`, a `run.lost` event lands in the timeline, and a human decides.
Automatic retry of lost runs (`on_node_lost: retry`) is deliberately not here
yet: it is at-least-once, and choosing that for you is not something a job
engine should do quietly.

## Try it in two minutes

```console
$ ./je demo
wrote 7 files to /Users/you/.je/jobs

  demo-hello    prints a line and exits           the smallest job that exists
  demo-counter  keeps a cursor and advances it    what other schedulers leave to you
  demo-flaky    fails about one run in three      watch the cursor NOT move
  demo-tick     runs every minute                 gives the scheduler something to do
```

They are ordinary files. Read them, change them, break them, `je demo --remove`
when you are done.

The tour that matters is `demo-flaky`. Run it eight times and look at what
happened:

```console
$ je runs demo-flaky
RUN  JOB         STATUS     STARTED              DURATION
8    demo-flaky  succeeded  2026-09-02 14:13:20  11ms
7    demo-flaky  succeeded  2026-09-02 14:13:19  14ms
6    demo-flaky  failed     2026-09-02 14:13:18  15ms
5    demo-flaky  succeeded  2026-09-02 14:13:17  11ms
...

$ je state history demo-flaky
v6  2026-09-02 14:13:20  run 8          processed -> 5
v5  2026-09-02 14:13:19  run 7          processed -> 4
v4  2026-09-02 14:13:17  run 5          processed -> 3
v3  2026-09-02 14:13:16  run 4          processed -> 2
v2  2026-09-02 14:13:13  run 2          processed -> 1
v1  2026-09-02 14:13:12  engine (seed)  processed -> 2026-09-02T19:13:12Z
```

Eight runs, three failures, and **five cursor versions** — runs 1, 3 and 6 are
missing from the history. The script advances its cursor *before* it knows
whether the work succeeded, which is the bug every hand-written watermark has;
the engine holds it until the exit code says the work actually happened, so a
failure reprocesses that batch instead of silently skipping it.

That is the entire pitch, and it is four commands.

## Status

Early, but it runs jobs on a schedule, unattended.

```
je demo           write four example jobs and a tour
je upgrade        install the latest release, checksum-verified
je quickstart     a control plane and a worker, in this terminal
je serve          run the control plane: schedules fire, API serves
je worker         run a worker: this is what actually executes jobs
je workers        what is attached, and what it can run
je waiting        what has not happened yet, and what is stuck
je run <job>      run a job now and follow its output
je jobs           what is loaded, and what is broken
je runs           recent runs
je logs <run>     what a run printed
je state get|history <job>    the cursor, and how it has moved
je events         the raw timeline with causation
je secret set|list|rm         values jobs declare and the engine injects
je status         is the control plane up, and how long was it down
je emit <type>    put an event into the engine (D16's single ingress)
```

**The CLI is a client and nothing else.** It has no path to the database of its
own — every command is an API call, including `je secret` and `je run`. So the
same commands work against a control plane in a container, in a cluster, or on
another machine: only the address changes.

`je run` queues the run, a worker executes it, and the output streams back here:

```console
$ je run weather-ingest
ingested 41 readings

ok  run 12 succeeded in 1.2s
    cursor  since  2026-08-13T04:00 -> 2026-09-02T11:15  (v41)
    emitted weather.ingested (event 88)
```

Ctrl-C detaches rather than cancelling: the run belongs to the control plane and
its worker, and it tells you how to follow it again.

### Schedules

```yaml
on:
  - every: 15m          # aligned to the clock: :00, :15, :30, :45
    catch_up: once
  - cron: "0 3 * * *"
    timezone: America/Denver
```

A schedule tracks its position on a **grid**, not a time since the last run.
That is what makes catch-up expressible: after the laptop wakes, the set of
missed windows is exactly enumerable, and `catch_up` decides what to do with
them — `skip` resumes from now, `once` fires a single run for the whole gap,
`all` fires one per window. In normal operation all three behave identically;
they only differ after a gap, which is when it matters.

Missed windows are recorded as events even when skipped, so a hole in a job's
history is explained rather than mysterious.

### What has not happened yet

```console
$ je waiting
SCHEDULED
  heartbeat  every 2s   next 2026-09-02 13:37:30  (in 1s)
  nightly    0 3 * * *  next 2026-09-03 04:00:00  (in 14h23m)

BLOCKED  (these will not run until you fix them)
  needs-token
    secret not set: ABSENT_TOKEN (set it with: je secret set ABSENT_TOKEN)

$ echo $?
3
```

Most job engines can show you what ran. This is the negative space — what is
scheduled, what is queued behind the concurrency cap, and what is blocked and
will never resolve itself. It exits 3 when something needs a human, so "is
everything OK?" is a query with an exit code rather than a vibe.

```console
$ je run counter
JE_STATE={'n': 2}
JE_LAST_SUCCESS_AT=2026-09-02T17:40:57Z

ok  run 2 succeeded in 29ms
    cursor  n  1 -> 2  (v3)
    emitted counter.ticked (event 5)
    output  {"counted": 2}
```

The cursor line is the part that does not exist elsewhere. D14 makes the
watermark durable and engine-managed, and commits it **only when the job exits
zero** — so a job that fails after doing half its work does not silently skip
the other half on the next run.

```console
$ je events
ID  WHEN                 TYPE            SOURCE  CAUSE  PAYLOAD
6   2026-09-02 12:40:57  run.succeeded   engine  run 2  {"job":"counter",...}
5   2026-09-02 12:40:57  counter.ticked  job     run 2  {"n": 2}
4   2026-09-02 12:40:57  run.requested   cli     -      {"job":"counter"}
```

### Writing a job

A job is a YAML file naming a command. The file's name is the job's name.

```yaml
# jobs/weather-ingest.yaml
name: Weather Ingest
command: ["python3", "scripts/ingest_weather.py"]
workdir: ~/code/almanac

state:
  primary_cursor: since
```

The engine talks to the job through the environment and three files, with no
SDK and nothing to import — **the filesystem is the contract** (D6). Everything
the engine promotes is discarded unless the job exits zero.

| | |
|---|---|
| `JE_STATE` | your cursor, as JSON. Seeded with the run's start time on a first run. |
| `JE_LAST_SUCCESS_AT` | when this job last succeeded. Engine-owned; absent if never. |
| `JOB_STATE_OUT_FILE` | write your new cursor here. Not writing it means "no change". |
| `JOB_OUTPUT_FILE` | structured output, for chained jobs. |
| `JOB_EVENTS_FILE` | append JSONL to emit events. |

Also set: `JOB_ID`, `RUN_ID`, `ATTEMPT`, `TRIGGERED_BY`, `EVENT_PAYLOAD`,
`JOB_WORKDIR`. Nothing else from the engine's own environment reaches the job
except `PATH`, `HOME`, `TZ` and a few other essentials — a job does not inherit
credentials by accident (D10).

### Secrets

Declared per job, injected per job, never printed.

```console
$ je secret set STATION_API_KEY
Value for STATION_API_KEY (not echoed):
set STATION_API_KEY

$ je jobs
JOB             STATUS         COMMAND                        FILE
weather-ingest  ok             python3 scripts/ingest.py      weather-ingest.yaml
```

Before the secret was set, that row read `misconfigured` and the job refused to
run, naming the secret and the command to fix it. **A missing secret is a
definition error, not a 3am exit code** — which is the whole of D10.

Values are stripped from log lines *before* they reach the database, so copying
the log file later cannot leak them. There is no `je secret get`.

### Not built yet

Retries, chains, job sources (D22), container executor, the TypeScript shim
(D21), and retention.

**The control plane reads job definitions only at startup.** Adding or editing a
job file needs a restart before it takes effect. Watching the jobs directory —
or a sync endpoint, which is what the split makes the more obvious answer — is a
v1 item and the largest remaining gap. A job declaring `language:` loads but is
marked misconfigured and will not run, rather than running without what it asked
for.

**Secrets reach a worker in the dispatch.** That is correct for a trusted
network and wrong for anything else, and it is the real work item behind putting
a worker on a machine you do not fully control (D10).

## Layout

Two structural rules. From D18: **the engine core is a library** — the control
plane is a thin wrapper around it, so nothing in `internal/engine` may call
`os.Exit`, print, read flags, or handle signals. From D20: **the control plane
never executes a job.** `internal/engine` starts no processes; `internal/worker`
is the only package that does.

| Package | Owns |
|---|---|
| `cmd/je` | the binary. Signals, and nothing else. |
| `internal/engine` | the core, as a library. Sole writer to the database. |
| `internal/store` | every SQL statement in the program (Q1). Nothing else queries. |
| `internal/model` | the F1 nouns. Depends on nothing. |
| `internal/api` | the HTTP contract. Every capability is an endpoint (D15). |
| `internal/daemon` | process concerns: listener, shutdown, runtime file. |
| `internal/worker` | the data plane. The only package that starts a process. |
| `internal/cli` | the `je` client. No database access of its own. |
| `internal/paths` | the only place that knows where files live. |
| `internal/lockfile` | the single-writer guarantee (D18). |
| `internal/jobdef` | parsing, validation, defaults, and D11 hashing. |
| `internal/executor` | running a command. Process today, container later (D1). |
| `internal/secrets` | the local secret store, and log redaction values (D10). |
| `internal/schedule` | cron and interval windows, including the DST rules (D9). |
| `internal/selfupdate` | finding, verifying and installing a new binary. |

## Building

```
make build        # ./je
make check        # fmt, vet, test
make image        # a scratch container image (D19)
make release-dry  # build every release artifact locally, as CI does
```

One direct dependency: `modernc.org/sqlite`, the pure-Go driver. That is what
keeps the binary static and cgo-free, which is in turn why one artifact is both
the control plane and the worker, and runs in a terminal, in a `FROM scratch`
image, in a cluster, and inside a Mac app (D18).

## Where things live

```
~/.je/            data directory (override with JE_DATA_DIR)
  state.db        runs, events, definitions, cursors
  logs.db         captured job output, separately (D4)
  lock            the single-writer flock
  secrets.json    the local secret store, mode 0600 (D10)
  daemon.json     the control plane's address, so clients can find it
  jobs/           job and chain definitions (override with JE_JOBS_DIR)
```
