'use client'
import { useState, useRef, useEffect, useCallback } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { Run, runTitle } from '@/lib/types'
import { authedFetch } from '@/lib/auth'
import { Icons } from './icons'

// ─── Event model ──────────────────────────────────────────────────────

type ChatEvent =
  | { kind: 'text';     id: string; who: 'user' | 'agent'; text: string; streaming?: boolean; autoTriggered?: boolean }
  | { kind: 'thinking'; id: string; text: string; streaming?: boolean }
  | { kind: 'tool';     id: string; toolUseId: string; name: string; input: unknown; result?: string; isError?: boolean; pending?: boolean }
  | { kind: 'patch';    id: string; patchId: string; scriptId: string; summary: string; status: 'pending' | 'applied' | 'rejected' | 'stale' }

function uid(prefix: string) {
  return `${prefix}-${Math.random().toString(36).slice(2, 9)}-${Date.now().toString(36)}`
}

// ─── Hydration from persisted DB rows ─────────────────────────────────

interface PersistedRow {
  id: string
  who: 'user' | 'agent' | 'system'
  kind: 'chat' | 'tool_call' | 'tool_result'
  source?: string | null
  content: string
}

interface PatchInfo {
  id: string
  status: string
}

// patchStatuses maps patch_id → current DB status, used to override the stored status
// (which is always 'pending' at write time) when re-hydrating chat history.
function hydrateEvents(rows: PersistedRow[], patchStatuses: Record<string, string> = {}): ChatEvent[] {
  const events: ChatEvent[] = []
  const toolByUseId: Record<string, Extract<ChatEvent, { kind: 'tool' }>> = {}

  for (const row of rows) {
    if (row.kind === 'chat') {
      if (row.who !== 'user' && row.who !== 'agent') continue
      if (!row.content) continue
      events.push({
        kind: 'text', id: row.id, who: row.who, text: row.content,
        autoTriggered: row.source === 'auto_triage',
      })
      continue
    }
    if (row.kind === 'tool_call') {
      try {
        const p = JSON.parse(row.content) as { id: string; name: string; input: unknown }
        const ev: Extract<ChatEvent, { kind: 'tool' }> = {
          kind: 'tool', id: row.id, toolUseId: p.id, name: p.name, input: p.input,
        }
        events.push(ev)
        toolByUseId[p.id] = ev
      } catch { /* ignore */ }
      continue
    }
    if (row.kind === 'tool_result') {
      try {
        const p = JSON.parse(row.content) as { tool_use_id: string; name: string; content: string; is_error?: boolean }
        const target = toolByUseId[p.tool_use_id]
        if (target) {
          target.result = p.content
          target.isError = p.is_error
        }
        if ((p.name === 'propose_script_patch' || p.name === 'propose_script_edit') && !p.is_error) {
          try {
            const parsed = JSON.parse(p.content || '{}') as { patch_id?: string; script_id?: string; summary?: string; status?: string }
            if (parsed.patch_id) {
              events.push({
                kind: 'patch', id: row.id,
                patchId: parsed.patch_id, scriptId: parsed.script_id || '',
                summary: parsed.summary || 'Script patch proposed',
                // Prefer live status from DB over the stored snapshot (which is always 'pending').
                status: (patchStatuses[parsed.patch_id] ?? parsed.status ?? 'pending') as 'pending' | 'applied' | 'rejected' | 'stale',
              })
            }
          } catch { /* ignore */ }
        }
      } catch { /* ignore */ }
      continue
    }
  }
  return events
}

// ─── SSE stream parser ─────────────────────────────────────────────────
// Shared by both the interactive chat POST and the live auto-triage GET.

interface SSEHandlers {
  onDelta:        (text: string) => void
  onThinkingDelta:(text: string) => void
  onToolCall:     (id: string, name: string, input: unknown) => void
  onToolResult:   (toolUseId: string, name: string, content: string, isError: boolean) => void
  onDone:         () => void
  onError:        (msg: string) => void
}

