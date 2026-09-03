import React from 'react'
import { api, usePoll } from './api'
import { Load, Dot, ago, span, dur } from './bits'

// P3, made literal: two tabs over the same job. "Resolved" is what the engine
// actually decided, from /explain -- every field, including the ones nobody
// wrote. "Definition" is what the file says, which is only the choices somebody
// made. Neither is a fallback for the other.
function JobDrawer({ slug }) {
  const ex = usePoll(() => api.explain(slug), [slug], 10000)
  const job = usePoll(() => api.job(slug), [slug], 10000)
  const [tab, setTab] = React.useState('resolved')

  return (
    <>
      <div className="tabs">
        <button className={tab === 'resolved' ? 'on' : ''} onClick={() => setTab('resolved')}>Resolved</button>
        <button className={tab === 'definition' ? 'on' : ''} onClick={() => setTab('definition')}>Definition</button>
      </div>
      <div className="dc">
        {tab === 'resolved' ? (
          <Load state={ex}>
            {(d) => (
              <div style={{ padding: 16 }}>
                <p className="dim" style={{ margin: '0 0 14px' }}>{d.description}</p>
                <dl className="kv">
                  {d.fields.map((f) => (
                    <React.Fragment key={f.field}>
                      <dt>{f.field}</dt>
                      <dd>
                        {f.value}
                        {/* A line number means somebody wrote it; no line means
                            the engine chose it. That distinction is the whole
                            point of P3, so it is shown rather than flattened. */}
                        {f.line ? (
                          <span className="faint" style={{ marginLeft: 8, fontSize: 11 }}>{d.file_path}:{f.line}</span>
                        ) : (
                          <span className="faint" style={{ marginLeft: 8, fontSize: 11 }}>default</span>
                        )}
                      </dd>
                    </React.Fragment>
                  ))}
                </dl>
              </div>
            )}
          </Load>
        ) : (
          <Load state={job}>
            {(d) => <pre className="raw">{JSON.stringify(d.definition ?? d, null, 2)}</pre>}
          </Load>
        )}
      </div>
    </>
  )
}

function RunDrawer({ id }) {
  const state = usePoll(() => api.runDetail(id), [id], 3000)
  const [tab, setTab] = React.useState('detail')
  const logs = usePoll(() => (tab === 'logs' ? api.runLogs(id) : Promise.resolve(null)), [id, tab], 2000)

  return (
    <>
      <div className="tabs">
        <button className={tab === 'detail' ? 'on' : ''} onClick={() => setTab('detail')}>Detail</button>
        <button className={tab === 'logs' ? 'on' : ''} onClick={() => setTab('logs')}>Logs</button>
      </div>
      <div className="dc">
        {tab === 'logs' ? (
          <Load state={logs}>
            {(d) =>
              !d ? <div className="empty">—</div> :
              <pre className="raw" style={{ maxHeight: 'none' }}>
                {(d.lines || d.logs || []).map((l) => (typeof l === 'string' ? l : l.text ?? l.line)).join('\n') || '(no output)'}
              </pre>
            }
          </Load>
        ) : (
          <Load state={state}>
            {(d) => (
              <div style={{ padding: 16, display: 'grid', gap: 18 }}>
                <dl className="kv">
                  <dt>job</dt><dd>{d.job}</dd>
                  <dt>status</dt><dd><Dot status={d.run.status} />{d.run.status}</dd>
                  <dt>duration</dt><dd>{span(d.run.started_at, d.run.ended_at)}</dd>
                  <dt>worker</dt><dd>{d.run.worker_id}</dd>
                  <dt>definition</dt><dd className="faint">{d.run.definition_hash?.slice(0, 12)}</dd>
                  <dt>caused by</dt>
                  <dd>
                    {d.run.triggering_route_id
                      ? `route ${d.run.triggering_route_id} · event ${d.run.triggering_event_id}`
                      : d.run.triggering_event_id ? `event ${d.run.triggering_event_id}` : 'manual'}
                  </dd>
                  {d.primary_cursor && <><dt>cursor</dt><dd>{d.primary_cursor}</dd></>}
                </dl>

                {/* D14: the cursor moving (or not) is the feature, so the
                    version going in is shown next to what came out. */}
                {d.state_in && (
                  <div>
                    <div className="faint" style={{ fontSize: 10.5, textTransform: 'uppercase', letterSpacing: '.7px', marginBottom: 6 }}>
                      State in · v{d.state_in.Version}
                    </div>
                    <pre className="raw" style={{ padding: 0 }}>{JSON.stringify(d.state_in.Value, null, 2)}</pre>
                  </div>
                )}

                <div>
                  <div className="faint" style={{ fontSize: 10.5, textTransform: 'uppercase', letterSpacing: '.7px', marginBottom: 6 }}>
                    Attempts
                  </div>
                  <table>
                    <thead><tr><th>#</th><th>Status</th><th>Exit</th><th>Took</th></tr></thead>
                    <tbody>
                      {(d.attempts || []).map((a) => (
                        <tr key={a.id}>
                          <td className="mono faint">{a.number}</td>
                          <td><Dot status={a.status} />{a.status}</td>
                          <td className="mono dim">{a.exit_code}</td>
                          <td className="mono dim">{span(a.started_at, a.ended_at)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>

                {!!(d.emitted || []).length && (
                  <div>
                    <div className="faint" style={{ fontSize: 10.5, textTransform: 'uppercase', letterSpacing: '.7px', marginBottom: 6 }}>
                      Emitted
                    </div>
                    {d.emitted.map((e) => (
                      <div key={e.id} className="mono" style={{ fontSize: 11.5 }}>
                        <span className="dim">#{e.id}</span> {e.type}
                        <span className="faint"> · depth {e.depth}</span>
                      </div>
                    ))}
                  </div>
                )}
              </div>
            )}
          </Load>
        )}
      </div>
    </>
  )
}

export default function Drawer({ runId, jobSlug, onClose }) {
  if (!runId && !jobSlug) return null
  return (
    <>
      <div className="scrim" onClick={onClose} />
      <div className="drawer">
        <div className="dh">
          <h3>{jobSlug || `run #${runId}`}</h3>
          <button onClick={onClose}>esc</button>
        </div>
        {jobSlug ? <JobDrawer slug={jobSlug} /> : <RunDrawer id={runId} />}
      </div>
    </>
  )
}
