'use client'

import { useEffect, useState } from 'react'
import { Icons } from './icons'

interface Integration {
  id: string
  provider: string
  name: string
  config: Record<string, unknown>
  has_token: boolean
}

interface ApiKey {
  id: string
  name: string
  prefix: string
  scopes: string[]
  expires_at?: string | null
  created_at: string
  last_used_at?: string | null
}

interface AuditEvent {
  id: number
  actor_id: string
  actor_kind: string
  actor_label?: string
  method: string
  path: string
  status: number
  ip?: string
  created_at: string
}

type Tab = 'integrations' | 'apikeys' | 'audit'

// ghaWorkflowSnippet renders a copy-pasteable GitHub Actions job that triggers a
// Replay run on every push and then polls it to a verdict, failing the job on a
// failed run. Both calls use the same REPLAY_TOKEN — the trigger POSTs to
// /api/webhooks/run and the poll GETs /api/webhooks/run/{id}, both authed by the
// webhook token. `url` is the trigger endpoint; we derive the poll base from it.
function ghaWorkflowSnippet(url: string): string {
  return `# .github/workflows/replay.yml
name: Replay
on: [push]

jobs:
  replay:
    runs-on: ubuntu-latest
    steps:
      - name: Trigger Replay run
        id: trigger
        run: |
          run_id=$(curl -sf -X POST ${url} \\
            -H "Authorization: Bearer \${{ secrets.REPLAY_TOKEN }}" \\
            -H "Content-Type: application/json" \\
            -d '{
              "script_filename": "\${{ vars.REPLAY_SCRIPT }}",
              "environment_slug": "\${{ vars.REPLAY_ENVIRONMENT }}",
              "branch": "\${{ github.ref_name }}",
              "commit_sha": "\${{ github.sha }}",
              "repo": "\${{ github.repository }}"
            }' | jq -r .run_id)
          echo "run_id=$run_id" >> "$GITHUB_OUTPUT"

      - name: Wait for the verdict
        run: |
          for i in $(seq 1 60); do          # ~10 min ceiling
            status=$(curl -sf ${url}/\${{ steps.trigger.outputs.run_id }} \\
              -H "Authorization: Bearer \${{ secrets.REPLAY_TOKEN }}" | jq -r .status)
            echo "run status: $status"
            case "$status" in
              passed)           echo "Replay passed"; exit 0 ;;
              failed|cancelled) echo "Replay $status"; exit 1 ;;
            esac
            sleep 10
          done
          echo "timed out waiting for Replay run"; exit 1`
}

