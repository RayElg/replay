package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type webhookRunRequest struct {
	EnvironmentSlug string            `json:"environment_slug"`
	ScriptID        string            `json:"script_id"`
	ScriptFilename  string            `json:"script_filename"` // alternative to script_id: resolved to a script within the token's project
	Branch          string            `json:"branch"`
	CommitSHA       string            `json:"commit_sha"`
	Repo            string            `json:"repo"`
	EnvVars         map[string]string `json:"env_vars"`
}

// globalWebhookToken returns the global REPLAY_WEBHOOK_TOKEN — a single-token
// convenience for single-tenant self-hosted installs that don't use per-project
// tokens.
func globalWebhookToken() string {
	return os.Getenv("REPLAY_WEBHOOK_TOKEN")
}

// resolveWebhookProject returns the (workspace_id, project_id) for the given
// Bearer token. Per-project tokens win; falls back to the global token
// + default workspace if the request token matches REPLAY_WEBHOOK_TOKEN.
func resolveWebhookProject(r *http.Request, db *sql.DB) (workspaceID, projectID string, ok bool) {
	auth := r.Header.Get("Authorization")
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if token == "" {
		return "", "", false
	}
	// Per-project token: lookup by SHA-256 hash. Plaintext is never stored.
	err := db.QueryRowContext(r.Context(),
		`SELECT workspace_id, id FROM projects WHERE webhook_token_hash = $1`,
		hashWebhookToken(token),
	).Scan(&workspaceID, &projectID)
	if err == nil {
		return workspaceID, projectID, true
	}
	if err != sql.ErrNoRows {
		slog.Warn("webhook: token lookup failed", "error", err)
		return "", "", false
	}
	// Fallback: global token → default workspace's first project. Constant-time
	// digest compare so the global token isn't byte-by-byte guessable.
	if g := globalWebhookToken(); g != "" &&
		subtle.ConstantTimeCompare([]byte(hashWebhookToken(token)), []byte(hashWebhookToken(g))) == 1 {
		pid, err := resolveDefaultProject(r.Context(), db, defaultWorkspaceID)
		if err != nil {
			return "", "", false
		}
		return defaultWorkspaceID, pid, true
	}
	return "", "", false
}

