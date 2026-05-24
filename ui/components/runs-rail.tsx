'use client'
import { useState } from 'react'
import { Run, mapStatus, runTitle, runWhen } from '@/lib/types'

const FILTERS = [
  { id: 'all',    label: 'All' },
  { id: 'live',   label: 'Live' },
  { id: 'fail',   label: 'Failing' },
  { id: 'pass',   label: 'Passing' },
  { id: 'quar',   label: 'Cancelled' },
]

const WEBHOOK_SOURCE_LABELS: Record<string, string> = {
  gha: 'Triggered by GitHub Actions',
}

function webhookSourceTitle(s: string): string {
  return WEBHOOK_SOURCE_LABELS[s.toLowerCase()] ?? `Triggered by ${s}`
}

export function RunsRail({ runs, activeId, onPick }: {
  runs: Run[]
  activeId: string | null
  onPick: (id: string) => void
}) {
  const [filter, setFilter] = useState('all')
  const shown = filter === 'all'
    ? runs
    : runs.filter(r => mapStatus(r.status) === filter)

  return (
    <aside className="rail">
      <div className="rail-hd">
        <h3>Runs</h3>
        <span className="count">{runs.length}</span>
      </div>
      <div className="rail-filters">
        {FILTERS.map(f => (
          <button key={f.id}
                  className={'rail-filter' + (filter === f.id ? ' on' : '')}
                  onClick={() => setFilter(f.id)}>
            {f.label}
          </button>
        ))}
      </div>
      <div className="rail-list">
        {shown.map(r => {
          const ds = mapStatus(r.status)
          const isRerun = r.id !== r.root_run_id
          return (
            <div key={r.id}
                 className={'rail-row' + (r.id === activeId ? ' on' : '') + (isRerun ? ' rerun' : '')}
                 onClick={() => onPick(r.id)}>
              <span className={`sdot ${ds}`} />
              {isRerun && (
                <span className="rerun-indicator" title={`Rerun of ${r.root_run_id.slice(0, 8)}`}>↻</span>
              )}
              <div className="meta">
                <div className="title">
                  {runTitle(r)}
                  {r.has_agent_activity && (
                    <span title="Agent has analysed this run"
                          style={{ marginLeft: 5, fontSize: 10, opacity: 0.6, verticalAlign: 'middle' }}>✦</span>
                  )}
                  {r.status === 'failed' && !r.auto_triaged && (
                    <span title="Auto-triage pending"
                          style={{ marginLeft: 5, fontSize: 9, opacity: 0.55, verticalAlign: 'middle', fontFamily: 'var(--font-mono)' }}>⏳</span>
                  )}
                </div>
                <div className="sub">
                  {r.id.slice(0, 8)} · {r.branch || 'main'}
                  {r.script_name && <> · {r.script_name}</>}
                  {r.env_name && <> · <span className="env-badge">{r.env_slug}</span></>}
                  {r.webhook_source && (
                    <> · <span title={webhookSourceTitle(r.webhook_source)}
                                style={{ fontFamily: 'var(--font-mono)', fontSize: 9, background: 'var(--surface-sunken)', color: 'var(--text-3)', padding: '1px 4px', borderRadius: 3 }}>{r.webhook_source.toUpperCase()}</span></>
                  )}
                </div>
              </div>
              <div className="when">{runWhen(r)}</div>
            </div>
          )
        })}
        {shown.length === 0 && (
          <div style={{ padding: '32px 12px', textAlign: 'center', color: 'var(--text-3)', fontSize: 12 }}>
            No runs match this filter.
          </div>
        )}
        {runs.length === 0 && filter === 'all' && (
          <div style={{ padding: '32px 12px', textAlign: 'center', color: 'var(--text-3)', fontSize: 12 }}>
            No runs yet. Trigger one to get started.
          </div>
        )}
      </div>
    </aside>
  )
}
