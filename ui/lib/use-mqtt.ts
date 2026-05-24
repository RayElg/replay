'use client'
import { useEffect, useRef } from 'react'
import { subscribe } from './mqtt'

// useMqttTopic subscribes to an MQTT topic pattern (with + / # wildcards) and
// invokes `handler` for every matching message. Re-renders do not re-subscribe:
// we keep the handler in a ref and only resubscribe when `topic` changes.
//
// Pass `null` as topic to skip subscribing — useful when a topic depends on
// state that may be unset (e.g. no selected run).
export function useMqttTopic<T = unknown>(
  topic: string | null,
  handler: (topic: string, payload: T) => void,
) {
  const handlerRef = useRef(handler)
  handlerRef.current = handler

  useEffect(() => {
    if (!topic) return
    const unsub = subscribe(topic, (t, p) => handlerRef.current(t, p as T))
    return unsub
  }, [topic])
}
