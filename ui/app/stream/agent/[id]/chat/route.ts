// Streaming proxy for the agent chat SSE endpoint.
//
// Next's built-in rewrites buffer SSE responses in dev mode (the response body
// only flushes to the browser after several events accumulate, which makes the
// chat feel non-streaming). This route forwards the request to the control-plane
// and pipes the response body through as a ReadableStream so deltas arrive at
// the browser as soon as the control-plane emits them.

import { NextRequest } from 'next/server'

export const dynamic = 'force-dynamic'
export const runtime = 'nodejs'

const CONTROL_PLANE_URL = process.env.CONTROL_PLANE_URL || 'http://control-plane:8080'

export async function POST(req: NextRequest, { params }: { params: Promise<{ id: string }> }) {
  const { id } = await params
  const body = await req.text()
  const upstream = await fetch(`${CONTROL_PLANE_URL}/api/runs/${id}/agent/chat`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      cookie: req.headers.get('cookie') ?? '',
    },
    body,
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
