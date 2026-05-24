package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
)

// GitHub-native webhook receiver. Distinct from /api/webhooks/run (our custom
// CI trigger): this consumes GitHub's own push events, verifies them with
// HMAC, and resyncs any repo-linked scripts whose file changed — so edits in
// the repo flow into Replay without a manual "Sync" click.

type githubPushPayload struct {
	Ref        string `json:"ref"` // refs/heads/<branch>
	Repository struct {
		FullName string `json:"full_name"` // owner/repo
	} `json:"repository"`
	Commits []struct {
		Added    []string `json:"added"`
		Modified []string `json:"modified"`
	} `json:"commits"`
}

// changedPaths returns the unique set of added+modified file paths across all
// commits in a push. Removed files are excluded — there's nothing to re-fetch.
func changedPaths(p *githubPushPayload) map[string]bool {
	set := map[string]bool{}
	for _, c := range p.Commits {
		for _, f := range c.Added {
			set[f] = true
		}
		for _, f := range c.Modified {
			set[f] = true
		}
	}
	return set
}

// verifyGithubSignature checks the X-Hub-Signature-256 header against the raw
// body using the shared secret. GitHub sends "sha256=<hex>".
func verifyGithubSignature(sigHeader, secret string, body []byte) bool {
	if secret == "" || !strings.HasPrefix(sigHeader, "sha256=") {
		return false
	}
	want := strings.TrimPrefix(sigHeader, "sha256=")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	got := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(got), []byte(want))
}

func registerGithubWebhookRoute(r chi.Router, db *sql.DB) {
	// POST /api/webhooks/github — GitHub push events → resync linked scripts.
	r.Post("/api/webhooks/github", func(w http.ResponseWriter, req *http.Request) {
		event := req.Header.Get("X-GitHub-Event")
		raw, err := io.ReadAll(io.LimitReader(req.Body, 5<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		// Health check GitHub sends when a webhook is first configured.
		if event == "ping" {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		if event != "push" {
			writeJSON(w, http.StatusOK, map[string]any{"ignored": event})
			return
		}

		var payload githubPushPayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			http.Error(w, "invalid push payload", http.StatusBadRequest)
			return
		}
		fullName := payload.Repository.FullName
		branch := strings.TrimPrefix(payload.Ref, "refs/heads/")
		if fullName == "" || branch == "" {
			http.Error(w, "missing repository.full_name or branch ref", http.StatusBadRequest)
			return
		}
		changed := changedPaths(&payload)
		if len(changed) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{"synced": []string{}, "count": 0})
			return
		}

		sig := req.Header.Get("X-Hub-Signature-256")
		globalSecret := strings.TrimSpace(os.Getenv("REPLAY_GITHUB_WEBHOOK_SECRET"))

		// Find every github integration whose configured owner/repo matches the
		// pushed repo (possibly across workspaces if the same repo is connected
		// in more than one). GitHub delivers one signed request per configured
		// webhook, so exactly one secret should verify per delivery.
		rows, err := db.QueryContext(req.Context(), `
			SELECT id, workspace_id
			  FROM integrations
			 WHERE provider = 'github'
			   AND lower(config->>'owner') || '/' || lower(config->>'repo') = lower($1)`,
			fullName)
		if err != nil {
			http.Error(w, "integration lookup failed", http.StatusInternalServerError)
			return
		}
		type cand struct{ id, workspaceID string }
		var candidates []cand
		for rows.Next() {
			var c cand
			if err := rows.Scan(&c.id, &c.workspaceID); err == nil {
				candidates = append(candidates, c)
			}
		}
		rows.Close()
		if len(candidates) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{"synced": []string{}, "count": 0, "note": "no github integration matches " + fullName})
			return
		}

		synced := []string{}
		verified := false
		for _, c := range candidates {
			row, ext, secret, err := loadGithubIntegration(req.Context(), db, c.workspaceID, "id:"+c.id)
			if err != nil {
				continue
			}
			wantSecret := ext.WebhookSecret
			if wantSecret == "" {
				wantSecret = globalSecret
			}
			if !verifyGithubSignature(sig, wantSecret, raw) {
				continue
			}
			verified = true

			token, _, err := resolveGithubToken(req.Context(), row.ID, ext, secret)
			if err != nil {
				slog.Warn("github webhook: token resolve failed", "integration", c.id, "error", err)
				continue
			}
			ids, err := resyncChangedScripts(req.Context(), db, token, ext, c.id, branch, changed)
			if err != nil {
				slog.Warn("github webhook: resync failed", "integration", c.id, "error", err)
				continue
			}
			synced = append(synced, ids...)
		}

		// A signed-but-unverifiable delivery (secret mismatch / missing) is a
		// real auth failure — surface it rather than silently 200.
		if sig != "" && !verified {
			http.Error(w, "signature verification failed", http.StatusUnauthorized)
			return
		}
		slog.Info("github webhook: push processed", "repo", fullName, "branch", branch, "synced", len(synced))
		writeJSON(w, http.StatusOK, map[string]any{"synced": synced, "count": len(synced)})
	})
}

// resyncChangedScripts updates every github-linked script for the integration
// whose source_ref is the pushed branch and whose source_path was changed.
// Returns the script ids that were resynced.
func resyncChangedScripts(ctx context.Context, db *sql.DB, token string, ext *extendedGithubConfig, integrationID, branch string, changed map[string]bool) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, source_path FROM scripts
		 WHERE source_integration_id = $1 AND source_kind = 'github' AND source_ref = $2`,
		integrationID, branch)
	if err != nil {
		return nil, err
	}
	type target struct{ id, path string }
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.path); err == nil && changed[t.path] {
			targets = append(targets, t)
		}
	}
	rows.Close()

	var done []string
	for _, t := range targets {
		if _, err := resyncScript(ctx, db, token, ext.Owner, ext.Repo, t.id, t.path, branch); err != nil {
			slog.Warn("github webhook: script resync failed", "script", t.id, "path", t.path, "error", err)
			continue
		}
		done = append(done, t.id)
	}
	return done, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
