# Job Engine — Design Proposal v0.5

**Date:** 2026-09-02
**Status:** 21 items locked, and **the skeleton is built** — daemon, store, API,
CLI, and the engine's own lifecycle events, under test. 5 items still want your
input, none of them on the critical path for job #1.

---

## Changelog since v0.4

**This round is entirely about the job author's experience**, and it started with
you reacting badly to the first real job I wrote against D6:

> *"That whole writeFileSync/appendFileSync stuff just seems labourious... as a
> developer I think I'm writing history to a file, but I'm like where is that
> file — when in reality it's just the medium. The actual history and metadata is
> in the job-engine system itself."*

That's a precise complaint and it produced three changes and one withdrawal:

- **D21 (new) — shim injection.** Your idea, and it corrects an over-reach in D6.
  We banned SDKs because publishing them to five package managers is a
  maintenance disaster. But that cost is about *distribution*, not abstraction —
  a shim embedded in the binary and materialised at run time pays none of it, and
  is strictly better than a published SDK on version skew.
- **D14 — the engine seeds the cursor.** Your point that a first run shouldn't
  hand the author an empty object. Adopted, with one sharp constraint: it is a
  *seed*, never a maintained value, or we rebuild the exact dragon D14 exists to
  kill. Also added `lastSuccessAt`, which we can serve for free and which makes
  the two facts that must not be confused impossible to confuse.
- **D6 — state arrives in the environment, not a file.** The only surviving piece
  of a larger change I proposed and withdrew (below).

- **D22 (new) — job sources.** Definitions and their code arrive from
  registered sources, several at once, each logically separate but all feeding
  one engine and one timeline. This is D19's git source arriving early, for a
  local reason rather than a cluster one, and the multi-source shape improves on
  D19's global local-or-git mode switch.

**A proposal I made and withdrew, recorded because the reasoning matters.** I
proposed collapsing the four job-protocol channels into one tagged JSONL file, to
make jobs less laborious to write. You asked the right question:

> *"Is the only reason we initially reduced them to benefit dev experience? Do we
> gain something from going back to 4 channels? It seems like if we do, it's
> better to keep that as is, and utilize the shims sugar to help developers."*

Ergonomics was the *only* reason, and with D21 available it was the wrong layer to
pay at. Withdrawn; the four channels stand unchanged. The general rule that fell
out of it is stated in D21 and is the most useful thing in this round:

> **The protocol is shaped by mechanism. The shim is shaped by ergonomics.**

Before a sugar layer existed, the protocol was the only lever I had for developer
experience — so I reached for it, and started encoding a convenience concern into
a contract that every language, every container, and D20's node relay would have
to honour forever. Worth watching for: it is the same mistake in a different
costume every time.

## Changelog since v0.3

**Q3 is answered, so we can start building.** Option (a): I write it, explain the
patterns, you review. Everything below is now specified well enough to code against.

Your answers changed three things and added one:

- **D17 → chain files, not a routes file.** *"A route file should represent one
  chain, not all chains in the system."* Right, and it's better than what I
  proposed: it kills the god-file by structure rather than by discipline, and a
  *named* chain turns out to be something the tool can show status for. Revised.
- **D2 → simpler than I thought.** You don't write YAML comments, so the
  comment-preservation work I was bracing for mostly evaporates. Your "the CLI
  should have language around what you want to do" point produced a real design
  rule — see **P3**.
- **D15 → `je`, no web UI, daemon+client.** Locked. Your sleep question has a
  disappointing but clear answer, below.
- **Q2 → Almanac embedding is the biggest idea in this round** and it's now
  **D18**. Short version: the daemon+API decision (D15) already solved it, which is
  a nice payoff for a choice we made for unrelated reasons.

Also promoted: **P3 — files hold intent, the tool renders truth.** It surfaced
independently in three separate items, which is usually the sign that a principle
is real rather than invented.

## Changelog since v0.2

Your router observation landed as **D17 (Routing and topology)**, placed after D3
because that's where it belongs by topic even though it's numbered later. It
separates the three things "router" was doing at once, and produced knock-on edits
in four places:

- **D6 gains `JOB_EVENTS_FILE`** — a real hole. As written in v0.2, container jobs
  had no way to emit events at all, which would have made routers process-only.
- **D3's fan-in example moved to routes-file form**, which reads better — a fan-in
  relationship belongs to neither job, so authoring it inside one of them was always
  a little off.
- **D3's loop guard gets stronger**: declarative routes can be cycle-checked at
  load time rather than caught at depth 10 mid-incident.
- **D11 gains route provenance**, so "why did this run?" doesn't point at a job
  definition that doesn't contain the answer.

## Changelog since v0.1

Your answers moved these:

- **D2 → CLI-first, git-ops style.** This turned out to be the biggest structural
  change in the document. It cascades into D12 and produced the new **D15**.
- **D3 → stateful triggers.** Your idea about evaluating triggers against a
  *timeline* of events rather than a single event is good enough that I've promoted
  fan-in from a flat non-goal to a designed feature. Detailed proposal in D3.
- **D7 → manual re-run is an attempt, not always a new run.** You were right and I
  was modeling it too rigidly. Revised.
- **D9 → job state / cursors.** Your point about "last successful record" vs. "last
  run time" is the best product observation in your responses, and it's big enough
  that it's now its own item, **D14**.
- **D13 → tighter retention** (30 days), with one coupling constraint I added.
- **N1 → workers ≠ distributed** (terminology fixed), fan-in un-non-goaled.
- **Two of your responses were principles, not answers**, and I've promoted them to
  a new Part 0 (the Visibility Rule, and system actions are jobs).

New items opened by your answers: **D14** (job state), **D15** (interface
architecture), **D16** (daemon lifecycle and generic event ingress).

## Where things stand

| Item | Topic | Status |
|---|---|---|
| P1, P2 | Visibility Rule, engine work is jobs | AGREED (your words) |
| P3 | Files hold intent, tool renders truth | **NEW — react** |
| F1 | Vocabulary | AGREED |
| D1 | Executor interface | AGREED |
| D2 | Definitions on disk + CLI | AGREED (revised) |
| D3 | Chaining / stateful triggers | AGREED — v1.1, schema-ready |
| D17 | Chains and topology | AGREED (revised) — one open sub-question |
| D4 | SQLite | AGREED |
| D5 | Crash recovery | AGREED |
| D6 | Job protocol | AGREED (revised v0.5, cap settled) |
| D7 | Retries and re-runs | AGREED |
| D8 | Timeouts / concurrency | AGREED |
| D9 | Schedules | AGREED |
| D10 | Secrets | AGREED |
| D11 | Definition versioning | AGREED |
| D12 | Observability surface | AGREED |
| D13 | Retention | AGREED |
| D14 | Job state / cursors | AGREED (revised v0.5) — **2 questions open** |
| D15 | Daemon + API + CLI (`je`) | AGREED |
| D16 | Daemon lifecycle + event ingress | AGREED |
| D18 | Embedding / Almanac | **NEW — react** |
| D19 | Kubernetes deployment / local-to-cluster | **NEW — react** |
| D20 | Control plane + nodes / placement | **NEW — react** |
| D21 | Shim injection | **NEW (v0.5) — one open question** |
| D22 | Job sources | **NEW (v0.5) — two open questions** |
| N1 | Non-goals | AGREED |
| N2 | v1 done | AGREED |
| Q1 | Storage adapters | AGREED — SQLite only, no adapter |
| Q2 | First three jobs | PARTIAL — see D18 |
| Q3 | Working style | AGREED — option (a) |

**Nothing here blocks continuing.** D14's remaining questions have defaults I'm
comfortable building against, and D3 is v1.1 anyway. The items wanting input —
P3, D17's sub-question, D18, D19, D20, D21 — can be answered while code is being
written, with two exceptions worth naming:

- **D20's constraint list should be agreed before any node code exists**, since
  the constraints are the design rather than a detail of it.
- ~~D6's state-size cap~~ — **settled: 64KB**, state arrives in the environment.

Same protocol: type in the `Your response` blocks, save, hand it back. Items marked
AGREED with no block are locked; type under one to reopen it.

---

## Part 0 — Principles

Two of your v0.1 answers weren't answers to the question asked — they were rules
for the whole project. They belong at the top.

### P1. The Visibility Rule

**Status:** NEW

> *"If we can't make something visible, I think we should declare the feature as
> stalled until we have a good model for how to make it visible. I think that rule
> should apply throughout the system as we build features."* — you, in D3

Adopted as a hard rule. Restated for the doc:

**A feature is not done when it works. It is done when a human can see it working,
see it failing, and see why.** If we can't describe the view before we build the
mechanism, we don't build the mechanism yet.

Three consequences worth naming, because this rule has teeth:

- It applies to the **negative space**, not just to runs. A trigger that is
  *waiting* — for a peer job, for a window to close — is a thing happening in the
  system, and it must be visible. This is the single largest gap in every job
  engine I know of: they can all show you what ran, and none can show you what
  didn't run and what it's waiting for. If we do only one thing better than the
  incumbents, I'd like it to be this.
- It gates D3 (stateful triggers) directly: pending trigger state is invisible by
  nature, so we don't ship the feature until the "what am I waiting on?" view is
  designed.
- It gates D14 (cursors): job state that moves silently is exactly the dragon you
  described, so the cursor's movement over time is part of the feature, not a
  follow-up.

### P2. The engine's own work is jobs

**Status:** NEW

> *"I really like that this is another job and really agree with system actions
> being jobs like everything else. Plus it grows out our trigger library."* — you,
> in D13

Adopted. Retention/vacuum, log compaction, health checks, and anything else the
engine does on a timer are defined as ordinary jobs with a `system.` prefix. They
appear in the job list, they have runs, they have logs, they can fail visibly.

Two riders:

- **This is a forcing function, not decoration.** If a system job is awkward to
  express in our own job format, the format has a hole. Free dogfooding on every
  internal feature.
- **They're filtered from the default view**, or the "is everything OK?" screen
  becomes 80% housekeeping. Visible on request, not in your face.

### P3. Files hold intent; the tool renders truth

**Status:** NEW — react

This one I'm proposing rather than quoting, but it came out of *your* answers — it
showed up independently in three places, which is usually how you tell a principle
is real instead of invented:

- **D2:** *"other systems [scaffold the full template] and I just get lost in all
  the configuration options."*
- **D17:** you rejected a file that contains the whole topology.
- **D12/P1:** the waiting view — the engine tells you what it's about to do.

The common thread: **a config file should contain only what you decided. Everything
else — defaults, resolved values, the full picture — is the tool's job to show on
demand.**

Consequences:

- **Scaffolds are minimal.** `je new` writes the four lines you asked for, not a
  commented catalog of every option. Nothing appears in a file that isn't a
  deliberate choice, so a file always reads as *intent*.
- **`je explain <job>` shows effective config** — declared values, inherited
  defaults, and where each came from. This is where the "what are my options?"
  question gets answered, not in a template.
- **`je routes` / `je chains` show resolved topology** regardless of authoring
  location (D17).
- **`je waiting` shows intended future work** (P1).

It also resolves a tension I'd been carrying: I kept wanting to put more in the
YAML so it would be self-documenting, and you kept wanting less in it. P3 says
you're right, *provided* the tool answers the questions the file no longer does.
That's a real obligation on us, not a free pass to be terse.

**Your response** (all three principles):

```



```

---

## Part 1 — Framing

### F1. Vocabulary

**Status:** AGREED

Externally the project is a **job engine**. Internally we use the precise
vocabulary: Event, Source, Trigger, Job, Run, Attempt, Executor. Event-first
architecture: a schedule is not a special mode, it's a source that emits events;
so is a manual invocation; so is a finished run.

> *"I agree...we should keep it externally named a job-engine...but I like the
> vocabulary."*

One addition since v0.1, from D14: **Job State** — a small, engine-managed,
per-job blob of durable memory (a cursor/watermark). It's the eighth noun.

**Ninth noun, added in v0.4 from D20: Node** — a machine holding a durable session
with the control plane. The distinction that matters, and the reason this isn't just
called a *worker*:

- A **Source** emits events and needs **no session, no registration, no identity**.
  A git hook is a source. A Shortcut is a source. A phone firing `location.arrived`
  from the background is a source. This is D16's one-ingress rule, and it must stay
  stateless — an iOS app *cannot* hold a connection in the background, so making
  emission depend on a session would break the phone case outright.
- A **Node** registers, advertises capabilities, and holds a bidirectional session,
  which is what lets the control plane push work and attention *to* it.

**Worker is a role a node plays, not an entity.** A node may carry the *execute*
role (runs jobs pinned by `runs_on`), the *receive* role (gets attention items
pushed), or both — and it emits events like anything else, which is a property of
having a network connection rather than a privilege of being a node.

One machine commonly wears several hats: your phone is a Source always, and a Node
only while the app is foregrounded. That's why the two concepts stay separate.

*Naming note:* **node** collides with Kubernetes' node, and D19 puts this project
next to a cluster. When ambiguous, say *engine node* vs *cluster node*. The
alternative was *device*, rejected because a container acting as an executor is not
a device. In prose, "control plane / data plane" describes the split accurately;
`node` is just the CLI-friendly name for a data plane member.

---

## Part 2 — The core decisions

### D1. Execution: executor interface, process first

**Status:** AGREED

Two implementations behind one interface: `process` (default, zero dependencies,
1-second feedback loop) and `container` (prebuilt image, Docker optional at
runtime). Identical job protocol either way, so promoting a job from process to
container is a one-line change. `process` jobs have no isolation — documented, not
mitigated.

> *"I like the concept of an executor interface. That means we can grow into what
> we need... initially testing of jobs can be really fast... I think other job
> engines often lack this workflow benefit."*

---

### D2. Definitions on disk, driven by a CLI

**Status:** AGREED (revised in v0.4)

**Your answer.**

> *"I almost think that initially having a good CLI can solve a lot of these
> problems. We could make it easy to edit frequently changed items... and it could
> just update the config file and do a git commit. Almost like git-ops style
> infrastructure frameworks. So we get file based storage, but we get nice tooling
> from day one... and I would argue that a CLI is far easier to create than a UI...
> but can be equally powerful."*

**Where I land.** Agreed on all three parts: files stay the source of truth, the
CLI is the primary way you touch them, and yes — a CLI is dramatically cheaper to
build than a UI and loses very little. The general question of CLI-vs-web as the
*whole product's* interface is big enough that it's now **D15**; this item is just
about definitions.

