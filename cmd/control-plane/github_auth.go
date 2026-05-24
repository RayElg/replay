package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// GitHub auth resolver.
//
// We support two flavours of auth on the same integration row:
//
//   1. Personal access token (PAT) — `config.auth_kind == "pat"` (or absent).
//      The `encrypted_token` column holds the PAT verbatim. Token is returned
//      as-is on every call; nothing is cached because the PAT itself is the
//      bearer.
//
//   2. GitHub App installation — `config.auth_kind == "app"`. The
//      `encrypted_token` column holds the App's PEM-encoded RSA private key.
//      `config.app_id` and `config.installation_id` complete the triple. On
//      each call we mint a short-lived (10 min) signed JWT, exchange it for a
//      1-hour installation token via GitHub's API, and cache the result.
//
// The resolver returns a token + the expiry, so callers that loop or batch
// many requests can pre-check freshness if they want. The Bearer header
// format is identical for both auth kinds, so downstream code (`githubGET`)
// stays the same.
//
// Concurrency: one cache entry per integration_id, behind a single mutex.
// Token mints are not in the hot path (1h cache hits dominate), so a single
// global mutex is fine for now.

type githubAuthKind string

const (
	githubAuthPAT githubAuthKind = "pat"
	githubAuthApp githubAuthKind = "app"
)

// extendedGithubConfig is the superset of githubConfig that captures App-mode
// fields. Old PAT-only integration rows decode here cleanly: the App fields
// stay empty and AuthKind defaults to "pat".
type extendedGithubConfig struct {
	Owner          string `json:"owner"`
	Repo           string `json:"repo"`
	DefaultRef     string `json:"default_ref"`
	AuthKind       string `json:"auth_kind"`       // "pat" (default) or "app"
	AppID          string `json:"app_id"`          // App-mode only
	InstallationID string `json:"installation_id"` // App-mode only
	WebhookSecret  string `json:"webhook_secret"`  // shared secret for verifying GitHub push webhooks (optional)
}

func (c *extendedGithubConfig) authKind() githubAuthKind {
	if c.AuthKind == "app" {
		return githubAuthApp
	}
	return githubAuthPAT
}

type cachedInstallationToken struct {
	Token     string
	ExpiresAt time.Time
}

var (
	installationTokenMu    sync.Mutex
	installationTokenCache = map[string]cachedInstallationToken{}
)

// resolveGithubToken returns a usable Bearer token for the given integration.
// `integrationID` keys the App installation-token cache so multiple
// integrations don't fight over the same slot.
//
// `secret` is the value out of `encrypted_token` *after* decryption — either a
// PAT or a PEM private key, depending on cfg.AuthKind. Passing the decrypted
// value in (instead of fetching it here) keeps `secrets.go` as the single
// place that touches encrypted columns and makes this function easy to unit
// test with synthetic inputs.
func resolveGithubToken(ctx context.Context, integrationID string, cfg *extendedGithubConfig, secret string) (string, time.Time, error) {
	if secret == "" {
		return "", time.Time{}, errors.New("integration has no stored credential — re-add it with a PAT or App private key")
	}
	switch cfg.authKind() {
	case githubAuthPAT:
		// PATs don't have a useful expiry from our perspective — GitHub will
		// reject them when revoked. Pretend they last forever so callers can
		// treat the return type uniformly.
		return secret, time.Now().Add(365 * 24 * time.Hour), nil

	case githubAuthApp:
		if cfg.AppID == "" || cfg.InstallationID == "" {
			return "", time.Time{}, errors.New("github app integration is missing app_id/installation_id in config")
		}

		installationTokenMu.Lock()
		if cached, ok := installationTokenCache[integrationID]; ok && time.Until(cached.ExpiresAt) > 5*time.Minute {
			installationTokenMu.Unlock()
			return cached.Token, cached.ExpiresAt, nil
		}
		installationTokenMu.Unlock()

		jwt, err := buildGithubAppJWT(cfg.AppID, secret)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("build app jwt: %w", err)
		}
		token, expiresAt, err := exchangeInstallationToken(ctx, cfg.InstallationID, jwt)
		if err != nil {
			return "", time.Time{}, err
		}
		installationTokenMu.Lock()
		installationTokenCache[integrationID] = cachedInstallationToken{Token: token, ExpiresAt: expiresAt}
		installationTokenMu.Unlock()
		return token, expiresAt, nil
	}
	return "", time.Time{}, fmt.Errorf("unknown github auth kind %q", cfg.AuthKind)
}

