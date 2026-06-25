package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

// ownedByWorkspace reports whether id exists in the given table within
// workspaceID. table is an internal literal; switching over it keeps the SQL
// static. A malformed id errors the query and reads as "not owned".
func ownedByWorkspace(ctx context.Context, db *sql.DB, table, id, workspaceID string) bool {
	var q string
	switch table {
	case "scripts":
		q = `SELECT 1 FROM scripts WHERE id = $1 AND workspace_id = $2`
	case "environments":
		q = `SELECT 1 FROM environments WHERE id = $1 AND workspace_id = $2`
	case "runs":
		q = `SELECT 1 FROM runs WHERE id = $1 AND workspace_id = $2`
	default:
		return false
	}
	var one int
	return db.QueryRowContext(ctx, q, id, workspaceID).Scan(&one) == nil
}

type RunRequest struct {
	ProjectID  string            `json:"project_id"`
	RootRunID  string            `json:"root_run_id"`
	Branch     string            `json:"branch"`
	CommitSHA  string            `json:"commit_sha"`
	Repo       string            `json:"repo"`
	TestFilter string            `json:"test_filter"`
	ScriptID   string            `json:"script_id"`
	EnvID      string            `json:"env_id"`
	EnvVars    map[string]string `json:"env_vars"`
}

