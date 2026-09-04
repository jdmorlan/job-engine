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

Updating is `je upgrade`. It verifies the same checksum before replacing itself,
and then offers to restart what this machine is running:

```console
$ je upgrade
  current  v0.4.1
  latest   v0.5.0

downloading je_0.5.0_darwin_arm64.tar.gz (5.5 MB)
verified sha256 194902ec097ce0a4

upgraded to v0.5.0 at /Users/you/.local/bin/je

still running the old version:
  control-plane  v0.4.1   (container)
  web            v0.4.1   (container)

restart them on v0.5.0?  [y/N] y

control-plane: pulling ghcr.io/jdmorlan/job-engine:v0.5.0
  restarted on v0.5.0
```

`je` is one binary playing three parts, so replacing it upgrades the CLI, the
control plane and any worker here at once — but a running process keeps
executing the old code until it restarts. **You should not have to know whether
that process is a container or a launchd job**, so this handles both: a
container is recreated from its own configuration, with the same mounts, ports
and flags and a new tag.

It asks first, because restarting a control plane drops whatever is mid-flight
and that is a poor thing to do as a side effect of swapping a file. `--yes`
skips the question and `--restart` skips the download. The exception is a
deployment you drive with `docker compose`: that file is yours and owns its
containers, so `je upgrade` reports them and changes nothing.

**Some commands act on this machine, not on the engine.** `je help` marks them
with a `*`, and it is a distinction worth learning once: `je runs` works
identically against a control plane in a cluster, and `je control-plane remove`
does not — it removes a service *here*, and a control plane in Kubernetes
carries on. The danger is not an error, it is a command that succeeds and
changes something other than what you meant.

**Definitions live in a repository. That is the only kind of source there is.**

```console
$ je source add house you/house-jobs
$ je demo                              the examples, from this project's repo
```

There is no local jobs directory, and the engine's data directory holds engine
state and a cache — nothing you authored. That is not tidiness: a directory of
job files on the control plane's disk could only ever run on a worker that
shared that disk, so it broke the moment there were two machines. A repository
travels; every worker fetches the tree its job belongs to, so the same job runs
on your laptop, in a container, or in a cluster without changing anything.

Job names carry their source — `house/water-plants` — and `je new` writes into
the repository you are standing in:

```console
$ cd ~/repos/house-jobs
$ je new water-plants --language python
wrote water-plants.yaml
wrote scripts/water-plants.py
```

Then commit, push, and `je source sync`.

In development, `je reset` tears down everything on this machine — containers,
services, volumes, databases, certificates and the cache of fetched trees.
Nothing it removes is a definition, because none of them are here. It only removes what *this* data directory owns, and
says what it left and why:

```console
$ je reset
This will remove, on this machine only:

  the control-plane container (je-control-plane)
  docker volume je-data
  .je/state.db
  .je/cache
  ...

There is no undo. Type ".je" to confirm:
```

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
               start one:  je worker run
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

Or `je control-plane install --docker`, which sets the same thing up and leaves
the CLI able to manage it: **you should never have to run a docker command to
use the engine.** A CLI on the host of a containerised control plane finds it,
takes the certificate it needs out of the container by itself, and a worker
started there enrolls with nothing pasted — `je upgrade` restarts it too. The
exception is this compose file, which is yours: it owns its containers, so the
CLI reports them and leaves them alone.

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
    start one:  je worker run --labels macos
```

To run a worker on your Mac, enroll it first. `je enroll` on the control plane
decides the name and the labels and prints a token and the authority's
fingerprint; the machine becoming a worker redeems them:

```console
$ je enroll macbook --labels macos          # on the control plane
token    Cg2Mq-cHkyFaXKHeUkRbtHUdJCLd5-B3jbR6qa65XkI
worker   macbook
labels   macos
expires  in 15m0s

On that machine:
  je worker run --token Cg2Mq-cHkyFaXKHeUkRbtHUdJCLd5-B3jbR6qa65XkI \
    --addr control-plane:7620 --ca-pin 62548372672...