// buildGithubAppJWT signs an RS256 JWT proving we hold the App's private key.
// 10-minute lifetime is GitHub's maximum (they reject longer claims).
//
// We implement the signing by hand rather than pulling a JWT library — the
// surface is small (header + claims, base64url, single RSA sign) and avoiding
// a dependency for one signed token per hour is the right trade.
func buildGithubAppJWT(appID, privatePEM string) (string, error) {
	key, err := parsePKCS1or8PrivateKey(privatePEM)
	if err != nil {
		return "", err
	}
	now := time.Now()
	header := map[string]any{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iat": now.Add(-30 * time.Second).Unix(), // small skew tolerance
		"exp": now.Add(9 * time.Minute).Unix(),   // under GitHub's 10min cap
		"iss": appID,
	}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	signingInput := base64URL(hb) + "." + base64URL(cb)

	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64URL(sig), nil
}

// parsePKCS1or8PrivateKey accepts either traditional ("BEGIN RSA PRIVATE KEY")
// or PKCS8 ("BEGIN PRIVATE KEY") PEM. GitHub Apps used to emit PKCS1 and now
// emit either, depending on how the key was downloaded — support both rather
// than make the operator convert.
func parsePKCS1or8PrivateKey(privatePEM string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(privatePEM))
	if block == nil {
		return nil, errors.New("private key is not PEM-encoded")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github app key must be RSA")
	}
	return rsaKey, nil
}

func base64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// exchangeInstallationToken calls /app/installations/{id}/access_tokens with
// the App JWT and returns the short-lived installation token plus its expiry.
func exchangeInstallationToken(ctx context.Context, installationID, appJWT string) (string, time.Time, error) {
	url := "https://api.github.com/app/installations/" + installationID + "/access_tokens"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "replay-control-plane")
	resp, err := githubHTTP.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body := make([]byte, 1024)
		n, _ := resp.Body.Read(body)
		return "", time.Time{}, fmt.Errorf("github app token exchange: %d %s", resp.StatusCode, strings.TrimSpace(string(body[:n])))
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", time.Time{}, fmt.Errorf("decode installation token: %w", err)
	}
	if out.Token == "" {
		return "", time.Time{}, errors.New("github returned empty installation token")
	}
	return out.Token, out.ExpiresAt, nil
}

// loadGithubIntegration is the shared SQL-side loader used by both the agent
// tools and the new script-sync endpoints. It returns the integration row,
// extended config, and the *decrypted* token/key (the caller passes that to
// resolveGithubToken to actually obtain a Bearer credential).
//
// `selector` picks the row: "id:<uuid>" loads by primary key,
// "name:<owner/repo>" loads by integration name, "" loads the most recent
// GitHub integration for the workspace.
func loadGithubIntegration(ctx context.Context, db *sql.DB, workspaceID, selector string) (*integrationRow, *extendedGithubConfig, string, error) {
	var row integrationRow
	var enc string
	var err error
	switch {
	case strings.HasPrefix(selector, "id:"):
		id := strings.TrimPrefix(selector, "id:")
		err = db.QueryRowContext(ctx, `
			SELECT id, project_id, provider, name, config, encrypted_token, created_at, updated_at
			FROM integrations WHERE id = $1 AND workspace_id = $2 AND provider = 'github'`,
			id, workspaceID,
		).Scan(&row.ID, &row.ProjectID, &row.Provider, &row.Name, &row.Config, &enc, &row.CreatedAt, &row.UpdatedAt)
	case strings.HasPrefix(selector, "name:"):
		name := strings.TrimPrefix(selector, "name:")
		err = db.QueryRowContext(ctx, `
			SELECT id, project_id, provider, name, config, encrypted_token, created_at, updated_at
			FROM integrations WHERE workspace_id = $1 AND provider = 'github' AND name = $2`,
			workspaceID, name,
		).Scan(&row.ID, &row.ProjectID, &row.Provider, &row.Name, &row.Config, &enc, &row.CreatedAt, &row.UpdatedAt)
	default:
		err = db.QueryRowContext(ctx, `
			SELECT id, project_id, provider, name, config, encrypted_token, created_at, updated_at
			FROM integrations WHERE workspace_id = $1 AND provider = 'github'
			ORDER BY updated_at DESC LIMIT 1`,
			workspaceID,
		).Scan(&row.ID, &row.ProjectID, &row.Provider, &row.Name, &row.Config, &enc, &row.CreatedAt, &row.UpdatedAt)
	}
	if err == sql.ErrNoRows {
		return nil, nil, "", errors.New("no github integration matches")
	}
	if err != nil {
		return nil, nil, "", err
	}

	var cfg extendedGithubConfig
	if len(row.Config) > 0 {
		if err := json.Unmarshal(row.Config, &cfg); err != nil {
			return nil, nil, "", fmt.Errorf("integration config is malformed: %w", err)
		}
	}
	if cfg.Owner == "" || cfg.Repo == "" {
		return nil, nil, "", errors.New("github integration is missing owner/repo")
	}
	if cfg.DefaultRef == "" {
		cfg.DefaultRef = "main"
	}

	secret := ""
	if enc != "" {
		s, derr := decryptSecret(enc)
		if derr != nil {
			return nil, nil, "", fmt.Errorf("decrypt token: %w", derr)
		}
		secret = s
	}
	return &row, &cfg, secret, nil
}
