'use client'
import { useState, useEffect } from 'react'
import { useRouter, useParams } from 'next/navigation'
import { Icons } from '@/components/icons'

// /invite/{token} — public landing page for an emailed invite. The backend
// route /api/invites/accept verifies the token, creates the user, marks the
// invite consumed, and returns a session cookie in one shot — so the success
// path is "submit → redirected to /" with the user signed in. No additional
// /login round-trip.
//
// Errors we surface specifically because the user can do something about them:
//   404 — bad/used token: tell them to ask for a new invite
//   410 — expired:        same, but call out the expiry case
//   400 — weak password:  tell them the minimum length
// Anything else gets the generic error text from the server.
export default function InviteAcceptPage() {
  const router = useRouter()
  const params = useParams<{ token: string }>()
  const token = params?.token ?? ''
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // Probed once on mount — if the URL is missing a token we don't even render
  // a form. Cheap UX guard so back-button traffic on a broken link doesn't
  // post against the API.
  const [tokenValid, setTokenValid] = useState<boolean | null>(null)

  useEffect(() => {
    setTokenValid(Boolean(token && token.length > 4))
  }, [token])

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    if (password !== confirm) { setError('Passwords do not match.'); return }
    if (password.length < 8)  { setError('Password must be at least 8 characters.'); return }
    setBusy(true)
    try {
      const res = await fetch(`/api/invites/accept?token=${encodeURIComponent(token)}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ password, name }),
      })
      if (res.ok) {
        // Server set the session cookie — straight into the app.
        router.replace('/')
        return
      }
      if (res.status === 404) setError('This invite link is invalid or has already been used. Ask whoever invited you to send a new one.')
      else if (res.status === 410) setError('This invite has expired. Ask for a new one.')
      else setError((await res.text()) || `HTTP ${res.status}`)
    } catch {
      setError('Network error — try again.')
    } finally {
      setBusy(false)
    }
  }

  if (tokenValid === null) return <div className="auth-shell"><div className="auth-card" /></div>
  if (tokenValid === false) {
    return (
      <div className="auth-shell">
        <div className="auth-card">
          <div className="auth-brand"><div className="logo">R</div><span>replay</span></div>
          <h1 className="auth-title">Bad invite link</h1>
          <p style={{ fontSize: 12.5, color: 'var(--text-3)' }}>
            This URL doesn&rsquo;t include a valid invite token.
          </p>
        </div>
      </div>
    )
  }

  return (
    <div className="auth-shell">
      <div className="auth-card">
        <div className="auth-brand"><div className="logo">R</div><span>replay</span></div>
        <h1 className="auth-title">Accept invitation</h1>
        <p style={{ fontSize: 12.5, color: 'var(--text-3)', marginTop: -8, marginBottom: 12 }}>
          Set a password to join the workspace. You&rsquo;ll be signed in automatically.
        </p>
        <form onSubmit={submit} className="auth-form">
          <label className="auth-label">
            <span>Display name <em style={{ color: 'var(--text-3)' }}>(optional)</em></span>
            <input type="text" value={name} onChange={e => setName(e.target.value)} autoFocus />
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
            {busy ? 'Accepting…' : <>Accept &amp; sign in <Icons.chevR /></>}
          </button>
        </form>
      </div>
    </div>
  )
}
