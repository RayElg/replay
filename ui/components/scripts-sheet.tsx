'use client'
import { useState, useEffect } from 'react'
import { Script, Environment } from '@/lib/types'
import { Icons } from './icons'

const PLACEHOLDER = `import { test, expect } from '@playwright/test';

// BASE_URL is set per-environment and wires into Playwright's baseURL.
// Use relative paths — they'll resolve against the env's BASE_URL automatically.
//
// Other env vars (process.env.API_KEY, etc.) are set in the Environments panel.
// Replay always injects: REPLAY_RUN_ID, REPLAY_BRANCH, REPLAY_COMMIT, REPLAY_ENV

test('home page loads', async ({ page }) => {
  await page.goto('/');                           // resolves via BASE_URL
  await expect(page).toHaveTitle(/./);
});
`

type EditState = { name: string; filename: string; content: string; agents_md: string }

const AGENTS_MD_PLACEHOLDER = `Repo: owner/repo (which of this project's repos this script tests)
Files of interest: src/checkout/**, src/api/orders/**
Notes for the agent: …`

// Always-available vars injected by the runner regardless of environment
const REPLAY_VARS = [
  { key: 'REPLAY_RUN_ID',  desc: 'UUID of the current run' },
  { key: 'REPLAY_BRANCH',  desc: 'Git branch name' },
  { key: 'REPLAY_COMMIT',  desc: 'Commit SHA' },
  { key: 'REPLAY_ENV',     desc: 'Environment slug (e.g. "staging")' },
]

function EnvVarsReference({ envs }: { envs: Environment[] }) {
  const [open, setOpen] = useState(false)

  const hasUserVars = envs.some(e => Object.keys(e.env_vars).length > 0)

  return (
    <div className="envref">
      <button className="envref-toggle" onClick={() => setOpen(o => !o)}>
        <Icons.globe />
        <span>Available variables</span>
        <span className="envref-chevron" data-open={open ? '1' : '0'}>›</span>
      </button>
      {open && (
        <div className="envref-body">
          <div className="envref-section">
            <div className="envref-section-hd">Always injected by Replay</div>
            {REPLAY_VARS.map(v => (
              <div key={v.key} className="envref-row">
                <code>{v.key}</code>
                <span>{v.desc}</span>
              </div>
            ))}
          </div>

          {hasUserVars ? envs.filter(e => Object.keys(e.env_vars).length > 0).map(e => (
            <div key={e.id} className="envref-section">
              <div className="envref-section-hd">
                <span className="env-badge">{e.slug}</span> {e.name}
              </div>
              {Object.entries(e.env_vars).map(([k, v]) => {
                // Secrecy is an explicit, per-variable designation (set in the
                // Environments panel) — not a guess from the key name. The API
                // already returns secret values masked, so just flag them.
                const isSecret = (e.secret_keys ?? []).includes(k)
                return (
                  <div key={k} className="envref-row">
                    <code>{k}</code>
                    <span className="envref-val">{
                      isSecret
                        ? <span className="envref-secret"><Icons.key /> secret</span>
                        : v || <em>empty</em>
                    }</span>
                  </div>
                )
              })}
            </div>
          )) : (
            <div className="envref-section">
              <div className="envref-section-hd" style={{ color: 'var(--text-3)' }}>
                No user-defined vars yet — add them in the Environments panel.
              </div>
            </div>
          )}

          <div className="envref-hint">
            Access in scripts via <code>process.env.KEY</code>. Set <code>BASE_URL</code> in an environment to enable <code>page.goto(&apos;/&apos;)</code>.
          </div>
        </div>
      )}
    </div>
  )
}

