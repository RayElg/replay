package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

type WorkspaceResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"created_at"`
}

type ProjectResponse struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Name        string    `json:"name"`
	CreatedAt   time.Time `json:"created_at"`
}

func registerWorkspaceRoutes(r chi.Router, db *sql.DB) {
	// GET /api/workspaces/current — returns the workspace the caller is authenticated into.
	// Resolved by authMiddleware from the active authenticator (password / api_key);
	// single-tenant deployments default to the seed workspace.
	r.Get("/api/workspaces/current", func(w http.ResponseWriter, r *http.Request) {
		workspaceID := workspaceFromContext(r.Context())
		var ws WorkspaceResponse
		err := db.QueryRowContext(r.Context(),
			`SELECT id, name, slug, created_at FROM workspaces WHERE id = $1`, workspaceID,
		).Scan(&ws.ID, &ws.Name, &ws.Slug, &ws.CreatedAt)
		if err == sql.ErrNoRows {
			http.Error(w, "workspace not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ws)
	})

	// GET /api/settings — deployment-level settings the UI needs to render
	// honestly. encryption_configured reflects whether REPLAY_ENCRYPT_KEY is set;
	// when false, secret env-var values are stored in plaintext at rest, so the
	// settings UI warns instead of claiming values are "stored encrypted".
	r.Get("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"encryption_configured": encryptionConfigured(),
		})
	})

	// GET /api/projects — lists all projects in the current workspace, oldest first.
	// The UI can use this to build a project switcher; until then only one project
	// exists per workspace (the default seeded by the migration).
	r.Get("/api/projects", func(w http.ResponseWriter, r *http.Request) {
		workspaceID := workspaceFromContext(r.Context())
		rows, err := db.QueryContext(r.Context(),
			`SELECT id, workspace_id, name, created_at
			 FROM projects WHERE workspace_id = $1 ORDER BY created_at ASC`, workspaceID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		projects := []ProjectResponse{}
		for rows.Next() {
			var p ProjectResponse
			rows.Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.CreatedAt)
			projects = append(projects, p)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(projects)
	})
}
