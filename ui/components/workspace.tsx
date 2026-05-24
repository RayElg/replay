'use client'
import { useState, useEffect, useCallback, useRef, ReactNode } from 'react'

const RAIL_MIN = 180, RAIL_MAX = 480, RAIL_DEFAULT = 244
const AGENT_MIN = 260, AGENT_MAX = 720, AGENT_DEFAULT = 340
const DETAIL_MIN = 320
const STORAGE_KEY = 'replay-workspace-cols-v1'
// Below this viewport width the 3-column layout can't honour the per-column
// minimums, so we collapse the agent panel into a toggleable drawer.
const NARROW_BREAKPOINT = 1100

function readStored(): { rail: number; agent: number } {
  if (typeof window === 'undefined') return { rail: RAIL_DEFAULT, agent: AGENT_DEFAULT }
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY)
    if (!raw) return { rail: RAIL_DEFAULT, agent: AGENT_DEFAULT }
    const v = JSON.parse(raw)
    return {
      rail:  clamp(Number(v.rail)  || RAIL_DEFAULT,  RAIL_MIN,  RAIL_MAX),
      agent: clamp(Number(v.agent) || AGENT_DEFAULT, AGENT_MIN, AGENT_MAX),
    }
  } catch { return { rail: RAIL_DEFAULT, agent: AGENT_DEFAULT } }
}

const clamp = (n: number, lo: number, hi: number) => Math.max(lo, Math.min(hi, n))

export function Workspace({ rail, detail, agent }: { rail: ReactNode; detail: ReactNode; agent: ReactNode }) {
  const [railW,  setRailW]  = useState(RAIL_DEFAULT)
  const [agentW, setAgentW] = useState(AGENT_DEFAULT)
  const [active, setActive] = useState<'rail' | 'agent' | null>(null)
  const [narrow, setNarrow] = useState(false)
  const [agentOpen, setAgentOpen] = useState(true)
  const ref = useRef<HTMLDivElement>(null)

  // Hydrate from localStorage post-mount (SSR-safe)
  useEffect(() => {
    const v = readStored()
    setRailW(v.rail); setAgentW(v.agent)
  }, [])

  // Watch viewport width — collapse the agent panel below NARROW_BREAKPOINT.
  // When the viewport widens again, the panel reappears.
  useEffect(() => {
    const apply = () => setNarrow(window.innerWidth < NARROW_BREAKPOINT)
    apply()
    window.addEventListener('resize', apply)
    return () => window.removeEventListener('resize', apply)
  }, [])

  // Persist on change (debounced via rAF is overkill; localStorage write is cheap)
  useEffect(() => {
    try { window.localStorage.setItem(STORAGE_KEY, JSON.stringify({ rail: railW, agent: agentW })) } catch {}
  }, [railW, agentW])

  const startDrag = useCallback((which: 'rail' | 'agent') => (e: React.PointerEvent) => {
    e.preventDefault()
    setActive(which)
    document.body.dataset.resizing = '1'
    const startX = e.clientX
    const startRail = railW
    const startAgent = agentW
    const totalW = ref.current?.getBoundingClientRect().width ?? 1200

    const onMove = (ev: PointerEvent) => {
      const dx = ev.clientX - startX
      if (which === 'rail') {
        const max = Math.min(RAIL_MAX, totalW - agentW - DETAIL_MIN - 10)
        setRailW(clamp(startRail + dx, RAIL_MIN, max))
      } else {
        const max = Math.min(AGENT_MAX, totalW - railW - DETAIL_MIN - 10)
        setAgentW(clamp(startAgent - dx, AGENT_MIN, max))
      }
    }
    const onUp = () => {
      setActive(null)
      delete document.body.dataset.resizing
      window.removeEventListener('pointermove', onMove)
      window.removeEventListener('pointerup', onUp)
    }
    window.addEventListener('pointermove', onMove)
    window.addEventListener('pointerup',   onUp)
  }, [railW, agentW])

  // Double-click a resize handle to restore the default width for that column.
  const resetWidth = (which: 'rail' | 'agent') => () => {
    if (which === 'rail') setRailW(RAIL_DEFAULT)
    else setAgentW(AGENT_DEFAULT)
  }

  // On narrow viewports the agent panel overlays the detail rather than taking a column.
  if (narrow) {
    const cols = `${railW}px 5px minmax(0, 1fr)`
    return (
      <div className="workspace narrow" ref={ref} style={{ gridTemplateColumns: cols }}>
        {rail}
        <div className="resize-handle" data-active={active === 'rail' ? '1' : '0'}
             onPointerDown={startDrag('rail')} onDoubleClick={resetWidth('rail')}
             role="separator" aria-orientation="vertical" />
        <div className="workspace-detail-wrap">
          {detail}
          {!agentOpen && (
            <button className="agent-drawer-toggle" onClick={() => setAgentOpen(true)}>
              Agent
            </button>
          )}
          {agentOpen && (
            <div className="agent-drawer">
              <button className="agent-drawer-close" onClick={() => setAgentOpen(false)} aria-label="Close agent panel">×</button>
              {agent}
            </div>
          )}
        </div>
      </div>
    )
  }

  const cols = `${railW}px 5px minmax(0, 1fr) 5px ${agentW}px`

  return (
    <div className="workspace" ref={ref} style={{ gridTemplateColumns: cols }}>
      {rail}
      <div className="resize-handle" data-active={active === 'rail'  ? '1' : '0'}
           onPointerDown={startDrag('rail')} onDoubleClick={resetWidth('rail')}
           role="separator" aria-orientation="vertical" />
      {detail}
      <div className="resize-handle" data-active={active === 'agent' ? '1' : '0'}
           onPointerDown={startDrag('agent')} onDoubleClick={resetWidth('agent')}
           role="separator" aria-orientation="vertical" />
      {agent}
    </div>
  )
}
