'use client'
import { useEffect, useRef, useState } from 'react'
import { useRouter } from 'next/navigation'
import { fetchMe, type Me } from '@/lib/auth'

export function AccountMenu() {
  const router = useRouter()
  const [me, setMe] = useState<Me | null>(null)
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)

  // Reuse the cached probe from lib/auth so we don't re-hit /api/auth/me on
  // every component mount. The auth guard on the parent has already kicked it.
  useEffect(() => {
    fetchMe().then(setMe).catch(() => {})
  }, [])

  useEffect(() => {
    if (!open) return
    const close = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false)
    }
    window.addEventListener('mousedown', close)
    return () => window.removeEventListener('mousedown', close)
  }, [open])

  const logout = async () => {
    await fetch('/api/auth/logout', { method: 'POST' }).catch(() => {})
    router.replace('/login')
  }

  // Don't render until we know who the user is — keeps the avatar from
  // briefly flashing a placeholder before the email lands.
  if (!me) return null

  const initials = (me.email || '?')[0]?.toUpperCase() ?? '?'

  return (
    <div className="account-menu" ref={ref}>
      <button className="account-trigger" onClick={() => setOpen(o => !o)}>
        <span className="account-avatar">{initials}</span>
        <span style={{ fontSize: 12 }}>{me.email.split('@')[0]}</span>
      </button>
      {open && (
        <div className="account-pop">
          <div className="who">
            <div className="who-email">{me.email}</div>
          </div>
          <button onClick={logout} className="danger">Sign out</button>
        </div>
      )}
    </div>
  )
}