export function IntegrationsSheet({ onClose }: { onClose: () => void }) {
  const [tab, setTab] = useState<Tab>('integrations')
  const [integrations, setIntegrations] = useState<Integration[]>([])
  const [editing, setEditing] = useState<string | null>(null) // null | 'new' | integration.id
  // Webhook token is shown plaintext exactly once, on rotation. After that we
  // only have the prefix — the backend stores SHA-256(token) and can't read
  // the token back. This matches the API-key UX in the next tab.
  const [webhookPrefix, setWebhookPrefix] = useState<string>('')
  const [webhookExists, setWebhookExists] = useState<boolean>(false)
  const [revealedToken, setRevealedToken] = useState<string | null>(null)
  const [tokenCopied, setTokenCopied] = useState(false)
  // Operator-configured public base URL (REPLAY_EXTERNAL_URL). The webhook URLs
  // we hand to GitHub must be reachable from GitHub's cloud, so we prefer this
  // over window.location.origin — which is whatever host the admin happens to be
  // browsing from (e.g. localhost:3000 under `docker compose up`).
  const [externalURL, setExternalURL] = useState<string>('')

  const reload = () => {
    fetch('/api/integrations').then(r => r.json()).then((rows: Integration[]) => {
      if (Array.isArray(rows)) setIntegrations(rows)
    }).catch(() => {})
  }
  useEffect(reload, [])
  useEffect(() => {
    fetch('/api/webhooks/token').then(r => r.ok ? r.json() : null).then(d => {
      if (!d) return
      setWebhookPrefix(d.prefix ?? '')
      setWebhookExists(Boolean(d.exists))
    }).catch(() => {})
  }, [])
  useEffect(() => {
    fetch('/api/auth/config').then(r => r.ok ? r.json() : null).then(c => {
      if (c?.external_url) setExternalURL(c.external_url)
    }).catch(() => {})
  }, [])

  const copyToken = () => {
    if (!revealedToken) return
    navigator.clipboard.writeText(revealedToken).then(() => {
      setTokenCopied(true)
      setTimeout(() => setTokenCopied(false), 2000)
    })
  }

  const githubIntegrations = integrations.filter(i => i.provider === 'github')
  const editingIntegration = editing && editing !== 'new'
    ? integrations.find(i => i.id === editing)
    : undefined
  // Prefer the operator's public URL; fall back to the browsing origin.
  const originBase = externalURL || (typeof window !== 'undefined' ? window.location.origin : '')
  const webhookRunURL = `${originBase}/api/webhooks/run`

  return (
    <div className="cmdk-mask" onClick={e => { if (e.target === e.currentTarget) onClose() }}>
      <div className="cmdk-modal" style={{ width: 'min(640px, calc(100vw - 32px))' }}>
        <div className="cmdk-input-row" style={{ padding: '14px 18px', gap: 12 }}>
          <Icons.settings />
          <div style={{ fontWeight: 600, fontSize: 14 }}>Settings</div>
          <div style={{ flex: 1, display: 'flex', gap: 4, justifyContent: 'flex-end' }}>
            <button className={`btn sm ghost${tab === 'integrations' ? ' active' : ''}`} onClick={() => setTab('integrations')}>Integrations</button>
            <button className={`btn sm ghost${tab === 'apikeys' ? ' active' : ''}`} onClick={() => setTab('apikeys')}>API keys</button>
            <button className={`btn sm ghost${tab === 'audit' ? ' active' : ''}`} onClick={() => setTab('audit')}>Audit</button>
          </div>
          <button className="icon-btn" onClick={onClose}><Icons.x /></button>
        </div>

        {tab === 'apikeys' && <ApiKeysPanel />}
        {tab === 'audit'   && <AuditPanel />}
        {tab === 'integrations' && <>
        <div style={{ padding: '6px 12px 14px' }}>
          {/* GitHub — wired, supports multiple repos */}
          <div style={{ marginBottom: 4 }}>
            {githubIntegrations.length === 0 ? (
              <div className="int-row">
                <div className="int-icon"><Icons.github /></div>
                <div>
                  <div style={{ fontWeight: 600, fontSize: 13 }}>GitHub</div>
                  <div style={{ fontSize: 11.5, color: 'var(--text-3)', fontFamily: 'var(--font-mono)' }}>
                    Code search &amp; file reads for the agent
                  </div>
                </div>
                <span className="int-status available">not configured</span>
                <button className="btn sm" onClick={() => setEditing('new')}>Connect</button>
              </div>
            ) : (
              <>
                {githubIntegrations.map(g => (
                  <div key={g.id} className="int-row">
                    <div className="int-icon"><Icons.github /></div>
                    <div>
                      <div style={{ fontWeight: 600, fontSize: 13 }}>GitHub · {g.name}</div>
                      <div style={{ fontSize: 11.5, color: 'var(--text-3)', fontFamily: 'var(--font-mono)' }}>
                        ref {(g.config.default_ref as string) ?? 'main'}
                      </div>
                    </div>
                    <span className="int-status available" style={{ background: 'var(--status-pass-bg)', color: 'var(--status-pass)' }}>
                      connected
                    </span>
                    <button className="btn sm" onClick={() => setEditing(g.id)}>Edit</button>
                  </div>
                ))}
                <div style={{ padding: '4px 6px 6px' }}>
                  <button className="btn sm" onClick={() => setEditing('new')}>
                    + Add repo
                  </button>
                </div>
              </>
            )}
          </div>

        </div>

        {(editing === 'new' || (editing && editingIntegration)) && (
          <GithubConnectForm
            initial={editingIntegration}
            originBase={originBase}
            onCancel={() => setEditing(null)}
            onSaved={() => { setEditing(null); reload() }}
            onDeleted={() => { setEditing(null); reload() }}
          />
        )}

        {/* Webhook / GHA section */}
        <div style={{ borderTop: '1px solid var(--border)', padding: '12px 18px 14px' }}>
          <div style={{ fontWeight: 600, fontSize: 12, marginBottom: 8, color: 'var(--text-2)' }}>
            GitHub Actions webhook
          </div>
          <div style={{ fontSize: 11.5, color: 'var(--text-3)', marginBottom: 8 }}>
            Trigger runs from a workflow with{' '}
            <code style={{ fontFamily: 'var(--font-mono)' }}>POST /api/webhooks/run</code>.
            Store the token as a GitHub secret (<code style={{ fontFamily: 'var(--font-mono)' }}>REPLAY_TOKEN</code>).
          </div>
          {revealedToken ? (
            <>
              <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                <input
                  readOnly
                  value={revealedToken}
                  className="gh-input"
                  style={{ flex: 1, fontSize: 11, fontFamily: 'var(--font-mono)' }}
                />
                <button className="btn sm" onClick={copyToken}>
                  {tokenCopied ? 'Copied!' : 'Copy'}
                </button>
                <button className="btn sm" onClick={() => setRevealedToken(null)}>Done</button>
              </div>
              <div style={{ fontSize: 11, color: 'var(--status-flaky)', marginTop: 6 }}>
                Copy this now — it won&apos;t be shown again. Rotating generates a new one.
              </div>
            </>
          ) : (
            <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
              <div style={{ flex: 1, fontSize: 11, fontFamily: 'var(--font-mono)', color: 'var(--text-3)' }}>
                {webhookExists ? `${webhookPrefix}${'•'.repeat(24)}` : 'no token yet'}
              </div>
              <button className="btn sm" onClick={async () => {
                const res = await fetch('/api/webhooks/token/rotate', { method: 'POST' })
                if (res.ok) {
                  const d = await res.json() as { token: string, prefix: string }
                  setRevealedToken(d.token)
                  setWebhookPrefix(d.prefix)
                  setWebhookExists(true)
                }
              }}>{webhookExists ? 'Rotate' : 'Generate'}</button>
            </div>
          )}
          <div style={{ fontSize: 11, color: 'var(--text-3)', marginTop: 8 }}>
            Payload: <code style={{ fontFamily: 'var(--font-mono)' }}>{'{ branch, commit_sha, repo, environment_slug, script_filename | script_id, env_vars }'}</code>
          </div>

          <details className="gha-guide">
            <summary>Use it in a GitHub Actions workflow</summary>
            <pre className="gha-snippet">{ghaWorkflowSnippet(webhookRunURL)}</pre>
            <div style={{ fontSize: 11, color: 'var(--text-3)', marginTop: 6, lineHeight: 1.6 }}>
              In GitHub → Settings → Secrets and variables → Actions, add:
              <br />• secret <code style={{ fontFamily: 'var(--font-mono)' }}>REPLAY_TOKEN</code> — the token above.
              <br />• variable <code style={{ fontFamily: 'var(--font-mono)' }}>REPLAY_SCRIPT</code> — the script filename to run
              (as shown in the Scripts panel). Each run executes exactly one script; filenames must be unique, or use{' '}
              <code style={{ fontFamily: 'var(--font-mono)' }}>script_id</code> instead.
              <br />• variable <code style={{ fontFamily: 'var(--font-mono)' }}>REPLAY_ENVIRONMENT</code> — an environment slug (optional; leave unset for none).
            </div>
          </details>
        </div>
        </>}
      </div>
    </div>
  )
}

