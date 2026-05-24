package main

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type ScriptRequest struct {
	Name     string `json:"name"`
	Filename string `json:"filename"`
	Content  string `json:"content"`
	AgentsMD string `json:"agents_md"`
}

func registerScriptRoutes(r chi.Router, db *sql.DB) {
	r.Get("/api/scripts", func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.QueryContext(r.Context(), `
			SELECT id, project_id, name, filename, content, COALESCE(agents_md, ''),
			       created_at, updated_at,
			       source_kind, source_integration_id, source_repo,
			       source_path, source_ref, source_sha, synced_at
			FROM scripts WHERE workspace_id = $1 ORDER BY created_at DESC`,
			workspaceFromContext(r.Context()))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		scripts := []ScriptResponse{}
		for rows.Next() {
			s, scanErr := scanScriptWithSource(rows)
			if scanErr != nil {
				http.Error(w, scanErr.Error(), http.StatusInternalServerError)
				return
			}
			scripts = append(scripts, s)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(scripts)
	})

	r.Get("/api/scripts/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		row := db.QueryRowContext(r.Context(), `
			SELECT id, project_id, name, filename, content, COALESCE(agents_md, ''),
			       created_at, updated_at,
			       source_kind, source_integration_id, source_repo,
			       source_path, source_ref, source_sha, synced_at
			FROM scripts WHERE id = $1 AND workspace_id = $2`, id, workspaceFromContext(r.Context()))
		s, err := scanScriptWithSource(row)
		if err == sql.ErrNoRows {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s)
	})

	r.Post("/api/scripts", func(w http.ResponseWriter, r *http.Request) {
		var req ScriptRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if req.Name == "" || req.Filename == "" {
			http.Error(w, "name and filename required", http.StatusBadRequest)
			return
		}
		s := ScriptResponse{
			ID:        uuid.New().String(),
			ProjectID: projectIDFromContext(r.Context()),
			Name:      req.Name,
			Filename:  req.Filename,
			Content:   req.Content,
			AgentsMD:  req.AgentsMD,
		}
		err := db.QueryRowContext(r.Context(), `
			INSERT INTO scripts (id, project_id, workspace_id, name, filename, content, agents_md)
			VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''))
			RETURNING created_at, updated_at`,
			s.ID, s.ProjectID, workspaceFromContext(r.Context()), s.Name, s.Filename, s.Content, s.AgentsMD,
		).Scan(&s.CreatedAt, &s.UpdatedAt)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(s)
	})

	r.Put("/api/scripts/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var req ScriptRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		res, err := db.ExecContext(r.Context(), `
			UPDATE scripts SET name = $1, filename = $2, content = $3, agents_md = NULLIF($4, ''), updated_at = now()
			WHERE id = $5 AND workspace_id = $6`, req.Name, req.Filename, req.Content, req.AgentsMD, id, workspaceFromContext(r.Context()))
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

	registerScriptDeleteRoute(r, db)
}

// scriptRowScanner adapts both *sql.Row and *sql.Rows so the same scan code
// reads both single-row Get and multi-row List queries.
type scriptRowScanner interface {
	Scan(dest ...any) error
}

// scanScriptWithSource decodes a script row including the optional source_*
// columns. The source columns are NULL for inline scripts (those not imported
// from a repo), so they go through sql.Null* and translate to empty strings /
// nil pointer on the response.
func scanScriptWithSource(row scriptRowScanner) (ScriptResponse, error) {
	var s ScriptResponse
	var sourceKind, sourceIntegration, sourceRepo, sourcePath, sourceRef, sourceSHA sql.NullString
	var syncedAt sql.NullTime
	err := row.Scan(
		&s.ID, &s.ProjectID, &s.Name, &s.Filename, &s.Content, &s.AgentsMD,
		&s.CreatedAt, &s.UpdatedAt,
		&sourceKind, &sourceIntegration, &sourceRepo,
		&sourcePath, &sourceRef, &sourceSHA, &syncedAt,
	)
	if err != nil {
		return s, err
	}
	if sourceKind.Valid && sourceKind.String != "inline" {
		s.SourceKind = sourceKind.String
		s.SourceIntegrationID = sourceIntegration.String
		s.SourceRepo = sourceRepo.String
		s.SourcePath = sourcePath.String
		s.SourceRef = sourceRef.String
		s.SourceSHA = sourceSHA.String
		if syncedAt.Valid {
			t := syncedAt.Time
			s.SyncedAt = &t
		}
	}
	return s, nil
}

func registerScriptDeleteRoute(r chi.Router, db *sql.DB) {
	r.Delete("/api/scripts/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		res, err := db.ExecContext(r.Context(), `DELETE FROM scripts WHERE id = $1 AND workspace_id = $2`, id, workspaceFromContext(r.Context()))
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
