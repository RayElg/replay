package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
)

type contextKey int

const (
	ctxWorkspaceID contextKey = iota
	ctxProjectID
	ctxAuthResult
)

// defaultWorkspaceID is the seeded workspace used in single-tenant and self-hosted mode.
// Matches the seed row inserted by migration 0001.
const defaultWorkspaceID = "00000000-0000-0000-0000-000000000001"

// AuthResult is the identity attached to an authenticated request.
type AuthResult struct {
	WorkspaceID string
	ActorID     string
	ActorKind   string // "api_key" | "password"
	SessionID   string // cookie/session ID for cookie-based authenticators; "" for bearer
	Email       string // populated for password; "" for api_key
}

// Authenticator classifies a request. Implementations either return
// a non-nil *AuthResult (authenticated) or errAnonymous (not my packet,
// try the next authenticator in the chain). Any other error is a hard 401.
type Authenticator interface {
	Authenticate(r *http.Request) (*AuthResult, error)
	Name() string
}

// errAnonymous means "this authenticator did not recognise the request" —
// the chain should keep trying. Anything else short-circuits with a 401.
var errAnonymous = errors.New("anonymous request")

type chainAuth []Authenticator

func (c chainAuth) Name() string { return "chain" }

func (c chainAuth) Authenticate(r *http.Request) (*AuthResult, error) {
	for _, a := range c {
		res, err := a.Authenticate(r)
		if err == nil && res != nil {
			return res, nil
		}
		if err != nil && !errors.Is(err, errAnonymous) {
			return nil, err
		}
	}
	return nil, errAnonymous
}

// newAuthChain wires the active authenticators. Browser sessions use password
// auth; the API-key authenticator is always appended last so non-browser
// callers (runner, CI, GitHub-Actions webhook scripts) can use a bearer token.
func newAuthChain(db *sql.DB) Authenticator {
	return chainAuth{
		newPasswordAuth(db),
		newAPIKeyAuth(db),
	}
}

func workspaceFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxWorkspaceID).(string)
	return v
}

func projectIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxProjectID).(string)
	return v
}

func authResultFromContext(ctx context.Context) *AuthResult {
	v, _ := ctx.Value(ctxAuthResult).(*AuthResult)
	return v
}

// publicPath returns true for routes that bypass the authenticator chain.
// They either authenticate themselves (webhooks via their own bearer token),
// are bootstrap endpoints that have to be reachable pre-login (/api/auth/*),
// or are health/static metadata callers like /healthz.
func publicPath(p string) bool {
	if p == "/healthz" {
		return true
	}
	// Most /api/auth/* endpoints are bootstrap/handshake routes that must be
	// reachable pre-login. The ones below are authed:
	//   /api/auth/me               — "who am I?" probe the UI gates on
	//   /api/auth/password         — change-password requires existing session
	//   /api/auth/broker-credentials[/rotate] — issue MQTT creds for the caller
	//   /api/auth/broker-token     — issue MQTT JWT for the caller (Enterprise)
	// Everything else under /api/auth/ (login, setup, password reset, etc.)
	// is public.
	if strings.HasPrefix(p, "/api/auth/") &&
		p != "/api/auth/me" &&
		p != "/api/auth/password" &&
		p != "/api/auth/broker-credentials" &&
		p != "/api/auth/broker-credentials/rotate" &&
		p != "/api/auth/broker-token" {
		return true
	}
	// Invite acceptance is pre-login by definition.
	if p == "/api/invites/accept" {
		return true
	}
	// Only the actual webhook receivers are public — admin endpoints
	// (token display / rotation) require an authenticated UI session. The
	// GitHub receiver authenticates itself via HMAC signature, not a session.
	// The run-status poll (/api/webhooks/run/{id}) self-authenticates with the
	// webhook token inside the handler, same as the trigger.
	if p == "/api/webhooks/run" || p == "/api/webhooks/github" ||
		strings.HasPrefix(p, "/api/webhooks/run/") {
		return true
	}
	return false
}

func authMiddleware(db *sql.DB) func(http.Handler) http.Handler {
	chain := newAuthChain(db)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if publicPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			res, err := chain.Authenticate(r)
			if err != nil || res == nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="replay"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			projectID, err := resolveDefaultProject(r.Context(), db, res.WorkspaceID)
			if err != nil {
				http.Error(w, "workspace has no project", http.StatusInternalServerError)
				return
			}
			ctx := context.WithValue(r.Context(), ctxWorkspaceID, res.WorkspaceID)
			ctx = context.WithValue(ctx, ctxProjectID, projectID)
			ctx = context.WithValue(ctx, ctxAuthResult, res)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// resolveDefaultProject returns the oldest project for the given workspace.
// When a multi-project UI lands, individual routes can accept an explicit
// project_id query / path param and bypass this function.
func resolveDefaultProject(ctx context.Context, db *sql.DB, workspaceID string) (string, error) {
	var projectID string
	err := db.QueryRowContext(ctx,
		`SELECT id FROM projects WHERE workspace_id = $1 ORDER BY created_at ASC LIMIT 1`,
		workspaceID,
	).Scan(&projectID)
	return projectID, err
}

// authConfigResponse is the unauthenticated payload returned by /api/auth/config.
// The login page reads this on load to decide between the login form and the
// first-run /setup redirect.
type authConfigResponse struct {
	HasUsers      bool   `json:"has_users"`      // false → show /setup
	TrustedBroker bool   `json:"trusted_broker"` // true → UI fetches JWT from /api/auth/broker-token instead of SCRAM creds
	ExternalURL   string `json:"external_url"`   // operator-configured public base URL (REPLAY_EXTERNAL_URL); "" → UI falls back to window.location.origin
}

func registerAuthConfigRoute(r chi.Router, db *sql.DB) {
	r.Get("/api/auth/config", func(w http.ResponseWriter, req *http.Request) {
		var n int
		_ = db.QueryRowContext(req.Context(),
			`SELECT COUNT(*) FROM users WHERE password_hash IS NOT NULL`).Scan(&n)
		cfg := authConfigResponse{
			HasUsers:      n > 0,
			TrustedBroker: mqttTrustedBroker(),
			// Empty when unset — the UI falls back to window.location.origin, which
			// is only ever wrong for operators reaching the UI by a non-public host
			// (localhost, LAN IP, tunnel). Setting REPLAY_EXTERNAL_URL pins it.
			ExternalURL: strings.TrimRight(strings.TrimSpace(os.Getenv("REPLAY_EXTERNAL_URL")), "/"),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cfg)
	})

	// The UI polls /api/auth/me on every page load; the password and api-key
	// authenticators both populate AuthResult the same way.
	r.Get("/api/auth/me", func(w http.ResponseWriter, req *http.Request) {
		ar := authResultFromContext(req.Context())
		if ar == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(meResponse{
			UserID:      ar.ActorID,
			Email:       ar.Email,
			WorkspaceID: ar.WorkspaceID,
		})
	})
}
