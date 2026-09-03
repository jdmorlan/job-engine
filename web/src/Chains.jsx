import React from 'react'
import { ReactFlow, Background, Controls, Handle, Position } from '@xyflow/react'
import '@xyflow/react/dist/style.css'
import { api, usePoll } from './api'
import { Load, dur, ago, span } from './bits'

// A chain is a display grouping, not a runtime entity (D17) -- so this canvas
// renders routes, which are the things that actually fire. The trigger job and
// each step become nodes; the event pattern that connects them becomes the edge
// label, because that pattern IS the wiring and hiding it would make the graph
// prettier and less true (P3).

function StepNode({ data }) {
  const s = data.run?.status
  const tone = !data.run ? 'never' : s === 'succeeded' ? 'ok' : s === 'running' ? 'running' : 'failed'
  return (
    <div className={`node ${tone} ${data.trigger ? 'trigger' : ''}`}>
      <Handle type="target" position={Position.Left} style={{ opacity: data.trigger ? 0 : 1 }} />
      <div className="t">{data.job}</div>
      <div className="m">
        {data.trigger ? 'trigger' : `step ${data.step}`}
        {data.run ? ` · ${s} · ${span(data.run.started_at, data.run.ended_at)}` : ' · never run'}
      </div>
      <Handle type="source" position={Position.Right} />
    </div>
  )
}

const nodeTypes = { step: StepNode }

// Renders `match` the way the file wrote it: an event type, and the narrowing
// conditions under it. This is the edge's whole reason to exist.
function label(on) {
  if (!on) return ''
  const parts = [on.event]
  if (on.where) {
    for (const [k, v] of Object.entries(on.where)) parts.push(`${k}=${v}`)
  }
  return parts.join(' · ')
}

function graph(chain) {
  const nodes = []
  const edges = []
  const X = 250
  nodes.push({
    id: 'trigger',
    type: 'step',
    position: { x: 0, y: 0 },
    data: { job: chain.trigger?.job ?? '(no trigger yet)', run: chain.trigger, trigger: true },
  })
  let prev = 'trigger'
  chain.steps.forEach((st, i) => {
    const id = `s${st.step}`
    nodes.push({
      id,
      type: 'step',
      position: { x: X * (i + 1), y: 0 },
      data: { job: st.job, step: st.step, run: st.run },
    })
    edges.push({
      id: `e${id}`,
      source: prev,
      target: id,
      label: label(st.on),
      animated: st.run?.status === 'running',
      style: { stroke: '#39404f' },
      labelStyle: { fill: '#8b93a5', fontSize: 10.5, fontFamily: 'ui-monospace, monospace' },
      labelBgStyle: { fill: '#161920' },
      labelBgPadding: [5, 3],
      labelBgBorderRadius: 3,
    })
    prev = id
  })
  return { nodes, edges }
}

function ChainFlow({ chain, onPick }) {
  const { nodes, edges } = React.useMemo(() => graph(chain), [chain])
  return (
    <ReactFlow
      nodes={nodes}
      edges={edges}
      nodeTypes={nodeTypes}
      fitView
      fitViewOptions={{ padding: 0.25 }}
      proOptions={{ hideAttribution: true }}
      onNodeClick={(_, n) => n.data.run && onPick(n.data.run.id)}
      nodesDraggable={false}
      style={{ background: '#0f1115' }}
    >
      <Background color="#262b36" gap={18} />
      <Controls showInteractive={false} />
    </ReactFlow>
  )
}

export default function Chains({ onPick }) {
  const state = usePoll(api.chains, [], 4000)
  const [sel, setSel] = React.useState(null)

  return (
    <Load state={state} empty={(d) => (d.chains?.length ? null : 'no chains defined')}>
      {(d) => {
        const chains = d.chains
        const chain = chains.find((c) => c.name === sel) || chains[0]
        return (
          <>
            <div className="head">
              <h1>{chain.name}</h1>
              <p>
                {chain.description} · <span className="mono">{chain.file_path}</span> ·{' '}
                <span className="pill">{chain.state}</span>
                {chain.duration_ns ? ` · last pass ${dur(chain.duration_ns)}` : ''}
              </p>
              {chains.length > 1 && (
                <div className="tabs" style={{ marginTop: 10, borderBottom: 0, background: 'none', padding: 0 }}>
                  {chains.map((c) => (
                    <button key={c.name} className={c === chain ? 'on' : ''} onClick={() => setSel(c.name)}>
                      {c.name}
                    </button>
                  ))}
                </div>
              )}
            </div>
            <div className="body">
              <div className="canvas">
                <ChainFlow chain={chain} onPick={onPick} />
              </div>
            </div>
          </>
        )
      }}
    </Load>
  )
}