// ─── API Keys panel ────────────────────────────────────────────────────

// Closed vocabulary — must match validAPIScopes in cmd/control-plane/auth_apikey.go.
const SCOPE_OPTIONS: Array<{ id: string; label: string; desc: string }> = [
  { id: 'admin',   label: 'Admin',   desc: 'Unrestricted access (default)' },
  { id: 'read',    label: 'Read',    desc: 'GET endpoints only' },
  { id: 'webhook', label: 'Webhook', desc: 'POST /api/webhooks/run only' },
]

// Expiry presets — the API accepts an ISO timestamp; we compute it client-side
// from the preset so the form stays a single select. Users who need an exact
// date can manage that later via the API.
const EXPIRY_PRESETS: Array<{ id: string; label: string; days: number | null }> = [
  { id: 'never', label: 'Never',     days: null },
  { id: '30d',   label: '30 days',   days: 30 },
  { id: '90d',   label: '90 days',   days: 90 },
  { id: '1y',    label: '1 year',    days: 365 },
]

function expiryToISO(presetID: string): string | null {
  const p = EXPIRY_PRESETS.find(p => p.id === presetID)
  if (!p || p.days === null) return null
  const d = new Date(Date.now() + p.days * 24 * 60 * 60 * 1000)
  return d.toISOString()
}

