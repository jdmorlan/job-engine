# Job Engine — the pitch

**Date:** 2026-08-14
**Companion to:** `PROPOSAL.md`

---

## The 30-second version

> Background work today has two options: cron, which runs things but tells you
> nothing, or a platform-scale orchestrator, which tells you everything but needs a
> team to operate. There's nothing good in between for one person or one small team.
>
> `je` is a single static binary with an embedded database. It runs your scheduled
> and event-driven work, and it can always answer three questions: **what ran, why
> it ran, and what it actually processed.** It starts as a download on your laptop
> and ends up in your cluster without you rewriting anything.

---

## The 2-minute version

Every system eventually needs things to happen in the background — on a schedule,
or because something else happened. Almost everyone solves this twice: badly with
cron at first, then expensively with a real orchestrator once cron burns them.

The gap between those is enormous and mostly empty. Cron gives you no history, no
retries, no idea whether last night's run worked. The orchestrators fix that, but
they arrive with a scheduler, a metadata database, a message broker, workers, a web
service, and a deployment story that assumes someone's job is keeping it alive. For
a single person with fifteen real jobs, the second option costs more than the
problem.

`je` targets the middle: **durable, observable, event-driven background work with
zero operational surface.** One binary. SQLite. No broker, no Redis, no Postgres,
nothing to run alongside it.

What that buys structurally is unusual. Because it's one static binary with no
dependencies, the *same artifact* runs foreground on a laptop, as a background
daemon, inside a container, and embedded as the scheduling layer of a desktop app.
Most tools in this space can be one or two of those. Being all four is what makes
the progression below possible at all.

---

## What it tries to get right that others haven't

### 1. Visibility is the founding constraint, not a dashboard

Most job systems treat observability as a feature you add: a UI, some metrics, a log
viewer. Here it's the rule that generates the design — *a job you can't see is a job
you'll be confused by at 2am.*

Concretely, that principle is why the engine records its own downtime as events, why
it can print the causation chain that led to any run, why the router's decision is
stored rather than inferred, and why "nothing needs your attention" is a query with
an exit code rather than a vibe. Those decisions weren't features on a roadmap. They
each fell out of refusing to ship a state a human couldn't explain afterward.

The test isn't the demo. It's the 2am incident.

### 2. It knows what you processed, not just when it ran

This is the most underserved idea in the project and the one most likely to matter
in practice.

Almost every scheduler tracks *when a job last ran*. That's the wrong fact. What
you need is *what it last successfully processed* — the cursor. Run time and
progress diverge the instant anything fails, retries, runs late, or gets skipped
while the machine was asleep, which is to say constantly.

Treating job state as a first-class, durable, inspectable thing means catch-up after
downtime is correct by construction rather than by a hand-written watermark table in
every single job. Anyone who has written ETL has built this by hand, badly, more
than once.

### 3. One ingress, and no integrations at all

The engine knows nothing about the outside world. There is no plugin directory, no
source catalog, no webhook framework. There's one command:

```
je emit homekit.motion --payload '{"room":"office"}'
```

That single escape hatch turns a HomeKit automation, a git hook, a file watcher, a
CI pipeline, a phone shortcut, or a desktop app into an event source without the
engine containing a line of code about any of them.

The unusual claim is that **this is better than integrations, not a substitute for
them.** Integrations rot: they hold credentials, break on vendor changes, and each
one is a subscription state machine someone has to maintain. Pushing that
responsibility outward is what keeps a single binary a single binary — and it's why
the feature list stays short while the coverage stays wide.

### 4. Laptop to cluster is a progression, not a port

This is the part almost nothing in the category does.

| Stage | Cost to get there |
|---|---|
| Run a job in the foreground | download a binary |
| Real daemon, real schedules | one command |
| Always-on cluster | one manifest |
| GitOps, repo as source of truth | one commit |

The governing rule: **a job definition that works locally runs in the cluster
unmodified.** No Kubernetes-shaped fields leak into job files, and every command you
learned locally works against the remote engine by switching context.

Tools tend to pick an end of this. The laptop-scale ones are toys you outgrow; the
cluster-scale ones can't be tried in five minutes, so they're only ever adopted by
decision rather than by discovery. Making the whole path continuous means you can
find out whether it's useful before you commit to operating it.

### 5. Definitions are files, and the tool writes them

Jobs are YAML on disk — diffable, reviewable, revertible. But you're not expected to
hand-author schema from memory: the CLI has language for what you want to do and
writes the file for you.

Files hold intent. The tool renders truth. Neither pretends to be the other, which
is what keeps a git-backed workflow from needing a control plane to make it work.

### 6. The non-goals are load-bearing

No distributed workers. No workflow DSL. No execution replay. No multi-tenancy.
No hosted version.

Every one of those is a real feature that real systems need — and every one is a
tax on the thing this is trying to be. The list exists so the project can stay
finishable, and so the answer to "why is this simple?" is a decision rather than an
omission.

---

## Why now

Background processing is quietly becoming the load-bearing part of AI-era systems.
The interesting behavior isn't a single model call — it's *things happening because
other things happened*: capture something, enrich it, react to it, surface it at the
moment it's useful.

That's a pipeline of independent pieces, and the processing organ in the middle is
exactly this. The bet isn't that a job engine is novel. It's that the systems being
built now need a reliable, legible, embeddable one that a single person can actually
run — and that the seams between it and the pieces around it matter more than any
one piece being clever.

Build individual pieces that integrate well. It's all about the seams.

---

## What it isn't

Not a workflow orchestrator. Not a data platform. Not a distributed task queue.
Not a hosted service. Not a replacement for the platform-scale tools at the scale
those tools are for.

It's the thing you reach for when cron isn't enough and Airflow is absurd — which,
for most people most of the time, is the actual situation.

---

## The honest one-liner

**Cron you can debug.**
