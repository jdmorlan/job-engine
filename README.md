# job-engine

**Cron you can debug.**

`je` is a single static binary with an embedded database. It runs your scheduled
and event-driven work, and it can always answer three questions: **what ran, why
it ran, and what it actually processed.**

See [`PITCH.md`](PITCH.md) for why, and [`PROPOSAL.md`](PROPOSAL.md) for the
design decisions. Comments in the code cite those decisions by number (`D14`,
`P1`) rather than restating them.

## Try it in two minutes

```console
$ go build ./cmd/je && ./je demo
wrote 7 files to /Users/you/.je/jobs

  demo-hello    prints a line and exits           the smallest job that exists
  demo-counter  keeps a cursor and advances it    what other schedulers leave to you
  demo-flaky    fails about one run in three      watch the cursor NOT move
  demo-tick     runs every minute                 gives the daemon something to do
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
je serve          run the daemon: schedules fire, jobs run
je waiting        what has not happened yet, and what is stuck
je run <job>      run a job now, in the foreground
je jobs           what is loaded, and what is broken
je runs           recent runs
je logs <run>     what a run printed
je state get|history <job>    the cursor, and how it has moved
je events         the raw timeline with causation
je secret set|list|rm         values jobs declare and the engine injects
je status         is the daemon up, and how long was it down
je emit <type>    put an event into the engine (D16's single ingress)
```

Every command works the same whether or not a daemon is running: it talks to
the daemon when one is listening and opens the database itself when not. `je
run` against a live daemon queues the run there and streams its output back, so
you never have to stop the scheduler to run something by hand.

```console
$ je run weather-ingest       # daemon running; output streams live
ingested 41 readings

ok  run 12 succeeded in 1.2s
    cursor  since  2026-08-13T04:00 -> 2026-09-02T11:15  (v41)
    emitted weather.ingested (event 88)
```

Ctrl-C detaches rather than cancelling: the run belongs to the daemon, and it
tells you how to follow it again.

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
(D21), `je install` for launchd, and retention. A job declaring `language:`
loads but is marked misconfigured and will not run, rather than running without
what it asked for.

## Layout

The one structural rule, from D18: **the engine core is a library.** The daemon
is a thin wrapper around it and the CLI is a client of the daemon, so nothing in
`internal/engine` may call `os.Exit`, print, read flags, or handle signals.

| Package | Owns |
|---|---|
| `cmd/je` | the binary. Signals, and nothing else. |
| `internal/engine` | the core, as a library. Sole writer to the database. |
| `internal/store` | every SQL statement in the program (Q1). Nothing else queries. |
| `internal/model` | the F1 nouns. Depends on nothing. |
| `internal/api` | the HTTP contract. Every capability is an endpoint (D15). |
| `internal/daemon` | process concerns: listener, shutdown, runtime file. |
| `internal/cli` | the `je` client. |
| `internal/paths` | the only place that knows where files live. |
| `internal/lockfile` | the single-writer guarantee (D18). |
| `internal/jobdef` | parsing, validation, defaults, and D11 hashing. |
| `internal/executor` | running a command. Process today, container later (D1). |
| `internal/secrets` | the local secret store, and log redaction values (D10). |
| `internal/schedule` | cron and interval windows, including the DST rules (D9). |

## Building

```
make build     # ./je
make check     # fmt, vet, test
make image     # a scratch container image (D19)
```

One direct dependency: `modernc.org/sqlite`, the pure-Go driver. That is what
keeps the binary static and cgo-free, which is in turn why the same artifact
runs in a terminal, under launchd, in a `FROM scratch` image, and inside a Mac
app (D18).

## Where things live

```
~/.je/            data directory (override with JE_DATA_DIR)
  state.db        runs, events, definitions, cursors
  logs.db         captured job output, separately (D4)
  lock            the single-writer flock
  secrets.json    the local secret store, mode 0600 (D10)
  daemon.json     the running daemon's address, so the CLI can find it
  jobs/           job and chain definitions (override with JE_JOBS_DIR)
```