async function consumeSSEStream(body: ReadableStream<Uint8Array>, signal: AbortSignal, h: SSEHandlers) {
  const reader  = body.getReader()
  const decoder = new TextDecoder()
  let buf = ''
  try {
    while (true) {
      if (signal.aborted) break
      const { value, done } = await reader.read()
      if (done) break
      buf += decoder.decode(value, { stream: true })
      let idx
      while ((idx = buf.indexOf('\n\n')) >= 0) {
        const frame = buf.slice(0, idx); buf = buf.slice(idx + 2)
        let event = 'message', data = ''
        for (const ln of frame.split('\n')) {
          if (ln.startsWith('event:')) event = ln.slice(6).trim()
          else if (ln.startsWith('data:')) data += ln.slice(5).trim()
        }
        let payload: Record<string, unknown> = {}
        try { payload = JSON.parse(data || '{}') } catch { continue }

        if      (event === 'delta'          && typeof payload.text === 'string') h.onDelta(payload.text)
        else if (event === 'thinking_delta' && typeof payload.text === 'string') h.onThinkingDelta(payload.text)
        else if (event === 'tool_call')   h.onToolCall(String(payload.id), String(payload.name), payload.input)
        else if (event === 'tool_result') h.onToolResult(String(payload.tool_use_id), String(payload.name), String(payload.content ?? ''), Boolean(payload.is_error))
        else if (event === 'done')  { h.onDone(); return }
        else if (event === 'error') h.onError(typeof payload.error === 'string' ? payload.error : 'agent error')
      }
    }
  } finally {
    reader.releaseLock()
  }
}

// ─── UI atoms ─────────────────────────────────────────────────────────

function PatchBanner({ patchId, summary, status, onChange }: {
  patchId: string, summary: string, status: string, onChange: (status: string) => void
}) {
  const [busy, setBusy] = useState(false)
  const apply = async () => {
    setBusy(true)
    try {
      const res = await authedFetch(`/api/script-patches/${patchId}/apply`, { method: 'POST' })
      if (res.ok) onChange('applied')
      else onChange(res.status === 409 ? 'stale' : status)
    } finally { setBusy(false) }
  }
  const reject = async () => {
    setBusy(true)
    try {
      const res = await authedFetch(`/api/script-patches/${patchId}/reject`, { method: 'POST' })
      if (res.ok) onChange('rejected')
    } finally { setBusy(false) }
  }
  return (
    <div className="patch-banner" data-status={status}>
      <span className="pb-dot" />
      <span className="pb-summary">{summary}</span>
      <span className="pb-spacer" />
      {status === 'pending' && <>
        <button onClick={reject} disabled={busy}>Reject</button>
        <button className="primary" onClick={apply} disabled={busy}>Apply</button>
      </>}
      {status === 'applied'  && <span style={{ fontSize: 11, color: 'var(--status-pass)' }}>applied</span>}
      {status === 'rejected' && <span style={{ fontSize: 11, color: 'var(--text-3)' }}>rejected</span>}
      {status === 'stale'    && <span style={{ fontSize: 11, color: 'var(--status-flaky)' }}>stale — script changed</span>}
    </div>
  )
}

function ToolPill({ ev }: { ev: Extract<ChatEvent, { kind: 'tool' }> }) {
  const mutating = ev.name === 'propose_script_patch' || ev.name === 'propose_script_edit' || ev.name === 'rerun_run'
  const state = ev.pending ? 'run' : ev.isError ? 'err' : mutating ? 'mut' : 'ok'
  const inputText = typeof ev.input === 'string' ? ev.input : JSON.stringify(ev.input, null, 2)
  const inputPreview = inputText.replace(/\s+/g, ' ').trim()
  const statusLabel = state === 'run' ? 'running' : state === 'err' ? 'error' : 'done'

  let imageDataUrl: string | null = null
  if (ev.name === 'read_image' && ev.result && !ev.isError) {
    try {
      const parsed = JSON.parse(ev.result) as { base64?: string; media_type?: string }
      if (parsed.base64 && parsed.media_type) {
        imageDataUrl = `data:${parsed.media_type};base64,${parsed.base64}`
      }
    } catch { /* ignore */ }
  }

  return (
    <details className="tool-pill" data-state={state}>
      <summary>
        <span className="tp-chev">▶</span>
        <span className="tp-icon" />
        <span className="tp-name">{ev.name}</span>
        <span className="tp-preview">{inputPreview}</span>
        <span className="tp-status">{statusLabel}</span>
      </summary>
      <div className="tp-body">
        {inputText && <>
          <div className="tp-label">input</div>
          <pre>{inputText}</pre>
        </>}
        {ev.result && <>
          {imageDataUrl ? (
            <>
              <div className="tp-label">image</div>
              <img src={imageDataUrl} style={{ maxWidth: '100%', maxHeight: '400px' }} alt="tool result" />
            </>
          ) : (
            <>
              <div className="tp-label">{ev.isError ? 'error' : 'result'}</div>
              <pre>{ev.result}</pre>
            </>
          )}
        </>}
      </div>
    </details>
  )
}

