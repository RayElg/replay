package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookieName  = "replay_session"
	defaultSessionDays = 30 // overridable via REPLAY_SESSION_LIFETIME_DAYS
	bcryptCost         = 12
)

// sessionLifetime is read once at first use so an operator can set
// REPLAY_SESSION_LIFETIME_DAYS for stricter (e.g. 7-day) or looser (e.g.
// 90-day) policy. Invalid values fall back to the default.
var sessionLifetime = func() time.Duration {
	if v := os.Getenv("REPLAY_SESSION_LIFETIME_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
			return time.Duration(n) * 24 * time.Hour
		}
	}
	return defaultSessionDays * 24 * time.Hour
}()

type passwordAuth struct {
	db *sql.DB
}

func newPasswordAuth(db *sql.DB) Authenticator { return &passwordAuth{db: db} }

func (a *passwordAuth) Name() string { return "password" }

func (a *passwordAuth) Authenticate(r *http.Request) (*AuthResult, error) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return nil, errAnonymous
	}
	var userID, workspaceID, email string
	var expiresAt time.Time
	err = a.db.QueryRowContext(r.Context(), `
		SELECT s.user_id, s.workspace_id, s.expires_at, u.email
		  FROM user_sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.id = $1`, c.Value,
	).Scan(&userID, &workspaceID, &expiresAt, &email)
	if err == sql.ErrNoRows {
		return nil, errAnonymous
	}
	if err != nil {
		slog.Error("password: session lookup failed", "error", err)
		return nil, errAnonymous
	}
	if time.Now().After(expiresAt) {
		// Async cleanup; treat as anonymous so caller can re-login.
		go func(sid string) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, _ = a.db.ExecContext(ctx, `DELETE FROM user_sessions WHERE id = $1`, sid)
		}(c.Value)
		return nil, errAnonymous
	}
	// Touch last_seen — debounced so we don't UPDATE on every request.
	touchSession(a.db, c.Value)
	return &AuthResult{
		WorkspaceID: workspaceID,
		ActorID:     userID,
		ActorKind:   "password",
		SessionID:   c.Value,
		Email:       email,
	}, nil
}

// ─── last_seen debounce ────────────────────────────────────────────────

// touchedSessions records the last time we issued an UPDATE for each session,
// keyed by session ID. Lets us defer hot UPDATEs to once-per-minute under
// active use without inventing a real cache layer.
var (
	touchedSessionsMu sync.Mutex
	touchedSessions   = map[string]time.Time{}
)

func touchSession(db *sql.DB, sessionID string) {
	touchedSessionsMu.Lock()
	if last, ok := touchedSessions[sessionID]; ok && time.Since(last) < time.Minute {
		touchedSessionsMu.Unlock()
		return
	}
	touchedSessions[sessionID] = time.Now()
	touchedSessionsMu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = db.ExecContext(ctx, `UPDATE user_sessions SET last_seen_at = now() WHERE id = $1`, sessionID)
	}()
}

// ─── login rate limiter ────────────────────────────────────────────────

// Login attempts are tracked per (email, source-IP) tuple with a sliding
// window. Survives only within a single process — acceptable for v1, since
// the threshold (5/min) is well below what any real attacker would need.
type loginAttempt struct {
	timestamps []time.Time
}

var (
	loginAttemptsMu sync.Mutex
	loginAttempts   = map[string]*loginAttempt{}
)

const (
	loginMaxAttempts = 5
	loginWindow      = 5 * time.Minute
)

// loginAttemptKey combines email + IP so a single shared IP doesn't lock
// every account, while an attacker rotating emails on one IP still trips
// the limit.
func loginAttemptKey(email, ip string) string { return strings.ToLower(email) + "|" + ip }

func recordLoginAttempt(key string) (allowed bool, retryAfter time.Duration) {
	loginAttemptsMu.Lock()
	defer loginAttemptsMu.Unlock()
	now := time.Now()
	a := loginAttempts[key]
	if a == nil {
		a = &loginAttempt{}
		loginAttempts[key] = a
	}
	// Drop entries outside the window.
	cut := now.Add(-loginWindow)
	keep := a.timestamps[:0]
	for _, t := range a.timestamps {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	a.timestamps = keep
	if len(a.timestamps) >= loginMaxAttempts {
		return false, loginWindow - now.Sub(a.timestamps[0])
	}
	a.timestamps = append(a.timestamps, now)
	return true, 0
}

func clearLoginAttempts(key string) {
	loginAttemptsMu.Lock()
	delete(loginAttempts, key)
	loginAttemptsMu.Unlock()
}

// ─── HTTP routes ───────────────────────────────────────────────────────

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type setupRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
}