The git-ops framing is the part I want to sharpen, because "the CLI edits YAML and
commits" has two known traps and I'd rather hit them on paper.

**Trap 1: machine-edited YAML loses your comments.** If you hand-write a job file
with comments explaining *why* a timeout is 4 hours, and then run
`je set sync-stars --timeout 6h`, a naive YAML round-trip rewrites the file and
your comment is gone. Go's `yaml.v3` Node API can preserve comments and ordering,
but only if we're disciplined: parse to Node, mutate the specific node, re-encode.
It's fiddly and it's the kind of thing that quietly breaks.

My recommendation: **field-scoped edits via the Node API**, plus a golden test
suite of files with comments in awkward places that must survive a round-trip. If
that proves miserable in practice, the fallback is that the CLI only ever *creates*
files and *reads* them, and edits are your editor's job — which is a real
degradation, so I'd rather pay the fiddly cost.

**Trap 2: auto-commit is either great or infuriating.** Committing on every
`je enable`/`je disable` produces a repo history of 400 one-line commits. My
recommendation:

- The jobs directory is a git repo **if you make it one**. The engine never runs
  `git init` behind your back, and it works fine without git.
- Auto-commit is **opt-in** via config (`git.auto_commit: true`), off by default.
- When on, commits are one-per-CLI-command with a generated message
  (`job: set timeout=6h on sync-stars`), and the CLI prints the commit SHA it made.
- The engine **never** pushes, never pulls, never resolves conflicts. Local repo
  only. Anything else is a distributed-systems problem in disguise.
- `je history <job>` shells out to `git log -p` scoped to that file, which gives
  you definition history for free — a nice payoff for the whole approach, and it
  complements D11's hash-based snapshots (git tells you *who and why*; D11's
  snapshots tell you *what actually ran*).

**Open questions for you:**

1. Auto-commit opt-in and off by default — agree, or do you want it on by default?
2. If comment-preserving round-trips prove painful, which way do you fall: CLI
   writes and accepts comment loss, or CLI stops editing and only scaffolds?
3. Does `je new <name>` scaffold a file from a template and open `$EDITOR`?

**Your response:**

```
1. auto-commit off by default works for me
2. I just don't write that many comments in yaml config files...so maybe I just don't understand the importance of this
3. I think so...but it can't be the full template...other systems do that and I just get lost in all the configuration options...I think this is where 
the CLI could help...by having language around what you want to do...and it can fill out the config for you. You should still have the option to do it 
all yourself...but I think sometimes for common things...its nice to have the tool do it for you. 


```

**Resolution (v0.4).** All three settled, and one of them made the work smaller.

1. **Auto-commit off by default.** Locked as proposed.

2. **Comment preservation: deprioritized, and that's a real win.** You don't write
   many YAML comments, which means the fiddly `yaml.v3` Node-API work I was bracing
   for isn't worth paying for up front. Plan: straightforward round-trip; if it ever
   eats something you cared about, we add preservation then. One consequence I want
   on the record — **we should not put explanatory comments in scaffolded files
   either**, since a CLI edit would silently delete them and that's a worse
   experience than never having them. That constraint pointed straight at your third
   answer, and at **P3**.

3. **Task-oriented CLI, minimal files.** This is now P3, so the short version here:
   `je new` asks what you're trying to do and writes only what you answered.

```
$ je new weather-ingest
  Command to run?      python scripts/ingest_weather.py
  How often?           every 15m
  Needs any secrets?   STATION_API_KEY
  ✓ wrote jobs/weather-ingest.yaml (9 lines)
    je explain weather-ingest   — see all effective settings and defaults
```

The file has nine lines and every one of them is something you said. Everything
else — the 1-hour default timeout, `overlap: skip`, retry defaults — lives in the
engine and is visible via `je explain`, which is exactly the "I get lost in all the
configuration options" complaint answered without hiding anything.

Non-interactive form for scripting and for when you know what you want:
`je new weather-ingest --every 15m --command "python scripts/ingest_weather.py"`.

---

### D3. Chaining, and your stateful-triggers idea

**Status:** AGREED — v1.1, schema-ready. *(You left this block empty; I'm treating
silence as no objection to the v1.1 sequencing, since nothing in v1 depends on it.
The two questions below are still worth answering when you get to them — especially
`trigger.expired`, which is cheap now and awkward to retrofit.)*

**Your answer.**

> *"I agree... but I wonder if we can have the concept of stateful triggers... so
> I'm not just evaluating triggers on job definitions when we receive an event...
> I'm also checking a timeline of events... I think there is a way with something
> like that to achieve fan-in type structures."*

**Where I land.** This is the best idea in your responses and I've changed my
recommendation because of it. In v0.1 I made fan-in a non-goal because I was
assuming the only way to get it is a DAG engine. You're right that there's a
cheaper path, and it stays inside the event model rather than bolting a second
model on top.

The reframing: a trigger is not a function of one event, it's a **standing query
over the event log**. "When A succeeded *and* B succeeded, both within the last 6
hours, and I haven't already fired for this pair" is a query, not a graph. Because
the event log is already durable in SQLite and already carries causation (D3/D12),
the machinery mostly exists.

**Proposed shape** (revised in v0.3 to routes-file form, per D17 — a fan-in
relationship belongs to neither job, so authoring it inside one of them was always
slightly off):

```yaml
# chains/daily-weather.yaml   (v0.4: chain file, one flow per file — D17)
steps:
  - on:
      all_of:
        - { event: run.succeeded, where: { job: extract-weather } }
        - { event: run.succeeded, where: { job: extract-power } }
      within: 6h            # all conditions satisfied inside this window
      fire: once_per_window # or: every_time
    run: daily-rollup
```

Semantics, stated tightly because this is where these features go wrong:

- **Evaluation is on arrival.** Each incoming event updates the satisfaction state
  of every trigger that could match it. Fully satisfied → fire. No polling.
- **Pending state is durable.** A partially-satisfied trigger survives restart —
  it's a row, not memory. This matters enormously on a laptop that sleeps.
- **The window is a sliding lookback** from the incoming event, evaluated against
  the event log. Restart-safe by construction, since the log is the state.
- **`fire: once_per_window`** dedupes: after firing, the consumed events are marked
  so the same pair can't fire twice.
- **Timeouts are events too.** A trigger that goes unsatisfied for its full window
  emits `trigger.expired`, which is itself matchable. That's how you get "alert me
  if the nightly pair didn't both land by 6am" — a genuinely useful thing that
  most engines can't express at all.

**What the Visibility Rule (P1) demands before we build this:**

```
$ je waiting
TRIGGER                     WAITING ON        SATISFIED           EXPIRES
daily-rollup/all_of         extract-power     extract-weather     4h 12m
                                              (03:04, run 8821)
```

I'd go further: this view is the feature. The mechanism is a few hundred lines;
being able to answer "why hasn't the rollup run?" without reading logs is the
thing that makes it worth having.

**Two things I still want to bound, or this becomes Temporal:**

- **No cross-run mutable state in triggers.** A trigger may inspect the event log;
  it may not accumulate its own scratch memory. The log is the only state.
- **No `where` expression language beyond field equality and simple comparison in
  v1.** The moment we ship a full expression DSL we've built a workflow language
  by accident. Start with equality on event fields; extend only when a real job
  needs it.

**Sequencing recommendation.** Design it now, build it in **v1.1**, but put the
`trigger_state` table in the schema from day one so it isn't a migration later.
The reason to defer the build: fan-in has no value until you have two jobs that
need to fan in, and per N2 you have one real job identified. I'd rather prove the
core loop on weather ingest and then build this against a real second and third
job than build it speculatively.

**Loop guard**, strengthened in v0.3: events carry a causation chain and depth, and
we refuse past depth 10 — but that's a runtime backstop that fires *during* an
incident. Because routes are declarative (D17), we can do better: **the trigger
graph is statically checkable at load time**, so `A → B → A` is rejected when you
save the file, with the cycle printed. The depth counter remains for the cases
static analysis can't see — dynamic routers (D17) and self-triggering chains
through `je emit`. Also cap the number of triggers a single event may satisfy, so
one event can't fan out to 50 runs by accident.

**Open questions for you:**

1. v1.1 sequencing, or do you want fan-in in v1 proper?
2. Does `trigger.expired` — "the thing I was waiting for never came" — feel as
   valuable to you as it does to me? It's the cheapest alerting primitive we could
   possibly get, and it comes almost free with this design.
3. Any other stateful-trigger shape you had in mind that `all_of` + window doesn't
   cover? You said "a timeline of events," which is broader than fan-in — e.g.
   "when event X happens 3 times in an hour," or "when X happens and Y hasn't."

**Your response:**

```



```

---

### D17. Chains and topology

**Status:** AGREED (revised in v0.4 — "routes file" became "chain file")

*(Numbered late, placed here by topic — it's a core-model item, not an appendix to
Part 5.)*

**Your observation.**

> *"It feels like we're missing the concept of a router... what if I wanted a job to
> essentially be a router for other jobs? So when this event happens, run this job —
> but really that job is just a router for other jobs that need to run. It's like
> the inverse of the job having to define its trigger... which maybe isn't good...
> but I feel like there is something in the idea of having a router."*

**There is something in it, and the reason it felt unresolved is that "router" is
three different things at once.** Separating them makes it obvious which parts to
build:

1. **Locality** — one place to read the topology. Today triggers live inside job
   files, so answering "what happens after weather-ingest succeeds?" means grepping
   every job. That's the standing complaint about implicit event-driven systems:
   the wiring exists, but it isn't written down anywhere. This is a **P1 visibility
   problem**, not an ergonomics nitpick.
2. **Dynamic dispatch** — a routing decision that requires computation. "If the
   reading is anomalous, run the alert job; otherwise run the rollup." Static
   `where` equality can't express that. This needs code.
3. **Fan-out ownership** — one event, many jobs, decided in one place. Mostly a
   special case of (1).

**Recommendation: build the first two, refuse the third framing.**

#### (a) Dynamic dispatch is already free — and that's the architecture validating itself

A router job needs no new primitive. It triggers on an event, runs code, emits
events; other jobs trigger on those. That's the existing loop, closed:

```
weather.ingested → [route-weather] → emits rollup.requested | alert.requested
                                   → whichever job listens, runs
```

No new noun, no new mechanism. That this composes without adding machinery is
evidence F1's event model is carrying its weight.

**But it exposed a real hole in D6.** `je emit` (D16) works for external sources,
but a **container** job can't use it — the binary isn't in the image and it would
need network access back to the daemon. As v0.2 stood, container jobs could not
emit events at all, which would have silently made routers process-only. Fixed by
adding a third file to the existing pattern (now folded into D6):

```
JOB_OUTPUT_FILE   → structured output       (D6)
JOB_STATE_FILE    → cursor                  (D14)
JOB_EVENTS_FILE   → events to emit, JSONL   (new)
```

Committed on success, same as state. A router appends a line and exits 0. Identical
in a process or a container, in any language, with no SDK — **the filesystem is our
SDK** (D6), now for the third time, which is a good sign the convention is real
rather than a coincidence.

#### (b) Locality: make routes separately authorable

The key realization: **triggers are already a separate noun in F1.** A Trigger is a
`(match, target)` pair. We only accidentally decided they must be *authored* inside
job files — the data model never required it. So this costs very little:

```yaml
# routes/weather.yaml
routes:
  - on: { event: run.succeeded, where: { job: weather-ingest } }
    run: daily-rollup

  - on: { event: homekit.motion, where: { room: office } }
    run: log-presence
```

Both authoring locations compile to the same rows. Nothing downstream changes.

#### (c) What I'd refuse

Making Router a third entity type alongside Job and Trigger. A router is either a
route table (config) or a job that emits events (code). Introducing a noun that is
secretly one of those two is how a small system starts feeling large. Routers stay
a **pattern**, not a mechanism — a job that only emits events is a router by
convention, with no special powers, no special config, and no special status in the
UI beyond being visibly what it is.

#### The rule I'd propose

> A job declares **when it wants to run on its own** (schedules). It does not
> declare **who it depends on** (inter-job wiring). That lives in routes.

Nobody wants "runs at 3am" in a separate file — a job's own clock is its own
business. But a job knowing the *names of its peers* is coupling that makes job
files non-portable and the topology ungreppable. This split gives a clean answer to
"where do I look?": the job file for its own clock, `routes/` for everything about
how jobs relate.

**The honest alternative**, which is also defensible: let the downstream job declare
its dependencies, Makefile-style. That's pull instead of push, it never produces a
god-file, and plenty of good tools work that way. I lean routes anyway because it
makes the whole graph loadable and statically checkable at once (see the D3 loop
guard), but this is a genuine fork and worth your reaction rather than my default.

#### Impacts on existing items

| Item | Impact |
|---|---|
| **D3** fan-in | **Improves.** A fan-in relationship belongs to neither job; authoring it inside one of them was always slightly off. Example moved to routes form. |
| **D3** loop guard | **Improves materially.** Declarative routes are cycle-checkable at load time — `A → B → A` is rejected when you save, not discovered at depth 10 during an incident. |
| **D6** protocol | `JOB_EVENTS_FILE` added. Would have been needed eventually regardless. |
| **D11** versioning | Small addition: a run's provenance must record the *route* identity and hash, not just the job's — otherwise "why did this run?" points at a definition that doesn't contain the answer. One column. |
| **D12 / P1** | Pays off. `je routes` renders the full resolved topology regardless of where each trigger was authored, so **authoring location becomes a style preference rather than a comprehension problem.** This is D15's CLI-first decision earning its keep. |
| **D2** CLI + git | None. Routes files are just more watched YAML. |
| Causation chains | Router-as-job makes chains one hop longer but more informative — you can see the decision and read its logs. Router-as-config keeps them short. The chain view handles either. |

#### Guidance we'd document