function ApiKeysPanel() {
  const [keys, setKeys] = useState<ApiKey[]>([])
  const [newName, setNewName] = useState('')
  // Selected scopes default to admin so a "just give me a key" flow still works
  // without the user toggling anything. Toggling on read/webhook implicitly
  // takes admin off — only one is meaningful at a time for human keys.
  const [newScopes, setNewScopes] = useState<string[]>(['admin'])
  const [newExpiry, setNewExpiry] = useState<string>('never')
  const [advanced, setAdvanced] = useState(false)
  const [newKey, setNewKey] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const reload = () => {
    fetch('/api/api-keys').then(r => r.ok ? r.json() : []).then(setKeys).catch(() => {})
  }
  useEffect(reload, [])

  const toggleScope = (id: string) => {
    setNewScopes(prev => {
      const has = prev.includes(id)
      // Treat admin as exclusive vs. read/webhook so the resulting key isn't
      // "admin + read" (admin already covers read; the backend would accept it
      // but the UI would mislead about what's enforced).
      if (id === 'admin') return has ? prev.filter(x => x !== 'admin') : ['admin']
      const stripped = prev.filter(x => x !== 'admin')
      return has ? stripped.filter(x => x !== id) : [...stripped, id]
    })
  }

  const create = async () => {
    setError(null)
    if (!newName.trim()) { setError('name required'); return }
    setBusy(true)
    try {
      const body: { name: string; scopes: string[]; expires_at?: string } = {
        name: newName.trim(),
        scopes: newScopes.length === 0 ? ['admin'] : newScopes,
      }
      const iso = expiryToISO(newExpiry)
      if (iso) body.expires_at = iso
      const res = await fetch('/api/api-keys', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      })
      if (!res.ok) { setError(await res.text() || `HTTP ${res.status}`); return }
      const d = await res.json() as { full_key: string }
      setNewKey(d.full_key)
      setNewName('')
      setNewScopes(['admin'])
      setNewExpiry('never')
      setAdvanced(false)
      reload()
    } finally { setBusy(false) }
  }

  const remove = async (id: string) => {
    if (!confirm('Revoke this API key? Any service using it will start failing.')) return
    const res = await fetch(`/api/api-keys/${id}`, { method: 'DELETE' })
    if (res.ok) reload()
  }

  return (
    <div style={{ padding: '6px 18px 14px' }}>
      <div style={{ fontSize: 11.5, color: 'var(--text-3)', marginBottom: 10 }}>
        API keys authenticate non-browser callers — CI scripts, the GitHub Actions webhook,
        runners. Scope each key as narrowly as the caller needs.
      </div>

      <div style={{ display: 'flex', gap: 8, marginBottom: 8 }}>
        <input
          className="gh-input" style={{ flex: 1, fontSize: 12 }}
          placeholder="key name (e.g. github-actions)"
          value={newName}
          onChange={e => setNewName(e.target.value)}
          onKeyDown={e => { if (e.key === 'Enter') create() }}
        />
        <button className="btn sm primary" onClick={create} disabled={busy}>
          {busy ? 'Creating…' : 'New key'}
        </button>
      </div>

      <div style={{ marginBottom: 12 }}>
        <button className="btn sm ghost" onClick={() => setAdvanced(a => !a)}
                style={{ fontSize: 11, padding: '2px 6px' }}>
          {advanced ? '▾' : '▸'} Scopes &amp; expiry
        </button>
        {advanced && (
          <div style={{ marginTop: 8, padding: 10, borderRadius: 6, background: 'var(--surface-2)',
                        display: 'flex', flexDirection: 'column', gap: 10, fontSize: 12 }}>
            <div>
              <div style={{ color: 'var(--text-3)', marginBottom: 4 }}>Scopes</div>
              <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
                {SCOPE_OPTIONS.map(s => (
                  <label key={s.id} style={{ display: 'flex', alignItems: 'center', gap: 6, cursor: 'pointer' }}>
                    <input type="checkbox" checked={newScopes.includes(s.id)}
                           onChange={() => toggleScope(s.id)} />
                    <span><strong>{s.label}</strong> <span style={{ color: 'var(--text-3)' }}>— {s.desc}</span></span>
                  </label>
                ))}
              </div>
            </div>
            <div>
              <div style={{ color: 'var(--text-3)', marginBottom: 4 }}>Expires</div>
              <select className="scripts-field" value={newExpiry}
                      onChange={e => setNewExpiry(e.target.value)}
                      style={{ width: 'auto' }}>
                {EXPIRY_PRESETS.map(p => (
                  <option key={p.id} value={p.id}>{p.label}</option>
                ))}
              </select>
            </div>
          </div>
        )}
      </div>

      {error && <div style={{ fontSize: 12, color: 'var(--status-fail)' }}>{error}</div>}

      {newKey && (
        <div className="apikey-newkey">
          <Icons.key />
          <input readOnly value={newKey} onFocus={e => e.currentTarget.select()} />
          <button className="btn sm" onClick={() => {
            navigator.clipboard.writeText(newKey).then(() => {/* no toast — modal is short */})
          }}>Copy</button>
          <button className="btn sm" onClick={() => setNewKey(null)}>Done</button>
        </div>
      )}
      {newKey && (
        <div style={{ fontSize: 11, color: 'var(--text-3)', marginTop: -4, marginBottom: 12 }}>
          This key will only be shown once. Store it now.
        </div>
      )}

      {keys.length === 0 && !newKey && (
        <div style={{ fontSize: 12, color: 'var(--text-3)', padding: '12px 0' }}>
          No API keys yet.
        </div>
      )}

      {keys.map(k => {
        const expired = k.expires_at && new Date(k.expires_at).getTime() < Date.now()
        return (
          <div key={k.id} className="apikey-row">
            <div>
              <div style={{ fontWeight: 600 }}>
                {k.name}
                {expired && (
                  <span style={{ marginLeft: 6, fontSize: 10, padding: '1px 4px', borderRadius: 3,
                                 background: 'var(--status-fail-bg)', color: 'var(--status-fail)' }}>
                    expired
                  </span>
                )}
              </div>
              <div className="apikey-meta">
                {(k.scopes ?? []).join(', ') || 'admin'}
                {' · created '}{new Date(k.created_at).toLocaleDateString()}
                {k.expires_at && <> · expires {new Date(k.expires_at).toLocaleDateString()}</>}
                {k.last_used_at ? <> · last used {timeAgo(k.last_used_at)}</> : <> · never used</>}
              </div>
            </div>
            <span className="apikey-prefix">{k.prefix}…</span>
            <button className="btn sm" onClick={() => remove(k.id)} style={{ color: 'var(--status-fail)' }}>Revoke</button>
          </div>
        )
      })}
    </div>
  )
}

