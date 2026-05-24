package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
)

// pgmqtt password-auth path. This is the default and recommended setup as of
// the pgmqtt:auth image: the broker authenticates every CONNECT against a
// PostgreSQL LOGIN role (SCRAM-SHA-256 in pg_authid), so the browser talks to
// the broker directly on :9001. For multi-tenant deployments that need
// per-workspace topic ACLs (which Community pgmqtt does not provide), use a
// pgmqtt Enterprise license with the `jwt`/`acl` features — the JWT helpers
// live in broker.go.
//
// We provision two kinds of roles:
//   - replay_runner: one global role used by every runner process. Password is
//     supplied via REPLAY_RUNNER_MQTT_PASSWORD so the runner can read it from
//     env without round-tripping to control-plane.
//   - replay_ws_<short>: one per workspace, used by browser clients. Lazily
//     created on first /api/auth/broker-credentials hit; persisted in the
//     mqtt_credentials table so subsequent hits return the same value.
//
// All roles match the `replay\_%` LIKE filter so pgmqtt rejects CONNECT
// attempts from any other PostgreSQL role even if their password leaks.

const (
	mqttRoleFilter = `replay\_%`
	mqttRunnerRole = "replay_runner"
)

// configurePgmqttAuth sets the broker GUCs that turn on password auth and
// ensures the runner role exists. Best-effort — logs and continues if the
// caller is not a superuser, since dev setups sometimes connect as a
// non-superuser and we'd rather come up degraded than fail boot.
func configurePgmqttAuth(ctx context.Context, db *sql.DB) {
	if os.Getenv("REPLAY_PGMQTT_AUTOCONFIG") == "false" {
		slog.Info("pgmqtt: autoconfig disabled via REPLAY_PGMQTT_AUTOCONFIG=false")
		return
	}
	var hasExt bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'pgmqtt')`).Scan(&hasExt); err != nil {
		slog.Warn("pgmqtt: extension probe failed — skipping autoconfig", "error", err)
		return
	}
	if !hasExt {
		slog.Info("pgmqtt: extension not installed — skipping autoconfig")
		return
	}
	gucs := []string{
		"ALTER SYSTEM SET pgmqtt.password_auth_enabled = 'on'",
		"ALTER SYSTEM SET pgmqtt.password_auth_required = 'on'",
		fmt.Sprintf("ALTER SYSTEM SET pgmqtt.password_auth_role_filter = '%s'", mqttRoleFilter),
	}
	for _, q := range gucs {
		if _, err := db.ExecContext(ctx, q); err != nil {
			slog.Warn("pgmqtt: ALTER SYSTEM failed — broker may still accept anonymous connects",
				"query", q, "error", err)
			return
		}
	}
	if _, err := db.ExecContext(ctx, `SELECT pg_reload_conf()`); err != nil {
		slog.Warn("pgmqtt: pg_reload_conf failed", "error", err)
		return
	}
	if err := ensureRunnerRole(ctx, db); err != nil {
		slog.Warn("pgmqtt: runner role provisioning failed", "error", err)
		return
	}
	slog.Info("pgmqtt: password auth enabled", "role_filter", mqttRoleFilter)
}

// ensureRunnerRole creates (or syncs the password of) replay_runner. The
// runner reads the same password from REPLAY_RUNNER_MQTT_PASSWORD so the two
// sides stay in lockstep without any out-of-band handoff.
func ensureRunnerRole(ctx context.Context, db *sql.DB) error {
	pw := strings.TrimSpace(os.Getenv("REPLAY_RUNNER_MQTT_PASSWORD"))
	if pw == "" {
		slog.Warn("pgmqtt: REPLAY_RUNNER_MQTT_PASSWORD not set — runner cannot connect to authed broker")
		return nil
	}
	return upsertRole(ctx, db, mqttRunnerRole, pw)
}

// upsertRole CREATEs a LOGIN role or ALTERs its password if it already exists.
// CREATE/ALTER ROLE doesn't accept bind parameters, so we have Postgres
// compose the statement server-side via format(%I, %L) and then execute the
// composed text. That keeps role-name and password escaping in pg_catalog
// rather than reinventing it here.
func upsertRole(ctx context.Context, db *sql.DB, role, password string) error {
	if !validRoleName(role) {
		return fmt.Errorf("invalid role name %q", role)
	}
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_authid WHERE rolname = $1)`, role).Scan(&exists); err != nil {
		return err
	}
	verb := "CREATE"
	if exists {
		verb = "ALTER"
	}
	var stmt string
	if err := db.QueryRowContext(ctx,
		`SELECT format($1 || ' ROLE %I WITH LOGIN PASSWORD %L', $2::text, $3::text)`,
		verb, role, password,
	).Scan(&stmt); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return err
	}
	return nil
}

// validRoleName guards the unparameterisable role-name slot in CREATE/ALTER
// ROLE. We restrict to the shape we mint ourselves (replay_*, lowercase
// alnum/underscore) so callers can't smuggle SQL through the role name.
func validRoleName(s string) bool {
	if !strings.HasPrefix(s, "replay_") || len(s) > 63 {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
		if !ok {
			return false
		}
	}
	return true
}

// ─── Browser credentials ───────────────────────────────────────────────