func registerRunRoutes(r chi.Router, db *sql.DB, s3 *minio.Client, bucket string) {
	r.Post("/api/runs", func(w http.ResponseWriter, r *http.Request) {
		var req RunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		workspaceID := workspaceFromContext(r.Context())
		projectID := projectIDFromContext(r.Context())

		// Referenced script/env/root-run must belong to this workspace — the
		// runner fetches them by id, so an unchecked id is a cross-tenant read.
		if req.ScriptID != "" && !ownedByWorkspace(r.Context(), db, "scripts", req.ScriptID, workspaceID) {
			http.Error(w, "script_id not found in this workspace", http.StatusBadRequest)
			return
		}
		if req.EnvID != "" && !ownedByWorkspace(r.Context(), db, "environments", req.EnvID, workspaceID) {
			http.Error(w, "env_id not found in this workspace", http.StatusBadRequest)
			return
		}
		if req.RootRunID != "" && !ownedByWorkspace(r.Context(), db, "runs", req.RootRunID, workspaceID) {
			http.Error(w, "root_run_id not found in this workspace", http.StatusBadRequest)
			return
		}

		runID := uuid.New().String()

		var rootRunID interface{}
		if req.RootRunID != "" {
			rootRunID = req.RootRunID
		}
		var scriptID interface{}
		if req.ScriptID != "" {
			scriptID = req.ScriptID
		}
		var envID interface{}
		if req.EnvID != "" {
			envID = req.EnvID
		}
		var repoVal interface{}
		if req.Repo != "" {
			repoVal = req.Repo
		}

		envVarsJSON := json.RawMessage(`{}`)
		if len(req.EnvVars) > 0 {
			if b, err := json.Marshal(req.EnvVars); err == nil {
				envVarsJSON = b
			}
		}

		_, err := db.ExecContext(r.Context(),
			`INSERT INTO runs (id, project_id, workspace_id, root_run_id, branch, commit_sha, repo, test_filter, script_id, env_id, env_vars, status)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'queued')`,
			runID, projectID, workspaceID,
			rootRunID, req.Branch, req.CommitSHA, repoVal, req.TestFilter, scriptID, envID, envVarsJSON,
		)
		if err != nil {
			slog.Error("failed to insert run", "error", err)
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		slog.Info("queued run", "run_id", runID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"run_id": runID})
	})

	r.Get("/api/runs", func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.QueryContext(r.Context(), `
			SELECT r.id, r.project_id, r.branch, r.commit_sha, r.repo, r.status,
			       r.auto_triaged,
			       r.test_filter,
			       r.script_id, s.name AS script_name,
			       r.env_id, e.name AS env_name, e.slug AS env_slug,
			       r.started_at, r.finished_at, r.created_at, r.root_run_id,
			       EXISTS(
			           SELECT 1 FROM agent_messages am
			           WHERE am.run_id = r.root_run_id AND am.who = 'agent'
			       ) AS has_agent_activity,
			       r.webhook_source,
			       r.triage_classification, r.triage_confidence, r.triage_summary, r.triaged_at
			FROM runs r
			LEFT JOIN scripts s ON s.id = r.script_id
			LEFT JOIN environments e ON e.id = r.env_id
			WHERE r.workspace_id = $1
			ORDER BY r.created_at DESC LIMIT 200`, workspaceFromContext(r.Context()))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		runs := []RunListItem{}
		for rows.Next() {
			var item RunListItem
			if err := rows.Scan(
				&item.ID, &item.ProjectID, &item.Branch, &item.CommitSHA, &item.Repo, &item.Status,
				&item.AutoTriaged,
				&item.TestFilter,
				&item.ScriptID, &item.ScriptName,
				&item.EnvID, &item.EnvName, &item.EnvSlug,
				&item.StartedAt, &item.FinishedAt, &item.CreatedAt, &item.RootRunID,
				&item.HasAgentActivity,
				&item.WebhookSource,
				&item.TriageClassification, &item.TriageConfidence, &item.TriageSummary, &item.TriagedAt,
			); err != nil {
				slog.Error("scan run row", "error", err)
				continue
			}
			runs = append(runs, item)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(runs)
	})

	r.Get("/api/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")

		var run RunDetailResponse
		run.Results = []RunResultResponse{}
		run.Artifacts = []ArtifactResponse{}

		err := db.QueryRowContext(r.Context(), `
			SELECT r.id, r.project_id, r.branch, r.commit_sha, r.repo, r.status,
			       r.auto_triaged,
			       r.test_filter,
			       r.script_id, s.name AS script_name,
			       r.env_id, e.name AS env_name, e.slug AS env_slug,
			       r.started_at, r.finished_at, r.created_at, r.root_run_id,
			       EXISTS(
			           SELECT 1 FROM agent_messages am
			           WHERE am.run_id = r.root_run_id AND am.who = 'agent'
			       ) AS has_agent_activity,
			       r.webhook_source,
			       r.triage_classification, r.triage_confidence, r.triage_summary, r.triaged_at
			FROM runs r
			LEFT JOIN scripts s ON s.id = r.script_id
			LEFT JOIN environments e ON e.id = r.env_id
			WHERE r.id = $1 AND r.workspace_id = $2`, id, workspaceFromContext(r.Context())).Scan(
			&run.ID, &run.ProjectID, &run.Branch, &run.CommitSHA, &run.Repo, &run.Status,
			&run.AutoTriaged,
			&run.TestFilter,
			&run.ScriptID, &run.ScriptName,
			&run.EnvID, &run.EnvName, &run.EnvSlug,
			&run.StartedAt, &run.FinishedAt, &run.CreatedAt, &run.RootRunID,
			&run.HasAgentActivity,
			&run.WebhookSource,
			&run.TriageClassification, &run.TriageConfidence, &run.TriageSummary, &run.TriagedAt)
		if err == sql.ErrNoRows {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Results — run already scoped to workspace above, so child rows inherit safely.
		resRows, err := db.QueryContext(r.Context(),
			`SELECT id, test_name, status, duration_ms, logs FROM run_results WHERE run_id = $1`, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Map result ID → slice index so steps can be appended safely after potential reallocation.
		resultsByID := map[string]int{}
		for resRows.Next() {
			var rr RunResultResponse
			rr.Steps = []StepResponse{}
			resRows.Scan(&rr.ID, &rr.TestName, &rr.Status, &rr.DurationMS, &rr.Logs)
			resultsByID[rr.ID] = len(run.Results)
			run.Results = append(run.Results, rr)
		}
		resRows.Close()

		if len(run.Results) > 0 {
			stepRows, sErr := db.QueryContext(r.Context(), `
				SELECT s.run_result_id, s.idx, s.api_name, s.selector, s.url, s.status,
				       s.start_ms, s.duration_ms, s.error
				FROM steps s
				JOIN run_results rr ON s.run_result_id = rr.id
				WHERE rr.run_id = $1
				ORDER BY s.run_result_id, s.idx`, id)
			if sErr == nil {
				for stepRows.Next() {
					var rrid string
					var step StepResponse
					stepRows.Scan(&rrid, &step.Idx, &step.APIName, &step.Selector, &step.URL,
						&step.Status, &step.StartMS, &step.DurationMS, &step.Error)
					if idx, ok := resultsByID[rrid]; ok {
						run.Results[idx].Steps = append(run.Results[idx].Steps, step)
					}
				}
				stepRows.Close()
			} else {
				slog.Warn("steps query failed", "error", sErr)
			}
		}

		// Artifacts with presigned URLs
		artRows, err := db.QueryContext(r.Context(), `
			SELECT a.id, a.kind, a.storage_key, a.size_bytes
			FROM artifacts a JOIN run_results rr ON a.run_result_id = rr.id
			WHERE rr.run_id = $1`, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer artRows.Close()
		for artRows.Next() {
			var art ArtifactResponse
			artRows.Scan(&art.ID, &art.Kind, &art.StorageKey, &art.SizeBytes)
			if u, err := s3.PresignedGetObject(r.Context(), bucket, art.StorageKey, 7*24*time.Hour, nil); err == nil {
				art.URL = u.String()
			} else {
				slog.Error("presign failed", "key", art.StorageKey, "error", err)
			}
			run.Artifacts = append(run.Artifacts, art)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(run)
	})

	// POST /api/runs/{id}/cancel — marks queued or running runs as cancelled.
	// The runner cooperates by checking status before persisting results, so an
	// in-flight Playwright execution will still finish on the host but its output
	// is discarded. Finalised runs (passed/failed/cancelled) can't be cancelled.
	r.Post("/api/runs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		res, err := db.ExecContext(r.Context(), `
			UPDATE runs SET status = 'cancelled', finished_at = COALESCE(finished_at, NOW())
			WHERE id = $1 AND workspace_id = $2 AND status IN ('queued', 'running')`,
			id, workspaceFromContext(r.Context()))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			http.Error(w, "run not cancellable (already finalised or not found)", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	r.Get("/api/artifacts/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var key string
		err := db.QueryRowContext(r.Context(), `
			SELECT a.storage_key FROM artifacts a
			JOIN run_results rr ON a.run_result_id = rr.id
			JOIN runs r ON rr.run_id = r.id
			WHERE a.id = $1 AND r.workspace_id = $2`,
			id, workspaceFromContext(r.Context())).Scan(&key)
		if err == sql.ErrNoRows {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		u, err := s3.PresignedGetObject(r.Context(), bucket, key, 7*24*time.Hour, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, u.String(), http.StatusTemporaryRedirect)
	})
}
