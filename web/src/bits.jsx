import React from 'react'

export function Dot({ status }) {
  return <span className={`dot s-${status || 'none'}`} title={status || 'never run'} />
}

export function ago(ts) {
  if (!ts) return '—'
  const d = (Date.now() - new Date(ts).getTime()) / 1000
  if (d < 0) return `in ${dur(-d * 1e9)}`
  if (d < 60) return `${Math.floor(d)}s ago`
  if (d < 3600) return `${Math.floor(d / 60)}m ago`
  if (d < 86400) return `${Math.floor(d / 3600)}h ago`
  return `${Math.floor(d / 86400)}d ago`
}

export function dur(ns) {
  if (ns == null) return '—'
  const ms = ns / 1e6
  if (ms < 1000) return `${Math.round(ms)}ms`
  if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`
  return `${Math.floor(ms / 60000)}m${Math.round((ms % 60000) / 1000)}s`
}

export function span(a, b) {
  if (!a || !b) return '—'
  return dur((new Date(b) - new Date(a)) * 1e6)
}

export function Panel({ title, children, right }) {
  return (
    <div className="panel">
      {title && (
        <h2 style={{ display: 'flex' }}>
          <span style={{ flex: 1 }}>{title}</span>
          {right}
        </h2>
      )}
      {children}
    </div>
  )
}

export function Load({ state, children, empty }) {
  if (state.error) return <div className="err">{String(state.error.message || state.error)}</div>
  if (state.loading) return <div className="empty">loading…</div>
  const d = state.data
  if (empty && empty(d)) return <div className="empty">{empty(d)}</div>
  return children(d)
}
