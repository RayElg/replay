package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// EnvironmentRequest carries the environment name, slug, and the env_vars
// the runner will export into the test process.
//
// AT-REST STORAGE: values in env_vars are encrypted with replaycrypto under
// REPLAY_ENCRYPT_KEY. Keys stay cleartext so the UI can list them without
// fetching every value. Plaintext rows that predate this migration decrypt
// as-is and get re-encrypted on the next save.
//
// SECRETS: keys named in SecretKeys are write-only — the API masks their values
// on read (secretMask) and never returns plaintext to the browser. On save, a
// value still equal to secretMask means "unchanged": the stored ciphertext is
// preserved rather than overwritten with the mask string.
type EnvironmentRequest struct {
	Name       string            `json:"name"`
	Slug       string            `json:"slug"`
	EnvVars    map[string]string `json:"env_vars"`
	SecretKeys []string          `json:"secret_keys"`
}

// secretMask is the placeholder the API returns in place of a secret value, and
// the sentinel it recognises on write to mean "keep the existing ciphertext".
// Chosen to be visually obvious and vanishingly unlikely as a real value.
const secretMask = "••••••••"

// buildStoredEnvVars encrypts incoming values for storage. For a secret key
// whose incoming value is still the mask, it preserves the existing ciphertext
// instead of overwriting it (write-only semantics) — so editing an environment
// without retyping every secret doesn't clobber them. existingEnc is the current
// stored (encrypted) map, or nil for a brand-new environment.
func buildStoredEnvVars(incoming map[string]string, secretKeys []string, existingEnc map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(incoming))
	for k, v := range incoming {
		// The mask is never a legitimate value: whenever it comes back, keep the
		// stored ciphertext. Unconditional (not gated on secretKeys) so unmarking
		// a secret can't accidentally persist the mask string as the real value.
		if v == secretMask {
			if prev, ok := existingEnc[k]; ok {
				out[k] = prev
			}
			// No prior value behind the mask — drop the placeholder.
			continue
		}
		enc, err := encryptSecret(v)
		if err != nil {
			if errors.Is(err, errNoSecretKey) {
				out[k] = v // no key configured: store plaintext (opt-in encryption)
				continue
			}
			return nil, err
		}
		out[k] = enc
	}
	return out, nil
}

// maskedEnvVars returns the env_vars map as the API exposes it: secret keys
// replaced with the mask, the rest verbatim. Used to echo a freshly written
// environment back without leaking secret plaintext.
func maskedEnvVars(in map[string]string, secretKeys []string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if contains(secretKeys, k) {
			out[k] = secretMask
		} else {
			out[k] = v
		}
	}
	return out
}

