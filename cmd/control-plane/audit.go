package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Audit log writes are buffered through a single goroutine so the request
// path never blocks on disk. Under back-pressure we drop the oldest pending
// event — losing a few audit rows beats stalling user requests.
const (
	auditBufferSize  = 1024
	auditFlushSize   = 100
	auditFlushPeriod = 500 * time.Millisecond
)

type auditEvent struct {
	WorkspaceID string
	ActorID     string
	ActorKind   string
	Method      string
	Path        string
	Status      int
	IP          string
	UserAgent   string
	RequestID   string
	Metadata    map[string]any
	CreatedAt   time.Time
}

type auditSink struct {
	db     *sql.DB
	ch     chan auditEvent
	stopMu sync.Mutex
	stop   chan struct{}
}

var globalAudit *auditSink

func startAuditSink(ctx context.Context, db *sql.DB) {
	s := &auditSink{
		db:   db,
		ch:   make(chan auditEvent, auditBufferSize),
		stop: make(chan struct{}),
	}
	globalAudit = s
	go s.run(ctx)
}

func (s *auditSink) run(ctx context.Context) {
	batch := make([]auditEvent, 0, auditFlushSize)
	timer := time.NewTimer(auditFlushPeriod)
	defer timer.Stop()
	flushIfPending := func() {
		if len(batch) > 0 {
			s.writeBatch(ctx, batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case <-ctx.Done():
			flushIfPending()
			return
		case ev := <-s.ch:
			batch = append(batch, ev)
			if len(batch) >= auditFlushSize {
				flushIfPending()
			}
		case <-timer.C:
			flushIfPending()
			// Always reset — the timer is the heartbeat that catches the last
			// few events sitting in batch under low traffic. Skipping the
			// reset on empty batches strands future events until a full batch
			// triggers a flush manually (which may never happen).
			timer.Reset(auditFlushPeriod)
		}
	}
}

func (s *auditSink) writeBatch(ctx context.Context, events []auditEvent) {
	// One multi-row INSERT keeps round-trips minimal even at burst load.
	// Argument layout: $1..$11 per row.
	if len(events) == 0 {
		return
	}
	var sb strings.Builder
	sb.WriteString(`INSERT INTO audit_events
		(workspace_id, actor_id, actor_kind, method, path, status, ip, user_agent, request_id, metadata, created_at)
		VALUES `)
	args := make([]any, 0, len(events)*11)
	for i, ev := range events {
		if i > 0 {
			sb.WriteByte(',')
		}
		base := i*11 + 1
		// Use $base..$base+10 placeholders.
		sb.WriteString("($")
		sb.WriteString(strconv.Itoa(base))
		for k := 1; k < 11; k++ {
			sb.WriteString(",$")
			sb.WriteString(strconv.Itoa(base + k))
		}
		sb.WriteByte(')')
		var metaJSON any
		if ev.Metadata != nil {
			b, _ := json.Marshal(ev.Metadata)
			metaJSON = b
		}
		var ip any
		if ev.IP != "" {
			ip = ev.IP
		}
		args = append(args,
			ev.WorkspaceID, ev.ActorID, ev.ActorKind,
			ev.Method, ev.Path, ev.Status,
			ip, ev.UserAgent, ev.RequestID, metaJSON, ev.CreatedAt)
	}
	if _, err := s.db.ExecContext(ctx, sb.String(), args...); err != nil {
		slog.Warn("audit: batch insert failed", "error", err, "events", len(events))
	}
}

// emitAudit drops the event if the buffer is saturated. Sink lives for the
// life of the process; nil-check guards against pre-startup callers.
func emitAudit(ev auditEvent) {
	if globalAudit == nil {
		return
	}
	select {
	case globalAudit.ch <- ev:
	default:
		// Drop oldest by skipping this enqueue. We trade a missed audit row
		// against unbounded memory growth under burst.
	}
}

// ─── Per-request metadata bag ──────────────────────────────────────────

type auditMetaKey struct{}

// withAuditMeta returns a context with a fresh metadata bag attached.
func withAuditMeta(ctx context.Context) context.Context {
	return context.WithValue(ctx, auditMetaKey{}, &auditMetaBag{})
}

type auditMetaBag struct {
	mu sync.Mutex
	m  map[string]any
}

// AuditAttach lets a handler annotate the current request with a key/value
// pair that ends up in the audit_events.metadata JSONB. No-op if audit isn't
// running.
func AuditAttach(ctx context.Context, key string, value any) {
	bag, _ := ctx.Value(auditMetaKey{}).(*auditMetaBag)
	if bag == nil {
		return
	}
	bag.mu.Lock()
	defer bag.mu.Unlock()
	if bag.m == nil {
		bag.m = map[string]any{}
	}
	bag.m[key] = value
}

func metaFromCtx(ctx context.Context) map[string]any {
	bag, _ := ctx.Value(auditMetaKey{}).(*auditMetaBag)
	if bag == nil {
		return nil
	}
	bag.mu.Lock()
	defer bag.mu.Unlock()
	if len(bag.m) == 0 {
		return nil
	}
	out := make(map[string]any, len(bag.m))
	for k, v := range bag.m {
		out[k] = v
	}
	return out
}

// ─── Middleware ────────────────────────────────────────────────────────

// auditMiddleware records every mutating request (POST/PUT/PATCH/DELETE) plus
// any 4xx/5xx response on a non-mutating route — those are still security-
// relevant (e.g. someone probing endpoints with a stolen API key).
func auditMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Don't audit health checks, public auth/config probing, or our
			// own internal endpoints — too noisy and not security-relevant.
			if r.URL.Path == "/healthz" || r.URL.Path == "/api/auth/config" {
				next.ServeHTTP(w, r)
				return
			}
			ctx := withAuditMeta(r.Context())
			rec := &statusRecorder{ResponseWriter: w, status: 200}
			next.ServeHTTP(rec, r.WithContext(ctx))

			mutating := r.Method == http.MethodPost || r.Method == http.MethodPut ||
				r.Method == http.MethodPatch || r.Method == http.MethodDelete
			interesting := mutating || rec.status >= 400
			if !interesting {
				return
			}
			ar := authResultFromContext(ctx)
			workspaceID := ""
			actorID := "anonymous"
			actorKind := "anonymous"
			if ar != nil {
				workspaceID = ar.WorkspaceID
				actorID = ar.ActorID
				actorKind = ar.ActorKind
			}
			// Public-path handlers (webhooks) authenticate themselves and can
			// attach their resolved workspace via AuditAttach so the audit row
			// is correctly scoped instead of bucketing under the default
			// workspace + "anonymous".
			meta := metaFromCtx(ctx)
			if workspaceID == "" {
				if v, ok := meta["workspace_id"].(string); ok && v != "" {
					workspaceID = v
				}
			}
			if actorKind == "anonymous" {
				if v, ok := meta["actor_kind"].(string); ok && v != "" {
					actorKind = v
				}
				if v, ok := meta["actor_id"].(string); ok && v != "" {
					actorID = v
				}
			}
			if workspaceID == "" {
				workspaceID = defaultWorkspaceID
			}
			emitAudit(auditEvent{
				WorkspaceID: workspaceID,
				ActorID:     actorID,
				ActorKind:   actorKind,
				Method:      r.Method,
				Path:        r.URL.Path,
				Status:      rec.status,
				IP:          clientIP(r),
				UserAgent:   r.UserAgent(),
				RequestID:   middleware.GetReqID(ctx),
				Metadata:    meta,
				CreatedAt:   time.Now(),
			})
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		// Default 200 if handler called Write without WriteHeader.
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}

