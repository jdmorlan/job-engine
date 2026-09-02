# job-engine

**Cron you can debug.**

`je` is a single static binary with an embedded database. It runs your scheduled
and event-driven work, and it can always answer three questions: **what ran, why
it ran, and what it actually processed.**

See [`PITCH.md`](PITCH.md) for why, and [`PROPOSAL.md`](PROPOSAL.md) for the
design decisions. Comments in the code cite those decisions by number (`D14`,
`P1`) rather than restating them.

## Status

Early. The walking skeleton is in place: the daemon runs, owns the database,
serves the API, and records its own lifecycle. There is no scheduler and no
executor yet, so nothing runs jobs.

What works today:

```
je serve          run the daemon in the foreground
je status         is it running, and how long was it down before this start
je emit <type>    put an event into the engine (D16's single ingress)
je events         the raw timeline, newest first
je version
```

```console
$ je serve &
$ je status
engine    running (dev)
uptime    3s
data dir  /Users/you/.je

$ je emit homekit.motion --payload '{"room":"office"}'
event 2 homekit.motion

$ je events
ID  WHEN                 TYPE            SOURCE  PAYLOAD
2   2026-09-02 11:16:18  homekit.motion  cli     {"room":"office"}
1   2026-09-02 11:16:18  engine.started  engine  -
```

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
