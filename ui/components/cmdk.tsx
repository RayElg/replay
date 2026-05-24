'use client'
import { useState, useEffect, useRef } from 'react'
import { Run, mapStatus, runTitle, runWhen, runDuration } from '@/lib/types'
import { Icons } from './icons'

const ACTIONS = [
  { id: 'trigger',  icon: 'zap',    label: 'Trigger run',              sub: 'branch · suite · env',         hint: ['⌘', 'T'] },
  { id: 'describe', icon: 'bot',    label: 'Describe a test to write', sub: 'agent will scaffold the spec', hint: ['⌘', 'N'] },
]

export function CommandPalette({ runs, onPick, onClose, onTrigger, onAsk }: {
  runs: Run[]
  onPick: (id: string) => void
  onClose: () => void
  onTrigger: () => void
  onAsk: (q: string) => void
}) {
  const [q, setQ]         = useState('')
  const [cursor, setCursor] = useState(0)
  const inputRef          = useRef<HTMLInputElement>(null)

  useEffect(() => { inputRef.current?.focus() }, [])

  const looksLikeQuery = q.length > 3 && /\s/.test(q)

  const matches = runs.filter(r =>
    !q ||
    runTitle(r).toLowerCase().includes(q.toLowerCase()) ||
    r.id.includes(q.toLowerCase())
  ).slice(0, 6)

  const flat = [
    ...ACTIONS.map(a => ({ ...a, kind: 'action' as const })),
    ...matches.map(r => ({
      id: r.id, label: runTitle(r),
      sub: `${r.id.slice(0,8)} · ${runDuration(r)} · ${runWhen(r)}`,
      kind: 'run' as const, status: mapStatus(r.status),
      icon: undefined as undefined,
    })),
  ]

  const onKey = (e: React.KeyboardEvent) => {
    if (e.key === 'ArrowDown') { e.preventDefault(); setCursor(c => Math.min(flat.length - 1, c + 1)) }
    else if (e.key === 'ArrowUp') { e.preventDefault(); setCursor(c => Math.max(0, c - 1)) }
    else if (e.key === 'Enter') {
      e.preventDefault()
      if (looksLikeQuery) { onAsk(q); onClose(); return }
      const picked = flat[cursor]
      if (!picked) return
      if (picked.kind === 'run') { onPick(picked.id); onClose() }
      else if (picked.id === 'trigger') { onTrigger(); onClose() }
      else { onAsk(q || picked.label); onClose() }
    } else if (e.key === 'Escape') { onClose() }
  }

  const icn = (name: string | undefined, status?: string) => {
    if (!name && status) return <span className={`sdot ${status}`} />
    const C = Icons[name as keyof typeof Icons]
    return C ? <C /> : null
  }

  return (
    <div className="cmdk-mask" onClick={e => { if (e.target === e.currentTarget) onClose() }}>
      <div className="cmdk-modal">
        <div className="cmdk-input-row">
          <Icons.search />
          <input ref={inputRef} className="cmdk-input"
                 placeholder="Search runs, trigger a job, or describe a test…"
                 value={q}
                 onChange={e => { setQ(e.target.value); setCursor(0) }}
                 onKeyDown={onKey} />
          {looksLikeQuery && <span className="cmdk-tag">ask agent</span>}
        </div>

        <div className="cmdk-list">
          {looksLikeQuery && (
            <>
              <div className="cmdk-section-hd">Ask agent</div>
              <div className={'cmdk-item' + (cursor === 0 ? ' on' : '')}
                   onMouseEnter={() => setCursor(0)}
                   onClick={() => { onAsk(q); onClose() }}>
                <div className="ico" style={{ background: 'var(--accent-soft)', color: 'var(--accent)' }}>
                  <Icons.bot />
                </div>
                <div className="lbl">
                  <div>&ldquo;{q}&rdquo;</div>
                  <div className="sub">Agent will scope this & propose a plan</div>
                </div>
                <div className="hint"><kbd>↵</kbd></div>
              </div>
            </>
          )}

          {!looksLikeQuery && (
            <>
              <div className="cmdk-section-hd">Actions</div>
              {ACTIONS.map((a, i) => (
                <div key={a.id}
                     className={'cmdk-item' + (cursor === i ? ' on' : '')}
                     onMouseEnter={() => setCursor(i)}
                     onClick={() => {
                       if (a.id === 'trigger') onTrigger()
                       else onAsk(a.label)
                       onClose()
                     }}>
                  <div className="ico">{icn(a.icon)}</div>
                  <div className="lbl">
                    <div>{a.label}</div>
                    <div className="sub">{a.sub}</div>
                  </div>
                  {a.hint && (
                    <div className="hint">{a.hint.map((k, j) => <kbd key={j}>{k}</kbd>)}</div>
                  )}
                </div>
              ))}
            </>
          )}

          {matches.length > 0 && (
            <>
              <div className="cmdk-section-hd">Runs</div>
              {matches.map((r, i) => {
                const idx = (looksLikeQuery ? 1 : ACTIONS.length) + i
                return (
                  <div key={r.id}
                       className={'cmdk-item' + (cursor === idx ? ' on' : '')}
                       onMouseEnter={() => setCursor(idx)}
                       onClick={() => { onPick(r.id); onClose() }}>
                    <div className="ico">
                      <span className={`sdot ${mapStatus(r.status)}`} />
                    </div>
                    <div className="lbl">
                      <div>{runTitle(r)}</div>
                      <div className="sub">{r.id.slice(0, 8)} · {runDuration(r)} · {runWhen(r)}</div>
                    </div>
                  </div>
                )
              })}
            </>
          )}
        </div>

        <div className="cmdk-footer">
          <span><kbd>↑↓</kbd> nav</span>
          <span><kbd>↵</kbd> select</span>
          <span><kbd>esc</kbd> close</span>
          <span className="right">replay · ⌘K</span>
        </div>
      </div>
    </div>
  )
}