function ThinkingPane({ ev }: { ev: Extract<ChatEvent, { kind: 'thinking' }> }) {
  return (
    <details className="thinking-pane" data-streaming={ev.streaming ? '1' : '0'} open={ev.streaming}>
      <summary>
        <span className="th-chev">▶</span>
        <span className="th-icon" />
        <span className="th-title">{ev.streaming ? 'thinking…' : 'thinking'}</span>
        <span className="th-len">{ev.text.length} chars</span>
      </summary>
      <pre className="th-body">{ev.text}</pre>
    </details>
  )
}

function TextBubble({ ev }: { ev: Extract<ChatEvent, { kind: 'text' }> }) {
  return (
    <div className={`msg ${ev.who}`}>
      {ev.autoTriggered && ev.who === 'user' && (
        <div style={{ fontSize: 10, color: 'var(--text-3)', marginBottom: 3, fontFamily: 'var(--font-mono)', display: 'flex', alignItems: 'center', gap: 4 }}>
          <Icons.sparkle />
          auto-triage
        </div>
      )}
      <div className="bubble">
        {ev.who === 'agent'
          ? <div className="md"><ReactMarkdown remarkPlugins={[remarkGfm]}>{ev.text}</ReactMarkdown></div>
          : ev.text}
      </div>
    </div>
  )
}

// ─── Main component ───────────────────────────────────────────────────

interface ConversationRun {
  id: string
  status: string
  created_at: string
}

interface ConversationInfo {
  root_run_id: string
  runs: ConversationRun[]
}

