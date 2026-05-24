'use client'
import { useState, useEffect } from 'react'
import { Environment, SECRET_MASK } from '@/lib/types'
import { Icons } from './icons'

type EditState = { name: string; slug: string; envVarsText: string; secretKeys: string[] }

function envVarsToText(vars: Record<string, string>): string {
  return Object.entries(vars).map(([k, v]) => `${k}=${v}`).join('\n')
}

function textToEnvVars(text: string): Record<string, string> {
  const result: Record<string, string> = {}
  for (const line of text.split('\n')) {
    const trimmed = line.trim()
    if (!trimmed || trimmed.startsWith('#')) continue
    const eq = trimmed.indexOf('=')
    if (eq < 1) continue
    result[trimmed.slice(0, eq).trim()] = trimmed.slice(eq + 1)
  }
  return result
}

export function EnvironmentsSheet({ onClose }: { onClose: () => void }) {
  const [envs, setEnvs]         = useState<Environment[]>([])
  const [selected, setSelected] = useState<Environment | null>(null)
  const [editing, setEditing]   = useState<EditState | null>(null)
  const [creating, setCreating] = useState(false)
  const [saving, setSaving]     = useState(false)
  // Default true so we never flash a plaintext warning before /api/settings
  // resolves; flipped to false only on a confirmed "no key" response.
  const [encryptionOn, setEncryptionOn] = useState(true)

  useEffect(() => {
    fetch('/api/environments').then(r => r.json()).then(data => {
      if (Array.isArray(data)) {
        setEnvs(data)
        if (data.length > 0) { setSelected(data[0]); openEdit(data[0]) }
      }
    })
    fetch('/api/settings').then(r => r.json())
      .then(s => setEncryptionOn(s?.encryption_configured !== false))
      .catch(() => {})
  }, [])

  const openEdit = (e: Environment) => {
    setSelected(e)
    setEditing({
      name: e.name,
      slug: e.slug,
      envVarsText: envVarsToText(e.env_vars),
      secretKeys: e.secret_keys ?? [],
    })
    setCreating(false)
  }

  const startCreate = () => {
    setSelected(null)
    setCreating(true)
    setEditing({ name: 'New Environment', slug: 'new-env', envVarsText: '# KEY=value\n', secretKeys: [] })
  }

  // Keys actually present in the editor text — drives the secret toggles and
  // prunes secretKeys that reference deleted lines on save.
  const editorKeys = editing ? Object.keys(textToEnvVars(editing.envVarsText)) : []

  const toggleSecret = (key: string) => setEditing(prev => {
    if (!prev) return prev
    const has = prev.secretKeys.includes(key)
    return { ...prev, secretKeys: has ? prev.secretKeys.filter(k => k !== key) : [...prev.secretKeys, key] }
  })

  const save = async () => {
    if (!editing) return
    setSaving(true)
    try {
      const envVars = textToEnvVars(editing.envVarsText)
      // Only keep secret flags for keys that still exist.
      const secretKeys = editing.secretKeys.filter(k => k in envVars)
      const body = { name: editing.name, slug: editing.slug, env_vars: envVars, secret_keys: secretKeys }
      if (creating) {
        const res = await fetch('/api/environments', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        })
        const created: Environment = await res.json()
        setEnvs(prev => [...prev, created])
        setSelected(created)
        setCreating(false)
        openEdit(created)
      } else if (selected) {
        await fetch(`/api/environments/${selected.id}`, {
          method: 'PUT', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        })
        // Re-fetch the canonical row so secret values come back masked and
        // secret_keys reflect what was persisted.
        const updated: Environment = {
          ...selected, name: body.name, slug: body.slug,
          env_vars: Object.fromEntries(Object.entries(envVars).map(
            ([k, v]) => [k, secretKeys.includes(k) ? SECRET_MASK : v])),
          secret_keys: secretKeys,
        }
        setEnvs(prev => prev.map(e => e.id === selected.id ? updated : e))
        setSelected(updated)
        openEdit(updated)
      }
    } finally {
      setSaving(false)
    }
  }

  const del = async () => {
    if (!selected) return
    await fetch(`/api/environments/${selected.id}`, { method: 'DELETE' })
    const remaining = envs.filter(e => e.id !== selected.id)
    setEnvs(remaining)
    if (remaining.length > 0) { setSelected(remaining[0]); openEdit(remaining[0]) }
    else { setSelected(null); setEditing(null) }
  }

  const set = (k: 'name' | 'slug' | 'envVarsText') => (e: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) =>
    setEditing(prev => prev ? { ...prev, [k]: e.target.value } : prev)

  const varCount = editorKeys.length

  return (
    <div className="cmdk-mask" onClick={e => { if (e.target === e.currentTarget) onClose() }}>
      <div className="scripts-modal">
        <div className="scripts-hd">
          <Icons.globe />
          <span>Environments</span>
          <div style={{ flex: 1 }} />
          <button className="btn sm" onClick={startCreate}><Icons.plus /> New</button>
          <button className="icon-btn" onClick={onClose}><Icons.x /></button>
        </div>

        <div className="scripts-body">
          <div className="scripts-list">
            {envs.map(e => (
              <div key={e.id}
                   className={'scripts-item' + (selected?.id === e.id ? ' on' : '')}
                   onClick={() => openEdit(e)}>
                <div className="scripts-item-name">{e.name}</div>
                <div className="scripts-item-file">{e.slug}</div>
              </div>
            ))}
            {/* Optimistic row for an unsaved new environment so it's visible in
                the list as you fill it in. */}
            {creating && editing && (
              <div className="scripts-item on">
                <div className="scripts-item-name">
                  {editing.name || 'New Environment'}
                  <span className="env-unsaved-tag">unsaved</span>
                </div>
                <div className="scripts-item-file">{editing.slug || 'new-env'}</div>
              </div>
            )}
            {envs.length === 0 && !creating && (
              <div className="scripts-list-empty">No environments yet.</div>
            )}
          </div>

          {editing ? (
            <div className="scripts-editor">
              <div className="scripts-meta">
                <input className="scripts-field" placeholder="Environment name"
                       value={editing.name} onChange={set('name')} />
                <input className="scripts-field mono" placeholder="slug"
                       value={editing.slug} onChange={set('slug')} />
              </div>
              <div className="env-vars-label">
                <span>Environment variables</span>
                <span className="env-vars-count">{varCount} var{varCount !== 1 ? 's' : ''}</span>
              </div>
              <textarea className="scripts-code env-vars-editor"
                        spellCheck={false} placeholder="# One KEY=VALUE per line"
                        value={editing.envVarsText} onChange={set('envVarsText')} />

              {editorKeys.length > 0 && (
                <div className="env-secrets">
                  <div className="env-secrets-hd">
                    Secrets
                    <span className="env-secrets-hint">
                      {encryptionOn
                        ? 'marked values are write-only — stored encrypted and never shown again'
                        : 'marked values are write-only — but not encrypted at rest (see below)'}
                    </span>
                  </div>
                  {!encryptionOn && (
                    <div className="env-secrets-warn">
                      <Icons.warn />
                      <span>
                        No encryption key is configured, so secret values are stored
                        in <strong>plaintext</strong> at rest. Set <code>REPLAY_ENCRYPT_KEY</code> on
                        the control plane and runner (e.g. <code>openssl rand -hex 32</code>) to encrypt them.
                      </span>
                    </div>
                  )}
                  <div className="env-secrets-list">
                    {editorKeys.map(k => {
                      const isSecret = editing.secretKeys.includes(k)
                      return (
                        <label key={k} className="env-secret-row" data-on={isSecret ? '1' : '0'}>
                          <input type="checkbox" checked={isSecret} onChange={() => toggleSecret(k)} />
                          {isSecret ? <Icons.key /> : <Icons.globe />}
                          <code>{k}</code>
                        </label>
                      )
                    })}
                  </div>
                </div>
              )}

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
              Select an environment to edit, or create a new one.
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
