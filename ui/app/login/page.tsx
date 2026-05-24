'use client'
import { useEffect, useState } from 'react'
import { useRouter } from 'next/navigation'
import { Icons } from '@/components/icons'

interface AuthConfig {
  has_users: boolean
}

export default function LoginPage() {
  const router = useRouter()
  const [config, setConfig] = useState<AuthConfig | null>(null)
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    fetch('/api/auth/config').then(r => r.ok ? r.json() : null).then((c: AuthConfig | null) => {
      if (!c) return
      setConfig(c)
      // First-run on a fresh install — bounce to /setup before the user sees the login form.
      if (!c.has_users) router.replace('/setup')
    })
  }, [router])

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    setBusy(true)
    try {
      const res = await fetch('/api/auth/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email, password }),
      })
      if (res.ok) {
        router.replace('/')
        return
      }
      if (res.status === 429) {
        const retry = res.headers.get('Retry-After')
        setError(`Too many attempts. Try again in ${retry ?? 'a few minutes'}.`)
      } else {
        setError(res.status === 401 ? 'Invalid email or password.' : await res.text())
      }
    } finally {
      setBusy(false)
    }
  }

  if (!config) return <div className="auth-shell"><div className="auth-card" /></div>

  return (
    <div className="auth-shell">
      <div className="auth-card">
        <div className="auth-brand">
          <div className="logo">R</div>
          <span>replay</span>
        </div>
        <h1 className="auth-title">Sign in</h1>

        <form onSubmit={submit} className="auth-form">
          <label className="auth-label">
            <span>Email</span>
            <input
              type="email"
              autoComplete="username"
              value={email}
              onChange={e => setEmail(e.target.value)}
              required
              autoFocus
            />
          </label>
          <label className="auth-label">
            <span>Password</span>
            <input
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={e => setPassword(e.target.value)}
              required
            />
          </label>
          {error && <div className="auth-error">{error}</div>}
          <button className="btn primary" type="submit" disabled={busy}>
            {busy ? 'Signing in…' : <>Sign in <Icons.chevR /></>}
          </button>
          {/* No "forgot password" link — self-hosted resets are an operator
              concern (`control-plane reset-link --email …` produces a link
              the admin delivers out-of-band). */}
        </form>
      </div>
    </div>
  )
}
