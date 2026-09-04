# demo

The examples `je demo` registers. Ordinary job files in an ordinary directory —
which is the point: this is what a jobs repository looks like.

```sh
je demo          # registers this directory as a source named "demo"
je demo --remove # unregisters it
```

It is registered as a **subpath** of this repository rather than as a repository
of its own:

```sh
je source add demo github.com/jdmorlan/job-engine --path demo --ref v0.4.2
```

Two reasons that is better than a separate repo. It pins to the tag of the
binary that registered it, so the examples can never reference something the
engine does not have. And it exercises `--path`, which is the feature that lets
one repository hold `python-jobs/` and `typescript-jobs/` and register them as
separate sources — worth having under test by the thing everybody runs first.

| job | what it does | why it is here |
|---|---|---|
| `demo-hello` | prints a line and exits | the smallest job that exists |
| `demo-counter` | keeps a cursor and advances it | what other schedulers leave to you |
| `demo-flaky` | fails about one run in three | watch the cursor *not* move |
| `demo-tick` | runs every minute | gives the scheduler something to do |
| `demo-ingest` | emits an event when it finishes | the start of a chain |
| `demo-report` | runs because that event happened | no clock, no polling |
| `demo-archive` | runs after the report succeeds | and not at all if it doesn't |

Each job is a folder: `demo-counter/job.yaml` is the definition and
`demo-counter/counter.sh` is the code it runs, sitting next to it.

`chains/demo-pipeline.yaml` wires the last three into one named flow. A chain
runs nothing itself — it is a display grouping over the routes that do.

Nothing here needs a secret, a token, or write access to anything.