func registerWebhookRoutes(r chi.Router, db *sql.DB) {
	// GET /api/webhooks/token — returns metadata only. Plaintext is never
	// recoverable: we store SHA-256(token) and the first 8 chars as a display
	// hint. Anyone who needs the actual token has to rotate.
	r.Get("/api/webhooks/token", func(w http.ResponseWriter, r *http.Request) {
		projectID := projectIDFromContext(r.Context())
		if projectID == "" {
			http.Error(w, "no project", http.StatusServiceUnavailable)
			return
		}
		var prefix sql.NullString
		err := db.QueryRowContext(r.Context(),
			`SELECT webhook_token_prefix FROM projects WHERE id = $1`, projectID,
		).Scan(&prefix)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"prefix": prefix.String,
			"exists": prefix.Valid && prefix.String != "",
		})
	})

	// POST /api/webhooks/token/rotate — the only path that reveals a plaintext
	// token. Replaces the hash + prefix; the prior token is unrecoverable from
	// our side after this call returns.
	r.Post("/api/webhooks/token/rotate", func(w http.ResponseWriter, r *http.Request) {
		projectID := projectIDFromContext(r.Context())
		if projectID == "" {
			http.Error(w, "no project", http.StatusServiceUnavailable)
			return
		}
		fresh := newWebhookToken()
		hash := hashWebhookToken(fresh)
		prefix := fresh[:8]
		_, err := db.ExecContext(r.Context(),
			`UPDATE projects SET webhook_token_hash = $1, webhook_token_prefix = $2 WHERE id = $3`,
			hash, prefix, projectID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		AuditAttach(r.Context(), "project_id", projectID)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":  fresh,
			"prefix": prefix,
		})
	})

	// POST /api/webhooks/run — external trigger (GitHub Actions, etc).
	// Authenticated by either a per-project webhook token (preferred) or the
	// global REPLAY_WEBHOOK_TOKEN for single-tenant installs.
	r.Post("/api/webhooks/run", func(w http.ResponseWriter, r *http.Request) {
		workspaceID, projectID, ok := resolveWebhookProject(r, db)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Tell the audit middleware where this request actually landed.
		// Without this, public-path webhook calls would all bucket under the
		// default workspace + "anonymous" actor.
		AuditAttach(r.Context(), "workspace_id", workspaceID)
		AuditAttach(r.Context(), "project_id", projectID)
		AuditAttach(r.Context(), "actor_kind", "webhook")
		AuditAttach(r.Context(), "actor_id", "webhook:"+projectID)
		var req webhookRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		var envID interface{}
		if req.EnvironmentSlug != "" {
			var eid string
			err := db.QueryRowContext(r.Context(),
				`SELECT id FROM environments WHERE slug = $1 AND workspace_id = $2`,
				req.EnvironmentSlug, workspaceID,
			).Scan(&eid)
			if err == sql.ErrNoRows {
				http.Error(w, "environment not found: "+req.EnvironmentSlug, http.StatusBadRequest)
				return
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			envID = eid
		}

		// Resolve which script to run. script_id wins when given; otherwise
		// script_filename is looked up within the token's project. Filenames
		// aren't unique by schema, so an ambiguous match is a hard error rather
		// than an arbitrary pick.
		var scriptID interface{}
		if req.ScriptID != "" {
			// Scope script_id to the token's workspace+project so a token can't
			// run another tenant's script by id.
			var one int
			err := db.QueryRowContext(r.Context(),
				`SELECT 1 FROM scripts WHERE id = $1 AND workspace_id = $2 AND project_id = $3`,
				req.ScriptID, workspaceID, projectID).Scan(&one)
			if err == sql.ErrNoRows {
				http.Error(w, "script not found: "+req.ScriptID, http.StatusBadRequest)
				return
			}
			if err != nil {
				http.Error(w, "database error", http.StatusInternalServerError)
				return
			}
			scriptID = req.ScriptID
		} else if req.ScriptFilename != "" {
			rows, err := db.QueryContext(r.Context(),
				`SELECT id FROM scripts WHERE filename = $1 AND workspace_id = $2 AND project_id = $3 LIMIT 2`,
				req.ScriptFilename, workspaceID, projectID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			var ids []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				ids = append(ids, id)
			}
			rows.Close()
			switch len(ids) {
			case 0:
				http.Error(w, "script not found: "+req.ScriptFilename, http.StatusBadRequest)
				return
			case 1:
				scriptID = ids[0]
			default:
				http.Error(w, "ambiguous script_filename (multiple scripts share this filename) — use script_id: "+req.ScriptFilename, http.StatusBadRequest)
				return
			}
		}

		var repo interface{}
		if req.Repo != "" {
			repo = req.Repo
		}

		envVarsJSON := json.RawMessage(`{}`)
		if len(req.EnvVars) > 0 {
			if b, err := json.Marshal(req.EnvVars); err == nil {
				envVarsJSON = b
			}
		}

		runID := uuid.New().String()
		_, err := db.ExecContext(r.Context(), `
			INSERT INTO runs
				(id, project_id, workspace_id, branch, commit_sha, repo, script_id, env_id, env_vars, status, webhook_source)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'queued', 'gha')`,
			runID, projectID, workspaceID, req.Branch, req.CommitSHA, repo, scriptID, envID, envVarsJSON,
		)
		if err != nil {
			slog.Error("webhook: failed to insert run", "error", err)
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		slog.Info("webhook: queued run", "run_id", runID, "branch", req.Branch, "repo", req.Repo, "workspace_id", workspaceID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"run_id": runID})
	})

	// GET /api/webhooks/run/{id} — poll a run's status using the same webhook
	// token that triggered it, so CI can wait for a verdict without a separate
	// read-scoped API key. Scoped to the token's project: a token can only read
	// runs it could have created.
	r.Get("/api/webhooks/run/{id}", func(w http.ResponseWriter, r *http.Request) {
		workspaceID, projectID, ok := resolveWebhookProject(r, db)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		id := chi.URLParam(r, "id")
		var (
			status         string
			classification sql.NullString
			summary        sql.NullString
			finishedAt     sql.NullTime
		)
		err := db.QueryRowContext(r.Context(), `
			SELECT status, triage_classification, triage_summary, finished_at
			FROM runs WHERE id = $1 AND workspace_id = $2 AND project_id = $3`,
			id, workspaceID, projectID,
		).Scan(&status, &classification, &summary, &finishedAt)
		if err == sql.ErrNoRows {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		out := map[string]any{"run_id": id, "status": status}
		if classification.Valid {
			out["triage_classification"] = classification.String
		}
		if summary.Valid {
			out["triage_summary"] = summary.String
		}
		if finishedAt.Valid {
			out["finished_at"] = finishedAt.Time
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
}

func newWebhookToken() string {
	return strings.ReplaceAll(uuid.New().String(), "-", "")
}

// hashWebhookToken returns the SHA-256 hex digest of a plaintext token. Used
// both at issuance (store) and at lookup (compare). The token is 32 hex chars
// (122 bits of entropy from uuid.New()) so unsalted SHA-256 is fine — there
// is no realistic offline brute-force surface.
func hashWebhookToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