// Flush delegates to the wrapped writer so SSE handlers (agent chat/live)
// can stream. Without this, embedding http.ResponseWriter hides the
// underlying Flusher and `w.(http.Flusher)` fails ("streaming unsupported").
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ─── Read API + retention ──────────────────────────────────────────────

type auditEventResponse struct {
	ID          int64  `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	ActorID     string `json:"actor_id"`
	ActorKind   string `json:"actor_kind"`
	// ActorLabel is a human-readable identity resolved from actor_id: a user's
	// email or an API key's name. Empty for anonymous/unresolved actors.
	ActorLabel string          `json:"actor_label"`
	Method     string          `json:"method"`
	Path       string          `json:"path"`
	Status     int             `json:"status"`
	IP         string          `json:"ip,omitempty"`
	UserAgent  string          `json:"user_agent,omitempty"`
	RequestID  string          `json:"request_id,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

func registerAuditRoutes(r chi.Router, db *sql.DB) {
	r.Get("/api/audit-events", func(w http.ResponseWriter, req *http.Request) {
		workspaceID := workspaceFromContext(req.Context())
		limit := 100
		if v := req.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
				limit = n
			}
		}
		before := req.URL.Query().Get("before") // ISO timestamp; "" → now
		actor := req.URL.Query().Get("actor")
		pathQ := req.URL.Query().Get("path")

		var sb bytes.Buffer
		// Resolve a friendly actor label via text-keyed LEFT JOINs (no uuid cast,
		// so non-uuid actor_ids like "anonymous" don't error). Audit columns are
		// ae.-qualified to stay unambiguous against the joined tables.
		sb.WriteString(`SELECT ae.id, ae.workspace_id, ae.actor_id, ae.actor_kind, ae.method, ae.path, ae.status,
			COALESCE(host(ae.ip), ''), COALESCE(ae.user_agent, ''), COALESCE(ae.request_id, ''),
			COALESCE(u.email, k.name, ''), ae.metadata, ae.created_at
			FROM audit_events ae
			LEFT JOIN users u ON u.id::text = ae.actor_id
			LEFT JOIN api_keys k ON k.id::text = ae.actor_id
			WHERE ae.workspace_id = $1`)
		args := []any{workspaceID}
		if before != "" {
			sb.WriteString(` AND ae.created_at < $`)
			sb.WriteString(strconv.Itoa(len(args) + 1))
			args = append(args, before)
		}
		if actor != "" {
			sb.WriteString(` AND ae.actor_id = $`)
			sb.WriteString(strconv.Itoa(len(args) + 1))
			args = append(args, actor)
		}
		if pathQ != "" {
			sb.WriteString(` AND ae.path ILIKE $`)
			sb.WriteString(strconv.Itoa(len(args) + 1))
			args = append(args, "%"+pathQ+"%")
		}
		sb.WriteString(` ORDER BY ae.created_at DESC LIMIT $`)
		sb.WriteString(strconv.Itoa(len(args) + 1))
		args = append(args, limit)

		rows, err := db.QueryContext(req.Context(), sb.String(), args...)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		out := []auditEventResponse{}
		for rows.Next() {
			var ev auditEventResponse
			var meta sql.NullString
			if err := rows.Scan(&ev.ID, &ev.WorkspaceID, &ev.ActorID, &ev.ActorKind,
				&ev.Method, &ev.Path, &ev.Status,
				&ev.IP, &ev.UserAgent, &ev.RequestID, &ev.ActorLabel, &meta, &ev.CreatedAt); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if meta.Valid {
				ev.Metadata = json.RawMessage(meta.String)
			}
			out = append(out, ev)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
}

// gcAuditEvents deletes rows older than REPLAY_AUDIT_RETENTION_DAYS (default 90).
func gcAuditEvents(ctx context.Context, db *sql.DB) {
	days := 90
	if v := os.Getenv("REPLAY_AUDIT_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	tick := func() {
		res, err := db.ExecContext(ctx,
			`DELETE FROM audit_events WHERE created_at < now() - ($1::int * interval '1 day')`,
			days)
		if err != nil {
			slog.Warn("gc: audit_events delete failed", "error", err)
			return
		}
		if n, _ := res.RowsAffected(); n > 0 {
			slog.Info("gc: audit rows pruned", "count", n, "retention_days", days)
		}
	}
	tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
