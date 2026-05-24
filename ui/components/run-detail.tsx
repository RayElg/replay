'use client'
import { useState, useRef, useEffect } from 'react'
import { Run, RunResult, Artifact, Step, mapStatus, mapResultStatus, statusLabel, runTitle, runDuration, runWhen, fmtTime, triageMeta } from '@/lib/types'
import { Icons } from './icons'

export function RunDetail({ run, onRerun, onCancel, onTrigger, noRunsYet }: {
  run: Run | null
  onRerun?: () => void
  onCancel?: () => void
  onTrigger?: () => void
  noRunsYet?: boolean
}) {
  if (!run) {
    if (noRunsYet) {
      return (
        <main className="canvas">
          <div className="empty-canvas">
            <Icons.zap />
            <p>No runs yet. Trigger your first run to get started.</p>
            {onTrigger && (
              <button className="btn primary sm" onClick={onTrigger}>
                <Icons.zap /> Trigger run
              </button>
            )}
          </div>
        </main>
      )
    }
    return (
      <main className="canvas">
        <div className="empty-canvas">
          <Icons.video />
          <p>Select a run from the left to view details.</p>
        </div>
      </main>
    )
  }

  const videoArtifact = run.artifacts?.find(a => a.kind === 'video')
  // Include both the test's screenshot(s) and the video keyframes extracted by the runner.
  // Sort so the failure screenshot (if any) leads, then frames in capture order.
  const screenshots = (run.artifacts ?? [])
    .filter(a => a.kind === 'screenshot' || a.kind === 'video_frame')
    .sort((a, b) => {
      if (a.kind !== b.kind) return a.kind === 'screenshot' ? -1 : 1
      return a.storage_key.localeCompare(b.storage_key)
    })
  const results       = run.results ?? []
  const ds            = mapStatus(run.status)

  return (
    <main className="canvas">
      <RunHeader run={run} ds={ds} onRerun={onRerun} onCancel={onCancel} />
      <TriageVerdict run={run} />
      <div className="run-body">
        <VideoPlayer videoArtifact={videoArtifact} status={run.status} />
        <StepsPane results={results} screenshots={screenshots} />
      </div>
    </main>
  )
}

