'use client'

// Lightweight client-side auth shim. We don't ship a real auth library because
// every authenticated route goes through the same /api/auth/me probe — keep
// the surface tiny.

import { useEffect, useState } from 'react'

let cachedMe: Me | null = null
let inflight: Promise<Me | null> | null = null

export interface Me {
  user_id: string
  email: string
  workspace_id: string
}

export async function fetchMe(): Promise<Me | null> {
  if (cachedMe) return cachedMe
  if (inflight) return inflight
  inflight = fetch('/api/auth/me')
    .then(r => r.ok ? r.json() as Promise<Me> : null)
    .then(m => { if (m) cachedMe = m; inflight = null; return m })
    .catch(() => { inflight = null; return null })
  return inflight
}

export function clearMeCache() { cachedMe = null }

// useAuthGuard redirects to /login if the caller is not authenticated.
// Returns `null` while the check is in flight so callers can short-circuit
// rendering the main shell.
export function useAuthGuard(): { authed: boolean; me: Me | null } | null {
  const [state, setState] = useState<{ authed: boolean; me: Me | null } | null>(
    cachedMe ? { authed: true, me: cachedMe } : null,
  )
  useEffect(() => {
    let alive = true
    fetchMe().then(me => {
      if (!alive) return
      if (me) setState({ authed: true, me })
      else {
        // Don't use next/navigation here — the import path adds complexity
        // and a raw redirect is sufficient pre-shell.
        window.location.replace('/login')
      }
    })
    return () => { alive = false }
  }, [])
  return state
}

// authedFetch wraps fetch with a 401 → /login bounce. Use anywhere a real
// authenticated call could fire — keeps the redirect logic out of every
// component.
export async function authedFetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
  const res = await fetch(input, init)
  if (res.status === 401) {
    clearMeCache()
    if (typeof window !== 'undefined' && !window.location.pathname.startsWith('/login')
        && !window.location.pathname.startsWith('/setup')) {
      window.location.replace('/login')
    }
  }
  return res
}
