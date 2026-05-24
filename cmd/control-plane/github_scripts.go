package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// Script-from-repo wiring (snapshot model):
//
//   - Import copies the file content into scripts.content and records the
//     source linkage (integration_id, path, ref, sha) so resync knows what to
//     pull.
//   - Resync re-fetches from the same path/ref and updates content + sha. It's
//     driven manually via /sync or automatically by the GitHub push webhook
//     (see github_webhook.go).
//   - Status compares scripts.source_sha to the latest commit on source_ref.
//
// The runner still executes scripts out of the DB; repo content never reaches
// the runner directly.

func registerGithubScriptRoutes(r chi.Router, db *sql.DB) {
	// GET /api/integrations/{id}/repo-tree?path=&ref=
	r.Get("/api/integrations/{id}/repo-tree", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		dirPath := strings.TrimPrefix(r.URL.Query().Get("path"), "/")
		ref := r.URL.Query().Get("ref")

		row, ext, secret, err := loadGithubIntegration(r.Context(), db, workspaceFromContext(r.Context()), "id:"+id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		token, _, err := resolveGithubToken(r.Context(), row.ID, ext, secret)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if ref == "" {
			ref = ext.DefaultRef
		}
		entries, err := listRepoTree(r.Context(), token, ext.Owner, ext.Repo, dirPath, ref)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"path":    dirPath,
			"ref":     ref,
			"entries": entries,
		})
	})

	// POST /api/scripts/import/github  { integration_id, paths: [], ref? }
	r.Post("/api/scripts/import/github", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IntegrationID string   `json:"integration_id"`
			Paths         []string `json:"paths"`
			Ref           string   `json:"ref"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if body.IntegrationID == "" || len(body.Paths) == 0 {
			http.Error(w, "integration_id and paths are required", http.StatusBadRequest)
			return
		}

		row, ext, secret, err := loadGithubIntegration(r.Context(), db, workspaceFromContext(r.Context()), "id:"+body.IntegrationID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		token, _, err := resolveGithubToken(r.Context(), row.ID, ext, secret)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if body.Ref == "" {
			body.Ref = ext.DefaultRef
		}
		projectID := projectIDFromContext(r.Context())
		if projectID == "" {
			http.Error(w, "no project context", http.StatusBadRequest)
			return
		}

		imported := make([]map[string]string, 0, len(body.Paths))
		errs := make([]map[string]string, 0)
		for _, p := range body.Paths {
			p = strings.TrimPrefix(p, "/")
			if p == "" {
				continue
			}
			file, ferr := fetchRepoFile(r.Context(), token, ext.Owner, ext.Repo, p, body.Ref)
			if ferr != nil {
				errs = append(errs, map[string]string{"path": p, "error": ferr.Error()})
				continue
			}
			scriptID, ierr := upsertImportedScript(r.Context(), db, workspaceFromContext(r.Context()), projectID, row.ID, ext.Owner+"/"+ext.Repo, body.Ref, p, file)
			if ierr != nil {
				errs = append(errs, map[string]string{"path": p, "error": ierr.Error()})
				continue
			}
			imported = append(imported, map[string]string{"id": scriptID, "path": p})
		}

		w.Header().Set("Content-Type", "application/json")
		if len(imported) == 0 {
			w.WriteHeader(http.StatusBadGateway)
		} else {
			w.WriteHeader(http.StatusCreated)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"imported": imported,
			"errors":   errs,
		})
	})

	// POST /api/scripts/{id}/sync — refetch and update.
	r.Post("/api/scripts/{id}/sync", func(w http.ResponseWriter, r *http.Request) {
		scriptID := chi.URLParam(r, "id")
		info, err := loadScriptSource(r.Context(), db, scriptID, workspaceFromContext(r.Context()))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if info.kind != "github" {
			http.Error(w, "this script isn't linked to a repo", http.StatusBadRequest)
			return
		}
		row, ext, secret, err := loadGithubIntegration(r.Context(), db, workspaceFromContext(r.Context()), "id:"+info.integrationID)
		if err != nil {
			http.Error(w, "source integration missing or deleted", http.StatusGone)
			return
		}
		token, _, err := resolveGithubToken(r.Context(), row.ID, ext, secret)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		newSHA, err := resyncScript(r.Context(), db, token, ext.Owner, ext.Repo, scriptID, info.path, info.ref)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         scriptID,
			"source_sha": newSHA,
			"synced_at":  time.Now(),
		})
	})

	// GET /api/scripts/{id}/sync-status — does the upstream ref still match
	// our stored sha?
	r.Get("/api/scripts/{id}/sync-status", func(w http.ResponseWriter, r *http.Request) {
		scriptID := chi.URLParam(r, "id")
		info, err := loadScriptSource(r.Context(), db, scriptID, workspaceFromContext(r.Context()))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if info.kind != "github" {
			http.Error(w, "this script isn't linked to a repo", http.StatusBadRequest)
			return
		}
		row, ext, secret, err := loadGithubIntegration(r.Context(), db, workspaceFromContext(r.Context()), "id:"+info.integrationID)
		if err != nil {
			http.Error(w, "source integration missing or deleted", http.StatusGone)
			return
		}
		token, _, err := resolveGithubToken(r.Context(), row.ID, ext, secret)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		latest, err := latestFileSHA(r.Context(), token, ext.Owner, ext.Repo, info.path, info.ref)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":           scriptID,
			"current_sha":  info.sha,
			"upstream_sha": latest,
			"up_to_date":   latest == info.sha,
			"synced_at":    info.syncedAt,
			"source_repo":  info.repo,
			"source_path":  info.path,
			"source_ref":   info.ref,
		})
	})
}

// resyncScript re-fetches a linked script's file at the given ref and writes
// the new content + sha back. Shared by the manual /sync endpoint and the
// push webhook. Returns the upstream blob sha.
func resyncScript(ctx context.Context, db *sql.DB, token, owner, repo, scriptID, sourcePath, ref string) (string, error) {
	file, err := fetchRepoFile(ctx, token, owner, repo, sourcePath, ref)
	if err != nil {
		return "", err
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE scripts
		   SET content    = $1,
		       source_sha = $2,
		       synced_at  = now(),
		       updated_at = now()
		 WHERE id = $3`,
		file.Content, file.SHA, scriptID); err != nil {
		return "", err
	}
	return file.SHA, nil
}

