export interface Run {
  id: string
  project_id: string
  branch: string
  commit_sha: string
  repo: string | null
  status: 'queued' | 'running' | 'passed' | 'failed' | 'cancelled'
  auto_triaged: boolean
  has_agent_activity: boolean
  test_filter: string
  script_id: string | null
  script_name: string | null
  env_id: string | null
  env_name: string | null
  env_slug: string | null
  started_at: string | null
  finished_at: string | null
  created_at: string
  root_run_id: string
  webhook_source?: string | null
  triage_classification?: TriageClassification | null
  triage_confidence?: 'low' | 'medium' | 'high' | null
  triage_summary?: string | null
  triaged_at?: string | null
  results?: RunResult[]
  artifacts?: Artifact[]
}

export type TriageClassification =
  | 'real_failure'
  | 'test_bug'
  | 'flake'
  | 'environment'
  | 'inconclusive'

// Display metadata for each verdict classification: human label + the
// design-status token that drives its dot/pill colour.
export const triageMeta: Record<TriageClassification, { label: string; tone: DesignStatus }> = {
  real_failure: { label: 'Real failure', tone: 'fail' },
  test_bug:     { label: 'Test bug',     tone: 'fail' },
  flake:        { label: 'Flake',        tone: 'quar' },
  environment:  { label: 'Environment',  tone: 'quar' },
  inconclusive: { label: 'Inconclusive', tone: 'live' },
}

export interface Environment {
  id: string
  project_id: string
  name: string
  slug: string
  env_vars: Record<string, string>
  // Keys in env_vars whose values are secret: returned masked (SECRET_MASK) by
  // the API, never in plaintext. Manual designation, set in the Environments UI.
  secret_keys: string[]
  created_at: string
}

// SECRET_MASK is the placeholder the API returns in place of a secret value, and
// the sentinel the API recognises on save to mean "keep the stored value". Must
// stay in sync with secretMask in cmd/control-plane/environments.go.
export const SECRET_MASK = '••••••••'

export interface RunResult {
  id: string
  test_name: string
  status: 'passed' | 'failed' | 'skipped' | 'timedout'
  duration_ms: number
  logs: string
  steps?: Step[]
}

export interface Step {
  idx: number
  api_name: string
  selector: string
  url: string
  status: 'passed' | 'failed'
  start_ms: number
  duration_ms: number
  error: string
}

export interface Artifact {
  id: string
  kind: 'video' | 'video_frame' | 'trace' | 'trace_summary' | 'screenshot' | 'log'
  storage_key: string
  size_bytes: number
  url: string
}

// Design status tokens: maps DB status to visual dot/pill class
export type DesignStatus = 'pass' | 'fail' | 'live' | 'quar'

export function mapStatus(s: Run['status']): DesignStatus {
  switch (s) {
    case 'passed':   return 'pass'
    case 'failed':   return 'fail'
    case 'running':  return 'live'
    case 'queued':   return 'live'
    case 'cancelled':return 'quar'
  }
}

export function mapResultStatus(s: RunResult['status']): DesignStatus {
  switch (s) {
    case 'passed':  return 'pass'
    case 'failed':  return 'fail'
    case 'timedout':return 'fail'
    case 'skipped': return 'quar'
  }
}

export function statusLabel(s: DesignStatus): string {
  return { pass: 'passed', fail: 'failed', live: 'running', quar: 'cancelled' }[s]
}

export function runTitle(run: Run): string {
  if (run.script_name) return run.script_name
  if (run.test_filter) return run.test_filter
  return `run · ${run.id.slice(0, 8)}`
}

export function runDuration(run: Run): string {
  // Queued runs haven't started — there's nothing to duration.
  if (run.status === 'queued') return 'queued'
  // Running with a started_at: show elapsed wall time so stuck runs are visible
  // (a 'running' run with no progress for 10m says so explicitly).
  if (run.status === 'running') {
    if (!run.started_at) return 'running…'
    const ms = Date.now() - new Date(run.started_at).getTime()
    return `running · ${fmtDurationShort(ms)}`
  }
  // Started but never finished (cancelled mid-flight, runner crashed) — still useful to show.
  if (!run.started_at) return '—'
  const end = run.finished_at ? new Date(run.finished_at).getTime() : Date.now()
  const ms = end - new Date(run.started_at).getTime()
  return fmtDurationShort(ms)
}

function fmtDurationShort(ms: number): string {
  if (ms < 0) return '—'
  const s = Math.floor(ms / 1000)
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ${s % 60}s`
  const h = Math.floor(m / 60)
  return `${h}h ${m % 60}m`
}

export function runWhen(run: Run): string {
  const ms = Date.now() - new Date(run.created_at).getTime()
  const s = Math.floor(ms / 1000)
  if (s < 60) return 'just now'
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ago`
  const d = Math.floor(h / 24)
  if (d < 7) return `${d}d ago`
  return new Date(run.created_at).toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
}

export function fmtTime(secs: number): string {
  const m = Math.floor(secs / 60)
  const s = Math.floor(secs % 60)
  return `${m}:${s.toString().padStart(2, '0')}`
}

export interface Script {
  id: string
  project_id: string
  name: string
  filename: string
  content: string
  agents_md: string
  created_at: string
  updated_at: string

  // Source linkage. Absent / empty string means "inline".
  source_kind?: 'github' | ''
  source_integration_id?: string
  source_repo?: string
  source_path?: string
  source_ref?: string
  source_sha?: string
  synced_at?: string
}
