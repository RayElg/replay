// Proxy for the auto-triage live SSE stream. Forwards GET requests to the
// control-plane agentBus endpoint so clients receive events in real time.

import { NextRequest } from 'next/server'

export const dynamic = 'force-dynamic'
export const runtime = 'nodejs'

const CONTROL_PLANE_URL = process.env.CONTROL_PLANE_URL || 'http://control-plane:8080'

export async function GET(req: NextRequest, { params }: { params: Promise<{ id: string }> }) {
  const { id } = await params
  const upstream = await fetch(`${CONTROL_PLANE_URL}/api/runs/${id}/agent/live`, {
    signal: req.signal,
    headers: { cookie: req.headers.get('cookie') ?? '' },
  })
  if (!upstream.body) {
    return new Response(await upstream.text(), { status: upstream.status })
  }
  return new Response(upstream.body, {
    status: upstream.status,
    headers: {
      'Content-Type': 'text/event-stream',
      'Cache-Control': 'no-cache, no-transform',
      'Connection': 'keep-alive',
      'X-Accel-Buffering': 'no',
    },
  })
}
