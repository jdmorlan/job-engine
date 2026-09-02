# job-engine

**Cron you can debug.**

`je` is a single static binary with an embedded database. It runs your scheduled
and event-driven work, and it can always answer three questions: **what ran, why
it ran, and what it actually processed.**

See [`PITCH.md`](PITCH.md) for why, and [`PROPOSAL.md`](PROPOSAL.md) for the
design decisions. Comments in the code cite those decisions by number (`D14`,
`P1`) rather than restating them.

## Status

Early, but it runs jobs. This is stage 0 of the progression in D19: a binary you
download and run in the foreground. There is no scheduler yet, so jobs run when
you ask them to.

```
je run <job>      run a job now, in the foreground
je jobs           what is loaded, and what is broken
je runs           recent runs
je logs <run>     what a run printed
je state get|history <job>    the cursor, and how it has moved
je events         the raw timeline with causation
je serve          run the daemon
je status         is the daemon up, and how long was it down
je emit <type>    put an event into the engine (D16's single ingress)
```

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

### Not built yet

Scheduler, retries, chains, secrets, container executor, the TypeScript shim
(D21), and the daemon-side run API. A job declaring `secrets:` or `language:`
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
  daemon.json     the running daemon's address, so the CLI can find it
  jobs/           job and chain definitions (override with JE_JOBS_DIR)
```
