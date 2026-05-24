package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// apiKeyPrefix is the human-recognisable namespace on every key. Spotting a
// stray "rplx_..." in a log or commit is what makes accidental disclosure
// catch-able by grep / secret scanners.
const apiKeyPrefix = "rplx_"

// apiKeyEntropyBytes — 16 bytes → 26 base32 chars → 128 bits.
const apiKeyEntropyBytes = 16

type apiKeyAuth struct {
	db *sql.DB
}

func newAPIKeyAuth(db *sql.DB) Authenticator { return &apiKeyAuth{db: db} }

func (a *apiKeyAuth) Name() string { return "api_key" }

func (a *apiKeyAuth) Authenticate(r *http.Request) (*AuthResult, error) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return nil, errAnonymous
	}
	tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	if !strings.HasPrefix(tok, apiKeyPrefix) {
		// Not an API key — let another authenticator handle it.
		return nil, errAnonymous
	}

	hash := hashAPIKey(tok)
	var id, workspaceID string
	var scopesJSON string
	var expiresAt sql.NullTime
	// pgx's stdlib driver doesn't auto-decode TEXT[]; round-trip via JSON keeps
	// us off lib/pq.Array without a separate dependency. The schema default
	// guarantees scopesJSON is at least "[\"admin\"]" so json.Unmarshal can't
	// return a nil slice that the caller would mis-interpret as "no scopes".
	err := a.db.QueryRowContext(r.Context(),
		`SELECT id, workspace_id, COALESCE(to_json(scopes)::text, '["admin"]'), expires_at
		   FROM api_keys WHERE key_hash = $1`, hash,
	).Scan(&id, &workspaceID, &scopesJSON, &expiresAt)
	if err == sql.ErrNoRows {
		// Bearer was shaped like an API key but didn't match anything — that's
		// a hard 401, not "try next". Otherwise a misformatted key could fall
		// through to password auth and confuse the user.
		return nil, errInvalidAPIKey
	}
	if err != nil {
		slog.Error("api_key: lookup failed", "error", err)
		return nil, errInvalidAPIKey
	}

	var scopes []string
	if err := json.Unmarshal([]byte(scopesJSON), &scopes); err != nil {
		// Treat a malformed scopes column as conservatively unscoped — better
		// to fail closed than to grant admin on a corrupt row.
		slog.Warn("api_key: scopes parse failed; refusing", "key_id", id, "error", err)
		return nil, errInvalidAPIKey
	}

	if expiresAt.Valid && time.Now().After(expiresAt.Time) {
		return nil, errInvalidAPIKey
	}

	if !apiKeyScopesAllow(scopes, r.Method, r.URL.Path) {
		// Distinct from "invalid key" so logs can tell scope-refusals from
		// totally-bogus tokens. Still returns 401 to the client to avoid
		// leaking scope information to attackers.
		slog.Warn("api_key: scope refused", "key_id", id, "method", r.Method, "path", r.URL.Path, "scopes", scopes)
		return nil, errInvalidAPIKey
	}

	// Fire-and-forget last_used update. Losing a few of these under load is fine.
	go func(keyID string) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = a.db.ExecContext(ctx,
			`UPDATE api_keys SET last_used_at = now() WHERE id = $1`, keyID)
	}(id)

	return &AuthResult{
		WorkspaceID: workspaceID,
		ActorID:     id,
		ActorKind:   "api_key",
	}, nil
}

// apiKeyScopesAllow gates a request against the scope set on the key.
// Vocabulary (extend cautiously — each new scope is a permanent compat surface):
//
//	admin   — unrestricted (the default scope a key gets)
//	read    — GET / HEAD on any route
//	webhook — POST /api/webhooks/run only
//	runner  — reserved (no enforcement)
//
// A key with an empty scope list is treated as admin (the scopes column defaults
// to ['admin'], so this only happens if a caller explicitly clears it). Once
// scopes are set explicitly, "admin" must be present for full access.
func apiKeyScopesAllow(scopes []string, method, path string) bool {
	if len(scopes) == 0 {
		return true
	}
	has := func(s string) bool {
		for _, v := range scopes {
			if v == s {
				return true
			}
		}
		return false
	}
	if has("admin") {
		return true
	}
	if has("read") && (method == http.MethodGet || method == http.MethodHead) {
		return true
	}
	if has("webhook") && method == http.MethodPost && path == "/api/webhooks/run" {
		return true
	}
	return false
}

// errInvalidAPIKey turns into a 401 instead of falling through to other authenticators.
var errInvalidAPIKey = newAuthError("invalid API key")

// validAPIScopes is the closed vocabulary recognised by apiKeyScopesAllow.
// Mint here when adding a new scope so the input validator and the gate stay
// in lockstep.
var validAPIScopes = map[string]struct{}{
	"admin":   {},
	"read":    {},
	"webhook": {},
	"runner":  {},
}

// normaliseScopes drops unknown scopes and defaults empty input to ["admin"]
// to match the schema default — so a UI that sends `{name: "x"}` gets the
// same behaviour as a key minted via `control-plane bootstrap-key`.
func normaliseScopes(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if _, ok := validAPIScopes[s]; !ok || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return []string{"admin"}
	}
	return out
}

// authError is a hard 401 from a specific authenticator — distinct from
// errAnonymous which means "not my packet".
type authError struct{ msg string }

func newAuthError(s string) *authError { return &authError{s} }
func (e *authError) Error() string     { return e.msg }