function AgentsMDEditor({ value, onChange }:
  { value: string; onChange: (e: React.ChangeEvent<HTMLTextAreaElement>) => void }) {
  const [open, setOpen] = useState(value.length > 0)
  return (
    <div className="envref">
      <button className="envref-toggle" onClick={() => setOpen(o => !o)}>
        <Icons.flask />
        <span>AGENTS.md for this script {value ? '' : '(empty)'}</span>
        <span className="envref-chevron" data-open={open ? '1' : '0'}>›</span>
      </button>
      {open && (
        <div className="envref-body">
          <div className="envref-hint" style={{ marginBottom: 6 }}>
            Free-form guidance the agent sees alongside this script (e.g. which repo it targets, which files matter).
          </div>
          <textarea className="scripts-code" spellCheck={false}
                    style={{ minHeight: 110, height: 110 }}
                    placeholder={AGENTS_MD_PLACEHOLDER}
                    value={value} onChange={onChange} />
        </div>
      )}
    </div>
  )
}

export function ScriptsSheet({ onClose }: { onClose: () => void }) {
  const [scripts, setScripts]   = useState<Script[]>([])
  const [envs, setEnvs]         = useState<Environment[]>([])
  const [selected, setSelected] = useState<Script | null>(null)
  const [editing, setEditing]   = useState<EditState | null>(null)
  const [creating, setCreating] = useState(false)
  const [saving, setSaving]     = useState(false)
  const [importing, setImporting] = useState(false)
  const [syncing, setSyncing] = useState(false)
  const [idCopied, setIdCopied] = useState(false)

  useEffect(() => {
    Promise.all([
      fetch('/api/scripts').then(r => r.json()),
      fetch('/api/environments').then(r => r.json()),
    ]).then(([s, e]) => {
      if (Array.isArray(s)) setScripts(s)
      if (Array.isArray(e)) setEnvs(e)
    })
  }, [])

  const selectScript = (s: Script) => {
    setSelected(s)
    setEditing({ name: s.name, filename: s.filename, content: s.content, agents_md: s.agents_md ?? '' })
    setCreating(false)
  }

  const startCreate = () => {
    setSelected(null)
    setCreating(true)
    setEditing({ name: 'New Script', filename: 'tests/new-test.spec.ts', content: PLACEHOLDER, agents_md: '' })
  }

  const save = async () => {
    if (!editing) return
    setSaving(true)
    try {
      if (creating) {
        const res  = await fetch('/api/scripts', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(editing),
        })
        const created: Script = await res.json()
        setScripts(prev => [created, ...prev])
        setSelected(created)
        setCreating(false)
      } else if (selected) {
        await fetch(`/api/scripts/${selected.id}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(editing),
        })
        const updated = { ...selected, ...editing }
        setScripts(prev => prev.map(s => s.id === selected.id ? updated : s))
        setSelected(updated)
      }
    } finally {
      setSaving(false)
    }
  }

  const del = async () => {
    if (!selected) return
    await fetch(`/api/scripts/${selected.id}`, { method: 'DELETE' })
    setScripts(prev => prev.filter(s => s.id !== selected.id))
    setSelected(null)
    setEditing(null)
    setCreating(false)
  }

  const setField = (k: keyof EditState) => (e: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) =>
    setEditing(prev => prev ? { ...prev, [k]: e.target.value } : prev)

  // Pulls the latest content for a github-linked script. Used by the "Sync"
  // button on the editor toolbar.
  const sync = async () => {
    if (!selected || selected.source_kind !== 'github') return
    setSyncing(true)
    try {
      const res = await fetch(`/api/scripts/${selected.id}/sync`, { method: 'POST' })
      if (!res.ok) return
      const reloaded: Script = await fetch(`/api/scripts/${selected.id}`).then(r => r.json())
      setScripts(prev => prev.map(s => s.id === reloaded.id ? reloaded : s))
      setSelected(reloaded)
      setEditing({ name: reloaded.name, filename: reloaded.filename, content: reloaded.content, agents_md: reloaded.agents_md ?? '' })
    } finally {
      setSyncing(false)
    }
  }

  const onImported = (imported: Script[]) => {
    setScripts(prev => [...imported, ...prev.filter(p => !imported.some(i => i.id === p.id))])
    setImporting(false)
  }

  return (
    <div className="cmdk-mask" onClick={e => { if (e.target === e.currentTarget) onClose() }}>
      <div className="scripts-modal">
        <div className="scripts-hd">
          <Icons.flask />
          <span>Scripts</span>
          <div style={{ flex: 1 }} />
          <button className="btn sm" onClick={startCreate}><Icons.plus /> New</button>
          <button className="btn sm" onClick={() => setImporting(true)}><Icons.github /> Import</button>
          <button className="icon-btn" onClick={onClose}><Icons.x /></button>
        </div>

        <div className="scripts-body">
          <div className="scripts-list">
            {scripts.map(s => (
              <div key={s.id}
                   className={'scripts-item' + (selected?.id === s.id ? ' on' : '')}
                   onClick={() => selectScript(s)}>
                <div className="scripts-item-name">
                  {s.name}
                  {s.source_kind === 'github' && (
                    <span className="env-badge" title={`from ${s.source_repo}@${s.source_ref}:${s.source_path}`}
                          style={{ marginLeft: 6 }}>
                      github
                    </span>
                  )}
                </div>
                <div className="scripts-item-file">{s.filename}</div>
              </div>
            ))}
            {scripts.length === 0 && !creating && (
              <div className="scripts-list-empty">No scripts yet.</div>
            )}
          </div>

          {editing ? (
            <div className="scripts-editor">
              <div className="scripts-meta">
                <input className="scripts-field" placeholder="Script name"
                       value={editing.name} onChange={setField('name')} />
                <input className="scripts-field mono" placeholder="path/to/test.spec.ts"
                       value={editing.filename} onChange={setField('filename')} />
              </div>
              {selected && !creating && (
                // Script ID — the value a GHA webhook / API trigger passes as
                // script_id to run exactly this script. Copyable because it's
                // otherwise not surfaced anywhere in the UI.
                <div className="script-id-row">
                  <span className="script-id-label">ID</span>
                  <code>{selected.id}</code>
                  <button className="icon-btn" title="Copy script ID"
                          onClick={() => {
                            navigator.clipboard.writeText(selected.id).then(() => {
                              setIdCopied(true)
                              setTimeout(() => setIdCopied(false), 1500)
                            })
                          }}>
                    {idCopied ? <Icons.check /> : <Icons.copy />}
                  </button>
                </div>
              )}
              {selected?.source_kind === 'github' && (
                <div style={{ fontSize: 11.5, color: 'var(--text-3)', padding: '4px 10px', display: 'flex', alignItems: 'center', gap: 8 }}>
                  <Icons.github />
                  <span style={{ fontFamily: 'var(--font-mono)' }}>
                    {selected.source_repo}@{selected.source_ref}:{selected.source_path}
                  </span>
                  <span style={{ flex: 1 }} />
                  {selected.synced_at && (
                    <span title={`synced at ${selected.synced_at}`}>
                      sha {selected.source_sha?.slice(0, 7)}
                    </span>
                  )}
                  <button className="btn sm" onClick={sync} disabled={syncing}>
                    {syncing ? 'Syncing…' : 'Sync from repo'}
                  </button>
                </div>
              )}
              <EnvVarsReference envs={envs} />
              <AgentsMDEditor value={editing.agents_md} onChange={setField('agents_md')} />
              <textarea className="scripts-code" spellCheck={false}
                        value={editing.content} onChange={setField('content')}
                        onKeyDown={e => {
                          if (e.key === 'Tab') {
                            e.preventDefault()
                            const el = e.currentTarget
                            const start = el.selectionStart, end = el.selectionEnd
                            const next = el.value.slice(0, start) + '  ' + el.value.slice(end)
                            setField('content')({ target: { value: next } } as React.ChangeEvent<HTMLTextAreaElement>)
                            requestAnimationFrame(() => { el.selectionStart = el.selectionEnd = start + 2 })
                          }
                        }} />
              <div className="scripts-bar">
                {selected && !creating && (
                  <button className="btn sm danger" onClick={del}><Icons.trash /> Delete</button>
                )}
                <div style={{ flex: 1 }} />
                <button className="btn sm" onClick={onClose}>Cancel</button>
                <button className="btn primary sm" onClick={save} disabled={saving}>
                  {saving ? 'Saving…' : 'Save'}
                </button>
              </div>
            </div>
          ) : (
            <div className="scripts-editor-empty">
              Select a script to edit, or create a new one.
            </div>
          )}
        </div>
        {importing && <GithubImportModal onClose={() => setImporting(false)} onImported={onImported} />}
      </div>
    </div>
  )
}

// ─── Import-from-GitHub modal ─────────────────────────────────────────

interface GithubIntegration {
  id: string
  provider: string
  name: string
  config: { owner?: string; repo?: string; default_ref?: string; auth_kind?: string }
}

interface TreeEntry {
  name: string
  path: string
  type: 'file' | 'dir'
  size?: number
}

function GithubImportModal({ onClose, onImported }:
  { onClose: () => void; onImported: (s: Script[]) => void }) {
  const [integrations, setIntegrations] = useState<GithubIntegration[]>([])
  const [activeId, setActiveId] = useState<string | null>(null)
  const [ref, setRef] = useState<string>('')
  const [path, setPath] = useState<string>('')
  const [entries, setEntries] = useState<TreeEntry[]>([])
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [loading, setLoading] = useState(false)
  const [importing, setImportingNow] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    fetch('/api/integrations').then(r => r.json()).then((rows: GithubIntegration[]) => {
      const github = (rows || []).filter(i => i.provider === 'github')
      setIntegrations(github)
      if (github.length > 0) {
        setActiveId(github[0].id)
        setRef(github[0].config?.default_ref ?? 'main')
      }
    })
  }, [])

  const active = integrations.find(i => i.id === activeId)

  useEffect(() => {
    if (!activeId) return
    setLoading(true); setError(null)
    fetch(`/api/integrations/${activeId}/repo-tree?path=${encodeURIComponent(path)}&ref=${encodeURIComponent(ref)}`)
      .then(async r => {
        if (!r.ok) throw new Error(await r.text())
        return r.json()
      })
      .then((d: { entries: TreeEntry[] }) => setEntries(d.entries))
      .catch(e => setError(String(e?.message ?? e)))
      .finally(() => setLoading(false))
  }, [activeId, path, ref])

  const toggle = (p: string) => {
    setSelected(prev => {
      const next = new Set(prev)
      if (next.has(p)) next.delete(p); else next.add(p)
      return next
    })
  }

  const cd = (entry: TreeEntry) => {
    if (entry.type === 'dir') setPath(entry.path)
    else toggle(entry.path)
  }

  const up = () => {
    if (!path) return
    const i = path.lastIndexOf('/')
    setPath(i >= 0 ? path.slice(0, i) : '')
  }

  const runImport = async () => {
    if (!activeId || selected.size === 0) return
    setImportingNow(true); setError(null)
    try {
      const res = await fetch('/api/scripts/import/github', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ integration_id: activeId, paths: [...selected], ref }),
      })
      const data = await res.json() as { imported: { id: string; path: string }[]; errors: { path: string; error: string }[] }
      if (data.errors?.length) {
        setError(`Some files failed: ${data.errors.map(e => `${e.path} (${e.error})`).join(', ')}`)
      }
      if (data.imported?.length) {
        // Refetch full script rows for the imported IDs so we have content/source linkage.
        const fresh = await Promise.all(data.imported.map(i =>
          fetch(`/api/scripts/${i.id}`).then(r => r.json() as Promise<Script>)
        ))
        onImported(fresh)
      }
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setImportingNow(false)
    }
  }

  if (integrations.length === 0) {
    return (
      <div className="cmdk-mask" onClick={e => { if (e.target === e.currentTarget) onClose() }}>
        <div className="cmdk-modal" style={{ width: 'min(520px, calc(100vw - 32px))' }}>
          <div className="cmdk-input-row" style={{ padding: '14px 18px' }}>
            <Icons.github />
            <div style={{ fontWeight: 600 }}>Import from GitHub</div>
            <div style={{ flex: 1 }} />
            <button className="icon-btn" onClick={onClose}><Icons.x /></button>
          </div>
          <div style={{ padding: '6px 18px 18px', fontSize: 12, color: 'var(--text-3)' }}>
            No GitHub integration is configured for this workspace. Add one in Settings → Integrations first.
          </div>
        </div>
      </div>
    )
  }

  return (
    <div className="cmdk-mask" onClick={e => { if (e.target === e.currentTarget) onClose() }}>
      <div className="cmdk-modal" style={{ width: 'min(720px, calc(100vw - 32px))' }}>
        <div className="cmdk-input-row" style={{ padding: '14px 18px', gap: 10 }}>
          <Icons.github />
          <div style={{ fontWeight: 600 }}>Import from GitHub</div>
          <select className="gh-input" style={{ fontSize: 12 }}
                  value={activeId ?? ''}
                  onChange={e => { setActiveId(e.target.value); setPath(''); setSelected(new Set()) }}>
            {integrations.map(i => (
              <option key={i.id} value={i.id}>{i.name || `${i.config?.owner}/${i.config?.repo}`}</option>
            ))}
          </select>
          <input className="gh-input" style={{ width: 110, fontSize: 12 }}
                 value={ref} onChange={e => setRef(e.target.value)} placeholder="ref" />
          <div style={{ flex: 1 }} />
          <button className="icon-btn" onClick={onClose}><Icons.x /></button>
        </div>

        <div style={{ padding: '4px 18px 0', fontSize: 11.5, color: 'var(--text-3)', display: 'flex', gap: 6, alignItems: 'center' }}>
          {active?.config?.owner}/{active?.config?.repo}
          <span>·</span>
          <span style={{ fontFamily: 'var(--font-mono)' }}>/{path}</span>
          {path && <button className="btn sm" onClick={up}>up</button>}
        </div>

        <div style={{ padding: '8px 18px', maxHeight: 360, overflowY: 'auto' }}>
          {loading && <div style={{ fontSize: 12, color: 'var(--text-3)' }}>Loading…</div>}
          {error && <div style={{ fontSize: 12, color: 'var(--status-fail)' }}>{error}</div>}
          {!loading && entries.length === 0 && !error && (
            <div style={{ fontSize: 12, color: 'var(--text-3)' }}>Empty directory.</div>
          )}
          {entries.map(e => (
            <div key={e.path}
                 onClick={() => cd(e)}
                 className="int-row"
                 style={{ cursor: 'pointer', padding: '6px 4px' }}>
              <div style={{ width: 18 }}>
                {e.type === 'dir'
                  ? '📁'
                  : <input type="checkbox" checked={selected.has(e.path)} readOnly />}
              </div>
              <div style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{e.name}</div>
              <div style={{ flex: 1 }} />
              {e.type === 'file' && e.size != null && (
                <div style={{ fontSize: 11, color: 'var(--text-3)' }}>{Math.round(e.size / 1024)} KB</div>
              )}
            </div>
          ))}
        </div>

        <div style={{ padding: '10px 18px', borderTop: '1px solid var(--border)', display: 'flex', gap: 8 }}>
          <div style={{ fontSize: 12, color: 'var(--text-3)', alignSelf: 'center' }}>
            {selected.size} file{selected.size === 1 ? '' : 's'} selected
          </div>
          <div style={{ flex: 1 }} />
          <button className="btn sm" onClick={onClose}>Cancel</button>
          <button className="btn primary sm" onClick={runImport} disabled={importing || selected.size === 0}>
            {importing ? 'Importing…' : 'Import'}
          </button>
        </div>
      </div>
    </div>
  )
}
