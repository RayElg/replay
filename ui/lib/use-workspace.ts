'use client'
import { useEffect, useState } from 'react'

interface Workspace {
  id: string
  name: string
  slug: string
}

// Module-level cache: the workspace doesn't change during a session and
// composing MQTT topics from it is a hot path, so we want a synchronous answer
// after the first fetch.
let cached: Workspace | null = null
let inflight: Promise<Workspace | null> | null = null

async function fetchOnce(): Promise<Workspace | null> {
  if (cached) return cached
  if (inflight) return inflight
  inflight = fetch('/api/workspaces/current')
    .then(r => r.ok ? r.json() as Promise<Workspace> : null)
    .then(w => { if (w) cached = w; inflight = null; return w })
    .catch(() => { inflight = null; return null })
  return inflight
}

export function useWorkspace(): Workspace | null {
  const [ws, setWs] = useState<Workspace | null>(cached)
  useEffect(() => {
    if (cached) { setWs(cached); return }
    let alive = true
    fetchOnce().then(w => { if (alive) setWs(w) })
    return () => { alive = false }
  }, [])
  return ws
}
