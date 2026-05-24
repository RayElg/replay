'use client'
import { useState, useEffect } from 'react'
import { useRouter, useParams } from 'next/navigation'
import Link from 'next/link'
import { Icons } from '@/components/icons'

// /reset/{token} — second leg of the reset flow. The first leg is now the
// operator running `control-plane reset-link --email user@example.com` and
// handing the URL to the user; this page consumes the token.
//
// Backend doesn't expose a "does this token exist" probe (would let an
// attacker brute-force valid tokens cheaply), so we don't pre-validate. The
// submit either succeeds and bounces to /login, or it errors with a status
// code we map.
export default function ResetConfirmPage() {
  const router = useRouter()
  const params = useParams<{ token: string }>()
  const token = params?.token ?? ''
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)
  const [done, setDone] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [tokenPresent, setTokenPresent] = useState<boolean | null>(null)

  useEffect(() => { setTokenPresent(Boolean(token && token.length > 4)) }, [token])

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    if (password !== confirm) { setError('Passwords do not match.'); return }
    if (password.length < 8) { setError('Password must be at least 8 characters.'); return }
    setBusy(true)
    try {
      const res = await fetch('/api/auth/password/reset/confirm', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ token, new_password: password }),
      })
      if (res.ok) { setDone(true); return }
      if (res.status === 404) setError('This reset link is invalid or already used. Ask your administrator for a new one.')
      else if (res.status === 410) setError('This reset link has expired. Ask your administrator for a new one.')
      else setError((await res.text()) || `HTTP ${res.status}`)
    } catch {
      setError('Network error — try again.')
    } finally {
      setBusy(false)
    }
  }

  if (tokenPresent === null) return <div className="auth-shell"><div className="auth-card" /></div>
  if (tokenPresent === false) {
    return (
      <div className="auth-shell">
        <div className="auth-card">
          <div className="auth-brand"><div className="logo">R</div><span>replay</span></div>
          <h1 className="auth-title">Bad reset link</h1>
          <p style={{ fontSize: 12.5, color: 'var(--text-3)' }}>
            This URL is missing a valid reset token. Ask your administrator to
            generate a new one with <code>control-plane reset-link</code>.
          </p>
          <Link href="/login" className="btn primary" style={{ textDecoration: 'none', justifyContent: 'center' }}>
            Back to sign in <Icons.chevR />
          </Link>
        </div>
      </div>
    )
  }

  if (done) {
    return (
      <div className="auth-shell">
        <div className="auth-card">
          <div className="auth-brand"><div className="logo">R</div><span>replay</span></div>
          <h1 className="auth-title">Password updated</h1>
          <p style={{ fontSize: 12.5, color: 'var(--text-3)', marginTop: -8 }}>
            All your existing sessions were signed out. Use the new password to sign back in.
          </p>
          <button className="btn primary" onClick={() => router.replace('/login')}
                  style={{ justifyContent: 'center' }}>
            Sign in <Icons.chevR />
          </button>
        </div>
      </div>
    )
  }

  return (
    <div className="auth-shell">
      <div className="auth-card">
        <div className="auth-brand"><div className="logo">R</div><span>replay</span></div>
        <h1 className="auth-title">Set a new password</h1>
        <form onSubmit={submit} className="auth-form">
          <label className="auth-label">
            <span>New password</span>
            <input type="password" required autoComplete="new-password" autoFocus
                   value={password} onChange={e => setPassword(e.target.value)} />
          </label>
          <label className="auth-label">
            <span>Confirm</span>
            <input type="password" required autoComplete="new-password"
                   value={confirm} onChange={e => setConfirm(e.target.value)} />
          </label>
          {error && <div className="auth-error">{error}</div>}
          <button className="btn primary" type="submit" disabled={busy}>
            {busy ? 'Updating…' : <>Update password <Icons.chevR /></>}
          </button>
        </form>
      </div>
    </div>
  )
}
