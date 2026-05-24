'use client'
import { useEffect, useState } from 'react'
import { useRouter } from 'next/navigation'
import { Icons } from '@/components/icons'

export default function SetupPage() {
  const router = useRouter()
  const [allowed, setAllowed] = useState<boolean | null>(null)
  const [email, setEmail] = useState('')
  const [name, setName] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    fetch('/api/auth/config').then(r => r.ok ? r.json() : null).then(c => {
      // Setup is only meaningful before any user exists.
      if (!c) { setAllowed(false); return }
      if (c.has_users) {
        setAllowed(false)
        router.replace('/login')
        return
      }
      setAllowed(true)
    })
  }, [router])

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    if (password !== confirm) { setError('Passwords do not match.'); return }
    if (password.length < 8)  { setError('Password must be at least 8 characters.'); return }
    setBusy(true)
    try {
      const res = await fetch('/api/auth/setup', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email, name, password }),
      })
      if (res.ok) {
        router.replace('/')
        return
      }
      setError(await res.text() || `HTTP ${res.status}`)
    } finally {
      setBusy(false)
    }
  }

  if (allowed === null) return <div className="auth-shell"><div className="auth-card" /></div>
  if (allowed === false) return null

  return (
    <div className="auth-shell">
      <div className="auth-card">
        <div className="auth-brand"><div className="logo">R</div><span>replay</span></div>
        <h1 className="auth-title">Welcome — create the first user</h1>
        <p style={{ fontSize: 12.5, color: 'var(--text-3)', marginTop: -8, marginBottom: 12 }}>
          This account will own the workspace. Additional users can be added later.
        </p>
        <form onSubmit={submit} className="auth-form">
          <label className="auth-label">
            <span>Email</span>
            <input type="email" required autoFocus value={email} onChange={e => setEmail(e.target.value)} />
          </label>
          <label className="auth-label">
            <span>Display name <em style={{ color: 'var(--text-3)' }}>(optional)</em></span>
            <input type="text" value={name} onChange={e => setName(e.target.value)} />
          </label>
          <label className="auth-label">
            <span>Password</span>
            <input type="password" required autoComplete="new-password"
                   value={password} onChange={e => setPassword(e.target.value)} />
          </label>
          <label className="auth-label">
            <span>Confirm password</span>
            <input type="password" required autoComplete="new-password"
                   value={confirm} onChange={e => setConfirm(e.target.value)} />
          </label>
          {error && <div className="auth-error">{error}</div>}
          <button className="btn primary" type="submit" disabled={busy}>
            {busy ? 'Creating…' : <>Create account <Icons.chevR /></>}
          </button>
        </form>
      </div>
    </div>
  )
}
