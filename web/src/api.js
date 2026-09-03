import React from 'react'

// One place that knows the API exists. Every view below is a client of the same
// endpoints the CLI uses -- D15's "every capability is an endpoint" is what makes
// this a second client rather than a second implementation.

async function get(path) {
  const res = await fetch(path)
  if (!res.ok) throw new Error(`${res.status} ${res.statusText} — ${path}`)
  return res.json()
}

export const api = {
  health: () => get('/v1/health'),
  jobs: () => get('/v1/jobs'),
  job: (slug) => get(`/v1/jobs/${slug}`),
  explain: (slug) => get(`/v1/jobs/${slug}/explain`),
  state: (slug) => get(`/v1/jobs/${slug}/state`),
  runs: (params = '') => get(`/v1/runs${params}`),
  runDetail: (id) => get(`/v1/runs/${id}/detail`),
  runLogs: (id) => get(`/v1/runs/${id}/logs`),
  waiting: () => get('/v1/waiting'),
  chains: () => get('/v1/chains'),
  chain: (name) => get(`/v1/chains/${name}`),
  sources: () => get('/v1/sources'),
  workers: () => get('/v1/workers'),
  events: () => get('/v1/events'),
}

// Poll instead of holding a socket open. The control plane is the authority and
// a stale view that refreshes is honest; a view that silently stops updating is
// not (P1).
export function usePoll(fn, deps = [], ms = 4000) {
  const [state, setState] = React.useState({ data: null, error: null, loading: true })
  React.useEffect(() => {
    let alive = true
    const tick = () =>
      fn()
        .then((data) => alive && setState({ data, error: null, loading: false }))
        .catch((error) => alive && setState((s) => ({ ...s, error, loading: false })))
    tick()
    const id = setInterval(tick, ms)
    return () => {
      alive = false
      clearInterval(id)
    }
  }, deps)
  return state
}

// /v1/runs returns job_id and no slug, so a run list cannot name its own jobs.
// Joining here keeps phase 1 free of engine changes; the better fix is for the
// runs endpoint to carry the slug, since every caller needs it.
export function useJobNames() {
  const { data } = usePoll(api.jobs, [], 30000)
  return React.useMemo(() => {
    const m = {}
    for (const j of data?.jobs ?? []) m[j.id] = j.slug
    return m
  }, [data])
}