$ je worker run --token Cg2... --addr ... --ca-pin ...   # on the Mac
enrolled as macbook; identity written to ~/.je/worker.crt
```

The name and labels are decided where the token is minted, not by the machine
redeeming it — a label is a capability, and a worker that advertises its own can
grant itself whatever a label gates. The fingerprint is checked *before* the
token is sent, because a token is a bearer credential: whoever receives it
becomes this worker.

A worker sharing a machine with the control plane skips all of it. `je worker
run` there enrolls itself from a token the control plane leaves in its own data
directory, which is why `je quickstart` and `docker compose up -d` need nothing
pasted.

It opens no ports and dials out, so it works from a laptop behind NAT. It holds
no state, so killing it costs its in-flight runs and nothing else.

### What is on the wire

**HTTPS, always, verified both ways where there is something to verify.** The
control plane is its own certificate authority: it issues itself a server
certificate and issues every worker one at enrollment, so there is no public CA,
no domain to own, and nothing to renew by hand — a worker replaces its own
certificate on the heartbeat, authenticated by the certificate it is replacing.

A worker's identity is therefore *checked* rather than claimed. `je run --actor`
and a worker's name used to arrive in the request body, which made them
assertions; a verified certificate's common name cannot be asked for on somebody
else's behalf. A client that presents no certificate is simply nobody, which is
fine and ordinary: the CLI and the web client read, and reads need no identity.

There is no plaintext listener and no flag that brings one back. A `je` from
before this change cannot talk to a control plane after it, and says so rather
than failing in the handshake — **upgrading the control plane means restarting
its workers.**

#### Who did this

A person gets a certificate too, and then `je run` is attributed to somebody
rather than to whatever the request body claimed:

```console
$ je enroll jays-laptop --client       # on the control plane
$ je identity join --token ... --ca-pin ... --addr ...    # on the laptop
enrolled as jays-laptop

$ je events
ID  WHEN                 TYPE           ACTOR        SOURCE
8   2026-09-03 21:10:37  run.requested  jays-laptop  cli
```

Beside the control plane itself, `je identity join` needs no token — it reads
the one in the data directory, which it can only do if it already has the access
that would let it read the CA key.

**Issuing the first client identity changes the deployment**, and `je enroll
--client` says so before you redeem it: from then on, a request that *changes*
something must present a certificate. `je run`, `je secret set`, `je source add`
and minting further identities all refuse an unidentified caller. Reading is
untouched — a certificate answers "who is this", and "who may look" is a
question this system does not ask.

Before that first client exists there is nobody to be, so writes are open. The
gate is armed by the deployment's own state rather than by a setting, because a
setting can be true while no certificate exists, and that is the state where
nothing works.

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
registered github.com/jdmorlan/job-engine/demo as "demo" at a3f81c2f

  demo/demo-hello    prints a line and exits           the smallest job that exists
  demo/demo-counter  keeps a cursor and advances it    what other schedulers leave to you
  demo/demo-flaky    fails about one run in three      watch the cursor NOT move
  demo/demo-tick     runs every minute                 gives the scheduler something to do
  demo/demo-ingest   emits an event when it finishes   the start of a chain
  demo/demo-report   runs because that event happened  no clock, no polling
  demo/demo-archive  runs after the report succeeds    and not at all if it doesn't

and one chain, which runs nothing itself:
  demo/demo-pipeline  wires the three above into one named flow
```

Ordinary files in an ordinary directory — which is the point. The examples
arrive the way your own jobs would: registered as a source, pinned to a commit,
and named for where they came from. Fork them, break them, and `je demo --remove`
when you are done.

They are registered as a **subpath** — `--path demo` — rather than as a
repository of their own. That pins them to the tag of the binary that registered
them, so the examples can never reference something your engine does not have.
It is also the same mechanism that lets one repository hold `python-jobs/` and
`typescript-jobs/` and register them as two sources with different names.

The tour that matters is `demo/demo-flaky`. Run it eight times and look at what
happened:

```console
$ je runs demo/demo-flaky
RUN  JOB              STATUS     STARTED              DURATION
8    demo/demo-flaky  succeeded  2026-09-02 14:13:20  11ms
7    demo/demo-flaky  succeeded  2026-09-02 14:13:19  14ms
6    demo/demo-flaky  failed     2026-09-02 14:13:18  15ms
5    demo/demo-flaky  succeeded  2026-09-02 14:13:17  11ms
...

$ je state history demo/demo-flaky
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

## In a browser

The same facts, rendered where a terminal renders them worse (D23):

```console
$ je web run
web client    http://127.0.0.1:7621
control plane http://127.0.0.1:7620

ctrl-c to stop
```

No Docker, no npm, no separate install — the client is embedded in the binary
you already have, and `je web run` serves it and forwards `/v1` to the control
plane. It is a **client of the same API the CLI uses**: it holds no state, owns
no database, and can do nothing `je` cannot.

What it is for is the part a table cannot do well:

- **Chains as a canvas.** The trigger job, each step, and the *event pattern* on
  every edge — `run.succeeded · job=demo-report` — because that pattern is the
  wiring, not decoration.
- **The waiting view**, broken out into scheduled, queued, running and blocked.
  What the engine intends to do and has not done yet is the thing most schedulers
  cannot show you at all.
- **Resolved vs. definition.** Every field of a job with the file and line where
  somebody chose it, or the word *default* where the engine did. The file holds
  intent; the tool renders truth.

`je web start` runs it as a container instead, and `docker compose up -d` brings
it up beside the control plane and worker.

Authoring from the browser is not in this first cut. When it lands it will write
to a git repository and sync, never into the database — see D23.

## Status

Early, but it runs jobs on a schedule, unattended, and runs them off each
other's events.

```
je demo           register a repo of example jobs, a chain, and a tour
je upgrade        upgrade this deployment: the binary, and what runs here
je reset          tear it all down on this machine and start from scratch
je quickstart     a control plane and a worker, in this terminal
je control-plane run    the control plane: schedules, history, the API
je worker run           a worker: the thing that executes jobs
je workers              what is attached, and what it can run
je sync           reload job definitions, atomically
je waiting        what has not happened yet, and what is stuck
je run <job>      run a job now and follow its output
je init [dir]     set up a new jobs repository
je source         register where definitions come from, and what each provides
je new <job>      write a job file, and optionally the script it runs
je explain <job>  every effective value, and where each came from
je jobs           what is loaded, and what is broken
je runs           recent runs
je logs <run>     what a run printed
je state get|history <job>    the cursor, and how it has moved
je events         the raw timeline with causation
je chains         the flows, and how each one's last pass went
je chain <name>   every step of one flow, and its end-to-end duration
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

### Where jobs come from

Definitions and the code they run arrive from named **sources**. A source is a
whole tree, not a pile of YAML: the scripts a job runs live beside it and travel
with it, which is what makes a repo work unmodified on another machine.

```console
$ je init ~/code/weather-jobs
$ cd ~/code/weather-jobs
$ je new ingest --script
$ je source add weather .
```

Or point it at a repository and let the engine fetch it:

```console
$ je source add you/weather-jobs
resolving you/weather-jobs...
registered weather-jobs -> you/weather-jobs
  resolved main -> a3f81c2
  jobs    5
  chains  1 (2 route(s))

$ je source
NAME          KIND    WHERE                       REVISION  JOBS  SYNCED
local         dir     ~/.je/jobs                  -         2     -
weather-jobs  github  you/weather-jobs@main       a3f81c2   5     4m ago
```

No git binary is involved: a repository is a tarball over HTTPS, which is
`net/http`, `archive/tar` and `compress/gzip` and adds nothing to the dependency
list. A fetched repo therefore works anywhere the engine works, with no
toolchain to install. (Reading is the whole of this: *writing* definitions back
to a repository will use a real `git`, per D23.)

**A ref always resolves to a commit.** `--ref` takes a branch, a tag or a SHA,
and a bare `owner/repo` tracks whatever the repository says its own default
branch is — asked, not assumed, because "main" is a convention and not a rule.
The resolved commit is what gets cached, what a run records, and what
`source.synced` names:

```console
$ je source sync weather-jobs
weather-jobs  a3f81c2 -> 7e10b4f
  jobs  5

$ je events
42  source.synced  {"source":"weather-jobs","from":"a3f81c2","to":"7e10b4f",...}
```

Without that recorded commit, "what ran?" has no answer for any job whose code
came from a moving branch — D11 would quietly stop being true for every remote
job. Trees are cached under their commit, so a sync that resolves to what is
already here downloads nothing.

**Private repositories** authenticate with a token from the secret store. The
source records the secret's *name*, never a token, so a registration is safe to
print and back up:

```console
$ je secret set GITHUB_TOKEN
$ je source add you/private-jobs --token GITHUB_TOKEN
```

This is scheduled execution of code fetched from the internet, running with your
`PATH` and your `HOME` — the same trust model as a CI runner, which is fine for
repos you own and worth being clear-eyed about. Pinning to a tag or SHA is as
easy as tracking a branch, every update from a moving ref is a logged event with
both revisions, and extraction refuses any archive entry that would land outside
its own directory.

Fetched sources are re-read on request and once when the control plane starts.
A source that cannot be reached keeps serving the tree it last fetched and says
why in `je source`, so a laptop that wakes without a network keeps working.

**A job's name carries its source**, always, because two repos will eventually
both contain a `sync.yaml` and you may own neither:

```console
$ je run weather/ingest
$ je run sync
je run: "sync" is ambiguous: home/sync and weather/sync -- name the source, e.g. home/sync
```

Jobs from `local` keep bare names. That is a deliberate departure from treating
every source alike: until there is a second source, a prefix on every row of
every view carries no information, and the first job somebody writes should not
have to know that sources are a concept.

Two rules follow from sources being plural. **Authority is per source**: a repo
that will not parse keeps its last good tree serving and does not stop the
others loading — `je source` shows which one is stuck. And **a chain resolves
job names within its own source**, so a chain file never writes a source name
and the same repo registered twice wires itself correctly both times.

Removing a source stops its jobs running and keeps every run they did (D19).

### Writing a job

`je new weather-ingest` writes the file; `je new --script` also writes
`scripts/weather-ingest.sh` with the whole protocol in it, since there is no SDK
to read.

A job is a YAML file naming a command. The file's name is the job's name.

```yaml
# weather-ingest.yaml, in your jobs repository
name: Weather Ingest
command: ["python3", "scripts/ingest_weather.py"]
workdir: ~/code/almanac

state:
  primary_cursor: since
```

A job in a language with dependencies declares it, and the worker installs them
from the repository's own lockfile before running the command:

```yaml
# ingest.yaml, beside package.json and pnpm-lock.yaml
language: typescript
command: ["tsx", "ingest.ts"]
```

Installed once per tree and keyed on `(language, tool version, lockfile)`, so a
commit that did not touch dependencies installs nothing. The job's command is
still the job's command — the language only decides what gets installed and what
goes on PATH (D28).

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

The file above sets four things and the engine will use about fifteen. That is
only honest if you can see the other eleven, which is what `je explain` is for:

```console
$ je explain weather-ingest
weather-ingest  Weather Ingest
Pulls readings from the local station into the almanac store.

  command                 python3 scripts/ingest_weather.py  (weather-ingest.yaml:4)
  runtime                 process                            (default)
  runs_on                 default                            (default)
  timeout                 30m                                (weather-ingest.yaml:5)
  overlap                 skip                               (default)
  retry                   none                               (default)
  on_interrupt            fail                               (default)
  state.commit            on_success                         (default)
  state.primary_cursor    since                              (weather-ingest.yaml:12)
  secret STATION_API_KEY  set

starts when
  every 15m  catch_up: once  weather-ingest.yaml
```

Every line is either a decision you made, at the line you made it on, or a
default — there is no third category, and nothing is left for you to guess.

It also answers a question the file cannot. A job never names another job, so
"what starts this?" is not something its own file knows:

```console
$ je explain daily-rollup
...
starts when
  run.succeeded job=normalize-readings  chain daily-weather step 2  daily-weather.yaml
```

### Chains

A job never names another job. What happens after what lives in one file per
flow, and the file's name is the chain's name.