func generateAPIKey() (string, string, error) {
	raw := make([]byte, apiKeyEntropyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	body := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
	full := apiKeyPrefix + body
	prefix := full
	if len(full) > 9 {
		prefix = full[:9]
	}
	return full, prefix, nil
}

func hashAPIKey(full string) string {
	sum := sha256.Sum256([]byte(full))
	return hex.EncodeToString(sum[:])
}

// ─── HTTP routes ───────────────────────────────────────────────────────

type apiKeyResponse struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

type apiKeyCreateRequest struct {
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`     // empty → defaults to ["admin"]
	ExpiresAt *time.Time `json:"expires_at"` // nil → never expires
}

type apiKeyCreateResponse struct {
	apiKeyResponse
	FullKey string `json:"full_key"` // returned ONCE on create; never again.
}

func registerAPIKeyRoutes(r chi.Router, db *sql.DB) {
	r.Get("/api/api-keys", func(w http.ResponseWriter, req *http.Request) {
		workspaceID := workspaceFromContext(req.Context())
		rows, err := db.QueryContext(req.Context(),
			`SELECT id, name, key_prefix, to_json(scopes)::text, expires_at, created_at, last_used_at
			   FROM api_keys WHERE workspace_id = $1 ORDER BY created_at DESC`,
			workspaceID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		out := []apiKeyResponse{}
		for rows.Next() {
			var k apiKeyResponse
			var scopesJSON string
			var lastUsed, expiresAt sql.NullTime
			if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &scopesJSON, &expiresAt, &k.CreatedAt, &lastUsed); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_ = json.Unmarshal([]byte(scopesJSON), &k.Scopes)
			if expiresAt.Valid {
				t := expiresAt.Time
				k.ExpiresAt = &t
			}
			if lastUsed.Valid {
				t := lastUsed.Time
				k.LastUsedAt = &t
			}
			out = append(out, k)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})

	r.Post("/api/api-keys", func(w http.ResponseWriter, req *http.Request) {
		workspaceID := workspaceFromContext(req.Context())
		var body apiKeyCreateRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		name := strings.TrimSpace(body.Name)
		if name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		scopes := normaliseScopes(body.Scopes)
		full, prefix, err := generateAPIKey()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		scopesJSON, _ := json.Marshal(scopes)
		var expiresArg interface{}
		if body.ExpiresAt != nil {
			expiresArg = body.ExpiresAt.UTC()
		}
		var id string
		var createdAt time.Time
		err = db.QueryRowContext(req.Context(), `
			INSERT INTO api_keys (workspace_id, name, key_prefix, key_hash, scopes, expires_at)
			VALUES ($1, $2, $3, $4, (SELECT array_agg(value::text) FROM json_array_elements_text($5::json)), $6)
			RETURNING id, created_at`,
			workspaceID, name, prefix, hashAPIKey(full), string(scopesJSON), expiresArg,
		).Scan(&id, &createdAt)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(apiKeyCreateResponse{
			apiKeyResponse: apiKeyResponse{
				ID: id, Name: name, Prefix: prefix,
				Scopes: scopes, ExpiresAt: body.ExpiresAt,
				CreatedAt: createdAt,
			},
			FullKey: full,
		})
	})

	r.Delete("/api/api-keys/{id}", func(w http.ResponseWriter, req *http.Request) {
		workspaceID := workspaceFromContext(req.Context())
		id := chi.URLParam(req, "id")
		res, err := db.ExecContext(req.Context(),
			`DELETE FROM api_keys WHERE id = $1 AND workspace_id = $2`,
			id, workspaceID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// ─── Bootstrap ─────────────────────────────────────────────────────────

// bootstrapAPIKeyFromEnv consumes REPLAY_BOOTSTRAP_API_KEY on first boot,
// inserting it into api_keys for the default workspace if (and only if) the
// table is empty. Subsequent boots see the row already exists and noop.
//
// The env var is expected to hold a full "rplx_..." key — usually one minted
// by an operator with `control-plane bootstrap-key` ahead of time. Hashing
// happens here; the raw value is never persisted.
func bootstrapAPIKeyFromEnv(ctx context.Context, db *sql.DB) {
	key := strings.TrimSpace(os.Getenv("REPLAY_BOOTSTRAP_API_KEY"))
	if key == "" {
		return
	}
	if !strings.HasPrefix(key, apiKeyPrefix) {
		slog.Warn("bootstrap: REPLAY_BOOTSTRAP_API_KEY missing rplx_ prefix — ignoring")
		return
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys`).Scan(&n); err != nil {
		slog.Error("bootstrap: api_keys count failed", "error", err)
		return
	}
	if n > 0 {
		// Idempotent — don't even check the hash. Operator should remove the env
		// var once they're past first boot.
		slog.Info("bootstrap: api_keys already populated — ignoring REPLAY_BOOTSTRAP_API_KEY")
		return
	}
	hash := hashAPIKey(key)
	prefix := key
	if len(key) > 9 {
		prefix = key[:9]
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO api_keys (workspace_id, name, key_prefix, key_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (key_hash) DO NOTHING`,
		defaultWorkspaceID, "bootstrap", prefix, hash)
	if err != nil {
		slog.Error("bootstrap: api key insert failed", "error", err)
		return
	}
	slog.Info("bootstrap: seeded REPLAY_BOOTSTRAP_API_KEY into api_keys — remove the env var now")
}

// createBootstrapAPIKey is the implementation of `control-plane bootstrap-key`.
// Generates a fresh key, prints it once, returns. No HTTP server is involved
// so the operator can use this in environments where the API isn't reachable
// (or before any users exist).
func createBootstrapAPIKey(ctx context.Context, db *sql.DB, name string) (string, error) {
	full, prefix, err := generateAPIKey()
	if err != nil {
		return "", err
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO api_keys (workspace_id, name, key_prefix, key_hash)
		VALUES ($1, $2, $3, $4)`,
		defaultWorkspaceID, name, prefix, hashAPIKey(full))
	if err != nil {
		return "", err
	}
	return full, nil
}