// TriageVerdict renders the agent's structured conclusion when one has been
// recorded (via the submit_triage_verdict tool). Absent on un-triaged runs.
function TriageVerdict({ run }: { run: Run }) {
  const cls = run.triage_classification
  if (!cls) return null
  const meta = triageMeta[cls] ?? { label: cls, tone: 'live' as const }
  return (
    <div className="triage-verdict" style={{
      display: 'flex', flexDirection: 'column', gap: 6,
      margin: '0 18px 12px', padding: '10px 12px',
      border: '1px solid var(--border)', borderRadius: 8,
      background: 'var(--surface-sunken)',
    }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
        <span style={{ fontSize: 10.5, fontWeight: 600, letterSpacing: '0.08em', textTransform: 'uppercase', color: 'var(--text-3)' }}>
          Triage verdict
        </span>
        <span className={`pill ${meta.tone}`}>{meta.label}</span>
        {run.triage_confidence && (
          <span style={{ fontSize: 11, color: 'var(--text-3)' }}>· {run.triage_confidence} confidence</span>
        )}
      </div>
      {run.triage_summary && (
        <p style={{ margin: 0, fontSize: 12.5, lineHeight: 1.5, color: 'var(--text-2)' }}>{run.triage_summary}</p>
      )}
    </div>
  )
}

function RunHeader({ run, ds, onRerun, onCancel }: { run: Run; ds: string; onRerun?: () => void; onCancel?: () => void }) {
  const [rerunBusy, setRerunBusy] = useState(false)
  const [cancelBusy, setCancelBusy] = useState(false)

  const handleRerun = async () => {
    if (!onRerun || rerunBusy) return
    setRerunBusy(true)
    try { await onRerun() } finally { setRerunBusy(false) }
  }
  const handleCancel = async () => {
    if (!onCancel || cancelBusy) return
    setCancelBusy(true)
    try { await onCancel() } finally { setCancelBusy(false) }
  }
  const inFlight = run.status === 'queued' || run.status === 'running'

  return (
    <header className="run-hd">
      <div className="meta">
        <div className="title-row">
          <span className={`sdot ${ds}`} />
          <h1>{runTitle(run)}</h1>
          <span className={`pill ${ds}`}>{statusLabel(ds as 'pass' | 'fail' | 'live' | 'quar')}</span>
        </div>
        <div className="sub-row">
          <span>{run.id.slice(0, 8)}</span>
          <span style={{ color: 'var(--text-3)' }}>·</span>
          <span><Icons.branch /> {run.branch || 'main'}</span>
          <span style={{ color: 'var(--text-3)' }}>·</span>
          <span>{run.commit_sha ? run.commit_sha.slice(0, 7) : '—'}</span>
          {run.repo && (
            <>
              <span style={{ color: 'var(--text-3)' }}>·</span>
              <span style={{ fontFamily: 'var(--font-mono)', fontSize: 11 }}>{run.repo}</span>
            </>
          )}
          {run.script_name && (
            <>
              <span style={{ color: 'var(--text-3)' }}>·</span>
              <span><Icons.flask /> {run.script_name}</span>
            </>
          )}
          {run.env_name && (
            <>
              <span style={{ color: 'var(--text-3)' }}>·</span>
              <span><Icons.globe /> <span className="env-badge">{run.env_slug}</span></span>
            </>
          )}
          <span style={{ color: 'var(--text-3)' }}>·</span>
          <span>{runDuration(run)} · {runWhen(run)}</span>
        </div>
      </div>
      <div className="actions">
        {onCancel && inFlight && (
          <button className="btn sm danger" onClick={handleCancel} disabled={cancelBusy}
                  title="Stop this run; in-flight Playwright work is discarded.">
            <Icons.x /> {cancelBusy ? 'Cancelling…' : 'Cancel'}
          </button>
        )}
        {onRerun && (
          <button className="btn sm" onClick={handleRerun} disabled={rerunBusy}>
            <Icons.retry /> {rerunBusy ? 'Queuing…' : 'Rerun'}
          </button>
        )}
      </div>
    </header>
  )
}

function VideoPlayer({ videoArtifact, status }: { videoArtifact?: Artifact; status: Run['status'] }) {
  const videoRef = useRef<HTMLVideoElement>(null)
  // Track src separately so presigned URL refreshes (every 3s poll) don't reload the video.
  // Only update src when the underlying storage_key changes (genuinely different file).
  const [src, setSrc] = useState<string | undefined>()
  const [currentTime, setCurrentTime] = useState(0)
  const [duration, setDuration] = useState(0)
  const [playing, setPlaying] = useState(false)

  useEffect(() => {
    setSrc(videoArtifact?.url)
    setCurrentTime(0)
    setDuration(0)
    setPlaying(false)
  }, [videoArtifact?.storage_key])

  const togglePlay = () => {
    const v = videoRef.current
    if (!v) return
    if (playing) { v.pause(); setPlaying(false) }
    else { v.play(); setPlaying(true) }
  }

  const onScrub = (e: React.PointerEvent<HTMLDivElement>) => {
    if (!videoRef.current || !duration) return
    const rect = e.currentTarget.getBoundingClientRect()
    const seek = (t: number) => {
      const x = Math.max(0, Math.min(1, (t - rect.left) / rect.width))
      videoRef.current!.currentTime = x * duration
    }
    seek(e.clientX)
    const onMove = (ev: PointerEvent) => seek(ev.clientX)
    const onUp   = () => { window.removeEventListener('pointermove', onMove); window.removeEventListener('pointerup', onUp) }
    window.addEventListener('pointermove', onMove)
    window.addEventListener('pointerup', onUp)
  }

  const pct = duration > 0 ? (currentTime / duration) * 100 : 0

  return (
    <div className="video-wrap">
      <div className="video-stage">
        {src ? (
          <video
            ref={videoRef}
            src={src}
            onTimeUpdate={e => setCurrentTime(e.currentTarget.currentTime)}
            onDurationChange={e => setDuration(e.currentTarget.duration)}
            onPlay={() => setPlaying(true)}
            onPause={() => setPlaying(false)}
            onEnded={() => setPlaying(false)}
            preload="metadata"
          />
        ) : (
          <div className="no-video">
            <Icons.video />
            <span>
              {status === 'running' || status === 'queued'
                ? 'Recording in progress…'
                : 'No recording available'}
            </span>
          </div>
        )}
      </div>
      <div className="video-controls">
        <button className="play-btn" onClick={togglePlay} disabled={!src}>
          {playing ? <Icons.pause /> : <Icons.play />}
        </button>
        <span className="time">{fmtTime(currentTime)}</span>
        <div className="scrub" onPointerDown={onScrub}>
          <div className="scrub-track">
            <div className="scrub-fill" style={{ width: `${pct}%` }} />
          </div>
          <div className="scrub-thumb" style={{ left: `${pct}%` }} />
        </div>
        <span className="time">{fmtTime(duration)}</span>
        <button className="icon-btn" title="Frame back"
                onClick={() => { if (videoRef.current) videoRef.current.currentTime -= 1/30 }}>
          <Icons.chevL />
        </button>
        <button className="icon-btn" title="Frame forward"
                onClick={() => { if (videoRef.current) videoRef.current.currentTime += 1/30 }}>
          <Icons.chevR />
        </button>
        <button className="icon-btn" title="Fullscreen"
                onClick={() => videoRef.current?.requestFullscreen?.()}>
          <Icons.expand />
        </button>
      </div>
    </div>
  )
}

function StepTimeline({ steps }: { steps: Step[] }) {
  const totalMS = Math.max(1, ...steps.map(s => s.start_ms + s.duration_ms))
  return (
    <div className="step-timeline">
      {steps.map(s => {
        const failed = s.status === 'failed'
        const widthPct = Math.max(1, (s.duration_ms / totalMS) * 100)
        const leftPct = (s.start_ms / totalMS) * 100
        return (
          <div key={s.idx} className={`tl-row ${failed ? 'failed' : 'passed'}`}>
            <span className="tl-idx">{s.idx.toString().padStart(2, '0')}</span>
            <span className="tl-status" />
            <span className="tl-name">{s.api_name}</span>
            <span className="tl-target">{s.selector || s.url || ''}</span>
            <span className="tl-bar"><span className="tl-bar-fill" style={{ left: `${leftPct}%`, width: `${widthPct}%` }} /></span>
            <span className="tl-dur">{s.duration_ms}ms</span>
            {failed && s.error && <div className="tl-err">{s.error}</div>}
          </div>
        )
      })}
    </div>
  )
}

function StepIcon({ status }: { status: string }) {
  if (status === 'pass')  return <span className="ico" style={{ color: 'var(--status-pass)' }}><Icons.check /></span>
  if (status === 'fail')  return <span className="ico" style={{ color: 'var(--status-fail)' }}><Icons.x /></span>
  if (status === 'quar')  return <span className="ico" style={{ color: 'var(--text-3)' }}><Icons.warn /></span>
  return <span className="ico"><span className="sdot live" /></span>
}

function StepsPane({ results, screenshots }: { results: RunResult[]; screenshots: Artifact[] }) {
  const [selected, setSelected] = useState<string | null>(null)

  const counts = results.reduce<Record<string, number>>((a, r) => {
    const k = mapResultStatus(r.status)
    a[k] = (a[k] ?? 0) + 1
    return a
  }, {})

  return (
    <section className="steps-pane">
      <div className="steps-hd">
        <h3>Results</h3>
        <div className="summary">
          {counts.pass  != null && <span><span className="sdot pass" /> {counts.pass}</span>}
          {counts.fail  != null && <span><span className="sdot fail" /> {counts.fail}</span>}
          {counts.quar  != null && <span><span className="sdot quar" /> {counts.quar}</span>}
          {results.length === 0 && <span style={{ color: 'var(--text-3)' }}>no results yet</span>}
        </div>
      </div>
      <div className="steps-list">
        {results.map((r, i) => {
          const ds = mapResultStatus(r.status)
          const isOpen = selected === r.id
          const stepCount = r.steps?.length ?? 0
          return (
            <div key={r.id}>
              <div className={'step' + (isOpen ? ' on' : '')}
                   onClick={() => setSelected(isOpen ? null : r.id)}>
                <span className="idx">{(i + 1).toString().padStart(2, '0')}</span>
                <StepIcon status={ds} />
                <span className="label">{r.test_name}</span>
                {stepCount > 0 && (
                  <span style={{ fontSize: 10.5, color: 'var(--text-3)', fontFamily: 'var(--font-mono)' }}>
                    {stepCount} step{stepCount === 1 ? '' : 's'}
                  </span>
                )}
                <span className="dur">{r.duration_ms > 0 ? `${(r.duration_ms / 1000).toFixed(1)}s` : '—'}</span>
              </div>
              {isOpen && (
                <div className={`step-detail`}>
                  {r.steps && r.steps.length > 0 && <StepTimeline steps={r.steps} />}
                  {r.logs && (
                    <details className="step-logs-wrap">
                      <summary>stdout / stderr</summary>
                      <div className={`step-logs ${ds}`}>{r.logs}</div>
                    </details>
                  )}
                </div>
              )}
            </div>
          )
        })}

        {/* Screenshot + keyframe thumbnails */}
        {screenshots.length > 0 && (
          <div style={{ padding: '12px 18px 0', display: 'flex', flexDirection: 'column', gap: 8 }}>
            <div style={{ fontSize: 11, fontWeight: 600, color: 'var(--text-3)', letterSpacing: '0.08em', textTransform: 'uppercase' }}>
              Screenshots <span style={{ color: 'var(--text-3)', fontWeight: 500 }}>({screenshots.length})</span>
            </div>
            {/* eslint-disable @next/next/no-img-element */}
            <div style={{
              display: 'grid',
              gridTemplateColumns: 'repeat(auto-fill, minmax(140px, 1fr))',
              gap: 8,
            }}>
              {screenshots.map(s => {
                const base = s.storage_key.split('/').pop() || s.kind
                const label = s.kind === 'video_frame' ? `frame · ${base.replace(/\.\w+$/, '')}` : 'screenshot'
                return (
                  <a key={s.id} href={s.url} target="_blank" rel="noopener"
                     title={base}
                     style={{
                       display: 'flex', flexDirection: 'column',
                       borderRadius: 6, overflow: 'hidden',
                       border: '1px solid var(--border)',
                       background: 'var(--surface-sunken)',
                       textDecoration: 'none',
                     }}>
                    <img src={s.url} alt={label}
                         style={{ width: '100%', aspectRatio: '16 / 10', objectFit: 'cover', display: 'block' }} />
                    <div style={{
                      padding: '4px 6px',
                      fontSize: 10, color: 'var(--text-3)',
                      fontFamily: 'var(--font-mono)',
                      borderTop: '1px solid var(--border)',
                      whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis',
                    }}>{label}</div>
                  </a>
                )
              })}
            </div>
          </div>
        )}

        {results.length === 0 && (
          <div style={{ padding: '24px 18px', color: 'var(--text-3)', fontSize: 12 }}>
            {/* Results appear after the run completes */}
            No results yet.
          </div>
        )}
      </div>
    </section>
  )
}