// ─── Audit panel ───────────────────────────────────────────────────────

function AuditPanel() {
  const [events, setEvents] = useState<AuditEvent[]>([])
  const [pathFilter, setPathFilter] = useState('')

  useEffect(() => {
    const q = pathFilter ? `?path=${encodeURIComponent(pathFilter)}&limit=100` : '?limit=100'
    fetch(`/api/audit-events${q}`).then(r => r.ok ? r.json() : []).then(setEvents).catch(() => {})
  }, [pathFilter])

  return (
    <div style={{ padding: '6px 18px 14px', maxHeight: '60vh', overflow: 'auto' }}>
      <div style={{ display: 'flex', gap: 8, marginBottom: 10 }}>
        <input
          className="gh-input" style={{ flex: 1, fontSize: 12 }}
          placeholder="filter by path (e.g. /api/scripts)"
          value={pathFilter}
          onChange={e => setPathFilter(e.target.value)}
        />
      </div>
      {events.length === 0 && (
        <div style={{ fontSize: 12, color: 'var(--text-3)', padding: '12px 0' }}>
          No audit events match.
        </div>
      )}
      {events.map(ev => {
        const sClass = ev.status >= 500 ? 's5xx' : ev.status >= 400 ? 's4xx' : ''
        const actorName = ev.actor_label
          || (ev.actor_kind === 'anonymous' ? 'anonymous' : (ev.actor_id ? ev.actor_id.slice(0, 8) : '—'))
        const actorKind = ev.actor_kind === 'password' ? 'user' : ev.actor_kind
        return (
          <div key={ev.id} className="audit-row">
            <code className={`audit-method ${ev.method}`}>{ev.method}</code>
            <code className={`audit-status ${sClass}`}>{ev.status}</code>
            <code title={ev.path}>{ev.path}</code>
            <span className="audit-actor" title={`${ev.actor_kind} · ${ev.actor_id}`}>
              <span className="audit-actor-kind">{actorKind}:</span>{actorName}
            </span>
            <span className="audit-when">{timeAgo(ev.created_at)}</span>
          </div>
        )
      })}
    </div>
  )
}

