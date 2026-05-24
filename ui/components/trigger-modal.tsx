'use client'
import { useState, useEffect } from 'react'
import { Script, Environment } from '@/lib/types'
import { Icons } from './icons'

interface TriggerModalProps {
  onClose: () => void
  onTriggered: (runId: string) => void
}

const TRIGGER_LAST_BRANCH_KEY = 'replay-trigger-last-branch-v1'

export function TriggerModal({ onClose, onTriggered }: TriggerModalProps) {
  const [scripts, setScripts]       = useState<Script[]>([])
  const [envs, setEnvs]             = useState<Environment[]>([])
  const [scriptId, setScriptId]     = useState('')
  const [envId, setEnvId]           = useState('')
  const [branch, setBranch]         = useState(() => {
    if (typeof window === 'undefined') return 'main'
    return window.localStorage.getItem(TRIGGER_LAST_BRANCH_KEY) ?? 'main'
  })
  const [submitting, setSubmitting] = useState(false)
  const [error, setError]           = useState('')

  useEffect(() => {
    Promise.all([
      fetch('/api/scripts').then(r => r.json()),
      fetch('/api/environments').then(r => r.json()),
    ]).then(([s, e]) => {
      if (Array.isArray(s)) { setScripts(s); if (s.length > 0) setScriptId(s[0].id) }
      if (Array.isArray(e)) { setEnvs(e); if (e.length > 0) setEnvId(e[0].id) }
    })
  }, [])

  const trigger = async () => {
    setSubmitting(true)
    setError('')
    try {
      const selected = scripts.find(s => s.id === scriptId)
      const res = await fetch('/api/runs', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          branch,
          commit_sha: 'local',
          script_id: scriptId || undefined,
          env_id: envId || undefined,
          test_filter: selected?.name ?? '',
        }),
      })
      if (!res.ok) { setError('Failed to trigger run'); return }
      const { run_id } = await res.json()
      try { window.localStorage.setItem(TRIGGER_LAST_BRANCH_KEY, branch) } catch {}
      onTriggered(run_id)
      onClose()
    } catch {
      setError('Network error')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="cmdk-mask" onClick={e => { if (e.target === e.currentTarget) onClose() }}>
      <div className="cmdk-modal" style={{ width: 'min(420px, calc(100vw - 32px))' }}>
        <div className="cmdk-input-row" style={{ padding: '14px 18px' }}>
          <Icons.zap />
          <div style={{ flex: 1, fontWeight: 600, fontSize: 14 }}>Trigger run</div>
          <button className="icon-btn" onClick={onClose}><Icons.x /></button>
        </div>

        <div style={{ padding: '14px 18px', display: 'flex', flexDirection: 'column', gap: 12 }}>
          <div className="trigger-field">
            <label>Script</label>
            {scripts.length === 0 ? (
              <div style={{ fontSize: 12.5, color: 'var(--text-3)', padding: '8px 10px', background: 'var(--surface-2)', borderRadius: 7 }}>
                No scripts yet — <button className="inline-link" onClick={onClose}>create one first</button>
              </div>
            ) : (
              <select className="scripts-field" value={scriptId} onChange={e => setScriptId(e.target.value)}>
                {scripts.map(s => (
                  <option key={s.id} value={s.id}>{s.name} — {s.filename}</option>
                ))}
              </select>
            )}
          </div>

          <div className="trigger-field">
            <label>Environment</label>
            <select className="scripts-field" value={envId} onChange={e => setEnvId(e.target.value)}>
              <option value="">— none —</option>
              {envs.map(e => (
                <option key={e.id} value={e.id}>{e.name}</option>
              ))}
            </select>
          </div>

          <div className="trigger-field">
            <label>Branch</label>
            <input className="scripts-field" value={branch} onChange={e => setBranch(e.target.value)} placeholder="main" />
            <span style={{ fontSize: 10.5, color: 'var(--text-3)' }}>
              Metadata only — no checkout. The runner executes against your local Playwright project.
            </span>
          </div>

          {error && <div style={{ fontSize: 12, color: 'var(--status-fail)' }}>{error}</div>}
        </div>

        <div style={{ padding: '0 18px 16px', display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
          <button className="btn sm" onClick={onClose}>Cancel</button>
          <button className="btn primary sm" onClick={trigger} disabled={submitting || scripts.length === 0}>
            {submitting ? 'Triggering…' : <><Icons.zap /> Trigger</>}
          </button>
        </div>
      </div>
    </div>
  )
}