export function AgentPanel({ run, pendingAsk, onPendingAskConsumed }: {
  run: Run | null
  pendingAsk?: string
  onPendingAskConsumed?: () => void
}) {
  const [events, setEvents]   = useState<ChatEvent[]>([])
  const [draft, setDraft]     = useState('')
  const [sending, setSending] = useState(false)
  // True while the initial history/conversation fetch for a run is in flight.
  // The composer is disabled during this window so a message can't be sent
  // before the session is ready (which used to 401).
  const [loading, setLoading] = useState(false)
  const [liveActive, setLiveActive] = useState(false) // auto-triage stream in progress
  const [error, setError]     = useState<string | null>(null)
  const [convo, setConvo]     = useState<ConversationInfo | null>(null)
  const scrollRef             = useRef<HTMLDivElement>(null)
  // Kept in a ref so the idle-poll applyRows closure always reads the latest known
  // patch statuses without triggering re-renders or stale-closure bugs.
  const patchStatusesRef      = useRef<Record<string, string>>({})
  // Tracks whether a user chat POST is in flight. Set synchronously in send() so
  // the live-stream finally block can see it immediately (React state updates are
  // async and would arrive too late to prevent a concurrent applyRows stomp).
  const sendingRef            = useRef(false)

  // Pre-fill draft when a pending ask comes in from the command palette
  useEffect(() => {
    if (pendingAsk) {
      setDraft(pendingAsk)
      onPendingAskConsumed?.()
    }
  }, [pendingAsk, onPendingAskConsumed])

  // Load persisted messages from DB
  const loadMessages = useCallback(async (runId: string): Promise<PersistedRow[]> => {
    const res = await fetch(`/api/runs/${runId}/agent/messages`)
    if (!res.ok) return []
    return res.json()
  }, [])

  const applyRows = useCallback((rows: PersistedRow[]) => {
    if (Array.isArray(rows)) setEvents(hydrateEvents(rows, patchStatusesRef.current))
  }, []) // patchStatusesRef is stable — no dep needed

  // Load history, conversation info, and current patch statuses when run changes.
  // Patch statuses come from the API rather than the stored tool_result content because
  // the stored content always reflects the status at proposal time ('pending').
  useEffect(() => {
    if (!run) { setEvents([]); setConvo(null); setLoading(false); return }
    let cancelled = false
    setError(null)
    setLoading(true)
    Promise.all([
      fetch(`/api/runs/${run.id}/agent/messages`).then(r => r.json()) as Promise<PersistedRow[]>,
      fetch(`/api/runs/${run.id}/agent/conversation`).then(r => r.json()) as Promise<ConversationInfo>,
      fetch(`/api/script-patches?run_id=${run.id}`).then(r => r.json()).catch(() => []) as Promise<PatchInfo[]>,
    ])
      .then(([rows, convInfo, patches]) => {
        if (cancelled) return
        const patchStatuses: Record<string, string> = {}
        for (const p of patches ?? []) patchStatuses[p.id] = p.status
        patchStatusesRef.current = patchStatuses
        if (Array.isArray(rows)) setEvents(hydrateEvents(rows, patchStatuses))
        if (convInfo?.runs) setConvo(convInfo)
      })
      .catch(() => { if (!cancelled) setEvents([]) })
      .finally(() => { if (!cancelled) setLoading(false) })
    return () => { cancelled = true }
  }, [run?.id])

  // Backstop poll for new messages (every 20s when idle). The live SSE
  // stream is the primary signal for auto-triage; this only fills gaps from
  // reconnects or messages written by other tabs. The `active` flag prevents
  // an in-flight fetch that started just before cleanup from calling applyRows
  // after the interval was cleared (stale-closure race).
  useEffect(() => {
    if (!run || sending || liveActive) return
    let active = true
    const interval = setInterval(async () => {
      const rows = await loadMessages(run.id).catch(() => [])
      if (active) applyRows(rows)
    }, 20_000)
    return () => { active = false; clearInterval(interval) }
  }, [run?.id, sending, liveActive, loadMessages, applyRows])

  // Subscribe to live auto-triage stream whenever the run changes
  useEffect(() => {
    if (!run) return
    const ctrl = new AbortController()
    let active = false

    const subscribe = async () => {
      try {
        const res = await fetch(`/stream/agent/${run.id}/live`, { signal: ctrl.signal })
        if (!res.ok || !res.body) return
        active = true
        setLiveActive(true)
        await consumeSSEStream(res.body, ctrl.signal, {
          onDelta:         (text) => appendDelta(text),
          onThinkingDelta: (text) => appendThinkingDelta(text),
          onToolCall:      (id, name, input) => startToolCall(id, name, input),
          onToolResult:    (id, name, content, isErr) => completeToolCall(id, name, content, isErr),
          onDone:          () => { finalizeAll(); active = false; setLiveActive(false) },
          onError:         (msg) => setError(msg),
        })
      } catch {
        // aborted or connection closed cleanly
      } finally {
        if (active) setLiveActive(false)
        // Reload messages so any events we missed while connecting are visible.
        // Skip if the user started a chat send — applyRows would stomp the
        // optimistic user message and any in-flight thinking events.
        if (!ctrl.signal.aborted) {
          loadMessages(run.id).then(rows => {
            if (!sendingRef.current) applyRows(rows)
          }).catch(() => {})
        }
      }
    }

    subscribe()
    return () => ctrl.abort()
  }, [run?.id]) // eslint-disable-line react-hooks/exhaustive-deps

  // Track whether the user is near the bottom. Only auto-scroll when they are —
  // if they've scrolled up to read history, new events should not yank them down.
  const atBottomRef = useRef(true)
  useEffect(() => {
    const el = scrollRef.current
    if (!el) return
    const onScroll = () => {
      atBottomRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80
    }
    el.addEventListener('scroll', onScroll, { passive: true })
    return () => el.removeEventListener('scroll', onScroll)
  }, [])

  // Reset to "at bottom" whenever the active run changes so the first load scrolls down.
  useEffect(() => { atBottomRef.current = true }, [run?.id])

  const prevLenRef = useRef(0)
  useEffect(() => {
    const el = scrollRef.current
    if (!el) return
    const grew = events.length > prevLenRef.current
    prevLenRef.current = events.length
    if (grew && atBottomRef.current) el.scrollTop = el.scrollHeight
  }, [events])

  // ─── Stream mutators ───────────────────────────────────────────────

  const appendDelta = (text: string) => setEvents(prev => {
    if (!text) return prev
    const next = prev.slice()
    const last = next[next.length - 1]
    if (last && last.kind === 'text' && last.who === 'agent' && last.streaming) {
      next[next.length - 1] = { ...last, text: last.text + text }
    } else {
      next.push({ kind: 'text', id: uid('t'), who: 'agent', text, streaming: true })
    }
    return next
  })

  const appendThinkingDelta = (text: string) => setEvents(prev => {
    if (!text) return prev
    const next = prev.slice()
    const last = next[next.length - 1]
    if (last && last.kind === 'thinking' && last.streaming) {
      next[next.length - 1] = { ...last, text: last.text + text }
    } else {
      next.push({ kind: 'thinking', id: uid('th'), text, streaming: true })
    }
    return next
  })

  const finalizeStreamingText = (prev: ChatEvent[]): ChatEvent[] => {
    if (!prev.length) return prev
    let changed = false
    const next = prev.map(e => {
      if ((e.kind === 'text' || e.kind === 'thinking') && e.streaming) {
        changed = true
        return { ...e, streaming: false }
      }
      return e
    })
    return changed ? next : prev
  }

  const startToolCall = (toolUseId: string, name: string, input: unknown) => setEvents(prev => {
    const next = finalizeStreamingText(prev).slice()
    next.push({ kind: 'tool', id: uid('tc'), toolUseId, name, input, pending: true })
    return next
  })

  const completeToolCall = (toolUseId: string, name: string, content: string, isError: boolean) => setEvents(prev => {
    const next = prev.slice()
    for (let i = next.length - 1; i >= 0; i--) {
      const e = next[i]
      if (e.kind === 'tool' && e.toolUseId === toolUseId) {
        next[i] = { ...e, pending: false, result: content, isError }
        break
      }
    }
    if ((name === 'propose_script_patch' || name === 'propose_script_edit') && !isError) {
      try {
        const parsed = JSON.parse(content || '{}') as { patch_id?: string; script_id?: string; summary?: string; status?: string }
        if (parsed.patch_id) {
          next.push({
            kind: 'patch', id: uid('pb'),
            patchId: parsed.patch_id, scriptId: parsed.script_id || '',
            summary: parsed.summary || 'Script patch proposed',
            status: (parsed.status as 'pending' | 'applied' | 'rejected' | 'stale') || 'pending',
          })
        }
      } catch { /* ignore */ }
    }
    return next
  })

  const finalizeAll = () => setEvents(prev => finalizeStreamingText(prev))

  const setPatchStatus = (patchId: string, status: 'pending' | 'applied' | 'rejected' | 'stale') => {
    patchStatusesRef.current = { ...patchStatusesRef.current, [patchId]: status }
    setEvents(prev => prev.map(e => (e.kind === 'patch' && e.patchId === patchId) ? { ...e, status } : e))
  }

  // ─── Send ──────────────────────────────────────────────────────────

  const send = useCallback(async () => {
    const v = draft.trim()
    if (!v || !run || sending) return
    setDraft('')
    setSending(true)
    sendingRef.current = true
    setError(null)
    setEvents(prev => [...prev, { kind: 'text', id: uid('u'), who: 'user', text: v }])

    try {
      const res = await authedFetch(`/stream/agent/${run.id}/chat`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ message: v }),
      })
      if (!res.ok || !res.body) {
        const errText = await res.text().catch(() => `HTTP ${res.status}`)
        setError(errText.slice(0, 300))
        return
      }
      const ctrl = new AbortController()
      await consumeSSEStream(res.body, ctrl.signal, {
        onDelta:         (text) => appendDelta(text),
        onThinkingDelta: (text) => appendThinkingDelta(text),
        onToolCall:      (id, name, input) => startToolCall(id, name, input),
        onToolResult:    (id, name, content, isErr) => completeToolCall(id, name, content, isErr),
        onDone:          () => finalizeAll(),
        onError:         (msg) => setError(msg),
      })
    } catch (e) {
      setError(e instanceof Error ? e.message : 'network error')
    } finally {
      setSending(false)
      sendingRef.current = false
      finalizeAll()
    }
  }, [draft, run, sending]) // eslint-disable-line react-hooks/exhaustive-deps

  const reloadHistory = useCallback(async () => {
    if (!run) return
    const rows = await loadMessages(run.id).catch(() => [])
    applyRows(rows)
  }, [run?.id, loadMessages, applyRows]) // eslint-disable-line react-hooks/exhaustive-deps

  const idleHint = run
    ? `Watching run ${run.id.slice(0, 8)} — "${runTitle(run)}". Ask anything about the script, logs, or screenshots.`
    : 'Select a run to start agent triage.'

  // `loading` blocks input while history loads; `busy` also covers active streams.
  const composerDisabled = !run || sending || liveActive || loading
  const busy = sending || liveActive
  const showThinking = busy && (() => {
    const last = events[events.length - 1]
    if (!last) return true
    if (last.kind === 'tool' && last.pending) return false
    if (last.kind === 'thinking') return false
    if (last.kind === 'text' && last.who === 'agent' && last.streaming) return false
    return true
  })()

  return (
    <aside className="agent">
      <div className="agent-hd">
        <div className="av">R</div>
        <div className="who">
          <b>Replay agent</b>
          <span>
            {run ? (liveActive ? 'triaging…' : `watching · ${run.id.slice(0, 8)}`) : 'idle'}
          </span>
          {convo && convo.runs.length > 1 && (
            <span style={{ display: 'flex', gap: 4, marginTop: 3, flexWrap: 'wrap' }}>
              {convo.runs.map(cr => {
                const ds = cr.status === 'passed' ? 'pass' : cr.status === 'failed' ? 'fail' : cr.status === 'running' || cr.status === 'queued' ? 'live' : 'quar'
                return (
                  <span key={cr.id} title={`${cr.id.slice(0, 8)} · ${cr.status}`}
                        style={{ display: 'inline-flex', alignItems: 'center', gap: 3,
                                 fontFamily: 'var(--font-mono)', fontSize: 9.5, color: 'var(--text-3)' }}>
                    <span className={`sdot ${ds}`} style={{ width: 6, height: 6 }} />
                    {cr.id.slice(0, 6)}
                  </span>
                )
              })}
            </span>
          )}
        </div>
        <div className="right">
          <button className="icon-btn" title="Refresh history" onClick={reloadHistory} disabled={!run}>
            <Icons.retry />
          </button>
        </div>
      </div>

      <div className="chat-scroll" ref={scrollRef}>
        {loading && events.length === 0 && (
          <div className="msg agent"><div className="bubble">
            <span className="agent-typing">Loading conversation…</span>
          </div></div>
        )}
        {!loading && events.length === 0 && (
          <div className="msg agent"><div className="bubble">{idleHint}</div></div>
        )}

        {events.map(ev => {
          if (ev.kind === 'text')     return <TextBubble   key={ev.id} ev={ev} />
          if (ev.kind === 'thinking') return <ThinkingPane key={ev.id} ev={ev} />
          if (ev.kind === 'tool')     return <ToolPill     key={ev.id} ev={ev} />
          if (ev.kind === 'patch') return (
            <PatchBanner key={ev.id}
              patchId={ev.patchId}
              summary={ev.summary}
              status={ev.status}
              onChange={(s) => setPatchStatus(ev.patchId, s as 'pending' | 'applied' | 'rejected' | 'stale')}
            />
          )
          return null
        })}

        {showThinking && (
          <div className="msg agent"><div className="bubble"><span className="agent-typing">
            {liveActive ? 'auto-triage running…' : 'thinking…'}
          </span></div></div>
        )}

        {error && (
          <div className="msg agent">
            <div className="bubble" style={{ borderColor: 'var(--fail)', color: 'var(--fail)' }}>
              {error}
            </div>
          </div>
        )}
      </div>

      <div className="composer" data-loading={loading ? '1' : '0'}>
        <div className="composer-box">
          <textarea
            value={draft}
            onChange={e => setDraft(e.target.value)}
            onKeyDown={e => {
              if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); send() }
            }}
            rows={2}
            disabled={composerDisabled}
            placeholder={
              !run ? 'Select a run first'
              : loading ? 'Loading…'
              : 'Ask about this run…'
            }
          />
          <div className="composer-bar">
            <span className="grow" />
            <span style={{ fontSize: 10.5, color: 'var(--text-3)', fontFamily: 'var(--font-mono)', marginRight: 6 }}>↵ send · ⇧↵ newline</span>
            <button className="send" onClick={send} disabled={!draft.trim() || composerDisabled}>
              <Icons.send />
            </button>
          </div>
        </div>
      </div>
    </aside>
  )
}