type meResponse struct {
	UserID      string `json:"user_id"`
	Email       string `json:"email"`
	WorkspaceID string `json:"workspace_id"`
}

type changePasswordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

func registerPasswordAuthRoutes(r chi.Router, db *sql.DB) {
	r.Post("/api/auth/login", func(w http.ResponseWriter, req *http.Request) {
		var body loginRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		body.Email = strings.ToLower(strings.TrimSpace(body.Email))
		if body.Email == "" || body.Password == "" {
			http.Error(w, "email and password required", http.StatusBadRequest)
			return
		}
		ip := clientIP(req)
		key := loginAttemptKey(body.Email, ip)
		if ok, retry := recordLoginAttempt(key); !ok {
			w.Header().Set("Retry-After", retry.Round(time.Second).String())
			http.Error(w, "too many login attempts", http.StatusTooManyRequests)
			return
		}
		userID, workspaceID, ok, err := verifyPassword(req.Context(), db, body.Email, body.Password)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		clearLoginAttempts(key)

		sid, err := createUserSession(req.Context(), db, userID, workspaceID, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		setSessionCookie(w, req, sid)
		w.WriteHeader(http.StatusNoContent)
	})

	r.Post("/api/auth/logout", func(w http.ResponseWriter, req *http.Request) {
		c, err := req.Cookie(sessionCookieName)
		if err == nil && c.Value != "" {
			_, _ = db.ExecContext(req.Context(), `DELETE FROM user_sessions WHERE id = $1`, c.Value)
		}
		clearSessionCookie(w, req)
		w.WriteHeader(http.StatusNoContent)
	})

	r.Post("/api/auth/password", func(w http.ResponseWriter, req *http.Request) {
		ar := authResultFromContext(req.Context())
		if ar == nil || ar.ActorKind != "password" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body changePasswordRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if len(body.NewPassword) < 8 {
			http.Error(w, "new password must be at least 8 chars", http.StatusBadRequest)
			return
		}
		_, _, ok, err := verifyPassword(req.Context(), db, ar.Email, body.OldPassword)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "current password is incorrect", http.StatusUnauthorized)
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(body.NewPassword), bcryptCost)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, err = db.ExecContext(req.Context(),
			`UPDATE users SET password_hash = $1 WHERE id = $2`, string(hash), ar.ActorID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Revoke all other sessions for this user — the password they had is gone.
		_, _ = db.ExecContext(req.Context(),
			`DELETE FROM user_sessions WHERE user_id = $1 AND id <> $2`, ar.ActorID, ar.SessionID)
		w.WriteHeader(http.StatusNoContent)
	})

	r.Post("/api/auth/setup", func(w http.ResponseWriter, req *http.Request) {
		// Setup is allowed only when there are zero users with a password.
		// After that, account creation has to go through an authenticated
		// flow (which we don't expose yet — first-run-only).
		var n int
		if err := db.QueryRowContext(req.Context(),
			`SELECT COUNT(*) FROM users WHERE password_hash IS NOT NULL`).Scan(&n); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if n > 0 {
			http.Error(w, "setup already completed", http.StatusConflict)
			return
		}
		var body setupRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		body.Email = strings.ToLower(strings.TrimSpace(body.Email))
		body.Name = strings.TrimSpace(body.Name)
		if body.Email == "" || body.Password == "" {
			http.Error(w, "email and password required", http.StatusBadRequest)
			return
		}
		if len(body.Password) < 8 {
			http.Error(w, "password must be at least 8 chars", http.StatusBadRequest)
			return
		}
		if body.Name == "" {
			body.Name = body.Email
		}
		userID, err := createOrUpdateUser(req.Context(), db, defaultWorkspaceID, body.Email, body.Name, body.Password)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		sid, err := createUserSession(req.Context(), db, userID, defaultWorkspaceID, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		setSessionCookie(w, req, sid)
		w.WriteHeader(http.StatusNoContent)
	})
}

// ─── Helpers ───────────────────────────────────────────────────────────

func verifyPassword(ctx context.Context, db *sql.DB, email, password string) (userID, workspaceID string, ok bool, err error) {
	var hash sql.NullString
	err = db.QueryRowContext(ctx, `
		SELECT id, workspace_id, password_hash
		  FROM users WHERE lower(email) = lower($1)
	`, email).Scan(&userID, &workspaceID, &hash)
	if err == sql.ErrNoRows {
		// Constant-time pretend-hash to avoid timing-based user enumeration.
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$12$abcdefghijklmnopqrstuv0000000000000000000000000000000000"), []byte(password))
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	if !hash.Valid {
		return "", "", false, nil
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash.String), []byte(password)); err != nil {
		return "", "", false, nil
	}
	// Bump last_login_at; not critical-path so async is fine.
	go func(id string) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = db.ExecContext(ctx, `UPDATE users SET last_login_at = now() WHERE id = $1`, id)
	}(userID)
	return userID, workspaceID, true, nil
}

