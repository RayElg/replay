package main

import (
	"encoding/json"
	"fmt"
	"sync"
)

// agentBus routes SSE-formatted events from background agent runs (auto-triage)
// to any UI clients watching the same root run. Keyed by root_run_id.
type eventBus struct {
	mu   sync.Mutex
	subs map[string][]chan []byte
}

var agentBus = &eventBus{subs: make(map[string][]chan []byte)}

func (b *eventBus) subscribe(runID string) chan []byte {
	ch := make(chan []byte, 256)
	b.mu.Lock()
	b.subs[runID] = append(b.subs[runID], ch)
	b.mu.Unlock()
	return ch
}

func (b *eventBus) unsubscribe(runID string, ch chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	subs := b.subs[runID]
	for i, s := range subs {
		if s == ch {
			b.subs[runID] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	if len(b.subs[runID]) == 0 {
		delete(b.subs, runID)
	}
}

// publish sends raw SSE bytes to all subscribers. Drops if a subscriber's buffer is full.
func (b *eventBus) publish(runID string, p []byte) {
	b.mu.Lock()
	subs := make([]chan []byte, len(b.subs[runID]))
	copy(subs, b.subs[runID])
	b.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- p:
		default:
		}
	}
}

// eventEmitter abstracts the SSE write target so runAgentTurn works for both
// interactive HTTP (httpEmitter) and background auto-triage (busEmitter).
type eventEmitter interface {
	emit(event string, payload any)
}

// httpEmitter writes SSE directly to an HTTP response and flushes immediately.
type httpEmitter struct {
	w       interface{ Write([]byte) (int, error) }
	flusher interface{ Flush() }
}

func (e *httpEmitter) emit(event string, payload any) {
	writeSSE(e.w, event, payload)
	e.flusher.Flush()
}

// busEmitter publishes SSE events to all bus subscribers for the given run.
type busEmitter struct {
	runID string
}

func (e *busEmitter) emit(event string, payload any) {
	data, _ := json.Marshal(payload)
	agentBus.publish(e.runID, []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, data)))
}
