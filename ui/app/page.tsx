'use client'
import { useState, useEffect, useCallback, useRef } from 'react'
import { Run } from '@/lib/types'
import { RunsRail }          from '@/components/runs-rail'
import { RunDetail }         from '@/components/run-detail'
import { AgentPanel }        from '@/components/agent-panel'
import { Workspace }         from '@/components/workspace'
import { CommandPalette }    from '@/components/cmdk'
import { IntegrationsSheet }    from '@/components/integrations'
import { ScriptsSheet }         from '@/components/scripts-sheet'
import { EnvironmentsSheet }    from '@/components/environments-sheet'
import { TriggerModal }         from '@/components/trigger-modal'
import { Icons }             from '@/components/icons'
import { useMqttTopic }      from '@/lib/use-mqtt'
import { useWorkspace }      from '@/lib/use-workspace'
import { useAuthGuard }      from '@/lib/auth'
import { AccountMenu }       from '@/components/account-menu'

// pgmqtt run-change event shape, as configured by migration 024.
// Mirrors the Run interface but only contains columns pgmqtt can see; fields
// like script_name / env_name aren't on the row so we leave them untouched
// when patching, and re-fetch the full list when a new run id appears.
interface RunChangedEvent {
  op: 'insert' | 'update' | 'delete'
  id: string
  workspace_id: string
  project_id: string
  root_run_id: string
  branch: string
  commit_sha: string
  repo: string | null
  status: Run['status']
  auto_triaged: boolean
  test_filter: string
  script_id: string | null
  env_id: string | null
  webhook_source: string | null
  started_at: string | null
  finished_at: string | null
  created_at: string
}

// patchRunFromEvent merges an MQTT row-change payload into an existing Run.
// Fields populated only by the API JOIN (script_name, env_name, env_slug,
// has_agent_activity) are preserved from the prior state — the event payload
// can't see them.
function patchRunFromEvent(prev: Run, ev: RunChangedEvent): Run {
  return {
    ...prev,
    branch:         ev.branch,
    commit_sha:     ev.commit_sha,
    repo:           ev.repo,
    status:         ev.status,
    auto_triaged:   ev.auto_triaged,
    test_filter:    ev.test_filter,
    script_id:      ev.script_id,
    env_id:         ev.env_id,
    webhook_source: ev.webhook_source,
    started_at:     ev.started_at,
    finished_at:    ev.finished_at,
    root_run_id:    ev.root_run_id,
  }
}