func createUserSession(ctx context.Context, db *sql.DB, userID, workspaceID string, r *http.Request) (string, error) {
	var sid string
	ua := r.UserAgent()
	ip := clientIP(r)
	var ipArg interface{}
	if ip != "" {
		ipArg = ip
	}
	err := db.QueryRowContext(ctx, `
		INSERT INTO user_sessions (user_id, workspace_id, expires_at, user_agent, ip)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		userID, workspaceID, time.Now().Add(sessionLifetime), ua, ipArg,
	).Scan(&sid)
	return sid, err
}

func createOrUpdateUser(ctx context.Context, db *sql.DB, workspaceID, email, name, password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", err
	}
	var id string
	err = db.QueryRowContext(ctx, `
		INSERT INTO users (workspace_id, email, name, password_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (workspace_id, email) DO UPDATE SET
			password_hash = EXCLUDED.password_hash,
			name = EXCLUDED.name
		RETURNING id`,
		workspaceID, email, name, string(hash),
	).Scan(&id)
	return id, err
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, sid string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		Secure:   cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionLifetime),
		MaxAge:   int(sessionLifetime.Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// requestIsHTTPS reports whether the request reached us over TLS, honouring a
// terminating proxy's X-Forwarded-Proto header.
func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// cookieSecure decides whether to set the Secure flag. REPLAY_COOKIE_SECURE
// forces it on/off; absent, auto-detect from r.TLS or X-Forwarded-Proto.
func cookieSecure(r *http.Request) bool {
	switch strings.ToLower(os.Getenv("REPLAY_COOKIE_SECURE")) {
	case "true", "1":
		return true
	case "false", "0":
		return false
	}
	return requestIsHTTPS(r)
}

func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		if i := strings.IndexByte(f, ','); i >= 0 {
			return strings.TrimSpace(f[:i])
		}
		return strings.TrimSpace(f)
	}
	if a := r.Header.Get("X-Real-IP"); a != "" {
		return a
	}
	host, _, ok := strings.Cut(r.RemoteAddr, ":")
	if ok {
		return host
	}
	return r.RemoteAddr
}

// ─── Bootstrap ─────────────────────────────────────────────────────────

// bootstrapUserFromEnv reads REPLAY_BOOTSTRAP_USER_EMAIL/PASSWORD and creates
// a user in the default workspace if no password-backed users exist yet.
// Idempotent — only fires when users table has no rows with a password set.
func bootstrapUserFromEnv(ctx context.Context, db *sql.DB) {
	email := strings.TrimSpace(os.Getenv("REPLAY_BOOTSTRAP_USER_EMAIL"))
	password := os.Getenv("REPLAY_BOOTSTRAP_USER_PASSWORD")
	if email == "" || password == "" {
		return
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE password_hash IS NOT NULL`).Scan(&n); err != nil {
		slog.Error("bootstrap: user count failed", "error", err)
		return
	}
	if n > 0 {
		slog.Info("bootstrap: users already configured — ignoring REPLAY_BOOTSTRAP_USER_EMAIL/PASSWORD")
		return
	}
	if _, err := createOrUpdateUser(ctx, db, defaultWorkspaceID, email, email, password); err != nil {
		slog.Error("bootstrap: user create failed", "email", email, "error", err)
		return
	}
	slog.Info("bootstrap: seeded user — remove the REPLAY_BOOTSTRAP_USER_* env vars now", "email", email)
}

// createUserCLI is the implementation of `control-plane create-user`.
// Used by an operator who wants to bootstrap or reset a user without going
// through the HTTP setup flow.
func createUserCLI(ctx context.Context, db *sql.DB, email, name, password string) error {
	if email == "" || password == "" {
		return errors.New("email and password required")
	}
	if name == "" {
		name = email
	}
	_, err := createOrUpdateUser(ctx, db, defaultWorkspaceID, email, name, password)
	return err
}

// gcSessions deletes expired user_sessions rows. Runs on a 1h tick from main.
func gcSessions(ctx context.Context, db *sql.DB) {
	t := time.NewTicker(1 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			res, err := db.ExecContext(ctx,
				`DELETE FROM user_sessions WHERE expires_at < now()`)
			if err != nil {
				slog.Warn("gc: user_sessions delete failed", "error", err)
			} else if n, _ := res.RowsAffected(); n > 0 {
				slog.Info("gc: expired sessions cleared", "count", n)
			}
		}
	}
}
