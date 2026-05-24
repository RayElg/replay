package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

type integrationRow struct {
	ID        string
	ProjectID string
	Provider  string
	Name      string
	Config    json.RawMessage
	HasToken  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

type integrationCreateRequest struct {
	Provider string          `json:"provider"`
	Name     string          `json:"name"`
	Config   json.RawMessage `json:"config"`
	Token    string          `json:"token"`
}

func registerIntegrationRoutes(r chi.Router, db *sql.DB) {
	r.Get("/api/integrations", func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.QueryContext(r.Context(), `
			SELECT id, project_id, provider, name, config, encrypted_token, created_at, updated_at
			FROM integrations
			WHERE workspace_id = $1
			ORDER BY provider`, workspaceFromContext(r.Context()))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var row integrationRow
			var enc string
			rows.Scan(&row.ID, &row.ProjectID, &row.Provider, &row.Name, &row.Config, &enc, &row.CreatedAt, &row.UpdatedAt)
			out = append(out, map[string]any{
				"id":         row.ID,
				"provider":   row.Provider,
				"name":       row.Name,
				"config":     json.RawMessage(row.Config),
				"has_token":  enc != "",
				"created_at": row.CreatedAt,
				"updated_at": row.UpdatedAt,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})

	r.Post("/api/integrations", func(w http.ResponseWriter, r *http.Request) {
		var req integrationCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if req.Provider == "" {
			http.Error(w, "provider is required", http.StatusBadRequest)
			return
		}
		if len(req.Config) == 0 {
			req.Config = json.RawMessage(`{}`)
		}
		encToken, err := encryptSecret(req.Token)
		if err != nil {
			if errors.Is(err, errNoSecretKey) {
				http.Error(w, "REPLAY_ENCRYPT_KEY not configured on the server — cannot store tokens", http.StatusServiceUnavailable)
				return
			}
			http.Error(w, "encrypt: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// Upsert by (workspace, provider, name): one row per workspace+provider+repo combination.
		_, err = db.ExecContext(r.Context(), `
			INSERT INTO integrations (project_id, workspace_id, provider, name, config, encrypted_token, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, now())
			ON CONFLICT (workspace_id, provider, name) DO UPDATE SET
				config = EXCLUDED.config,
				encrypted_token = CASE WHEN EXCLUDED.encrypted_token = '' THEN integrations.encrypted_token ELSE EXCLUDED.encrypted_token END,
				updated_at = now()`,
			projectIDFromContext(r.Context()), workspaceFromContext(r.Context()), req.Provider, req.Name, req.Config, encToken)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	r.Delete("/api/integrations/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		res, err := db.ExecContext(r.Context(),
			`DELETE FROM integrations WHERE id = $1 AND workspace_id = $2`, id, workspaceFromContext(r.Context()))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
