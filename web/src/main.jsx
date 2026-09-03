import React from 'react'
import { createRoot } from 'react-dom/client'
import './styles.css'
import { api, usePoll } from './api'
import Chains from './Chains'
import Drawer from './Drawer'
import { Overview, Runs, Sources } from './Views'

const TABS = [
  ['overview', 'Overview'],
  ['chains', 'Chains'],
  ['runs', 'Runs'],
  ['sources', 'Sources'],
]

function App() {
  const [tab, setTab] = React.useState('overview')
  const [runId, setRunId] = React.useState(null)
  const [jobSlug, setJobSlug] = React.useState(null)
  const health = usePoll(api.health, [], 10000)

  const pick = (run, job) => { setRunId(run ?? null); setJobSlug(job ?? null) }
  const close = () => pick(null, null)

  React.useEffect(() => {
    const k = (e) => e.key === 'Escape' && close()
    window.addEventListener('keydown', k)
    return () => window.removeEventListener('keydown', k)
  }, [])

  return (
    <div className="app">
      <nav className="nav">
        <div className="brand">je<span>job engine</span></div>
        {TABS.map(([id, name]) => (
          <button key={id} className={tab === id ? 'on' : ''} onClick={() => setTab(id)}>{name}</button>
        ))}
        <div className="foot">
          {health.error ? (
            <span style={{ color: 'var(--fail)' }}>control plane unreachable</span>
          ) : health.data ? (
            <>
              {health.data.version}
              <br />
              {health.data.workers} worker{health.data.workers === 1 ? '' : 's'}
            </>
          ) : '…'}
        </div>
      </nav>
      <main className="main">
        {tab === 'overview' && <Overview onPick={pick} />}
        {tab === 'chains' && <Chains onPick={(r) => pick(r)} />}
        {tab === 'runs' && <Runs onPick={(r) => pick(r)} />}
        {tab === 'sources' && <Sources />}
      </main>
      <Drawer runId={runId} jobSlug={jobSlug} onClose={close} />
    </div>
  )
}

createRoot(document.getElementById('root')).render(<App />)