```yaml
# chains/daily-weather.yaml, in your jobs repository
description: ingest readings, normalise them, roll them up

steps:
  - on: { event: weather.ingested }
    run: normalize-readings

  - on: { event: run.succeeded, where: { job: normalize-readings } }
    run: daily-rollup
```

Two kinds of pattern, and they are the same mechanism. `weather.ingested` is an
event a job emitted itself by appending a line to `JOB_EVENTS_FILE`; the engine
emits `run.succeeded` about every run it finishes. `where` compares top-level
fields of the payload for equality and does exactly nothing else — the moment
that grows an expression language, we have written a workflow engine by
accident.

**A chain is a name, not a runtime entity.** There is no chain lock, no chain
state machine, no "the chain is running". Each step fires as an ordinary
trigger the instant its event lands. What the name buys is that a flow becomes
something you can talk about:

```console
$ je chains
CHAIN          STEPS  LAST     STATE
daily-weather  2      4m ago   complete (2m 14s)
photo-archive  2      3d ago   stopped at step 2: convert-photos failed

$ je chain daily-weather
daily-weather
ingest readings, normalise them, roll them up
chains/daily-weather.yaml

  trigger  weather-ingest      run 11  succeeded in 12s     
  step 1   normalize-readings  run 12  succeeded in 48s     on weather.ingested
  step 2   daily-rollup        run 13  succeeded in 1m14s   on run.succeeded job=normalize-readings

complete, 2m14.2s end to end
```

**End-to-end duration and end-to-end failure are only expressible because the
flow has a name.** "The chain takes 40 minutes now, it used to take 5" is not a
question any job-level view can answer. Nothing here is stored: the view walks
the causation the engine already records — a run points at the event that caused
it, and that event points at the run that emitted it — so there is no chain
state to maintain, and nothing to go stale.

When a step fails, downstream steps do not run, and there is no cancellation
involved: the event they were waiting for simply never happens. `je chain` says
where it stopped and exits 3.

Two things are refused when the file is saved, because both are silent at
runtime. A step naming a job that does not exist is a load error rather than a
rule that never fires. And a cycle — `a` triggering `b` triggering `a` — is
rejected with the loop printed, rather than discovered later by the causation
depth guard after ten runs of the wrong thing.

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

#### Secrets the control plane cannot read

The store above lives with the control plane, which means the control plane can
read it. That is right for one machine and wrong the moment a worker runs
somewhere you do not fully control, so a secret can instead be encrypted into
the source it belongs to:

```console
$ je secret set --source weather STATION_API_KEY
Value for STATION_API_KEY (not echoed):
set STATION_API_KEY in weather

commit this?  secrets(weather): set STATION_API_KEY
[y/N] y
committed: secrets(weather): set STATION_API_KEY
```

It edits **your checkout** and offers to commit, rather than sending the value
anywhere. That is the point: under D23 granting access should be a diff somebody
reviews, and the control plane's copy of a source tree is a cache that the next
sync overwrites.

The file is SOPS-shaped — **names cleartext, values encrypted** — so the control
plane can still tell that a declared secret exists without holding any key, and
`je jobs` still says `misconfigured` before it is set. The worker decrypts it,
and redacts it from log lines *before they cross the network*.

Who can read it is a list of machines, resolved by name:

```console
$ je worker keygen                       # on the machine that needs to read
wrote ~/.je/identity
public key  age19clwgef...
registered as buildbox's key on the control plane.

$ je secret recipients add --source weather buildbox
buildbox can now read weather's secrets
  age19clwgef...
  it can read every value in the file, including ones set earlier
```

**The name is the point.** A pasted age key is a string nobody checked — nothing
ties it to the machine you meant. `buildbox` resolves to the key that identity
registered over its own certificate, so "this machine may read production
credentials" is a statement about an identity the control plane issued. Pasting
a key still works and says plainly that nothing verified it.

### Not built yet

Retries, the container executor, and retention.

**Runtimes** (D28) install a job's dependencies before it runs:

