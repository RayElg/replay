package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// Script patches API — UI surfaces agent-proposed edits, humans apply or reject.
// Workspace scoping joins through scripts → workspace_id so cross-tenant fetches
// are impossible by id.

func registerScriptPatchRoutes(r chi.Router, db *sql.DB) {
	r.Get("/api/script-patches", func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status")
		runID := r.URL.Query().Get("run_id")

		var query string
		var args []any

		workspaceID := workspaceFromContext(r.Context())
		if runID != "" {
			// Filter to patches proposed during this run's conversation (all reruns share a root).
			query = `
				SELECT sp.id, sp.script_id, sp.proposed_by_run_id, sp.proposed_by,
				       sp.summary, sp.rationale, sp.status, sp.created_at
				FROM script_patches sp
				JOIN runs r ON sp.proposed_by_run_id = r.id
				WHERE r.root_run_id = (SELECT root_run_id FROM runs WHERE id = $1 AND workspace_id = $2)
				  AND r.workspace_id = $2`
			args = append(args, runID, workspaceID)
			if status != "" {
				query += ` AND sp.status = $3`
				args = append(args, status)
			}
			query += ` ORDER BY sp.created_at DESC`
		} else {
			query = `
				SELECT sp.id, sp.script_id, sp.proposed_by_run_id, sp.proposed_by, sp.summary, sp.rationale,
				       sp.status, sp.created_at
				FROM script_patches sp
				JOIN scripts s ON s.id = sp.script_id
				WHERE s.workspace_id = $1`
			args = append(args, workspaceID)
			if status != "" {
				query += ` AND sp.status = $2`
				args = append(args, status)
			}
			query += ` ORDER BY sp.created_at DESC LIMIT 50`
		}

		rows, err := db.QueryContext(r.Context(), query, args...)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var id, scriptID, proposedBy, summary, rationale, st string
			var runID sql.NullString
			var createdAt time.Time
			rows.Scan(&id, &scriptID, &runID, &proposedBy, &summary, &rationale, &st, &createdAt)
			out = append(out, map[string]any{
				"id":                 id,
				"script_id":          scriptID,
				"proposed_by_run_id": runID.String,
				"proposed_by":        proposedBy,
				"summary":            summary,
				"rationale":          rationale,
				"status":             st,
				"created_at":         createdAt,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})

	r.Get("/api/script-patches/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var pid, scriptID, proposedBy, summary, rationale, oldC, newC, st string
		var runID sql.NullString
		var decidedBy sql.NullString
		var createdAt time.Time
		var decidedAt *time.Time
		err := db.QueryRowContext(r.Context(), `
			SELECT sp.id, sp.script_id, sp.proposed_by_run_id, sp.proposed_by, sp.summary, sp.rationale,
			       sp.old_content, sp.new_content, sp.status, sp.decided_by, sp.decided_at, sp.created_at
			FROM script_patches sp
			JOIN scripts s ON s.id = sp.script_id
			WHERE sp.id = $1 AND s.workspace_id = $2`, id, workspaceFromContext(r.Context())).Scan(
			&pid, &scriptID, &runID, &proposedBy, &summary, &rationale,
			&oldC, &newC, &st, &decidedBy, &decidedAt, &createdAt)
		if err == sql.ErrNoRows {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":                 pid,
			"script_id":          scriptID,
			"proposed_by_run_id": runID.String,
			"proposed_by":        proposedBy,
			"summary":            summary,
			"rationale":          rationale,
			"old_content":        oldC,
			"new_content":        newC,
			"status":             st,
			"decided_by":         decidedBy.String,
			"decided_at":         decidedAt,
			"created_at":         createdAt,
		})
	})

	r.Post("/api/script-patches/{id}/apply", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		// Lock the patch row, verify it's still pending and not stale.
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()
		var scriptID, newC, oldC, st string
		err = tx.QueryRowContext(r.Context(), `
			SELECT sp.script_id, sp.new_content, sp.old_content, sp.status
			FROM script_patches sp
			JOIN scripts s ON s.id = sp.script_id
			WHERE sp.id = $1 AND s.workspace_id = $2
			FOR UPDATE`, id, workspaceFromContext(r.Context())).Scan(&scriptID, &newC, &oldC, &st)
		if err == sql.ErrNoRows {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if st != "pending" {
			http.Error(w, "patch is not pending (status="+st+")", http.StatusConflict)
			return
		}
		// Drift check: if the script has changed since we proposed the patch, mark stale.
		var current string
		if err := tx.QueryRowContext(r.Context(), `SELECT content FROM scripts WHERE id = $1`, scriptID).Scan(&current); err != nil {
			http.Error(w, "script gone: "+err.Error(), http.StatusGone)
			return
		}
		if current != oldC {
			_, _ = tx.ExecContext(r.Context(),
				`UPDATE script_patches SET status='stale', decided_at=now() WHERE id=$1`, id)
			tx.Commit()
			http.Error(w, "script has changed since the patch was proposed; marked stale", http.StatusConflict)
			return
		}
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE scripts SET content = $1, updated_at = now() WHERE id = $2`, newC, scriptID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE script_patches SET status='applied', decided_by='user', decided_at=now() WHERE id=$1`, id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	r.Post("/api/script-patches/{id}/reject", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		res, err := db.ExecContext(r.Context(), `
			UPDATE script_patches SET status='rejected', decided_by='user', decided_at=now()
			WHERE id=$1 AND status='pending'
			  AND script_id IN (SELECT id FROM scripts WHERE workspace_id = $2)`,
			id, workspaceFromContext(r.Context()))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			http.Error(w, "not found or not pending", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
