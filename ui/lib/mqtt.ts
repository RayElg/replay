// MQTT-over-WebSocket client for the UI. pgmqtt publishes row-change events to
// MQTT topics (configured by migration 024); we connect from the browser and
// turn those events into live UI updates instead of polling.
//
// One persistent connection per tab, lazily opened on the first subscriber.
// Multiple React components can subscribe to the same or overlapping topics —
// the broker-side subscription is shared so we only SUB once per unique topic.
//
// Auth: we fetch a workspace-scoped username/password from /api/auth/broker-
// credentials and pass them in the MQTT CONNECT packet. pgmqtt validates the
// password against pg_authid (SCRAM-SHA-256). Browsers talk to the broker
// directly on the WS port — no shim sits in front.
//
// Configuration:
//   NEXT_PUBLIC_MQTT_WS_URL — explicit broker URL (e.g. ws://example.com:9001)
//   defaults to ws(s)://<host>:9001/mqtt

import mqtt, { MqttClient } from 'mqtt'

type Handler = (topic: string, payload: unknown) => void

interface Subscription {
  // Set of handlers attached to this topic pattern. We keep a Set so the same
  // handler reference is not double-fired if accidentally subscribed twice.
  handlers: Set<Handler>
}

const subscriptions = new Map<string, Subscription>()
let client: MqttClient | null = null
let connecting: Promise<MqttClient> | null = null
let refreshTimer: ReturnType<typeof setTimeout> | null = null

function defaultBrokerURL(): string {
  if (typeof window === 'undefined') return ''
  const fromEnv = process.env.NEXT_PUBLIC_MQTT_WS_URL
  if (fromEnv) return fromEnv
  const proto = window.location.protocol === 'https:' ? 'wss' : 'ws'
  return `${proto}://${window.location.hostname}:9001/mqtt`
}

// brokerHandshake returns the values the MQTT CONNECT packet needs. Two paths:
//   - SCRAM mode (Community pgmqtt): /api/auth/broker-credentials returns a
//     workspace-scoped username/password and the broker validates against
//     pg_authid. There are no broker-side topic ACLs — single-tenant trust is
//     the boundary (see README → "Multi-tenant deployments").
//   - Trusted-broker mode (pgmqtt Enterprise with the jwt feature): the broker
//     validates a short-lived Ed25519 JWT presented as the CONNECT password,
//     and enforces sub_claims topic ACLs natively.
//
// We pick by querying /api/auth/config; it's cached for the lifetime of the
// page load by the browser anyway, so the extra hop is essentially free.
// brokerHandshake returns CONNECT credentials plus, in JWT mode, the seconds
// until the token expires so the caller can schedule a refresh.
async function brokerHandshake(): Promise<{
  username: string
  password: string
  client_id?: string
  expires_in?: number
} | null> {
  const cfg = await fetch('/api/auth/config').then(r => r.ok ? r.json() : null).catch(() => null)
  if (cfg?.trusted_broker) {
    const res = await fetch('/api/auth/broker-token', { method: 'POST' })
    if (!res.ok) return null
    const j = await res.json()
    // pgmqtt's jwt path inspects only the password field; username is ignored
    // but most MQTT clients require something there. Send the client_id so it
    // also surfaces in broker logs.
    return {
      username: j.client_id,
      password: j.token,
      client_id: j.client_id,
      expires_in: j.expires_in,
    }
  }
  const res = await fetch('/api/auth/broker-credentials', { method: 'POST' })
  if (!res.ok) return null
  return res.json()
}

async function ensureClient(): Promise<MqttClient> {
  if (client) return client
  if (connecting) return connecting
  // Assign `connecting` synchronously: createConnection() returns its promise
  // before its first await, so concurrent callers — React StrictMode's
  // double-invoked effects, or two components subscribing on first paint —
  // share one in-flight connection instead of each racing past the guard and
  // opening a separate socket. Clear the guard if it fails so the next caller
  // retries rather than re-receiving a cached rejection.
  const pending = createConnection()
  connecting = pending
  pending.catch(() => { if (connecting === pending) connecting = null })
  return pending
}