// ─── GitHub helpers ────────────────────────────────────────────────────

type repoTreeEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"` // "file" | "dir"
	Size int64  `json:"size,omitempty"`
}

func listRepoTree(ctx context.Context, token, owner, repo, dirPath, ref string) ([]repoTreeEntry, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s?ref=%s",
		owner, repo, url.PathEscape(dirPath), url.QueryEscape(ref))
	body, status, err := githubGET(ctx, token, u, nil)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("path %q not found on ref %q", dirPath, ref)
	}
	if status >= 300 {
		return nil, fmt.Errorf("github %d: %s", status, string(body))
	}
	// The contents API returns an array for directories, an object for files.
	// We only support directories here — the import endpoint is what fetches files.
	if len(body) > 0 && body[0] == '{' {
		return nil, fmt.Errorf("path %q is a file, not a directory", dirPath)
	}
	var raw []struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Type string `json:"type"`
		Size int64  `json:"size"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse github response: %w", err)
	}
	out := make([]repoTreeEntry, 0, len(raw))
	for _, e := range raw {
		out = append(out, repoTreeEntry{Name: e.Name, Path: e.Path, Type: e.Type, Size: e.Size})
	}
	return out, nil
}

type repoFile struct {
	Path    string
	SHA     string
	Content string
}

func fetchRepoFile(ctx context.Context, token, owner, repo, filePath, ref string) (*repoFile, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s?ref=%s",
		owner, repo, url.PathEscape(filePath), url.QueryEscape(ref))
	body, status, err := githubGET(ctx, token, u, nil)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("file %q not found on ref %q", filePath, ref)
	}
	if status >= 300 {
		return nil, fmt.Errorf("github %d: %s", status, string(body))
	}
	var raw struct {
		Path     string `json:"path"`
		SHA      string `json:"sha"`
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse github response: %w", err)
	}
	if raw.Type != "file" {
		return nil, fmt.Errorf("%q is a %s, not a file", filePath, raw.Type)
	}
	if raw.Encoding != "base64" {
		return nil, fmt.Errorf("unexpected encoding %q for %q", raw.Encoding, filePath)
	}
	// GitHub wraps base64 in newlines; standard decoder tolerates that with
	// the Std encoding (not RawStd).
	decoded, err := base64.StdEncoding.DecodeString(strings.NewReplacer("\n", "", "\r", "", " ", "").Replace(raw.Content))
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	return &repoFile{Path: raw.Path, SHA: raw.SHA, Content: string(decoded)}, nil
}

// latestFileSHA fetches just the metadata for a file (no content) so we can
// cheaply compare against scripts.source_sha during sync-status polls.
func latestFileSHA(ctx context.Context, token, owner, repo, filePath, ref string) (string, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s?ref=%s",
		owner, repo, url.PathEscape(filePath), url.QueryEscape(ref))
	// Use the "object" media type to skip content payload — costs less.
	body, status, err := githubGET(ctx, token, u, map[string]string{
		"Accept": "application/vnd.github.object+json",
	})
	if err != nil {
		return "", fmt.Errorf("github: %w", err)
	}
	if status >= 300 {
		return "", fmt.Errorf("github %d: %s", status, string(body))
	}
	var raw struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", fmt.Errorf("parse github response: %w", err)
	}
	if raw.SHA == "" {
		return "", errors.New("github returned empty sha")
	}
	return raw.SHA, nil
}

// ─── DB helpers ────────────────────────────────────────────────────────

func upsertImportedScript(ctx context.Context, db *sql.DB, workspaceID, projectID, integrationID, repo, ref, sourcePath string, file *repoFile) (string, error) {
	// Use the file's basename as both the script name and the playwright
	// filename. Operators can rename later in the UI.
	filename := path.Base(sourcePath)
	scriptName := strings.TrimSuffix(filename, path.Ext(filename))

	var id string
	err := db.QueryRowContext(ctx, `
		INSERT INTO scripts (workspace_id, project_id, name, filename, content,
		                     source_kind, source_integration_id,
		                     source_repo, source_path, source_ref, source_sha,
		                     synced_at)
		VALUES ($1, $2, $3, $4, $5, 'github', $6, $7, $8, $9, $10, now())
		ON CONFLICT DO NOTHING
		RETURNING id`,
		workspaceID, projectID, scriptName, filename, file.Content,
		integrationID, repo, sourcePath, ref, file.SHA,
	).Scan(&id)
	if err == sql.ErrNoRows {
		// Row already exists for this (project, source_integration, source_path).
		// Update content + sha rather than fail — re-importing is a sync.
		err = db.QueryRowContext(ctx, `
			UPDATE scripts
			   SET content    = $1,
			       source_ref = $2,
			       source_sha = $3,
			       synced_at  = now(),
			       updated_at = now()
			 WHERE source_integration_id = $4 AND source_path = $5
			 RETURNING id`,
			file.Content, ref, file.SHA, integrationID, sourcePath,
		).Scan(&id)
	}
	return id, err
}

type scriptSourceInfo struct {
	kind          string
	integrationID string
	repo          string
	path          string
	ref           string
	sha           string
	syncedAt      *time.Time
}

func loadScriptSource(ctx context.Context, db *sql.DB, scriptID, workspaceID string) (*scriptSourceInfo, error) {
	var info scriptSourceInfo
	var integrationID sql.NullString
	var repo, p, ref, sha sql.NullString
	var syncedAt sql.NullTime
	err := db.QueryRowContext(ctx, `
		SELECT s.source_kind, s.source_integration_id, s.source_repo,
		       s.source_path, s.source_ref, s.source_sha, s.synced_at
		  FROM scripts s
		  JOIN projects p ON p.id = s.project_id
		 WHERE s.id = $1 AND p.workspace_id = $2`,
		scriptID, workspaceID,
	).Scan(&info.kind, &integrationID, &repo, &p, &ref, &sha, &syncedAt)
	if err == sql.ErrNoRows {
		return nil, errors.New("script not found")
	}
	if err != nil {
		return nil, err
	}
	if integrationID.Valid {
		info.integrationID = integrationID.String
	}
	if repo.Valid {
		info.repo = repo.String
	}
	if p.Valid {
		info.path = p.String
	}
	if ref.Valid {
		info.ref = ref.String
	}
	if sha.Valid {
		info.sha = sha.String
	}
	if syncedAt.Valid {
		info.syncedAt = &syncedAt.Time
	}
	return &info, nil
}