type brokerCredentialsResponse struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// ClientID the browser should use in MQTT CONNECT. We mint it server-side
	// so an attacker can't trivially guess a colliding ID for another tenant
	// (which would let them force-disconnect the legitimate session — pgmqtt
	// kicks the prior owner on duplicate clientIds, irrespective of role).
	ClientID string `json:"client_id"`
}

// mqttClientID returns a per-request clientId namespaced by workspace + caller.
// Includes a random suffix so multiple tabs from the same user don't collide.
func mqttClientID(workspaceID, actorID string) (string, error) {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	suffix := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
	wsShort := strings.ReplaceAll(workspaceID, "-", "")
	if len(wsShort) > 12 {
		wsShort = wsShort[:12]
	}
	actorShort := strings.ReplaceAll(actorID, "-", "")
	if len(actorShort) > 8 {
		actorShort = actorShort[:8]
	}
	return "replay-ws-" + wsShort + "-" + actorShort + "-" + suffix, nil
}

func registerBrokerCredentialsRoute(r chi.Router, db *sql.DB) {
	r.Post("/api/auth/broker-credentials", func(w http.ResponseWriter, req *http.Request) {
		ar := authResultFromContext(req.Context())
		if ar == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		creds, err := ensureWorkspaceCredentials(req.Context(), db, ar.WorkspaceID)
		if err != nil {
			slog.Error("broker creds issuance failed", "error", err, "workspace", ar.WorkspaceID)
			http.Error(w, "could not issue broker credentials", http.StatusInternalServerError)
			return
		}
		AuditAttach(req.Context(), "broker_role", creds.Username)
		clientID, err := mqttClientID(ar.WorkspaceID, ar.ActorID)
		if err != nil {
			http.Error(w, "client_id generation failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(brokerCredentialsResponse{
			Username: creds.Username,
			Password: creds.Password,
			ClientID: clientID,
		})
	})

	r.Post("/api/auth/broker-credentials/rotate", func(w http.ResponseWriter, req *http.Request) {
		ar := authResultFromContext(req.Context())
		if ar == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		creds, err := rotateWorkspaceCredentials(req.Context(), db, ar.WorkspaceID)
		if err != nil {
			slog.Error("broker creds rotation failed", "error", err, "workspace", ar.WorkspaceID)
			http.Error(w, "could not rotate broker credentials", http.StatusInternalServerError)
			return
		}
		AuditAttach(req.Context(), "broker_role", creds.Username)
		clientID, err := mqttClientID(ar.WorkspaceID, ar.ActorID)
		if err != nil {
			http.Error(w, "client_id generation failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(brokerCredentialsResponse{
			Username: creds.Username,
			Password: creds.Password,
			ClientID: clientID,
		})
	})
}

type mqttCreds struct {
	RoleName string
	Username string
	Password string
}

func ensureWorkspaceCredentials(ctx context.Context, db *sql.DB, workspaceID string) (*mqttCreds, error) {
	var c mqttCreds
	err := db.QueryRowContext(ctx,
		`SELECT role_name, username, password FROM mqtt_credentials WHERE workspace_id = $1`,
		workspaceID,
	).Scan(&c.RoleName, &c.Username, &c.Password)
	if err == nil {
		return &c, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	role := workspaceRoleName(workspaceID)
	pw, err := generateMQTTPassword()
	if err != nil {
		return nil, err
	}
	if err := upsertRole(ctx, db, role, pw); err != nil {
		return nil, fmt.Errorf("create role: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO mqtt_credentials (workspace_id, role_name, username, password)
		VALUES ($1, $2, $2, $3)
		ON CONFLICT (workspace_id) DO UPDATE
		   SET password = EXCLUDED.password, rotated_at = now()`,
		workspaceID, role, pw); err != nil {
		return nil, err
	}
	return &mqttCreds{RoleName: role, Username: role, Password: pw}, nil
}

func rotateWorkspaceCredentials(ctx context.Context, db *sql.DB, workspaceID string) (*mqttCreds, error) {
	role := workspaceRoleName(workspaceID)
	pw, err := generateMQTTPassword()
	if err != nil {
		return nil, err
	}
	if err := upsertRole(ctx, db, role, pw); err != nil {
		return nil, fmt.Errorf("alter role: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO mqtt_credentials (workspace_id, role_name, username, password)
		VALUES ($1, $2, $2, $3)
		ON CONFLICT (workspace_id) DO UPDATE
		   SET password = EXCLUDED.password, rotated_at = now()`,
		workspaceID, role, pw); err != nil {
		return nil, err
	}
	return &mqttCreds{RoleName: role, Username: role, Password: pw}, nil
}

// workspaceRoleName derives a deterministic, valid Postgres role name from a
// UUID. The leading "replay_ws_" keeps it inside the role_filter LIKE pattern.
func workspaceRoleName(workspaceID string) string {
	// Strip dashes, lowercase, take first 12 chars. 48 bits of entropy is plenty
	// for uniqueness within one deployment and keeps the name well under 63 chars.
	clean := strings.ToLower(strings.ReplaceAll(workspaceID, "-", ""))
	if len(clean) > 12 {
		clean = clean[:12]
	}
	return "replay_ws_" + clean
}

func generateMQTTPassword() (string, error) {
	// 20 bytes → 32 base32 chars → 160 bits. ASCII-only because pgmqtt
	// password auth only handles ASCII reliably (per pgmqtt docs).
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)), nil
}
