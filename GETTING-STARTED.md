# Getting started

Fifteen minutes, from nothing to a job of your own running on a schedule and a
view of what it did.

No concepts up front. If you want the reasoning behind any of it, the
[README](README.md) has the design and `PROPOSAL.md` has the arguments — but
none of that is needed to get something running.

---

## 1. Install it (1 minute)

```console
$ curl -fsSL https://raw.githubusercontent.com/jdmorlan/job-engine/main/install.sh | sh
$ je version
```

One static binary. It is the CLI, the control plane and the worker — which
matters later, and not yet.

## 2. Start it (1 minute)

```console
$ je quickstart
je: control plane on 127.0.0.1:7620, one worker attached (default)
    try:  je jobs        in another terminal
          je run <job>
```

Leave that running and open a second terminal. Everything below happens there.

Two things just started: a **control plane**, which decides what should run and
remembers what did, and a **worker**, which actually runs it. They talk over
HTTPS with certificates the control plane issued itself — you will never see
that unless something breaks.

```console
$ je status
control plane  running (v0.6.0)
workers        1 online (default)
uptime         5s
```

## 3. Get some jobs (2 minutes)

Job definitions live in a git repository, always. `je demo` registers this
project's own examples so there is something to look at before you write
anything:

```console
$ je demo
registered github.com/jdmorlan/job-engine/demo as "demo" at 4a360e39

  demo/demo-hello    prints a line and exits
  demo/demo-counter  keeps a cursor and advances it
  demo/demo-flaky    fails about one run in three
  demo/demo-tick     runs every minute
  ...
```

```console
$ je jobs
JOB                STATUS  COMMAND                        FILE
demo/demo-hello    ok      /bin/sh -c "echo 'Hello fro…   demo-hello.yaml
demo/demo-counter  ok      /bin/sh scripts/counter.sh     demo-counter.yaml
```

A job's name carries the source it came from. `demo/demo-hello` is the
`demo-hello` job from the `demo` source.

## 4. Run one, and see what happened (2 minutes)

```console
$ je run demo/demo-hello
Hello from je. This job did nothing else.

ok  run 1 succeeded in 19ms
```

Now the thing most schedulers make you build yourself — a **cursor** that only
moves when the job succeeds:

```console
$ je run demo/demo-counter
the cursor said 0, so this is run number 1

ok  run 3 succeeded in 32ms
    cursor  count  2026-09-04T17:15:08Z -> 1  (v2)

$ je run demo/demo-counter
the cursor said 1, so this is run number 2

ok  run 4 succeeded in 28ms
    cursor  count  1 -> 2  (v3)
```

Run `demo/demo-flaky` a few times — it fails about one time in three — then:

```console
$ je state history demo/demo-flaky
```

The cursor moved only on the runs that worked. A job that fails half way does
not advance its watermark and quietly skip the records it never processed.

**Wait two minutes and look again.** `demo/demo-tick` runs every minute, and
nobody asked it to:

```console
$ je runs demo/demo-tick
```

## 5. Watch one job set off another (2 minutes)

```console
$ je run demo/demo-ingest
ingested 23 rows

ok  run 5 succeeded in 27ms
    emitted demo.ingested (event 16)

$ je chain demo/demo-pipeline
demo/demo-pipeline
ingest, report on what arrived, then archive

  trigger  demo/demo-ingest   run 5  succeeded in 27ms
  step 1   demo/demo-report   run 6  succeeded in 20ms  on demo.ingested
  step 2   demo/demo-archive  run 7  succeeded in 23ms  on run.succeeded job=demo/demo-report

complete, 2.023s end to end
```

Nothing polled. `demo-ingest` emitted an event, `demo-report` was waiting for
it, and `demo-archive` was waiting for `demo-report` to succeed.

## 6. Now one of your own (5 minutes)

This is the part that needs a git repository, because that is the only place
definitions live. `je init` writes the tree:

```console
$ je init ~/my-jobs
$ cd ~/my-jobs
```

Write a job. `--language` scaffolds the script too, with the whole protocol in
it as comments:

```console
$ je new water-plants --language python
wrote water-plants.yaml
wrote scripts/water-plants.py
```

Open `water-plants.yaml` and give it a schedule:

```yaml
name: Water Plants
language: python
command: ["python", "scripts/water-plants.py"]

on:
  - every: 1h
```

Push it, because the engine reads the repository rather than your disk:

```console
$ git init && git add -A && git commit -m "water the plants"
$ gh repo create my-jobs --private --source=. --push
```

Point the engine at it:

```console
$ je source add house <you>/my-jobs
resolving <you>/my-jobs...
registered house -> <you>/my-jobs
  resolved main -> a1b2c3d
  jobs  1

$ je run house/water-plants
```

The worker installed the job's Python dependencies from your lockfile before
running it. If `uv` was not on the machine, it will have told you to run
`je worker runtime install python` — nothing is installed behind your back.

**From now on the loop is:** edit, commit, push, `je source sync house`.

## Where to look when something is wrong

These four are the whole debugging story, and each answers one question:

```console
$ je status      is anything actually running
$ je waiting     what has not happened yet, and what is stuck
$ je jobs        which definitions loaded, and which are broken
$ je runs        what happened, newest first
```

`je waiting` is the one worth learning. It distinguishes *queued* from *blocked*
from **nobody can run this** — a job pinned to a label no worker advertises, or
written in a language no worker can prepare. Those look exactly like ordinary
queueing and never resolve on their own, so they are called out:

```console
$ je waiting
WAITING FOR A WORKER  (queued for a label nothing is serving)
  runs_on: macos
    3 run(s), jobs: house/water-plants
    start one:  je worker run --labels macos
  On another machine, enroll it first:  je enroll <name> --labels <label>
```

Everything has an exit code, so `je waiting` in a health check is meaningful
rather than decorative.

## When you are done

```console
$ je demo --remove      unregister the examples
$ je reset              tear it all down on this machine and start over
```

`je reset` removes the databases, certificates and caches — never your
repositories, which is where anything you wrote lives.

---

## What to read next

- **[README](README.md)** — how the system is put together, and why: the control
  plane/worker split, sources, secrets, identity.
- **`je help <command>`** — every command explains itself, including the
  reasoning where there was a real decision.
- **`PROPOSAL.md`** — the full argument for every design decision, including the
  ones that were rejected.