async function createConnection(): Promise<MqttClient> {
  const url = defaultBrokerURL()
  if (!url) throw new Error('mqtt: no broker URL (SSR?)')

  const creds = await brokerHandshake()
  if (!creds) {
    throw new Error('mqtt: broker credentials unavailable (not signed in?)')
  }

  return new Promise<MqttClient>((resolve, reject) => {
    // clientId must be unique per connection — multiple tabs from one user need
    // distinct IDs or the broker forcibly disconnects the prior one. The
    // server mints a workspace-namespaced ID so a malicious tab in another
    // tenant can't guess and collide with this session. Fall back to a local
    // random if the server didn't supply one (older deployments).
    const cid = creds.client_id ?? `replay-ui-${Math.random().toString(36).slice(2, 10)}-${Date.now()}`
    const c = mqtt.connect(url, {
      clientId: cid,
      reconnectPeriod: 2_000,
      connectTimeout: 8_000,
      keepalive: 30,
      // Disable clean=false (default true) so the broker doesn't queue messages
      // for a disconnected client — we don't need missed-message replay because
      // the UI does a full /api/runs fetch on reconnect.
      clean: true,
      username: creds.username,
      password: creds.password,
    })
    c.on('connect', () => {
      client = c
      connecting = null
      // Re-subscribe everything: on reconnect the broker has no memory of our subs.
      for (const topic of subscriptions.keys()) {
        c.subscribe(topic, { qos: 0 })
      }
      // In JWT mode the password is a short-lived token. Tear the connection
      // down ~60s before exp so the next subscribe rebuilds with a fresh token.
      // SCRAM mode has no expires_in so this is a no-op.
      if (refreshTimer) clearTimeout(refreshTimer)
      if (creds.expires_in && creds.expires_in > 60) {
        refreshTimer = setTimeout(() => {
          if (client === c) {
            client = null
            c.end(true)
          }
          // Lazily rebuild on next subscribe — open subscriptions will be
          // restored by the new client's on('connect') handler.
          void ensureClient()
        }, (creds.expires_in - 60) * 1000)
      }
      resolve(c)
    })
    c.on('message', (topic, payload) => {
      let parsed: unknown
      try { parsed = JSON.parse(payload.toString('utf8')) } catch { parsed = null }
      // Dispatch to every pattern whose topic-filter matches.
      for (const [pattern, sub] of subscriptions) {
        if (topicMatches(pattern, topic)) {
          for (const h of sub.handlers) {
            try { h(topic, parsed) } catch (e) { console.error('mqtt handler threw', e) }
          }
        }
      }
    })
    c.on('error', (err) => {
      console.warn('mqtt error', err.message)
      // Don't reject after first connect — mqtt.js handles reconnects internally.
      if (!client) {
        connecting = null
        reject(err)
      }
    })
  })
}

// topicMatches implements MQTT wildcard matching for the small subset we use:
//   +  — single-level wildcard
//   #  — multi-level wildcard (must be the last segment)
function topicMatches(pattern: string, topic: string): boolean {
  const p = pattern.split('/')
  const t = topic.split('/')
  for (let i = 0; i < p.length; i++) {
    if (p[i] === '#') return true
    if (i >= t.length) return false
    if (p[i] === '+') continue
    if (p[i] !== t[i]) return false
  }
  return p.length === t.length
}

// subscribe registers a handler for a topic pattern. Returns an unsubscribe
// function — call it on cleanup (React effect return). Handler is invoked with
// the concrete topic that matched (useful when subscribing to + or #).
export function subscribe(pattern: string, handler: Handler): () => void {
  let sub = subscriptions.get(pattern)
  if (!sub) {
    sub = { handlers: new Set() }
    subscriptions.set(pattern, sub)
    ensureClient().then(c => c.subscribe(pattern, { qos: 0 })).catch(() => {})
  }
  sub.handlers.add(handler)

  return () => {
    const s = subscriptions.get(pattern)
    if (!s) return
    s.handlers.delete(handler)
    if (s.handlers.size === 0) {
      subscriptions.delete(pattern)
      // Best-effort unsubscribe on the broker side. If the client isn't
      // connected yet there's nothing to unsubscribe — ensureClient will skip
      // re-subscribing because the pattern is gone from `subscriptions`.
      client?.unsubscribe(pattern)
    }
  }
}
