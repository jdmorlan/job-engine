import React from 'react'
import { api, usePoll, useJobNames } from './api'
import { Panel, Load, Dot, ago, span, dur } from './bits'

// ---------------------------------------------------------------- overview

export function Overview({ onPick }) {
  const jobs = usePoll(api.jobs, [], 5000)
  const names = useJobNames()
  const runs = usePoll(() => api.runs('?limit=12'), [], 4000)
  const waiting = usePoll(api.waiting, [], 4000)

  return (
    <>
      <div className="head">
        <h1>Overview</h1>
        <p>What ran, what is about to, and what is stuck.</p>
      </div>
      <div className="body cols">
        <div style={{ display: 'grid', gap: 18 }}>
          <Panel title="Recent runs">
            <Load state={runs} empty={(d) => (d.runs?.length ? null : 'nothing has run yet')}>
              {(d) => (
                <table>
                  <thead>
                    <tr><th>Run</th><th>Job</th><th>Status</th><th>Took</th><th>When</th></tr>
                  </thead>
                  <tbody>
                    {d.runs.slice(0, 12).map((r) => (
                      <tr key={r.id} onClick={() => onPick(r.id)}>
                        <td className="mono faint">#{r.id}</td>
                        <td className="mono">{r.job || names[r.job_id] || `job ${r.job_id}`}</td>
                        <td><Dot status={r.status} />{r.status}</td>
                        <td className="mono dim">{span(r.started_at, r.ended_at)}</td>
                        <td className="dim">{ago(r.ended_at || r.queued_at)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </Load>
          </Panel>

          <Panel title="Jobs">
            <Load state={jobs} empty={(d) => (d.jobs?.length ? null : 'no jobs loaded')}>
              {(d) => (
                <table>
                  <thead>
                    <tr><th>Job</th><th>Runs on</th><th>Source</th><th>Loaded</th></tr>
                  </thead>
                  <tbody>
                    {d.jobs.map((j) => (
                      <tr key={j.id} onClick={() => onPick(null, j.slug)}>
                        <td className="mono">{j.slug}<div className="faint" style={{ fontSize: 11 }}>{j.definition?.description}</div></td>
                        <td className="mono dim">{j.definition?.runs_on}</td>
                        <td className="mono dim">{j.source || 'local'}</td>
                        <td className="dim">{ago(j.loaded_at)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </Load>
          </Panel>
        </div>

        <div style={{ display: 'grid', gap: 18 }}>
          <Waiting state={waiting} />
          <Workers />
        </div>
      </div>
    </>
  )
}

// The negative space: what the engine intends to do and has not done (P1). This
// is the view the whole Visibility Rule is betting on, so it renders every
// bucket the endpoint has -- including the empty ones, because "nothing is
// blocked" is information and an absent section is not.
function Waiting({ state }) {
  const buckets = [
    ['scheduled', 'Scheduled', (w) => `${w.job} · ${w.schedule}`, (w) => ago(w.next)],
    ['queued', 'Queued', (w) => w.job, (w) => ago(w.since)],
    ['running', 'Running', (w) => w.job, (w) => ago(w.started_at)],
    ['blocked', 'Blocked', (w) => `${w.job} — ${w.reason}`, () => ''],
  ]
  return (
    <Panel title="Waiting">
      <Load state={state}>
        {(d) => (
          <div className="pad" style={{ display: 'grid', gap: 12 }}>
            {buckets.map(([key, name, line, when]) => {
              const items = d[key] || []
              return (
                <div key={key}>
                  <div className="faint" style={{ fontSize: 10.5, textTransform: 'uppercase', letterSpacing: '.7px', marginBottom: 4 }}>
                    {name}
                  </div>
                  {items.length === 0 ? (
                    <div className="faint mono" style={{ fontSize: 11.5 }}>none</div>
                  ) : (
                    items.map((w, i) => (
                      <div key={i} className="mono" style={{ fontSize: 11.5, display: 'flex', gap: 8 }}>
                        <span style={{ flex: 1 }}>{line(w)}</span>
                        <span className="dim">{when(w)}</span>
                      </div>
                    ))
                  )}
                </div>
              )
            })}
          </div>
        )}
      </Load>
    </Panel>
  )
}

function Workers() {
  const state = usePoll(api.workers, [], 6000)
  return (
    <Panel title="Workers">
      <Load state={state} empty={(d) => (d.workers?.length ? null : 'no workers attached — nothing will run')}>
        {(d) => (
          <div className="pad" style={{ display: 'grid', gap: 8 }}>
            {d.workers.map((w) => (
              <div key={w.id} className="mono" style={{ fontSize: 11.5 }}>
                <Dot status={w.online ? 'succeeded' : 'none'} />
                {w.name}
                <span className="faint"> · {w.labels?.join(', ')} · {w.version}</span>
                <div className="faint" style={{ marginLeft: 14, fontSize: 11 }}>
                  {w.online ? `seen ${ago(w.last_seen_at)}` : `offline, last seen ${ago(w.last_seen_at)}`}
                </div>
              </div>
            ))}
          </div>
        )}
      </Load>
    </Panel>
  )
}

// ---------------------------------------------------------------- runs

export function Runs({ onPick }) {
  const state = usePoll(() => api.runs('?limit=100'), [], 4000)
  const names = useJobNames()
  return (
    <>
      <div className="head">
        <h1>Runs</h1>
        <p>Every run the control plane remembers, newest first.</p>
      </div>
      <div className="body">
        <Panel>
          <Load state={state} empty={(d) => (d.runs?.length ? null : 'nothing has run yet')}>
            {(d) => (
              <table>
                <thead>
                  <tr><th>Run</th><th>Job</th><th>Status</th><th>Took</th><th>Worker</th><th>Why</th><th>When</th></tr>
                </thead>
                <tbody>
                  {d.runs.map((r) => (
                    <tr key={r.id} onClick={() => onPick(r.id)}>
                      <td className="mono faint">#{r.id}</td>
                      <td className="mono">{r.job || names[r.job_id] || `job ${r.job_id}`}</td>
                      <td><Dot status={r.status} />{r.status}</td>
                      <td className="mono dim">{span(r.started_at, r.ended_at)}</td>
                      <td className="mono dim">{r.worker_id?.replace(/^worker-/, '')}</td>
                      <td className="dim">{r.triggering_route_id ? 'route' : r.triggering_event_id ? 'event' : 'manual'}</td>
                      <td className="dim">{ago(r.ended_at || r.queued_at)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Load>
        </Panel>
      </div>
    </>
  )
}

// ---------------------------------------------------------------- sources

export function Sources() {
  const state = usePoll(api.sources, [], 8000)
  return (
    <>
      <div className="head">
        <h1>Sources</h1>
        <p>Where definitions come from. Authoring lands here, not in the database (D23).</p>
      </div>
      <div className="body">
        <Panel>
          <Load state={state} empty={(d) => (d.sources?.length ? null : 'no sources registered')}>
            {(d) => (
              <table>
                <thead>
                  <tr><th>Name</th><th>Kind</th><th>Ref</th><th>Revision</th><th>Jobs</th><th>Synced</th></tr>
                </thead>
                <tbody>
                  {d.sources.map((s) => (
                    <tr key={s.name}>
                      <td className="mono">{s.name}
                        <div className="faint" style={{ fontSize: 11 }}>{s.location || s.path}</div>
                      </td>
                      <td className="mono dim">{s.kind}</td>
                      <td className="mono dim">{s.ref || '—'}</td>
                      {/* Empty for dir sources today: D11 is not true for local
                          jobs until they are backed by a repo. Shown, not hidden. */}
                      <td className="mono dim">{s.revision ? s.revision.slice(0, 8) : <span className="faint">none</span>}</td>
                      <td className="mono dim">{s.jobs}</td>
                      <td className="dim">{s.last_error ? <span style={{ color: 'var(--fail)' }}>{s.last_error}</span> : ago(s.synced_at)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Load>
        </Panel>
      </div>
    </>
  )
}