export default function Page() {
  // Auth gate runs before anything else — until /api/auth/me resolves we
  // don't render the shell, so a 401 doesn't briefly flash the UI before
  // the redirect kicks in.
  const authState = useAuthGuard()
  const [runs, setRuns]               = useState<Run[]>([])
  // Honour a ?run=<id> deep link on first load (e.g. the link in a Replay PR
  // comment) so the referenced run opens directly.
  const [selectedId, setSelectedId]   = useState<string | null>(
    () => (typeof window === 'undefined' ? null : new URLSearchParams(window.location.search).get('run')),
  )
  const [selectedRun, setSelectedRun] = useState<Run | null>(null)
  const [cmdkOpen, setCmdkOpen]         = useState(false)
  const [intOpen, setIntOpen]           = useState(false)
  const [scriptsOpen, setScriptsOpen]   = useState(false)
  const [envsOpen, setEnvsOpen]         = useState(false)
  const [triggerOpen, setTriggerOpen]   = useState(false)
  const [toast, setToast]               = useState<string | null>(null)
  const [agentAsk, setAgentAsk]         = useState<string>('')
  const workspace = useWorkspace()

  // Fetch runs list. MQTT drives most updates now; the 30s interval below is
  // a backstop for the initial load and bridges any disconnects in the WS.
  const fetchRuns = useCallback(async () => {
    try {
      const res  = await fetch('/api/runs', { cache: 'no-store' })
      const data = await res.json()
      if (Array.isArray(data)) {
        setRuns(data)
        setSelectedId(prev => prev ?? data[0]?.id ?? null)
      }
    } catch (e) {
      console.error('Failed to fetch runs', e)
    }
  }, [])

  useEffect(() => {
    fetchRuns()
    // 2-minute backstop: MQTT is the primary signal, this catches missed
    // events after a reconnect or a tab woken from background. Anything
    // shorter just wastes API calls.
    const interval = setInterval(fetchRuns, 120_000)
    return () => clearInterval(interval)
  }, [fetchRuns])

  // Live run-list updates from MQTT. Each event either patches an existing
  // run in place (UPDATE) or triggers a full list refetch (INSERT — we can't
  // synthesise script_name / env_name from the payload alone).
  useMqttTopic<RunChangedEvent>(
    workspace ? `runs/${workspace.id}/+/changed` : null,
    useCallback((_topic, ev) => {
      if (!ev || ev.op === 'delete') return
      setRuns(prev => {
        const idx = prev.findIndex(r => r.id === ev.id)
        if (idx === -1) {
          // New run we don't know about yet — payload lacks joined fields, so
          // fetch the canonical list rather than synthesising a half-Run.
          fetchRuns()
          return prev
        }
        const next = prev.slice()
        next[idx] = patchRunFromEvent(next[idx], ev)
        return next
      })
      // Active-run detail: refresh it so results/artifacts stay current. We
      // could subscribe to run_results MQTT too, but a single fetch covers
      // results + step rows + presigned URLs in one round-trip.
      setSelectedId(curr => {
        if (curr === ev.id) {
          fetch(`/api/runs/${ev.id}`, { cache: 'no-store' })
            .then(r => r.json()).then(setSelectedRun).catch(() => {})
        }
        return curr
      })
    }, [fetchRuns]),
  )

  // Keep ?run=<id> in the address bar in sync with the selection so the URL is
  // shareable and matches the deep link used in PR comments.
  useEffect(() => {
    if (typeof window === 'undefined' || !selectedId) return
    const u = new URL(window.location.href)
    if (u.searchParams.get('run') !== selectedId) {
      u.searchParams.set('run', selectedId)
      window.history.replaceState(null, '', u)
    }
  }, [selectedId])

  // Fetch detail whenever selectedId changes
  useEffect(() => {
    if (!selectedId) { setSelectedRun(null); return }
    fetch(`/api/runs/${selectedId}`, { cache: 'no-store' })
      .then(r => r.json())
      .then(data => setSelectedRun(data))
      .catch(e => console.error('Failed to fetch run detail', e))
  }, [selectedId])

  // Live mid-execution result updates: every run_results change for the active
  // run triggers a detail refetch. This is what makes the per-test status pop
  // in as Playwright completes each spec.
  useMqttTopic(
    workspace && selectedId ? `runs/${workspace.id}/${selectedId}/result/changed` : null,
    useCallback(() => {
      if (!selectedId) return
      fetch(`/api/runs/${selectedId}`, { cache: 'no-store' })
        .then(r => r.json())
        .then(setSelectedRun)
        .catch(() => {})
    }, [selectedId]),
  )

  // 2-minute backstop for the active run — same reasoning as the list
  // heartbeat above. MQTT updates do the live work; this catches reconnect
  // edge cases.
  useEffect(() => {
    if (!selectedId) return
    const interval = setInterval(() => {
      fetch(`/api/runs/${selectedId}`, { cache: 'no-store' })
        .then(r => r.json())
        .then(data => setSelectedRun(data))
        .catch(() => {})
    }, 120_000)
    return () => clearInterval(interval)
  }, [selectedId])

  // ⌘K keyboard shortcut
  useEffect(() => {
    const h = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault(); setCmdkOpen(o => !o)
      } else if (e.key === 'Escape') {
        setCmdkOpen(false)
      }
    }
    window.addEventListener('keydown', h)
    return () => window.removeEventListener('keydown', h)
  }, [])

  // Toast lifecycle uses a ref so consecutive calls cancel any pending dismissal
  // — otherwise a stale setTimeout from an earlier toast can clear a newer one
  // before its full 2.4s window.
  const toastTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const flashToast = (msg: string) => {
    if (toastTimerRef.current) clearTimeout(toastTimerRef.current)
    setToast(msg)
    toastTimerRef.current = setTimeout(() => {
      setToast(null)
      toastTimerRef.current = null
    }, 2400)
  }

  const triggerRun = () => setTriggerOpen(true)

  const rerunSelected = useCallback(async () => {
    if (!selectedRun) return
    const res = await fetch('/api/runs', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        project_id:  selectedRun.project_id,
        root_run_id: selectedRun.root_run_id,
        branch:      selectedRun.branch,
        commit_sha:  selectedRun.commit_sha,
        repo:        selectedRun.repo ?? undefined,
        script_id:   selectedRun.script_id ?? undefined,
        env_id:      selectedRun.env_id ?? undefined,
      }),
    })
    if (res.ok) {
      const data = await res.json() as { run_id: string }
      flashToast('Rerun queued')
      await fetchRuns()
      setSelectedId(data.run_id)
    } else {
      flashToast('Rerun failed')
    }
  }, [selectedRun, fetchRuns])

  const cancelSelected = useCallback(async () => {
    if (!selectedRun) return
    const res = await fetch(`/api/runs/${selectedRun.id}/cancel`, { method: 'POST' })
    if (res.ok) {
      flashToast('Run cancelled')
      await fetchRuns()
    } else {
      const msg = res.status === 409 ? 'Run already finished' : 'Cancel failed'
      flashToast(msg)
    }
  }, [selectedRun, fetchRuns])

  const onRunTriggered = useCallback(async (runId: string) => {
    flashToast('Run triggered')
    await fetchRuns()
    setSelectedId(runId)
  }, [fetchRuns])

  // Breadcrumb shows the workspace slug (typically the org name in cloud mode,
  // 'default' for single-tenant). Falls back so we render something during the
  // workspace fetch.
  const project = workspace?.slug ?? 'replay'

  // Held until the auth probe completes. authState === null means the call
  // is still inflight; if it 401s the guard hook already redirected so we
  // never reach this branch with authed=false.
  if (!authState) return null

  return (
    <div className="shell">
      {/* Top bar */}
      <header className="topbar">
        <div className="tb-brand">
          <div className="logo">R</div>
          replay
        </div>
        <div className="tb-sep" />
        <div className="tb-crumb">
          <span>{project}</span>
          <Icons.chevR />
          <span className="here">runs</span>
          {selectedRun && (
            <>
              <Icons.chevR />
              <span className="here run-id">{selectedRun.id.slice(0, 8)}</span>
            </>
          )}
        </div>
        <div className="tb-spacer" />
        <div className="cmdk-pill" onClick={() => setCmdkOpen(true)}>
          <Icons.search />
          <span>Search · trigger · ask agent</span>
          <span className="kbd"><kbd>⌘</kbd><kbd>K</kbd></span>
        </div>
        <button className="btn ghost sm" onClick={() => setScriptsOpen(true)}>
          <Icons.flask /> Scripts
        </button>
        <button className="btn ghost sm" onClick={() => setEnvsOpen(true)}>
          <Icons.globe /> Environments
        </button>
        <button className="btn ghost sm" onClick={() => setIntOpen(true)}>
          <Icons.settings /> Settings
        </button>
        <button className="btn primary sm" onClick={triggerRun}>
          <Icons.zap /> Trigger run
        </button>
        <AccountMenu />
      </header>

      {/* Workspace */}
      <Workspace
        rail={<RunsRail runs={runs} activeId={selectedId} onPick={setSelectedId} />}
        detail={
          <RunDetail
            run={selectedRun}
            onRerun={selectedRun ? rerunSelected : undefined}
            onCancel={selectedRun ? cancelSelected : undefined}
            onTrigger={triggerRun}
            noRunsYet={runs.length === 0}
          />
        }
        agent={
          <AgentPanel
            run={selectedRun}
            pendingAsk={agentAsk}
            onPendingAskConsumed={() => setAgentAsk('')}
          />
        }
      />


      {/* Overlays */}
      {cmdkOpen && (
        <CommandPalette
          runs={runs}
          onPick={id => { setSelectedId(id); setCmdkOpen(false) }}
          onClose={() => setCmdkOpen(false)}
          onTrigger={triggerRun}
          onAsk={q => { setAgentAsk(q); setCmdkOpen(false) }}
        />
      )}
      {intOpen     && <IntegrationsSheet onClose={() => setIntOpen(false)} />}
      {scriptsOpen && <ScriptsSheet onClose={() => setScriptsOpen(false)} />}
      {envsOpen    && <EnvironmentsSheet onClose={() => setEnvsOpen(false)} />}
      {triggerOpen && <TriggerModal onClose={() => setTriggerOpen(false)} onTriggered={onRunTriggered} />}
      {toast && <div className="toast">{toast}</div>}
    </div>
  )
}
