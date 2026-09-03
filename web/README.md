# je web

The web client from **D23** — a second client of the same HTTP API the CLI uses,
per D15. It has no privileged access, no database path, and nothing it shows
comes from anywhere but `/v1`.

## Running it

The shipped path needs no npm — the built assets live in `internal/webui/dist`
and are embedded in the binary:

```sh
je web run                                       # serves on 127.0.0.1:7621
je web run --control-plane 127.0.0.1:7620        # point it somewhere else
je web start                                     # as a container instead
```

For working on the client itself, the Vite dev server has hot reload and proxies
`/v1` to a control plane:

```sh
make web                                  # against http://127.0.0.1:7620
JE_ADDR=http://127.0.0.1:7621 make web    # against somewhere else
```

Then open http://localhost:5273. Run `make web-build` when you are done, since
the binary embeds `internal/webui/dist` and that directory is committed.

## What phase 1 is

Read-only, and deliberately: it ships against the API exactly as it stands, with
**no engine changes**. That is the payoff D15 was buying when it made the CLI a
thin client instead of the engine itself.

| View | Endpoints |
|---|---|
| Overview — recent runs, jobs, waiting, workers | `/v1/runs`, `/v1/jobs`, `/v1/waiting`, `/v1/workers` |
| Chains — the react-flow canvas | `/v1/chains` |
| Runs — full history, drawer with attempts, state and emitted events | `/v1/runs`, `/v1/runs/{id}/detail`, `/v1/runs/{id}/logs` |
| Sources — where definitions come from | `/v1/sources` |
| Job drawer — resolved vs. definition | `/v1/jobs/{slug}/explain`, `/v1/jobs/{slug}` |

Phase 2 is authoring, and needs the write path D23 specifies.

## Two things worth knowing

**The canvas renders routes, not chains.** A chain is a display grouping and
nothing consults it at runtime (D17), so the nodes are the trigger job and each
step, and the edges are labelled with the event pattern that actually fires the
next one. Hiding that pattern would make the graph prettier and less true (P3).

**The job drawer has two tabs on purpose.** "Resolved" is `/explain` — every
field the engine decided, with a file and line where somebody chose it and the
word *default* where nobody did. "Definition" is what the file says. That split
is P3 made literal, and neither tab is a fallback for the other.

## Known gaps

- `/v1/runs` returns `job_id` and no slug, so the run tables join against
  `/v1/jobs` client-side to name their own rows. The better fix is for the runs
  endpoint to carry the slug — every caller needs it.
- Logs are polled rather than streamed. `/v1/runs/{id}/stream` (SSE) exists and
  should replace the poll.
- Views refresh on a 3–8s poll. Fine for now, and honest — a view that silently
  stops updating is worse than one that visibly lags.