```console
$ je worker runtimes
LANGUAGE    TOOL  STATUS
go          go    ready
python      uv    not installed -- je worker runtime install python
typescript  pnpm  ready

$ je worker runtime install python
downloading https://github.com/astral-sh/uv/releases/download/0.5.11/uv-aarch64-apple-darwin.tar.gz
verified sha256 695f3640d5b1a4e2

uv is ready for python jobs on this worker.
```

Nothing is installed that cannot be checked against a published checksum — the
same rule `je upgrade` applies to itself. A worker advertises what it can
prepare, so a job whose language nothing serves is **queued and visible** in
`je waiting` rather than dispatched to a machine that would fail it.

**Fan-in is not built.** A step waits on one condition. `all_of` with a window
(D3) — "when both of these landed, within six hours" — parses and is refused
with a message saying so, rather than being an unknown key. The `trigger_state`
table it needs is already in the schema, so it is a feature and not a
migration. The same goes for `trigger.expired`, the "the thing I was waiting
for never came" event, which is where the alerting story starts.

**Sources are re-read on request, not on a timer.** `je source sync`, and once
at start. A per-source `interval:` is the GitOps version and is not here: an
engine that silently pulls new code on a schedule before you have watched it do
so once is a lot of trust to extend up front.

**Only GitHub, and only over HTTPS.** Other hosts, and SSH, are not built.

**Definitions are reloaded on request, not watched.** `je sync` re-reads the
source and rebuilds the schedule table, so an edit takes effect without dropping
in-flight runs. Watching the directory automatically is still open. A job
declaring `language:` loads but is marked misconfigured and will not run, rather
than running without what it asked for.

**A secret in the control plane's own store reaches a worker in the dispatch.**
That is correct for a trusted network and wrong for anything else, which is why
`je secret set --source` exists: those never reach the control plane at all. Two
stores is the honest state of it, and the seam is deliberate rather than
overlooked (D10/D25).

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
| `internal/gitsource` | fetching a repository as a tarball, and unpacking it safely (D22). |
| `internal/selfupdate` | finding, verifying and installing a new binary. |

## Building

```
make build        # ./je
make check        # fmt, vet, test
make image        # a scratch container image (D19)
make release-dry  # build every release artifact locally, as CI does
make web          # the client's dev server, with hot reload
make web-build    # rebuild the assets the binary embeds
```

The web client's built assets are committed under `internal/webui/dist` and
embedded by `go:embed`. That is a generated artifact in version control on
purpose: it is what lets `go build ./cmd/je` produce a working binary on a
machine with no node toolchain, and what keeps the image a single static build.
Only `make web-build` regenerates it, and only changes under `web/` need it.

Four direct Go dependencies, and the list is meant to stay short:
`modernc.org/sqlite` (the pure-Go driver, which is what keeps the binary static
and cgo-free, so one artifact is both the control plane and the worker and runs
in a terminal, in a container, in a cluster, and inside a Mac app — D18),
`filippo.io/age` for encrypted secrets (D25), plus `gopkg.in/yaml.v3` and
`golang.org/x/term`.

`age` was chosen over the SOPS library by measurement rather than taste:
`filippo.io/age` pulls in 13 modules, `github.com/getsops/sops/v3/decrypt` pulls
in 341 — 187 of them AWS, Azure, GCP and Vault SDKs this project will never
call. The file format keeps SOPS's useful shape anyway (see D25).

The component that genuinely needs the native path is the **worker**: a job that
drives Apple Shortcuts or reaches a device on your LAN cannot run in a container,
so the binary has to stand on its own (D20). The control plane and the web client
run natively too, but nothing is designed around keeping that true — it is a
property of the binary, not a goal (D23).

## Where things live

```
~/.je/            data directory (override with JE_DATA_DIR)
  state.db        runs, events, definitions, cursors
  logs.db         captured job output, separately (D4)
  lock            the single-writer flock
  secrets.json    the local secret store, mode 0600 (D10)
  daemon.json     the control plane's address, so clients can find it
  ca/             the authority it issues worker and client identities from
  cache/
    sources/      fetched repositories, under the commit they came from
```

Nothing here is a definition. Everything under `cache/` can be deleted and
re-fetched, and everything else is state this engine produced.