Prefer **declarative routes**; reach for a **router job** only when the decision
genuinely requires computation. The reason is not stylistic: declarative routes are
statically analyzable — cycle-checked, graphable, and answerable ("what can trigger
job X?") without running anything. Code routers are opaque to all three.

#### The risk

The god-file. A 400-line `routes.yaml` is *worse* than scattered triggers, because
now everything is in one place **and** that place is unreadable. Mitigations: a
`routes/` directory split by domain, and `je routes --job X` for scoped questions.

**Open questions for you:**

1. The proposed rule — schedules on the job, inter-job wiring in routes — or the
   Makefile-style alternative where the downstream job declares its own deps?
2. Do we **allow both** authoring locations (job-local `on:` *and* routes files), or
   pick one? Allowing both means "where is this triggered from?" has two answers;
   the CLI resolving the truth mostly neutralizes that, but "mostly" is doing real
   work in that sentence. My lean: allow both, document the rule above as the
   convention, and let the tool keep everyone honest.
3. Static cycle rejection at load time — hard error (refuse to load the routes
   file) or warning (load it, flag the job)? I lean hard error, since a cycle is
   never intentional.

**Your response:**

```
yeah...I think a routes.yaml file is really bad. A route file should represent one chain...not all chains in the system. 


```

**Resolution (v0.4): chains, not routes.** You're right, and this is better than
what I proposed — I'd identified the god-file as a *risk to be disciplined about*,
and you've made it structurally impossible instead. That's the stronger fix.

But it does more than avoid a bad file. **A chain is a nameable thing, and a route
isn't.** "Route #3 in routes.yaml" is not something you can talk about, alert on, or
show status for. "The `daily-weather` chain is stalled waiting on `power-ingest`" is.
Under P1, that's the difference between a mechanism and a feature.

So the unit changes: `chains/<name>.yaml`, one coherent flow per file, and the
filename is the chain's name.

```yaml
# chains/daily-weather.yaml
description: Ingest readings, roll them up, alert if the pair doesn't land

steps:
  - on: { event: run.succeeded, where: { job: weather-ingest } }
    run: normalize-readings

  - on: { event: run.succeeded, where: { job: normalize-readings } }
    run: daily-rollup

  - on: { event: trigger.expired, where: { step: 1 } }   # v1.1
    run: notify-me
```

What the name buys us, all of it P1 payoff:

```
$ je chains
CHAIN            STEPS  LAST RUN          STATE
daily-weather    3      today 04:03       ✓ complete (2m 14s end-to-end)
photo-archive    2      3d ago            ⚠ stalled at step 2

$ je chain daily-weather
  weather-ingest      ✓ 04:01  (12s)
    └→ normalize      ✓ 04:01  (48s)
       └→ daily-rollup ✓ 04:03  (1m 14s)
```

**End-to-end duration and end-to-end failure become expressible**, which they aren't
when routes are anonymous rules. "The chain takes 40 minutes now, it used to take 5"
is a question no job-level view can answer.

**One thing I want to be careful about, and it's the open question below.** A chain
must stay an *organizing and display* device, not a runtime entity. The steps still
fire independently as ordinary triggers — there's no chain-level execution, no
chain state machine, no "the chain is running" lock. The moment a chain becomes a
runtime object with its own lifecycle, we've built a DAG engine with extra steps and
N1 is dead. So: **chains are how routes are named and grouped; routes are still what
executes.**

The consequence to accept: if `weather-ingest` succeeding should kick off two
unrelated flows, that's two routes in two chain files, not one route shared between
them. Routes are cheap; duplication of a *match* across two chains is fine and
arguably clearer than indirection.

**Also settled here:** job-local `on:` stays for a job's own schedule; inter-job
wiring lives in chain files. Both compile to the same `routes` table (Appendix A),
so `je routes` still shows one resolved truth (P3).

**One open sub-question:** when a step's `run:` target fails, is that a
*chain* failure worth surfacing as one thing ("daily-weather broke at step 2"), or
just an ordinary job failure that happens to sit inside a chain? I lean the former
for display purposes only — the chain view shows it, `je status --attention` names
the chain rather than the job — but with no runtime consequence: **downstream steps
simply never fire, because their triggering event never happens.** No cancellation
logic, no chain abort, nothing new to implement. Does that match what you'd want to
see when something breaks mid-chain?

**Your response (v0.4):**

```



```

---

## Part 3 — Durability and semantics

### D4. Storage: SQLite

**Status:** AGREED

`modernc.org/sqlite` (pure Go, no cgo, single static binary preserved). WAL mode.
State and logs in separate DB files. Large outputs on disk with paths in the DB.

> *"I like SQLite over our own format... I went too far... but my goal is to not
> have to spin up a bunch of supporting infrastructure for this to work."*

Your goal is met and then some: zero processes to install, one file to back up,
and — per your note — the CLI can expose it directly (`je db` drops you into a
SQLite shell for the times you want to ask a question we didn't build a view for).

See **Q1** for why I'm pushing back on a pluggable storage adapter.

---

### D5. Crash recovery

**Status:** AGREED

No durable execution. `running` at startup → `interrupted` (a distinct state, not
`failed`). Per-job `on_interrupt: retry | fail | ignore`, default `fail`. Container
runs reattach by container ID to recover the real exit code. Job idempotency is a
documented contract with the author.

> *"I agree with your recommendations."*

---

### D6. The job protocol

**Status:** AGREED (revised in v0.5 — state arrives in the environment; the
"no SDK" rule is narrowed by D21)

Exit code is the verdict. stdout/stderr captured line-by-line, timestamped,
streamed, stored, no format required. `JOB_OUTPUT_FILE` for structured output that
flows to chained jobs. SIGTERM → grace period → SIGKILL. No progress API in v1.

> *"Once you defined how job payloads can be shared... we should provide a path as
> an environment variable, that way it's easy for job authors to write those
> files... they don't have to care about where it goes specifically."*

That "engine tells you the path, you just write a file" pattern is now the
project's standard mechanism, and D14 reuses it exactly for job state. Worth
naming as a small design principle in its own right: **the filesystem is our SDK.**
Any language that can write a file can participate fully — no client library, no
protocol, nothing to version.

**Narrowed in v0.5 by D21.** That last clause was too strong. What we cannot
afford is *published* client libraries — five package managers, five release
processes, and a compatibility matrix between engine versions and client
versions. A shim embedded in the binary pays none of that. The rule as it should
have been written: **the filesystem is the contract, and any language that can
write a file participates fully.** Sugar on top of that contract is fine, and D21
specifies it.

**Added in v0.3 (from D17): `JOB_EVENTS_FILE`.** A job may emit events by appending
JSONL to this path — one event per line, `{"type": "...", "payload": {...}}` —
committed on success like state. This closes a hole: `je emit` (D16) can't be used
from a container job, since the binary isn't in the image and it would need network
access back to the daemon. Without this, container jobs couldn't participate in
chaining or routing at all. Emitted events inherit the run's causation, so the
chain stays intact automatically.

**Revised in v0.5: state arrives in the environment, not a file.** `JOB_STATE_FILE`
becomes `JE_STATE`, holding the JSON directly. The three *output* channels are
unchanged.

The justification has to be mechanical rather than ergonomic, or it is the same
mistake the withdrawn channel-merge made. It is:

> Outputs are files **because of commit-on-success**. The engine must be able to
> read them *after* the process has exited, and to discard them if it failed.
> Inputs carry no such requirement, so the reason does not apply to them.

It also deletes a file the engine must create, populate, and clean up on every
attempt, and it removes the most confusing member of the set — the one that looks
like storage but is a handoff.

**The cost is a size cap, and it is your call.** A single environment variable is
safe to about 64KB (Linux caps one at 128KB via `MAX_ARG_STRLEN`; macOS caps the
whole environment plus arguments at 1MB). A file allows the 1MB this item
originally specified. 64KB is roughly a thousand hashes, so it comfortably covers
a timestamp, a record id, a page token or an ETag, and starts to strain on "a set
of seen hashes" — which D14 does list as a legitimate cursor. My lean is 64KB and
an engine opinion that a cursor larger than that wants a real table in your own
store, but this is a door that is cheap to close now and awkward to reopen.

**Four channels, and they stay four.** Recorded because I proposed merging them
and withdrew it: separate channels are not ceremony, they carry information.

| Channel | Direction | Shape | Semantics |
|---|---|---|---|
| `JE_STATE` | in | JSON object in the env | the cursor as of run start (D14) |
| `JOB_STATE_OUT_FILE` | out | one JSON object | last write wins; commits on exit 0 |
| `JOB_OUTPUT_FILE` | out | one JSON object | structured output, flows to chained jobs |
| `JOB_EVENTS_FILE` | out | JSONL, append-only | ordered stream, all preserved |

What the separation buys, all of it lost by merging:

- **The file's shape encodes its semantics.** A single JSON object is visibly
  last-write-wins; an append-only JSONL file is visibly a stream. Merged, that
  becomes a *rule you have to know* rather than something the structure tells you.
- **Failure isolation and better errors.** A malformed line in a merged channel
  forces an incoherent choice: commit the state that parsed and drop the events,
  or fail a correct cursor advance because of a typo in an event. Separate files
  let the error name the channel, and keep the atomicity boundary defensible.
- **Three destinations, three tables.** State commits to `job_state`, output flows
  to chained jobs, events enter routing. Merging adds a demux step that exists
  only because we merged.
- **Volume asymmetry.** Events can be thousands of lines and a cursor is one small
  object. Reading megabytes to find a timestamp is silly.

Full v1 environment: `JOB_ID`, `RUN_ID`, `ATTEMPT`, `TRIGGERED_BY`,
`EVENT_PAYLOAD`, `JOB_WORKDIR`, `JE_STATE` (D14), `JE_LAST_SUCCESS_AT` (D14),
`JOB_STATE_OUT_FILE` (D14), `JOB_OUTPUT_FILE`, and `JOB_EVENTS_FILE` (D17).

**Your response (v0.5, on the state size cap):**

```
64kb is fine
```

**Resolution.** Locked at 64KB. State arrives in `JE_STATE`; there is no state
input file. A cursor that outgrows 64KB wants a real table in the job's own
store, and the engine will say so rather than silently truncating.

---

### D21. Shim injection — NEW

**Status:** NEW (v0.5) — one open question

*(Numbered last, placed here by topic: it sits directly on top of D6 and makes no
sense apart from it.)*

**Your observation:**

> *"I wish we could just understand the language we were writing in. That would be
> hard with docker though. But if we could know the language we could inject
> helper functions, which gives us a nice abstraction like an SDK — but we could
> manage them internally instead of having to have a bunch of packages in
> different package managers for each language."*

**This corrects a real over-reach in D6, and the correction is worth stating
precisely.** D6 banned SDKs. The cost it was actually avoiding was *distribution*:
publishing to npm and PyPI means release processes, semver, registry accounts,
lockfiles, and a compatibility matrix between engine versions and client versions.
That is what makes SDKs rot, and it is a real reason to refuse them.

An embedded shim pays none of it. The engine carries the shim in its binary and
materialises it at run time. So the fear was correct and the prohibition was too
broad — it banned the abstraction because of a cost that a different delivery
mechanism does not have.

And it is *better* than a published SDK on the thing SDKs are worst at:
**version skew becomes impossible.** The shim that runs your job is compiled into
the binary running your job. There is no version pair to be wrong. That is the
same argument D19 already makes for keeping the k8s generator inside the binary
rather than shipping it separately.

#### The rule this exposes, which is bigger than the feature

> **The protocol is shaped by mechanism. The shim is shaped by ergonomics.**

This is worth holding onto because it explains a mistake I made in the same round.
Before a sugar layer existed, the protocol was the only lever available for
developer experience — so I reached for it and proposed merging the four channels
into one, which would have encoded a convenience concern into a contract that
every language, every container, and D20's node relay must honour forever. With
two layers, each gets optimised for its own concern and neither compromises.

It is the same shape as P3 (files hold intent, the tool renders truth): a
separation that stops one artefact from being asked to do two jobs badly. If it
surfaces a third time, it should be promoted to a principle in Part 0 — which is
how P3 got there.

#### The three rules that keep this from eating D6

**R1. The protocol is the floor; the shim is sugar.** `JE_STATE` and the three
output files remain the contract. The shim reads one environment variable and
writes the same files any other language writes. A language we ship no shim for
participates exactly as fully as one we do — which is what D6 was protecting, and
it stays true.

**R2. The shim may never do anything the protocol cannot.** This is the
load-bearing rule. The moment `je.progress()` exists in the TypeScript shim and
nowhere else, "any language participates fully" is quietly false and the shim has
become the real API with the protocol as its legacy fallback. Convenience only,
never capability. If a shim wants to do something new, the protocol grows first
and every language gets it.

**R3. Declare the language; never detect it.** Inferring TypeScript from
`["npx", "tsx", "scripts/x.ts"]` is a heuristic, and it will be wrong on
`["bash", "-c", "python foo.py | jq"]`, on `["./run.sh"]`, and on
`["make", "ingest"]` — *silently* wrong, surfacing at 3am as a confusing
module-not-found. One declared line instead:

```yaml
command: ["npx", "tsx", "scripts/ingest-weather.ts"]
language: typescript      # opts into shim injection
```

P3 applies directly: a config file contains what you decided, and "this is a
TypeScript job" is a decision, not a default. Omit the line and you get the raw
protocol with no magic at all, so the whole feature is additive and no existing
job changes. It also fails at load time rather than at run time, which is the
same pit-of-success shape D10 uses for missing secrets.

#### What the shim exposes

Five things, each mapping one-to-one onto the protocol. That is the entire surface,
and R2 says it stays that way.

```typescript
je.state           // your cursor, seeded on first run (D14)
je.lastSuccessAt   // when this job last succeeded — read-only (D14)
je.event           // the payload of the event that triggered this run
je.setState(obj)   // last write wins; commits only on exit 0
je.emit(type, payload)
je.output(obj)
```

```typescript
import je from "./.je/je.mjs";      // written by the engine, gitignored

const readings = await fetchReadings(je.state.since);
console.log(`ingested ${readings.length} readings`);

je.setState({ since: readings.at(-1)!.ts });
je.emit("weather.ingested", { count: readings.length });
```

#### Docker, which you correctly flagged as the hard case

Less hard than it looks. A container job already needs a bind mount for the output
channels — `JOB_STATE_OUT_FILE` has to be a path the container can write and the
engine can read after exit. The shim rides that same mount, read-only. The
incremental cost is one more file in a directory we are already mounting, not new
machinery.

The genuine limits: an image with no runtime for the declared language cannot use
the shim, and a read-only root filesystem needs the mount to be somewhere writable
anyway for the output channels. Both fail loudly at start, and both have the same
remedy — drop `language:` and use the protocol.

#### The costs, honestly

- **Node's module resolution is the fiddly one.** Python is trivial (`PYTHONPATH`
  → `import je`), Ruby is trivial (`RUBYOPT=-r`). Node ESM resolves `node_modules`
  by walking up from the importing file, so a bare `from "je"` means either
  polluting your repo or preloading a global via `NODE_OPTIONS="--import ..."`.
  The relative `./.je/je.mjs` above is the boring option: no resolution magic, and
  identical inside a container.
- **Shims rot, just quietly instead of publicly.** You will use the TypeScript one
  daily; a Python one would sit untested for a year and be broken when someone
  needs it. There is no clever fix, only a CI job that runs a real job through
  every shim we ship.
- **It is one more thing in the binary.** Small — each shim is tens of lines — but
  it is surface that did not exist.

#### Sequencing

**Ship exactly one shim — TypeScript — and add a language when a real job needs
one.** Same reasoning as N2 preferring one real job to three invented ones, and
D20 refusing to build node infrastructure before a job demands it. Building a
five-language shim framework before one job has run in one language is how the
tail starts wagging the dog.

**Open question for you:**

1. Is `language:` the right key? It reads well but it is adjacent to D1's
   `runtime: process | container`, and having both `runtime` and `language` in one
   file invites confusion about which is which. Alternatives: `shim: typescript`
   (names the mechanism, so it cannot be mistaken for the executor, but leaks an
   implementation word into your file), or nesting it as `runtime.language`. I
   lean `language:` and documenting the pair, but you are the one who will read
   these files.

**Your response (v0.5):**

```



```

---

### D7. Retries and re-runs — revised

**Status:** AGREED (revised)

**Your answer.**

> *"I think it depends... if a job failed and I'm saying re-run it manually that's
> an attempt that just happened to be done by me (I'm the trigger)... I think it's
> important to still understand: was someone just trying to run this again because
> it failed, or was it a new job run?"*

**You're right and my v0.1 model was too rigid.** I collapsed "who caused it" into
"which table row it lives in," which loses exactly the information you want. The
fix is to separate those two things.

**Revised model.** Attempts get their own causation, same as runs do:

- A **run** is a unit of intent, caused by one event. It has 1..N attempts.
- An **attempt** is one execution, and *it* records the event that caused it too.
  An automatic retry is caused by `attempt.failed`; a manual retry is caused by
  `retry.requested` with your identity on it.
- Two distinct CLI verbs, because they're two distinct intents:
  - `je retry <run-id>` → **adds an attempt to the existing run.** Same intent
    ("the 3am sync"), new attempt, attributed to you. This is your case.
  - `je run <job>` → **new run.** New intent, fresh state (see D14), new event.

So the history can answer both of your questions: *"did the 3am sync eventually
succeed?"* is the run's status, and *"did a human have to intervene?"* is visible
in the attempt list, because attempt 3 says `triggered by: jay (manual retry)`
while attempts 1 and 2 say `triggered by: automatic retry`.

Schema consequence, now in Appendix A: `attempts` gains `triggering_event_id` and
`actor`.

Config unchanged: `retry: { max_attempts, backoff, initial_delay, max_delay }`,
default `max_attempts: 1`. Manual retries don't count against `max_attempts` — if
you're doing it by hand, you've made the decision the limit exists to protect.

---

### D8. Timeouts and concurrency

**Status:** AGREED

Per-job timeout, default 1 hour, marked `timed_out` distinctly from `failed`.
Overlap policy `skip` (default) | `queue` | `allow`. Global concurrency cap,
default 4, configurable, excess queues.

> *"I think this is a good starting point."*

---

### D9. Schedules

**Status:** AGREED

Catch-up policy per schedule: `skip` (default) | `once` | `all`. Missed windows are
recorded as events even when skipped, so gaps are explained rather than silent.
Cron plus explicit IANA timezone, with documented DST behavior. Simple intervals
(`every: 15m`) supported alongside cron.

Your response to this item was mostly about job state, which is now **D14**. One
piece of it stays here: the engine also records its own downtime (`engine.stopped`
/ `engine.started` — see D16), so a gap in a job's history can always be
attributed to *the machine was asleep* rather than *something went wrong*.
Distinguishing those two is most of what makes a laptop-hosted scheduler
trustworthy.

---

### D10. Secrets — revised

**Status:** AGREED (revised)

**Your answer.**

> *"The constraint is that it has to be intuitive... in past systems this bit
> people not because it was hard, but because it was [un]intuitive to the dev... we
> should make this a pit of success."*

Agreed, and it made me realize the v0.1 recommendation described *storage* and said
nothing about the experience. The pit of success here is mostly about **when you
find out something's wrong.** Concretely:

- **`je secret set GITHUB_TOKEN`** prompts without echo and writes to the store.
  Nobody hand-edits a secrets file; nobody pastes a token into a shell history.
- **Fail at load time, not at 3am.** A job declaring `secrets: [GITHUB_TOKEN]`
  when that secret isn't set is a **definition error**, surfaced the moment the
  file is saved — `je status` shows the job as misconfigured and it will not be
  scheduled. The failure mode we're eliminating is discovering a missing secret
  from a cryptic non-zero exit code eight hours later.
- **Injection is explicit and minimal.** Only declared secrets are injected. A job
  doesn't inherit your whole environment by accident.
- **Redaction on write**, not at render. Known secret values become `***` before
  the log line touches disk, so nothing leaks into the store even if someone later
  copies the DB. Imperfect against a job that transforms the value; catches the
  common case of a tool echoing its config.
- **The CLI never prints a secret value.** `je secret list` shows names, when
  set, and which jobs use them. There is no `get`. If you need the value, you know
  where the file is.
- Storage: a gitignored file in the engine's data directory for v1. OS keychain is
  a plausible v2 and slots in behind the same commands.

---

### D11. Definition versioning

**Status:** AGREED

Hash the normalized definition on load; store deduped snapshots; every run records
its hash; run detail shows the exact definition it ran under, diffed against
current when they differ. In-flight runs finish under the definition they started
with.

> *"I think this is a good start."*

With D2's git integration, you get two complementary histories: git says *who
changed it and why*, the snapshot says *what actually executed*. They answer
different questions and neither substitutes for the other.

**Added in v0.3 (from D17): route provenance.** Once a trigger can be authored
outside the job file, snapshotting the job definition is no longer sufficient — the
rule that *caused* the run lives elsewhere and can change independently. Runs
therefore record the triggering route's identity and hash alongside the job's, and
the run detail shows both. Otherwise "why did this run?" resolves to a definition
that doesn't contain the answer, which is the exact failure this item exists to
prevent.

---

## Part 4 — Job state

### D14. Job state / cursors

**Status:** AGREED (revised in v0.5 — the engine seeds the cursor, and serves
`lastSuccessAt`). 2 small questions still open, none blocking.

**Defaults I'll build against unless you say otherwise:**

1. **Opaque JSON with a display hint** — `state.primary_cursor: last_record_ts`
   tells the tool which key to show in status views. Full flexibility, still legible.
   *(v0.5: half-answered by the seeding decision below — state stays opaque, but
   the engine now guarantees one key is populated on a first run.)*
2. **Private per job.** Cross-job state reads make it a shared database, and shared
   mutable state between jobs is the thing that makes systems inexplicable. Jobs
   that need to hand data to each other already have `JOB_OUTPUT_FILE` (D6), which
   is explicit and shows up in the causation chain.
3. **`state_commit: always` deferred.** Not needed until a job actually makes
   resumable partial progress, and adding it later breaks nothing.

**Your answer** (given under D9, but it's its own thing):

> *"There are plenty of jobs that need to understand the last time they did
> something successfully... in nearly all job engines they leave that to the author
> entirely... I don't think we have to do that... there are dragons that I think we
> can completely eliminate by handling it for them — for example, the idea of a
> timestamp for the last successful record that you pulled versus the last time the
> job ran. Those can bite you... they get to pick what to store, but they don't
> have to think about where to store it."*

**This is the strongest product idea in your responses.** It's the thing every ETL
job needs, every job engine punts on, and every author re-implements badly —
usually as a timestamp file with no atomicity and no relationship to whether the
run succeeded. Worth doing, and cheap, because we already have durable storage and
a file-based convention (D6) to reuse.

The specific dragon you named is worth stating precisely, because the design falls
out of it: **the job's cursor must advance only when the work actually succeeded.**
The classic bug is a job that records "I ran at 04:00" and then fails at 04:03,
leaving the next run to start from 04:00 and silently skip everything in between.
The engine can eliminate that class of bug entirely by controlling *when* the
cursor commits.

**Proposal.** Symmetric with `JOB_OUTPUT_FILE`, so there's one pattern to learn:

- **`JOB_STATE_FILE`** — the engine writes the job's current state here (JSON)
  before the attempt starts. The job reads it. First run: `{}`.
- **`JOB_STATE_OUT_FILE`** — the job writes its new state here. Not writing it
  means "no change," unambiguously.
- **Commit on success only.** The engine promotes the out-file to the job's state
  if and only if the attempt exits 0. Failure, timeout, crash, interrupt → the
  cursor does not move. This is the whole point.
- **Contents are opaque** to the engine — a JSON object, size-capped (1MB). You
  pick what to store: a timestamp, a record ID, a page token, an ETag, a set of
  seen hashes.
- **Every commit is versioned** with the run that made it, so state has a history.

**Retry semantics**, which is where this gets subtle and where the value is:

- All attempts *within a run* see the **same input state** — the state as of run
  start. Attempt 2 must not inherit attempt 1's partial cursor, or a failed
  half-run silently skips records. This is the dragon again, one level down.
- A **new run** (`je run`) sees the current committed state.
- A **manual retry of an old run** (`je retry`) replays with the state that run
  *originally started from*, not today's — a true replay. `--state current`
  overrides. Getting this wrong in either direction causes silent data gaps, so
  the CLI prints which cursor it's using before it starts.

**Backfills become a first-class operation**, which they never are elsewhere:

```
$ je state get weather-ingest
{ "last_record_ts": "2026-08-13T04:00:00Z", "station": "KDEN-3" }

$ je state set weather-ingest --json '{"last_record_ts":"2026-08-01T00:00:00Z"}'
  ⚠ moving cursor backwards 12d. Next run will re-process from 2026-08-01.
  Committed as state v41. Previous value recoverable with: job state rollback

$ je state history weather-ingest
  v41  2026-08-13 10:02  jay (manual)      last_record_ts → 2026-08-01T00:00:00Z
  v40  2026-08-13 04:03  run 8821          last_record_ts → 2026-08-13T04:00:00Z
  v39  2026-08-13 03:03  run 8814          last_record_ts → 2026-08-13T03:00:00Z
```

**Visibility (P1).** The cursor's movement is part of the feature, not a follow-up.
Minimum: the history view above, plus the run detail showing state-in and state-out
for that specific run. "The cursor stopped moving on Tuesday even though runs kept
succeeding" is a bug class that is invisible everywhere else and obvious here.

**Added in v0.5: the engine seeds the cursor, so a first run is never handed an
empty object.**

> *"I think we should provide the last timestamp always — it's just now if we
> don't have one. I just think it's fairly simple to do, and if the job author
> doesn't like it they can still just override it. It's just one of those things
> that seems like we are helping them out a bit, if they need the timestamp."* — you

Adopted. It is the same argument that justified this item in the first place: the
author picks *what* to store, not where, and "there is nothing there yet" is a
case the engine can absorb instead of every job writing its own
`?? "2026-01-01"` fallback.

**`now` is also the right default value, and the reason is asymmetry.** Seeding
with the epoch means a first run tries to pull all of history — hammering the
source API, running for hours, quite possibly failing halfway and leaving a mess.
Seeding with the run's start time means the first run processes nothing and the
second run processes fifteen minutes. The recovery from "I actually wanted
history" is `je state set` backwards, which this item already makes first-class
and prints a warning for. There is no recovery from having already hammered your
weather station.

**The one trap, and it is this item's own dragon wearing a helpful hat.** There is
a sharp fork inside "always provide the last timestamp":

- **Seed** — the engine writes it once, on first run. Thereafter it changes only
  when the job sets it.
- **Maintained** — the engine keeps it current, advancing it on every successful
  run.

The second is *precisely* the bug this item exists to eliminate: the job that
records "I ran at 04:00", fails at 04:03, and leaves the next run to start from
04:00 and silently skip everything in between. Except now the engine would be
doing it, on every job, by default — and an author who reads the cursor and never
calls `setState` would get something that tracks *run time* while looking exactly
like something that tracks *data*.

**So: seed only. Set once, never auto-advanced.** It is a one-word difference in
the spec and the wrong word is invisible until it costs a day of data.

**What falls out of that, and it is a real improvement.** Once the seed is
strictly hands-off, the *other* fact is still missing — and we already have it,
because "when did this job last succeed?" is a query against `runs`. So provide
both, under names that cannot be mistaken for each other:

| | Meaning | Who owns it |
|---|---|---|
| `je.state` / `JE_STATE` | the watermark: what you last processed | the job |
| `je.lastSuccessAt` / `JE_LAST_SUCCESS_AT` | when this job last succeeded | the engine, read-only |

This item's whole thesis is that these two diverge the instant anything fails,
retries, runs late, or is skipped while the machine is asleep — and that
conflating them is the classic bug. Giving them different names and making one
unwritable means an author who wants the wrong one has to *type* the wrong one.
That is cheaper and far more durable than documenting the distinction. It costs
nothing: `lastSuccessAt` is a `SELECT`, not new stored state.

**Two details that carry real weight:**

- **"Now" is the run's start time, frozen — not the clock read inside the job.**
  This item already requires every attempt within a run to see the same input
  state; if attempt 1 fails at 04:00 and attempt 2 runs at 04:05, a re-evaluated
  "now" opens a five-minute hole. The same rule makes `je retry` of an old run
  replay its original seed for free.
- **The seed is stored as state v1, not computed on the fly**, so the cursor's
  origin is visible rather than magical:

```
$ je state history weather-ingest
  v2  2026-09-02 11:30  run 2          since → 2026-09-02T11:29:04Z
  v1  2026-09-02 11:15  engine (seed)  since → 2026-09-02T11:15:00Z
```

Magic that appears in the history is behaviour. Magic that doesn't is the thing
you cannot explain at 2am (P1).

**Options I'd offer:**

- `state_commit: on_success` (default) | `always` — `always` for jobs that make
  real partial progress and would rather resume than redo. Off by default because
  it reintroduces the dragon for anyone who doesn't need it.

**Open questions for you:**

1. ~~Opaque vs typed cursor~~ — **half-settled in v0.5.** State stays opaque JSON;
   the engine only guarantees the `primary_cursor` key is populated on a first
   run. Still open, and cheap either way: should the engine *warn* when a cursor
   that has always held a timestamp suddenly moves backwards, or is that too
   clever for something it is supposed to treat as opaque? I lean warn — it is
   the same "tell me before it costs me a day" instinct behind `je state set`
   already printing a backwards-movement warning.
2. Should state be readable by *other* jobs (`je state get X` from inside another
   job), or strictly private per job? Private is safer; shared makes it a tiny
   shared database, which invites misuse but is undeniably handy.
3. Does `state_commit: always` earn its place in v1, or defer?

**Your response:**

```



```

---

## Part 5 — Interface and observability

### D15. Daemon + API + CLI first

**Status:** AGREED — binary is `je`, no web UI in v1

This is the item your D2 and D12 answers implied but neither one asked directly:
*if the CLI is the primary interface, what happens to the web UI?*

**Recommendation: build the engine as a daemon with a local HTTP API, and make the
CLI a thin client over that API. Ship no web UI in v1.**

The reasoning is that this makes the CLI-vs-UI question stop being a fork at all:

- The **daemon** (`je serve`) owns the schedule loop, the executors, and the
  database. It's the only thing that touches SQLite.
- The **API** is JSON over HTTP on localhost. Every capability the system has is
  an endpoint. This is the real contract.
- The **CLI** is a client. It has no privileged access and no direct DB path
  (except a deliberate `je db` escape hatch for ad-hoc SQL when the daemon is
  down).
- A **web UI, if we ever build it**, is a second client of the same API, embedded
  with `go:embed`. It's additive, not a rewrite, and we can decide in six months
  with real usage instead of now with none.

The cost of this discipline is small — an HTTP layer you'd want anyway for the
daemon/CLI split — and it buys the entire deferral. It also means "another machine
can reach it" is a config change rather than an architecture change, which is
relevant to Q1.

**Does a CLI actually cover D12's seven observability requirements?** Going
through them honestly, because that was your implied question:

| D12 requirement | CLI verdict |
|---|---|
| Live log tailing | **Better.** `je logs -f <run>` is SSE → stdout. Pipes into grep. |
| Run timeline | **Fine.** An indented, timestamped tree renders well in a terminal. |
| "Why did this run?" causation chain | **Fine.** `je why <run>` prints the chain as a tree. |
| Job health view | **Fine.** `je status` is a table. This is what terminals are for. |
| Signal vs. noise / needs-attention | **Fine.** `je status --attention` and a nonzero exit code, which makes it scriptable in a way a UI isn't. |
| Notification on failure | **Unaffected.** Shell hook + desktop notification. |
| Duration trends / sparklines | **Worse.** Genuinely better in a browser. Acceptable v1 loss. |

Only the last one is a real loss, and it's the least important. Your instinct was
right: for a single-user local tool, the CLI is not a compromise — it's better for
five of seven, and it composes with the rest of your shell in ways a UI never does.

**A middle option worth knowing about:** a TUI (`je top`, via Bubble Tea) — a
live dashboard in the terminal, which recovers the sparkline/at-a-glance case
without a browser. I'd hold it as a v1.1 candidate, not v1, and I mention it only
so it's a known option rather than a surprise.

**Open questions for you:**

1. Web UI: **not in v1** (my recommendation), or do you want it in v1 anyway?
2. Is `job` the command name? Short, memorable, and it might collide with things
   in a zsh setup. Alternatives: `je`, `jobs` (definitely collides — that's a shell
   builtin), or something project-named.
3. Does the daemon-plus-thin-client split feel right, or would you rather the CLI
   just open the SQLite file directly and skip the daemon for read commands? (The
   latter is simpler and works when the daemon's down; the former means one code
   path and no locking surprises. I recommend the former, with `je db` as the
   escape hatch.)

**Your response:**

```
1. No web-ui in v1
2. I say "je"...it short...plus we can change this later right? 
3. I like the daemon-plus-thin-client...its a good approach. Plus with the daemon we can have it run I think even when the computer sleeps right? If not...its still a good idea. 


```

**Resolution (v0.4).**

1. **No web UI in v1.** Locked. The API makes it a later decision, not a rewrite.

2. **`je` it is** — and yes, changeable, though the window closes faster than you'd
   think: it ends up in muscle memory, shell aliases, launchd plists, and every
   example in the docs. Cheap now, annoying in three months, so let's treat it as
   decided. Project stays `job-engine`; binary is `je`. All examples in this
   document have been updated.

3. **Daemon + thin client locked. But the sleep answer is no**, and it's worth being
   precise since a lot follows from it.

   macOS suspends processes during sleep — the daemon doesn't run, doesn't get
   timers, doesn't wake itself. There's no daemon-shaped way around that. Power Nap
   only services specific Apple subsystems, not arbitrary agents. So the honest
   statement is: **while the lid is closed, nothing runs, and the engine's job is to
   be excellent about what happens when it wakes up.**

   Which is exactly why D9's catch-up policy and D16's `engine.stopped` /
   `engine.started` events carry more weight in this design than they would in a
   server-hosted one. On wake, the engine knows precisely how long it was down,
   which windows it missed, and — per policy — whether to fire once, fire all, or
   skip and say so. That's the difference between "my 3am job silently never ran"
   and "3 windows missed while asleep, 1 catch-up run fired at 08:14."

   **There is one real escape hatch worth knowing about:** `pmset schedule` /
   `pmset repeat wakeorpoweron` can tell macOS to physically wake the machine at a
   given time. A job could declare `wake_machine: true` and we'd register a wake.
   It genuinely works. I'd keep it out of v1 — it mutates system-level power
   settings, it's surprising if you don't remember enabling it, and it needs a
   careful uninstall story — but it's the answer if you ever have a job that
   truly must run at 3am. Noted here so it's a known option rather than a
   rediscovery.

4. **Added (from D18): the daemon takes an exclusive lock on its data directory**
   and refuses to start if another instance holds it. Two daemons on one SQLite
   file would double-fire every schedule — and the Almanac scenario (D18) makes a
   second instance a genuinely likely accident rather than a theoretical one.

---

### D16. Daemon lifecycle and generic event ingress

**Status:** AGREED

Two gaps I noticed when working through your HomeKit comment in N2. Neither was in
v0.1 and both are load-bearing.

**(a) Who starts the daemon?** If the engine is a long-running process on your Mac,
"it's running" can't depend on you remembering to start it — the whole value
proposition dies the first time you reboot and don't notice for a week.

Recommendation: **`je install`** writes a launchd LaunchAgent plist with
`RunAtLoad` and `KeepAlive`, so the daemon starts at login and restarts if it
crashes. **`je uninstall`** removes it. The daemon logs `engine.started` /
`engine.stopped` as events, so downtime is a visible, queryable fact rather than an
unexplained hole in the timeline — which is what makes D9's catch-up behavior
comprehensible after the fact. `je status` shows engine uptime and the last gap.

Note that macOS will still not run a sleeping laptop's jobs — launchd starts things
at wake, not during sleep. That's D9's catch-up policy's job, and it's why that
item matters more here than it would on a server.

**(b) How does anything outside the engine emit an event?** You raised HomeKit, and
said "I know that's other infrastructure." It isn't, if we do this right:

```
$ je emit homekit.motion --payload '{"room":"office"}'
```

**One command turns every external system into an event source** without us
writing a single integration. A HomeKit automation runs a Shortcut that runs this.
A git hook runs this. `launchd` running this on a `WatchPaths` trigger gives you
file-watching for free. Your Almanac app can shell out to it.

This is the highest-leverage 50 lines in the project. It means the v1 non-goals of
"webhook source" and "file-watch source" cost you nothing, because the escape
hatch covers both cases well enough that you may never ask for the real thing.

Add `--dedupe-key` so a source that fires twice doesn't cause two runs, and
`--wait` so a caller can block until the resulting run finishes and get its exit
code — which quietly makes the engine scriptable from anything.

**Open questions for you:**

1. `je install` writing a launchd plist — good, or too magical? (It's the
   difference between a tool you trust with a nightly job and one you don't.)
2. Is `je emit` enough to cover HomeKit, or were you imagining the engine holding
   a live subscription to the HomeKit event stream? The former is trivial and
   robust; the latter is a real integration with a daemon-inside-a-daemon problem.

**Your response:**

```
1. I agree
2. Not sure exactly on this...I'm not saying you're wrong...I just don't have enough knowledge myself to know if this makes sense. 


```

**Resolution (v0.4).**

1. **`je install` writes the launchd plist.** Locked.

2. **Taking the call on this one, since it's my area to know:** `je emit` is enough,
   and a live HomeKit subscription is not something we should build.

   The concrete path, so it's not hand-wavy: HomeKit automations can run a Shortcut,
   and a Shortcut can execute a shell command. So "motion in the office" →
   `je emit homekit.motion --payload '{"room":"office"}'` → any job you've wired to
   that event. Setup is a few taps in the Home app, and **the engine needs no
   HomeKit code at all** — it never authenticates to HomeKit, never holds a
   subscription, never breaks when Apple changes something.

   The alternative — the engine holding a live HomeKit connection — means an Apple
   framework dependency, a Swift/ObjC bridge from Go, HomeKit authorization prompts,
   and a reconnection state machine, all to receive an event we can already receive.
   It would be the single largest piece of platform-specific code in the project,
   and it would exist to save you a Shortcut.

   The general principle worth stating: **the engine has one ingress and knows
   nothing about the outside world.** Every integration is someone else's job to
   push us an event. That's what keeps a single binary a single binary.

---

### D18. Embedding — Almanac as a consumer — NEW

**Status:** NEW — react

**Your observation** (from Q2):

> *"I wonder if having this be a single binary will be nice — in that we could
> incorporate this job engine into that Mac app. That way it's not a custom job
> engine in Almanac, but us just using this re-usable job engine."*

**This is the most consequential thing you've said about the project's shape**, and
it deserves its own item because it introduces a second consumer. Every decision so
far assumed exactly one user interface (you, at a terminal). "An application uses
this as its scheduling layer" is a different constraint, and it's worth checking
whether the design survives it.

**It does, and better than I expected — because D15 already solved it.** We chose
daemon-plus-HTTP-API for unrelated reasons (deferring the web UI, avoiding two code
paths into SQLite). That decision happens to be exactly what an embedding consumer
needs:

- Almanac ships the `je` binary, starts it, and talks to it over the local API.
- No Go/Swift bridge, no cgo, no c-archive, no shared-memory anything.
- The engine stays a single static binary — which is *why* this works at all. A
  version of this project that needed Redis could not be embedded in a Mac app.
- Almanac's jobs are just job files it writes; its triggers are `je emit` calls or
  API posts. It uses the same primitives you do at the terminal.

So my recommendation is: **the API is the embedding story. Don't build in-process
library embedding.** Getting a Go library into a Swift app means a C-archive, a
bridging header, and manual memory discipline across the boundary — a meaningful
amount of fragile, platform-specific work to avoid running a subprocess that we've
already made easy to run.

**Structural implication worth honoring now**, because it's free if we do it from
the start and annoying to retrofit: **keep the engine core a library.** `internal/
engine` should have no assumptions about being a daemon or a CLI — no `os.Exit`, no
printing to stdout, no reading flags. The daemon is a thin wrapper around it, and
the CLI is a client of the daemon. That's good Go structure regardless, and it means
if in-process embedding ever does become necessary, the hard part is already done.

**Three things this changes or adds:**

1. **The data directory lock (added to D15).** If Almanac starts a daemon and you
   also have one from `je install`, two schedulers on one database double-fire
   every job. The daemon must take an exclusive lock and refuse to start otherwise,
   with an error that says which process holds it. This moved from "theoretically
   possible" to "will definitely happen on your machine" the moment Almanac entered
   the picture.
2. **Almanac needs its own data directory, or it doesn't.** Two modes worth naming:
   *shared* (Almanac's jobs appear in your `je status` alongside everything else —
   one engine, one history, full visibility) or *isolated* (Almanac gets a private
   data dir; its jobs are invisible to your CLI). I'd default to **shared**, because
   P1 says a job you can't see is a job you'll be confused by at 2am, and "why did
   my Mac spin up at 3am?" should be answerable in one command.
3. **N1's "multi-user/auth" non-goal holds**, but gets a rider: the API is now
   consumed by another program, not just your shell. Still localhost-only, still no
   auth in v1, but the API becomes a contract we should keep stable rather than
   change casually.

**The deeper resonance, which I'll flag and not act on.** You described Almanac's
"attention" concept — things surfacing when they need to, by time or place. That is
recognizably the same idea as D12's *needs-attention* triage and P1's *waiting
view*: both are systems deciding what a human should look at, and when. It's
plausible that the engine eventually emits attention items and Almanac renders them,
which would make the two projects genuinely complementary rather than one hosting
the other. Too early to design, but worth writing down so it isn't rediscovered.

**Open questions for you:**

1. Shared data directory (Almanac's jobs visible in `je status`) or isolated? I lean
   shared, strongly.
2. Is Almanac Swift, and is the "just run the binary and talk HTTP" approach
   acceptable to you architecturally — or is there a reason it needs to be in-process?
3. Should v1 do anything specific for this, or is it enough that the architecture
   doesn't preclude it? My strong lean: **nothing specific in v1.** Build the engine
   for you at a terminal, keep the core a library, and revisit when you actually try
   to wire Almanac to it.

**Your response:**

```



```

---

### D19. Kubernetes deployment — the local-to-cluster progression — NEW

**Status:** NEW — react

**Your observation:**

> *"We're like hey it's easy to do capture...but we're just talking about the
> code...we aren't talking about all of this setup that is required for Kubernetes.
> That piece needs to be packaged as well. I think we should strive for something as
> easy as me running it on my laptop...but for Kubernetes."*

This is the same argument as P1, pointed at the operator instead of the user:
friction anywhere in the loop kills the loop. A job engine that takes a weekend to
deploy is a job engine nobody deploys, and the quality of the engine is then
irrelevant. Deployment is a component of the system, not an afterthought to it.

**The organizing constraint is that this is a progression, not two products.**
Someone tries `je` on a laptop, it works, and moving it to a cluster should be a
step — not a port. Four stages, each of which must not invalidate the last:

| Stage | What it is | What it costs to get here |
|---|---|---|
| 0 | `je run weather-ingest` — foreground, no daemon, exit code | download a binary |
| 1 | `je install` — launchd daemon, real schedules (D16a) | one command |
| 2 | Cluster — same definitions, same commands, always-on | one manifest or CR |
| 3 | GitOps — the repo is the source of truth for jobs | one commit |

**Two rules make the progression real**, and everything else in this item follows
from them:

**R1. A job definition that works locally must run in the cluster unmodified.** No
Kubernetes-shaped fields in job YAML, ever. The moment a job file has to know where
it's running, the progression breaks and stage 2 becomes a rewrite. This is a
constraint on D2's schema more than on any deployment tooling.

**R2. Every command you learned locally works against the cluster.** `je --context
cluster status` is the same command, same output, different endpoint — `kubectl`
contexts, and for the same reason. Debugging the cluster shouldn't be a second skill.

```
je run weather-ingest              # local, foreground
je --context cluster logs -f       # same command, remote daemon
```

**What actually differs per cluster** is a short, enumerable list — which is why
this package can stay small. Three of these are correctness constraints the engine
knows about and the cluster doesn't:

- **`replicas: 1` and `strategy: Recreate`.** Two daemons on one SQLite file
  double-fire every schedule. D18's data-directory lock turns that into a crashloop
  rather than corruption, but a crashloop at 3am is still an outage you learn about
  late. Note the default `RollingUpdate` starts the new pod *before* terminating the
  old one, so the naive manifest is the broken one.
- **StorageClass must be RWO block or host-local — never NFS.** SQLite over NFS has
  real locking pathologies, and the failure is silent and weeks out.
- **`TZ` set explicitly.** Containers default to UTC; D9's schedules mean local time
  to a human. This is the "why did my 3am job run at 8pm" bug.
- Image reference, and how the API reaches your other devices.

**Recommendation on form factor: an operator, but for one specific reason.** The
weak argument — "it's the modern packaging" — doesn't survive contact with a
single-replica app plus a PVC; that's a CRD, a controller, and RBAC to produce three
objects. The strong argument is that the dangerous settings above must hold
*continuously*. A generator can emit `replicas: 1`; only a controller can keep it 1
after an HPA touches it or a bad merge lands. A validating webhook can reject a CR
pointing at NFS at admission time rather than corrupting the database three weeks
later. Holding an invariant whose violation is silent and catastrophic is a real
operator job.

It also produces the better GitOps artifact — a reviewable CR instead of a rendered
bundle:

```yaml
kind: JobEngine
spec:
  version: v1.2.0
  storageClass: longhorn
  timezone: America/Denver
  jobs:
    repo: github.com/you/jobs
    path: ./jobs
    interval: 1m
```

**Two honest costs.** First, it recurses the complaint: the operator itself must be
trivially installable, or the friction has moved rather than gone. Second, CRDs need
cluster-admin — nothing on a personal Talos box, a procurement conversation
elsewhere. The question that decides operator-vs-generator: **will there ever be
more than one `je` instance in a cluster?** One forever → an operator is heavy. One
per team or namespace, each with its own database → an operator is obviously right.

**P1 applies to the operator, and this is not decoration.** Operators are
notoriously the most opaque thing in a cluster: you apply a CR, nothing happens, and
the only way to find out why is to go read controller logs — which is exactly the
2am confusion P1 exists to prevent. An operator that silently enforces invariants is
*worse* than a generator, because now something is changing your cluster and not
telling you. Four requirements, all cheap if built in from the first reconcile:

- **The operator never mutates silently.** If it overrides a field — resets
  `replicas` to 1, refuses a StorageClass — that decision appears in the CR's status
  with the reason stated in a sentence a human reads: *"replicas reset to 1: two
  schedulers on one SQLite database double-fire every job."* Not a log line, not a
  metric. Enforcement without explanation is the failure mode.
- **`.status` carries real conditions**, not a phase string. `Ready`,
  `StorageValid`, `DefinitionsSynced`, `Degraded`, each with a message and a
  transition time. `kubectl describe jobengine` should answer "why isn't it running"
  completely, without opening controller logs.
- **Kubernetes Events for every decision**, including the boring ones. Reconciles
  that changed nothing are the ones that make a timeline trustworthy — the same
  reason D16 logs `engine.started` / `engine.stopped` rather than leaving holes.
- **`je k8s status` renders it**, so the answer arrives through the same CLI as
  everything else (R2, D15) rather than requiring a second toolchain.

**And the P2 version of the same idea**, which I think is the right long-term shape:
the operator's own work should be visible *in the engine*, not just in Kubernetes.
The operator emits `operator.reconciled`, `operator.rejected`, `operator.upgraded`
through the same ingress as everything else (D16), so "why did my engine restart at
3am" is answerable in `je status` alongside every other fact about the system —
rather than living in a separate observability world that only `kubectl` can see.
The one honest wrinkle: the operator often reconciles precisely when the engine is
unreachable, so Kubernetes Events remain the durable floor and the emitted events
are the enrichment. Both, not either.

**Job definitions arrive by git, pulled, not pushed.** This fits P3 better than an
API push does, and the pull direction matters concretely: a git webhook requires
GitHub to reach the cluster, which behind CGNAT it can't without exposing something.
A Flux-style outbound reconcile loop needs no ingress at all. It also gives D11 a
better answer than it currently has — **the commit SHA is the definition version**,
so `je why <run>` can point at the exact commit that defined the job that fired.

Three things it forces:

1. **One writer per instance.** Git and the API can't both be authoritative. Make it
   a mode: *local* (files on disk, `je run`) or *git* (repo is truth,
   definition-writing API calls rejected with a message saying why).
2. **Sync is atomic.** One unparseable file rejects the whole sync and the last good
   state keeps serving. Partial application leaves the engine in a state that exists
   in no commit.
3. **Deleting a file never deletes history.** A vanished job file tombstones the
   definition and stops future runs. Reverting a commit must not erase the timeline —
   the trustworthy timeline is the whole point (P1).

**The structural decision that makes all of this cheap, and the only one that costs
anything in v1: definition loading is a pluggable source, not "read this
directory."** Local disk is source #1, git is source #2, and neither is
special-cased. That is a small choice now and the difference between the GitOps
version being a feature and being a rewrite. Everything else here can wait.

**Two known gaps, stated rather than solved:**

- **Executor locality — now addressed in D20.** Moving the engine off the Mac means
  jobs can no longer read local files, run local scripts, or trigger Shortcuts. R1
  promises definitions port; it cannot promise the *environment* ports. The answer
  is a Mac-side node under one control plane (**D20**), which reworded N1's
  distributed-workers non-goal. Still not v1, and job #1 must not need it.
- **Secrets (D10).** The local secret store is where R1 strains hardest: a job
  referencing a secret by name should work in both places, which means the store
  needs a cluster-side backend. This is the real work item hiding in stage 2.

**Rider on N1.** This doesn't open the hosted/cloud non-goal — it's still one user
running their own instance. But "binds to localhost" becomes "binds to a trusted
network," with the Tailscale Kubernetes operator putting the Service on a tailnet so
the Mac and phone reach it identically from anywhere. No ingress controller, no
certs, no public surface. Auth remains a non-goal; the trust boundary just moves
from loopback to the tailnet.

**Sequencing.** Build none of this yet, for a sharper reason than "wait and see":
**the CRD is a public API.** Once a `JobEngine` CR is in someone's Flux repo,
changing its shape is a migration. You don't know which of those fields are real
until the engine has run locally for a month. What v1 owes the future is exactly
two things — **ship a container image from day one** (three lines for a static Go
binary, annoying to retrofit into a release process later), and **make definition
sources pluggable**.

**Open questions for you:**

1. Operator or generated manifests — which world are you in, one `je` per cluster or
   many? I lean operator *if* the answer is many, generator if it's one.
2. Does R1 (no Kubernetes-shaped fields in job YAML, ever) feel like a constraint
   you want me to enforce in the D2 schema from the start? It's cheap now and
   expensive to walk back.
3. Is the k8s package a separate repo/binary, or `je k8s ...` subcommands in the
   same binary? You said separate-but-tightly-integrated; I'd argue the *generator*
   belongs in the binary (so it can't drift from what that version needs) while the
   *distributed artifact* is separate.
4. Should stage 3 (git-sourced jobs) be a v1.1 target, or genuinely later? It's the
   one piece here that would change how you use the engine locally, not just how you
   deploy it.
5. On operator visibility: is `kubectl`-native surfacing (status conditions +
   Events) enough, or do you want the operator emitting into the engine's own event
   log from the start? The latter is the P2-consistent answer and it's what makes
   one timeline hold every fact — but it means the operator is a `je` client, which
   is a real coupling to take on deliberately.

**Your response:**

```



```

---

### D22. Job sources — NEW

**Status:** NEW (v0.5) — two open questions

**Your observation:**

> *"I want in a job definition to be able to reference a github repo or file
> location, that way I could have a repo of jobs or multiple repos, and I could
> run them from anywhere... I could have a collection of all jobs in a repo or I
> can have many repos and je can just run all of them, but I get to keep them
> logically separated."*

**Adopted, and it lands on machinery that already exists.** D19 specified
definition loading as a pluggable source rather than "read this directory," and
called that one of exactly two things v1 owed its future. The `Source` interface
is already built. This item is what plugs into it.

What is new here is the *plural*. D19 imagined one source at a time and framed
authority as a global mode: *"Git and the API can't both be authoritative. Make
it a mode: local or git."* Several registered sources is better, and the
improvement is concrete: **authority becomes per-source rather than global**, so
a scratch job on local disk and a fleet of git-managed jobs can coexist. You get
`je new` for experiments without giving up the repo being the truth for
everything that matters.

#### The shape

A source is a named place definitions come from. Registering one is a CLI
command that edits config, per D2:

```
$ je source add weather github.com/you/weather-jobs --path jobs
$ je source add home    github.com/you/home-automation
$ je source list
NAME     KIND    REF   REVISION   JOBS  SYNCED
local    dir     -     -          2     -
weather  github  main  a3f81c2    5     4m ago
home     github  v1.4  91be07d    3     2h ago
```

**Code travels with definitions.** A source is a whole tree, not just YAML: the
scripts a job runs live beside it in the same repo and arrive in the same fetch.
That is what makes "run them from anywhere" true, and it is why a *relative*
`workdir` resolves against the source root. A job that works in your repo works
on any machine that registers the repo, unmodified — the same promise R1 makes
about the cluster, pointed at your other laptop.

#### Identity, which is the part that needs deciding carefully

Two repos will eventually both contain a `sync.yaml`, and you may not own either
of them. So a job's identity is **qualified by its source** — `weather/ingest` —
and the short form resolves when it is unambiguous:

```
je run ingest              # fine while only one source has it
je run weather/ingest      # always works
je run sync                # error: ambiguous, naming home/sync and weather/sync
```

The qualified name is what the database stores, what events carry, and what
chains reference. The short form is a CLI convenience that resolves at the edge,
the same way P3 has the tool render truth while you type intent. The local
directory is a source named `local`, so nothing is a special case.

**A chain resolves job names within its own source first.** That keeps a repo
self-contained: a chain file in the weather repo referring to `normalize` means
*that* repo's normalize, and the repo stays portable rather than depending on
what else you happen to have registered.

#### Fetching, and why not `git`

Not by shelling out to `git`: it breaks the single-static-binary property and
there is no git in a `FROM scratch` image (D19 ships one). Not by vendoring a
pure-Go git implementation either — a large dependency for a small need.

**Fetch a tarball over HTTPS.** For a public GitHub repo that is
`codeload.github.com/<owner>/<repo>/tar.gz/<sha>`, which is `net/http`,
`archive/tar` and `compress/gzip` — all standard library, nothing added to the
dependency list, works anywhere the binary works.

It also has a property worth having on purpose: **fetching by SHA forces the
pinning question to be answered.** A `ref:` of a branch or tag resolves to a
commit once, visibly, and the fetch is then of something immutable, cached
content-addressed at `<data>/sources/<sha>/` and reusable forever.

That matters more than it sounds:

> Without a recorded SHA, "what ran?" is unanswerable for a job whose code came
> from a moving branch, and D11 quietly stops being true for every remote job.

So a run records the source revision alongside the definition hash, and
`je explain` shows the resolved commit. D19 anticipated exactly this — *"the
commit SHA is the definition version."*

#### What it forces

- **One authority per job name.** Two sources offering the same qualified name
  is a load error. Ambiguity in the *short* form is a resolution error at the
  CLI, which is a different and much friendlier failure.
- **Sync is a visible job.** P2: the engine's own work is jobs, so this is
  `system.sync`, emitting `source.synced` with the old and new revisions. Code
  fetched from a moving branch changing what runs tonight is precisely the thing
  that must never be silent.
- **Sync stays atomic.** One unparseable file rejects that source's sync and its
  last good tree keeps serving. Unchanged from D19, now scoped per source rather
  than globally — a broken weather repo must not stop the home jobs.
- **Definition-writing commands target `local` only.** `je new` and `je set`
  refuse on a git-sourced job and say to edit the repo, because the checkout is
  a cache rather than a working copy. This is D19's mode switch, made per-source.
- **Deleting a source never deletes history.** Same rule as a removed file: jobs
  tombstone, runs and cursors stay (D19, P1).

#### The security shape, stated plainly

This is scheduled execution of code fetched from the internet, running with your
`PATH` and your `HOME`. That is the same trust model as a CI runner and it is
fine for repos you own, but three things follow and none are optional:

- Pinning to a tag or SHA must be as easy as tracking a branch.
- An update from a moving ref is a logged event with both revisions, never a
  surprise discovered afterwards.
- Extraction must reject entries that escape the destination directory. A
  tarball containing `../../.ssh/authorized_keys` is the oldest trick there is,
  and the fetch code is the only place in this project that will ever unpack an
  archive from outside.

Public repos only for v1, which sidesteps credentials entirely and keeps this
from waiting on D10's cluster-side story.

**Open questions for you:**

1. **Sync cadence.** Manual `je source sync` plus a sync on daemon start is the
   honest v1, with a scheduled `system.sync` once the scheduler exists. Or do
   you want a per-source `interval:` from the start? I lean manual first — an
   engine that silently pulls new code on a timer before you have watched it do
   so once is a lot of trust to extend up front.
2. **Default ref.** Track `main` by default, or require an explicit `ref:` so
   that pinning is a decision rather than something you forget? I lean requiring
   it for tags and allowing a bare branch, but there is a real argument that
   defaulting to a moving branch is the wrong default for something that runs
   unattended.

**Your response (v0.5):**

```



```

---

### D20. Control plane and nodes — capability-based placement — NEW

**Status:** NEW — react. **This item must be agreed before any of it is built**;
the constraints below are the design, not an implementation detail of it.

**Your position:**

> *"I think you should have one control plane for JE...and multiple data planes
> (workers). One source of truth...or at least the ability to find the truth in one
> place... we just have to define the constraints of the system up front and figure
> out which ones we are willing to accept."*

**Why this opened.** D19 moves the engine off the Mac, and in doing so makes an
entire class of job permanently impossible. Apple Shortcuts is the clean example:
it's not a preference, a Linux container physically cannot run one. Same for
anything touching your local weather station, your local filesystem, or a device on
your LAN. Either that capability is gone forever, or something Mac-side executes.

**The alternative I proposed and you rejected, recorded because the reasoning
matters:** run a second `je` on the Mac and federate the two by `je emit`. Zero new
architecture, works today. It fails on **P1** — two databases means two timelines,
and "what happened last night" stops having a single answer. P1 is the founding
constraint of this document, so it wins. Noting it here so it isn't re-proposed as a
shortcut in six months.

**Vocabulary (F1, ninth noun).** The data plane members are **nodes**, and *worker*
is a **role** a node plays, not the entity itself. This matters because the same
machine is often two things at once: your phone is a **Source** whenever it emits
`location.arrived` — no session, no registration, per D16 — and a **Node** only
while the app is foregrounded and holding a connection. Emission must never require
a session; an iOS app can't hold one in the background, so coupling the two would
break the location case outright. See F1 for the full distinction.

**Second, a correction to something in the discussion: node liveness is not
consensus.** Consensus is required when several parties must agree on a value with
no trusted authority. Here there *is* one — the control plane is the sole writer to
SQLite and is correct by definition. No election, no quorum, no Raft. Liveness is a
lease with a timeout, decided unilaterally.

There is exactly one genuinely hard problem, and no algorithm solves it: **when a
node stops heartbeating, "it died" and "it's partitioned and still running your job"
are indistinguishable.** That ambiguity is inherent to the network, not to our
design. It gets resolved by policy — C6 — and that is the constraint that actually
requires your consent.

#### The constraints

**C1. One writer.** Nodes never touch the database, never schedule, never decide.
Everything goes through the API. The control plane is a single point of failure —
but it already is (one SQLite file, `replicas: 1` per D19), so this introduces no
new failure mode.

**C2. Nodes are stateless and disposable.** All durable state lives in the control
plane. A restarting node loses nothing but its in-flight runs. No node-side
database, no sync protocol, no reconciliation. This is the constraint that keeps
this from becoming a distributed system.

**C3. Jobs are pinned, not placed.** A job declares a capability label —
`runs_on: macos` — and runs go to a node advertising it *and* carrying the execute
role. No work stealing, no rebalancing, no load-aware scheduling. Every expensive
piece of distributed execution machinery exists because work is *fungible*; here it
isn't, so none of it is needed.

**C4. Nodes dial out.** The node opens a long-lived connection to the control plane
and holds it. No inbound ports on the node, no service discovery, no registry to
maintain, and it works from a laptop on cellular behind CGNAT. This one constraint
deletes an entire category of infrastructure, and it's the same pull-not-push
reasoning that made git sync the right shape in D19.

**C5. Liveness is a lease, not an election.** The node heartbeats on the open
stream; the control plane expires the lease on its own clock. Note that liveness
governs *roles*, not emission — a node going offline stops job dispatch and
attention delivery, and has no effect whatsoever on that machine's ability to emit
events as a Source (F1, D16).

**C6. The uncertainty window is a per-job policy.** Since dead and partitioned
cannot be distinguished, the job declares its tolerance:

- `on_node_lost: fail` — **at-most-once.** Run marked lost, surfaced in
  `--attention`, a human decides. Correct for anything non-idempotent.
- `on_node_lost: retry` — **at-least-once.** Handed to D7's retry logic, may
  execute twice. Correct for idempotent work.

There is no third option. Any system claiming otherwise is hiding the choice rather
than solving it; making it explicit per job is the honest version, and it's
consistent with D7 already treating re-runs as a declared policy rather than a
default.

**C7. Fencing on reconnect.** A node whose lease expired is told on reconnect that
its claim was revoked; it kills the process and discards results. This doesn't
eliminate the C6 window — nothing can — but it bounds it to the partition duration
and prevents a late result from writing state after the control plane has moved on.

**C8. An offline node produces a visible waiting state, never a silent backlog.**
`waiting for node: macos (offline 4h)` in `je status --attention` (P1). And a
schedule that fires while no node advertises the label follows **D9's existing
catch-up policy** — a closed laptop is precisely the sleep problem D9 already
solves, simply relocated. No new semantics required, which is a good sign the model
is right.

**C9. Everything streams through the control plane.** Logs, events, run state. One
place to look; D12 is unchanged from your side. The cost is a network hop and
buffering when a stream drops.

**C10. Version skew is refused, loudly.** A node whose version doesn't match the
control plane refuses to register and says why. Cheaper than protocol negotiation
and more honest than silent incompatibility. **This applies to the session, not to
emission** — an old phone build can always still emit, because a Source has no
version to skew (F1).

#### What a node looks like

```
$ je nodes
NAME      ROLES              LABELS       SESSION
macbook   execute, receive   macos        online 4d
imac      execute            macos        offline 4h
iphone    receive            ios          online (foreground)
```

The phone appears here only while foregrounded — and its location events keep
arriving regardless, because those come in as a Source. Two facts about one device,
which is exactly why *worker* was the wrong name for the row.

#### What remains a non-goal

Work stealing, autoscaling, node-to-node communication, multi-region placement,
and — stated explicitly because it's the one people assume — **control plane HA.**
No failover, no leader election, no standby. If the control plane is down, nothing
runs and you can see that it's down. Accepting that is precisely what keeps C5 from
becoming consensus.

#### The two things that are still real work

These are not hidden by the constraints above, and they're where the time will go:

- **Secrets reaching the node (D10).** The same unsolved problem D19 flagged for
  the cluster, and the harder half of it. A job referencing a secret by name must
  work in both places without the definition changing (D19's R1).
- **D6's job protocol over a network.** `JOB_EVENTS_FILE` is a local file. The node
  should relay it — the job's contract stays byte-identical whether it runs locally
  or remotely — rather than the protocol growing a network path. That keeps D6
  unchanged, which is worth some implementation awkwardness in the node.

Everything else on the constraint list is a table, a timeout, and a long-lived
HTTP stream.

#### Sequencing

Not v1. **N2's definition of done doesn't move**, and job #1 must not need a node.
The failure mode to avoid is building node infrastructure before a real job demands
it, then shaping jobs around the infrastructure. Write the first genuinely Mac-only
job, feel the gap, then build this against a concrete case.

What v1 owes it is small and mostly already true: the control plane owns all state
(C1) and the engine core is a library with the daemon as a thin wrapper (D18) — the
execute role is then largely the same executor code with a different source of work.

**Open questions for you:**

1. C6 is the one requiring real consent: are you comfortable with per-job
   `on_node_lost`, defaulting to `fail` (at-most-once) since it's the safe default
   and the surprising failure is a silent double-execution?
2. What happens when **two nodes advertise the same label**? Options: pick either
   (simplest, and fine when they're genuinely equivalent), or refuse to start with
   an error (safest, catches the accidental second laptop). I lean refuse — an
   unintended duplicate node is far more likely than a deliberate pair.
3. Is `runs_on` a single label or a selector (`runs_on: [macos, has-gpu]`)? Single
   is simpler and covers everything you've described; a selector is hard to remove
   later.
4. Is the command `je node` in the same binary, or a separate small binary? Same
   binary means one artifact and no version-skew ambiguity (C10); separate means the
   thing on your laptop is smaller and can't accidentally be started as a daemon.
   (Note the iOS case doesn't get a choice — that's app code either way, which is an
   argument for the session protocol being simple enough to reimplement.)
5. **Is `node` the right word**, given it collides with Kubernetes and D19 puts this
   next to a cluster? Alternatives considered and rejected: *worker* (names only one
   role), *device* (wrong for a container), *agent* (badly overloaded now), *peer*
   (implies equality that C1 denies). F1 records the reasoning; easy to change now
   and hard once it's in the CLI and the iOS app.

**Your response:**

```



```

---

### D12. The observability surface

**Status:** AGREED (rendered through the CLI per D15)

The seven requirements stand as written in v0.1 — live tailing, run timeline,
causation chain, job health view, needs-attention triage, terminal-failure
notification — with the surface being the CLI rather than a web UI, per D15.

> *"I like these... please see my comments about a CLI vs a Web UI though."*

A ninth from D17: **the topology view.** `je routes` renders the full resolved
trigger graph — every route, wherever it was authored — and `je routes --job X`
answers "what can trigger this?" and "what does this trigger?" That view is what
makes the two authoring locations safe to allow.

One addition from P1, which is now the eighth requirement and arguably the most
important: **the waiting view.** What is scheduled, what is pending on a trigger,
what is queued behind the concurrency cap, and what's blocked on a misconfiguration
(e.g. a missing secret from D10). Everything the engine intends to do but hasn't
done yet, in one place.

Still not in v1: OpenTelemetry, metrics endpoints, distributed tracing.

---

### D13. Retention — revised

**Status:** AGREED (revised)

> *"I almost think the defaults can be tighter... records could be 30 days... logs
> could be 30 days as well."*

Adopted, with one constraint I have to add:

- Run **logs**: 30 days.
- Run **records**: 30 days.
- **Events**: 30 days **but never shorter than run records** — the causation chain
  (D12) reads through events, so if events expire first, "why did this run?" breaks
  for runs still in the history. Enforced as a validation rule on the config, not
  left as a footgun.
- **Job state (D14)**: never auto-expired. Current state is tiny and losing it
  means re-processing from the beginning. State *history* trims to the last 100
  versions.
- Vacuum runs as `system.retention`, an ordinary job (P2).
- Per-job `keep_logs: forever` escape hatch.

---

## Part 6 — Scope

### N1. Non-goals — revised

**Status:** AGREED (revised)

> *"I do think that workers can be isolated from 'Distributed Execution' in
> cases... when I think workers I think concurrency. Also, before a full non-goal
> of fan-in, look at the response in D3."*

Both taken. Fixing the terminology first, because you're right that I conflated
two things: the engine absolutely has a **worker pool** — N goroutines pulling from
a queue, bounded by D8's concurrency cap. That's concurrency and it's core. What's
a non-goal is **remote workers**: a separate process on a separate machine, which
brings a queue protocol, heartbeats, leases, and split-brain. Revised list:

*(v0.4, later in the round: D20 splits this bullet further — the split-brain fear
was correct for fungible work and wrong for capability-pinned work, since one
control plane stays the sole writer. Terminology also moved on: the entity is a
**node**, and worker is a role it plays. See F1.)*

- **Distributed execution for scale.** ~~Remote/distributed workers~~ — **reworded
  per D20**, because the original bullet blocked two things that only share a name.
  In-process worker pool: yes, core. What stays out is *fungible* work spread across
  interchangeable machines for throughput: no work stealing, no rebalancing, no
  placement optimization, no autoscaling, no node-to-node communication, and no
  control-plane HA or leader election. What is **now permitted** — D20, not v1 — is
  **capability-based placement**: a job pinned to a node because only that machine
  can run it at all (Apple Shortcuts, a device on your LAN). That needs a lease and
  a heartbeat, but not consensus, because one control plane remains the sole writer.
- **Multi-user / auth / RBAC.** Binds to localhost — or, per D19, to a trusted
  network. (Revisit if Q1 changes.)
- **Durable/replayable workflows.** No execution replay. (D5.)
- **Workflow DSLs and DAGs.** ~~Fan-in~~ — **removed as a non-goal per D3.** Fan-in
  is in, as declarative standing queries over the event log. Still out: a workflow
  language, imperative orchestration code, compensation/rollback semantics, and
  visual pipeline editing.
- **Building container images from source.** You bring an image or a command.
- **A hosted/cloud version.** (See Q1 — you left the door open; I'd still keep it
  shut for v1.)

---

### N2. What "v1 done" means

**Status:** AGREED

**Revised in v0.4:** v1 is done when **weather ingest has run unattended for two
weeks and you haven't thought about it.** One real job honestly running beats three
invented ones — see Q2. Feature set:

- process + container executors
- schedule, manual, run-completion, and `je emit` event sources
- retries, timeouts, overlap policy, concurrency cap
- SQLite state + logs, with retention
- **job state / cursors (D14)**
- **CLI over a local API; daemon installed via launchd (D15, D16)**
- job definitions as watched YAML files, CLI-editable (D2)
- **chain files + `JOB_EVENTS_FILE`, with load-time cycle checking, `je chains`
  and `je routes` (D17)**
- **engine core as a library, daemon as a thin wrapper (D18)**
- **the TypeScript shim, and only that one (D21)**

Not in v1: web UI, fan-in triggers (v1.1, schema-ready), TUI, webhook listener,
notifications beyond a shell hook, secrets beyond the local store, shims for any
language a real job hasn't asked for yet (D21).

> *"I have my mac app (Almanac) on this machine — one of the things it does is
> weather data that it pulls from my local weather station. I wonder if that
> process of pulling in weather data could be done with the job engine."*

**Weather ingest is job #1**, and it's a good one: it's real, it's scheduled, it's
idempotent-ish, and — most usefully — it's a *cursor* job, so it exercises D14 from
day one rather than as an afterthought. It also naturally tests catch-up (D9),
since your laptop sleeps and weather data keeps accumulating.

> *"I think after we get this v1 up, we should have a session where we think about
> how a job engine could help me... I've mostly done ETL jobs, but I've never
> really liked job engines, so this project is also a way for me to understand job
> engines better."*

Noted, and it reframes the project usefully: this is partly an instrument for
figuring out what you actually want from job engines. That argues for shipping the
core loop fast and letting real jobs generate the roadmap — which is what N2's
definition of done already does. I'd add one thing: keep a running note of every
moment the tool annoys you. That list is the v2 spec, and it's the thing you can't
reconstruct later.

---

## Part 7 — Remaining questions

### Q1. Audience, and the storage adapter idea

**Status:** AGREED — SQLite only, no adapter

> *"Okay, let's keep it simple for now with SQLite. No storage adapter."*

Locked. The discipline still applies: all SQL in `internal/store`, no `interface`
until a second implementation exists, no gratuitous SQLite-isms. That keeps a future
port a contained project without paying for it now.

> *"There is a possibility of others using it, but it's not a major goal right now.
> I wonder if a storage adapter strategy might be the thing to think about now...
> that way someone could choose how much pre-steps they want for setting up a job
> engine."*

**I want to push back on the adapter, while agreeing with the goal behind it.**

The goal — someone else can run this without a big setup, or with Postgres if
that's what they have — is good. The mechanism worries me, for one specific reason:
**an abstraction designed against a single implementation is almost always the
wrong abstraction.** SQLite's characteristics leak into every design decision we've
made in this document: single-writer, embedded, synchronous, no network latency,
transactions that are nearly free. A storage interface built while those are all
true will encode them accidentally, and the first time someone points it at
Postgres, the interface will be wrong in ways that require rewriting the callers
anyway. You'd have paid the abstraction tax up front and still pay the port later.

There's a second cost that matters more given Q3: an adapter interface makes every
piece of code you write while learning Go more indirect. Interfaces, mocks, and
factories are exactly the layer that makes a codebase hard to read when you're new
to the language.

**What I'd do instead** — which gets most of the benefit for none of the cost:

- **All SQL lives in one package** (`internal/store`). Nothing else in the codebase
  writes a query. That's just good structure, and it's what makes a future port a
  contained project rather than an archaeology dig.
- **No `interface` until the second implementation exists.** In Go this is
  idiomatic advice, not just my preference — you define the interface at the point
  of use when you actually have two things to satisfy it.
- **Avoid gratuitous SQLite-isms** in schema and queries where the standard
  equivalent is just as good. Cheap discipline, real payoff.

Then, if someone shows up wanting Postgres, extracting the interface from a working
single-implementation store is a day of mechanical work with the compiler as your
guide, informed by two real implementations instead of one imagined one.

**Where I would spend the "others can use it" budget instead:** a single static
binary with zero dependencies is *already* the best answer to "how much pre-setup
do I need?" — it's `brew install`, `je install`, done. That's a better on-ramp
than storage flexibility, and we get it for free from decisions already made.

**Open question:** does that land, or do you still want the adapter? If your
instinct is "I'll regret not having it," say so — it's your project and it's a
recoverable mistake in either direction. I just don't want to pay for it now.

**Your response:**

```
okay...lets keep it simple for now with SQLite. No storage adapter


```

---

### Q2. The first jobs

**Status:** RESOLVED — one real job is enough (see below)

*(Asked in v0.2, answered in v0.4 — resolution below the block.)*

Job #1 is weather ingest from your local station (N2). **I need two more.**

The reason I keep asking: three real jobs is what stress-tests a design that
currently has one. Weather ingest is a scheduled cursor job, which exercises D9
and D14 — but it doesn't exercise chaining (D3), secrets (D10), containers (D1),
or anything that fails in interesting ways. If jobs #2 and #3 are also scheduled
cursor jobs, we'll have built a narrow tool and won't find out for months.

Ideal candidates would be *different in kind*: something triggered by an external
event rather than a clock, something that talks to a third-party API with a token,
something with a dependency on another job, or something you currently do by hand
and keep forgetting.

**Your response:**

```
I think weather ingest will do secrets...we have an API token that is used for it. 

I'm sorry I don't have other clear jobs...in my Alamanc app I've been working on this concept of attention...like things 
get put in front of you when they need to...thing time, place, etc. Its similar to this concept...I wonder if having this 
be a single binary will be nice...in that we could incorporate this job engine into that mac app...that way its not a custom 
job engine in Almanac...but us just using this re-usable job engine 


```

**Resolution (v0.4).**

**Secrets: confirmed.** Weather ingest uses an API token, so job #1 exercises D10
after all. Good — that was the gap I was most worried about, since secrets are the
easiest thing to get subtly wrong and the worst to discover late.

**On not having two more jobs: that's fine, and I'd stop looking.** You said this
project is partly a way to understand what you actually want from job engines.
Inventing jobs to satisfy a design document would defeat that — they'd be
hypothetical, and hypothetical jobs produce hypothetical requirements. Weather
ingest is real and it exercises schedules, catch-up, cursors, and secrets, which is
four of the six things I wanted covered. The remaining two — chaining and containers
— will get exercised the first time you actually want them, and that's the right
time to find out whether the design holds.

Revised N2 accordingly: **v1 is done when weather ingest has been running
unattended for two weeks and you haven't thought about it.** One real job, honestly
running, beats three invented ones.

**The Almanac idea is the biggest thing in this round — it's now D18.**

---

### Q3. How do you want to work in Go?

**Status:** AGREED — option (a)

*(This was the one blocking item. Answered — resolution below the block.)*

The fork, restated concretely:

- **(a) I write it, you review it.** Fastest to a working engine. I write idiomatic
  Go, explain the non-obvious parts in comments and in chat, and you learn by
  reading and modifying. Risk: you end up with a codebase you own but didn't build,
  and Go stays a language you can read but not write.
- **(b) I scaffold, you implement.** I set up the module structure, write the hard
  or boring parts (SQLite layer, HTTP plumbing), and hand you well-specified pieces
  with the surrounding types already defined. Slower to v1, much better retention.
  Risk: it stalls if you get busy.
- **(c) Hybrid, which is what I'd actually recommend.** I build the skeleton and
  the parts where Go-specific knowledge is load-bearing and getting it wrong is
  subtle — the concurrency in the scheduler, process lifecycle and signal handling,
  the SQLite access layer. You build the parts that are mostly domain logic and
  plain code — the YAML loading, the CLI commands, the trigger matching, the
  retention job. Those are where you'll learn the most per hour, and where a bug is
  obvious rather than a 3am mystery.

**Your response:**

```
I think option A to start with...I think having it explained to me and reviewing it is a good 
way to see patterns. I also think it means we can write idiomatic Go and it can stay as simple 
as possible...which I think is a good reliable thing to do anyway. 


```

**Resolution (v0.4).** Option (a): I write, explain, you review. How I'll hold up my
end:

- **Boring, idiomatic Go.** Standard library first, minimal dependencies, no clever
  abstractions. Your instinct that this is also just *better* is correct — the
  constraint and the good practice point the same direction here, which is lucky.
- **Explanations go where you'll re-read them**, meaning the code and the commit
  messages, not chat. Chat scrolls away; a comment explaining why a channel is
  buffered is there in six months.
- **Small, reviewable commits** organized by concept rather than by file, so reading
  the history teaches the system.
- **I'll flag the Go-specific parts explicitly** — the places where the idiom is
  non-obvious (context cancellation, `errgroup`, `defer` ordering, why a struct is
  a pointer). Those are the moments worth pausing on.

Switching later is easy: say the word at any point and we move specific subsystems
to (c), where you implement against a shape I've defined. The YAML loader, the CLI
commands, and the trigger matcher are the natural candidates — mostly domain logic,
obvious when wrong, and the highest learning-per-hour in the codebase.

---

## Appendix A — Data model (v0.5)

Still illustrative, but close to what we'll actually create.

```
events
  id, type, source, payload (json), caused_by_event_id,
  caused_by_run_id, depth, dedupe_key, actor, created_at
  -- + dedupe_key, actor (D16 job emit); events from JOB_EVENTS_FILE
  --   set caused_by_run_id automatically (D17)

jobs                         -- current loaded definitions
  id, name, definition_hash, file_path, enabled,
  loaded_at, load_error, config_error
  -- + config_error (D10: missing secret = not schedulable)

job_versions                 -- immutable snapshots, deduped by hash
  definition_hash, definition (json), first_seen_at

routes                       -- D17: triggers, wherever authored
  id, target_job_id, match (json), route_hash,
  chain_name, step_index,   -- null when authored job-local
  source (job_file | chain_file), file_path, enabled, load_error
  -- a job-local `on:` block compiles to rows here too, so there is
  --   exactly one trigger table regardless of authoring location
  -- chain_name is a naming/display grouping, NOT a runtime entity (D17)

runs
  id, job_id, definition_hash, triggering_event_id,
  triggering_route_id, route_hash,
  status (queued|running|succeeded|failed|interrupted|cancelled|timed_out),
  queued_at, started_at, ended_at, attempt_count,
  state_version_in, output (json), error
  -- + state_version_in (D14: which cursor this run started from)
  -- + triggering_route_id, route_hash (D11/D17: route provenance)

attempts
  id, run_id, attempt_number, triggering_event_id, actor,
  status, started_at, ended_at, exit_code, executor, container_id, error
  -- + triggering_event_id, actor (D7: automatic retry vs. human retry)

job_state                    -- D14, current cursor per job
  job_id, version, value (json), set_by_run_id, set_by_actor, created_at
  -- append-only; current = max(version); trimmed to last 100
  -- v1 is written by the engine at first run (v0.5 seeding): set_by_actor
  --   'engine', value = { <primary_cursor>: <run start time> }. Stored rather
  --   than computed so `je state history` shows where the cursor came from.

trigger_state                -- D3, schema present in v1, used in v1.1
  id, route_id, correlation_key,
  satisfied_conditions (json), window_started_at, expires_at, fired_at
  -- keyed on route_id rather than (job, index) now that routes exist (D17)

logs                         -- separate DB file
  run_id, attempt_number, seq, stream (stdout|stderr), ts, line
```

## Appendix B — Job and chain definitions (v0.5)

**What `je new weather-ingest` actually writes**, per P3 — only what you decided:

```yaml
# jobs/weather-ingest.yaml
name: Weather Ingest
description: Pulls readings from the local station into Almanac's store

command: ["npx", "tsx", "scripts/ingest-weather.ts"]
language: typescript          # D21: opt into the shim. Omit for the raw protocol.
workdir: ~/code/almanac

on:
  - every: 15m
    catch_up: once

state:
  primary_cursor: since

secrets: [STATION_API_KEY]
```

Everything else — `runtime: process`, `timeout: 1h`, `overlap: skip`,
`on_interrupt: fail`, `state.commit: on_success`, no retries — is a default living
in the engine, not a line in your file. To see the full picture:

```
$ je explain weather-ingest
  command        npx tsx scripts/ingest-weather.ts   (jobs/weather-ingest.yaml:5)
  runtime        process                             (default)
  language       typescript                          (jobs/weather-ingest.yaml:6)
  every          15m                                 (jobs/weather-ingest.yaml:10)
  catch_up       once                                (jobs/weather-ingest.yaml:11)
  timeout        1h                                  (default)
  overlap        skip                                (default)
  retry          none                                (default)
  on_interrupt   fail                                (default)
  state.commit   on_success                          (default)
  secrets        STATION_API_KEY                      ✓ set 3d ago
```

**The job itself, with the shim (D21):**

```typescript
// scripts/ingest-weather.ts
import je from "./.je/je.mjs";        // written by the engine, gitignored

// Seeded with this run's start time on a first run, so it is never undefined.
const readings = await fetchReadings(je.state.since, process.env.STATION_API_KEY!);

console.log(`ingested ${readings.length} readings`);   // captured and stored

je.setState({ since: readings.at(-1)?.ts ?? je.state.since });
je.emit("weather.ingested", { count: readings.length });
```

**And the same job without it**, because R1 says the protocol is the floor and
every language reaches it — this is what `language:` is sugar over:

```python
# scripts/ingest_weather.py — no shim, no SDK, nothing imported
import json, os

state = json.loads(os.environ["JE_STATE"])
readings = fetch_readings(state["since"], os.environ["STATION_API_KEY"])
print(f"ingested {len(readings)} readings")

with open(os.environ["JOB_STATE_OUT_FILE"], "w") as f:
    json.dump({"since": readings[-1]["ts"] if readings else state["since"]}, f)

with open(os.environ["JOB_EVENTS_FILE"], "a") as f:
    f.write(json.dumps({"type": "weather.ingested",
                        "payload": {"count": len(readings)}}) + "\n")
```

The v0.2 version of this file had 24 lines and 8 of them were restating defaults.
This is the same job.

```yaml
# jobs/daily-rollup.yaml — the job knows nothing about its peers (D17)
name: Daily Rollup

runtime: process
command: ["python", "scripts/rollup.py"]

timeout: 30m
overlap: skip
```

```yaml
# chains/daily-weather.yaml — one chain per file (D17)
description: Ingest readings, roll them up, alert if the pair doesn't land

steps:
  - on: { event: run.succeeded, where: { job: weather-ingest } }
    run: normalize-readings

  # fan-in (D3, v1.1)
  - on:
      all_of:
        - { event: run.succeeded, where: { job: normalize-readings } }
        - { event: run.succeeded, where: { job: power-ingest } }
      within: 6h
      fire: once_per_window
    run: daily-rollup

  # the pair never both landed (D3, v1.1)
  - on: { event: trigger.expired, where: { step: 1 } }
    run: notify-me
```

```yaml
# chains/presence.yaml — a different chain, its own file
description: React to office motion

steps:
  # external source via `je emit` (D16), driven by a HomeKit Shortcut
  - on: { event: homekit.motion, where: { room: office } }
    run: log-presence
```

Two things worth noting about how this evolved. In v0.2 the fan-in lived inside the
rollup job's own `on:` block, and the alert path had a `run:` key nested in a job's
trigger list — which never made sense, since that route's target isn't the job whose
file it was in. And in v0.3 both landed in a single `routes.yaml` that would have
grown into exactly the god-file I'd flagged as a risk. The chain form fixes both:
the contortion is gone rather than relocated, and the file has a natural size limit
because it describes one flow. That's the evidence D17 landed in the right place.

```yaml
# jobs/route-weather.yaml — a router job (D17): dispatch that needs computation
name: Route Weather
description: Decides what happens with a reading. Emits, runs nothing itself.

runtime: process
command: ["python", "scripts/route_weather.py"]
timeout: 30s
```

```python
# scripts/route_weather.py — the whole router protocol, no shim, no SDK
import json, os

event = json.loads(os.environ["EVENT_PAYLOAD"])
out   = "weather.anomalous" if event["temp_f"] < -20 else "weather.normal"

with open(os.environ["JOB_EVENTS_FILE"], "a") as f:
    f.write(json.dumps({"type": out, "payload": event}) + "\n")
```

The same router with the TypeScript shim is two lines, and does exactly the same
thing to exactly the same file — which is R2 working: the shim saves typing, not
capability.

**Your response** (reactions to either appendix):

```



```