// loadStoredEnvVars fetches the current encrypted env_vars for an environment,
// so a write can preserve secret values that came back masked.
func loadStoredEnvVars(ctx context.Context, db *sql.DB, id, workspaceID string) (map[string]string, error) {
	var raw []byte
	err := db.QueryRowContext(ctx,
		`SELECT env_vars FROM environments WHERE id = $1 AND workspace_id = $2`, id, workspaceID,
	).Scan(&raw)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

func registerEnvironmentRoutes(r chi.Router, db *sql.DB) {
	r.Get("/api/environments", func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.QueryContext(r.Context(), `
			SELECT id, project_id, name, slug, env_vars, to_json(secret_keys)::text, created_at
			FROM environments WHERE workspace_id = $1 ORDER BY name`,
			workspaceFromContext(r.Context()))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		envs := []EnvironmentResponse{}
		for rows.Next() {
			var env EnvironmentResponse
			var envVarsRaw []byte
			var secretKeysJSON string
			rows.Scan(&env.ID, &env.ProjectID, &env.Name, &env.Slug, &envVarsRaw, &secretKeysJSON, &env.CreatedAt)
			env.SecretKeys = []string{}
			_ = json.Unmarshal([]byte(secretKeysJSON), &env.SecretKeys)
			var encrypted map[string]string
			if err := json.Unmarshal(envVarsRaw, &encrypted); err != nil || encrypted == nil {
				encrypted = map[string]string{}
			}
			env.EnvVars = map[string]string{}
			for k, v := range encrypted {
				// Write-only: secret values never leave the server in plaintext.
				// The UI shows the mask so the operator knows a value is set.
				if contains(env.SecretKeys, k) {
					env.EnvVars[k] = secretMask
					continue
				}
				pt, derr := decryptSecret(v)
				if derr != nil {
					if errors.Is(derr, errNoSecretKey) {
						pt = v // no key configured: value stored as plaintext
					} else {
						slog.Warn("env_var decrypt failed", "env_id", env.ID, "key", k, "error", derr)
						pt = ""
					}
				}
				env.EnvVars[k] = pt
			}
			envs = append(envs, env)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(envs)
	})

	r.Post("/api/environments", func(w http.ResponseWriter, r *http.Request) {
		var req EnvironmentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if req.Name == "" || req.Slug == "" {
			http.Error(w, "name and slug required", http.StatusBadRequest)
			return
		}
		if req.EnvVars == nil {
			req.EnvVars = map[string]string{}
		}
		if req.SecretKeys == nil {
			req.SecretKeys = []string{}
		}
		encVars, err := buildStoredEnvVars(req.EnvVars, req.SecretKeys, nil)
		if err != nil {
			http.Error(w, "encrypt env_vars: "+err.Error(), http.StatusInternalServerError)
			return
		}
		varsJSON, _ := json.Marshal(encVars)
		secretsJSON, _ := json.Marshal(req.SecretKeys)
		env := EnvironmentResponse{
			ID:         uuid.New().String(),
			ProjectID:  projectIDFromContext(r.Context()),
			Name:       req.Name,
			Slug:       req.Slug,
			EnvVars:    maskedEnvVars(req.EnvVars, req.SecretKeys),
			SecretKeys: req.SecretKeys,
		}
		err = db.QueryRowContext(r.Context(), `
			INSERT INTO environments (id, project_id, workspace_id, name, slug, env_vars, secret_keys)
			VALUES ($1, $2, $3, $4, $5, $6,
				COALESCE((SELECT array_agg(value::text) FROM json_array_elements_text($7::json)), '{}'))
			RETURNING created_at`,
			env.ID, env.ProjectID, workspaceFromContext(r.Context()), env.Name, env.Slug, varsJSON, string(secretsJSON),
		).Scan(&env.CreatedAt)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(env)
	})

	r.Put("/api/environments/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var req EnvironmentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if req.EnvVars == nil {
			req.EnvVars = map[string]string{}
		}
		if req.SecretKeys == nil {
			req.SecretKeys = []string{}
		}
		// Load the current ciphertext so masked secrets (sent back unchanged) are
		// preserved instead of being overwritten with the mask string.
		existingEnc, lerr := loadStoredEnvVars(r.Context(), db, id, workspaceFromContext(r.Context()))
		if errors.Is(lerr, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		} else if lerr != nil {
			http.Error(w, lerr.Error(), http.StatusInternalServerError)
			return
		}
		encVars, err := buildStoredEnvVars(req.EnvVars, req.SecretKeys, existingEnc)
		if err != nil {
			http.Error(w, "encrypt env_vars: "+err.Error(), http.StatusInternalServerError)
			return
		}
		varsJSON, _ := json.Marshal(encVars)
		secretsJSON, _ := json.Marshal(req.SecretKeys)
		res, err := db.ExecContext(r.Context(), `
			UPDATE environments SET name = $1, slug = $2, env_vars = $3,
				secret_keys = COALESCE((SELECT array_agg(value::text) FROM json_array_elements_text($4::json)), '{}')
			WHERE id = $5 AND workspace_id = $6`,
			req.Name, req.Slug, varsJSON, string(secretsJSON), id, workspaceFromContext(r.Context()))
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

	r.Delete("/api/environments/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		res, err := db.ExecContext(r.Context(), `DELETE FROM environments WHERE id = $1 AND workspace_id = $2`, id, workspaceFromContext(r.Context()))
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