function timeAgo(iso: string): string {
  const ms = Date.now() - new Date(iso).getTime()
  const s = Math.floor(ms / 1000)
  if (s < 60)    return `${s}s ago`
  const m = Math.floor(s / 60)
  if (m < 60)    return `${m}m ago`
  const h = Math.floor(m / 60)
  if (h < 24)    return `${h}h ago`
  const d = Math.floor(h / 24)
  return `${d}d ago`
}

function GithubConnectForm({ initial, originBase, onCancel, onSaved, onDeleted }: {
  initial?: Integration
  originBase: string
  onCancel: () => void
  onSaved: () => void
  onDeleted: () => void
}) {
  const [owner, setOwner] = useState((initial?.config?.owner as string) ?? '')
  const [repo,  setRepo]  = useState((initial?.config?.repo  as string) ?? '')
  const [ref,   setRef]   = useState((initial?.config?.default_ref as string) ?? 'main')
  const [token, setToken] = useState('')
  // Shared secret used to verify GitHub push webhooks for repo→script auto-sync.
  // Preloaded from existing config so an edit doesn't wipe it (POST replaces config).
  const [webhookSecret, setWebhookSecret] = useState((initial?.config?.webhook_secret as string) ?? '')
  const [saving, setSaving] = useState(false)
  const [error,  setError]  = useState<string | null>(null)

  const webhookURL = `${originBase}/api/webhooks/github`

  const save = async () => {
    setError(null)
    if (!owner.trim() || !repo.trim()) { setError('owner and repo are required'); return }
    if (!initial && !token.trim())     { setError('a personal access token is required for first-time setup'); return }
    setSaving(true)
    try {
      const res = await fetch('/api/integrations', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          provider: 'github',
          name: `${owner.trim()}/${repo.trim()}`,
          config: { owner: owner.trim(), repo: repo.trim(), default_ref: ref.trim() || 'main', webhook_secret: webhookSecret.trim() },
          token: token.trim(),
        }),
      })
      if (!res.ok) { setError(await res.text() || `HTTP ${res.status}`); return }
      onSaved()
    } finally { setSaving(false) }
  }
  const remove = async () => {
    if (!initial?.id) return
    setSaving(true)
    try {
      const res = await fetch(`/api/integrations/${initial.id}`, { method: 'DELETE' })
      if (!res.ok) { setError(await res.text() || `HTTP ${res.status}`); return }
      onDeleted()
    } finally { setSaving(false) }
  }

  return (
    <div style={{ padding: '0 18px 16px', borderTop: '1px solid var(--border)' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '12px 0' }}>
        <Icons.github />
        <div style={{ fontWeight: 600, fontSize: 13 }}>
          {initial ? 'Edit GitHub integration' : 'Connect GitHub repository'}
        </div>
      </div>
      <div style={{ display: 'grid', gridTemplateColumns: '120px 1fr', gap: 8, alignItems: 'center', fontSize: 12 }}>
        <label>Owner</label>
        <input value={owner} onChange={e => setOwner(e.target.value)} placeholder="my-org" className="gh-input" />
        <label>Repo</label>
        <input value={repo}  onChange={e => setRepo(e.target.value)}  placeholder="my-repo" className="gh-input" />
        <label>Default ref</label>
        <input value={ref}   onChange={e => setRef(e.target.value)}   placeholder="main"    className="gh-input" />
        <label>Token</label>
        <input value={token} onChange={e => setToken(e.target.value)}
               type="password" autoComplete="off"
               placeholder={initial ? 'leave blank to keep existing token' : 'ghp_… or fine-grained PAT'}
               className="gh-input" />
        <label>Webhook secret</label>
        <input value={webhookSecret} onChange={e => setWebhookSecret(e.target.value)}
               type="password" autoComplete="off"
               placeholder="optional — enables push auto-sync"
               className="gh-input" />
      </div>
      <div style={{ marginTop: 6, fontSize: 11, color: 'var(--text-3)' }}>
        Token needs <code>contents:read</code>. Stored encrypted at rest.
      </div>
      <div style={{ marginTop: 6, fontSize: 11, color: 'var(--text-3)' }}>
        To auto-sync scripts when the repo changes, add a GitHub webhook →{' '}
        <code style={{ fontFamily: 'var(--font-mono)' }}>{webhookURL}</code>{' '}
        (content type <code>application/json</code>, event <code>push</code>) and set its secret to match the value above.
      </div>
      {error && <div style={{ marginTop: 8, fontSize: 12, color: 'var(--status-fail)' }}>{error}</div>}
      <div style={{ display: 'flex', gap: 8, marginTop: 14 }}>
        {initial && <button className="btn sm" onClick={remove} disabled={saving} style={{ color: 'var(--status-fail)' }}>Remove</button>}
        <span style={{ flex: 1 }} />
        <button className="btn sm" onClick={onCancel} disabled={saving}>Cancel</button>
        <button className="btn sm primary" onClick={save} disabled={saving}>{saving ? 'Saving…' : 'Save'}</button>
      </div>
    </div>
  )
}
