package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GitHub agent tool surface. Designed for use during failure triage:
//   github_search_code   — find symbols / strings across the connected repo
//   github_read_file     — read a file at a path (optionally on a specific ref)
//   github_list_directory — list a directory at a path
//
// All tools read the project's saved github integration; if no integration is configured
// the tools return an error directing the user to set one up.

type githubConfig struct {
	Owner      string `json:"owner"`
	Repo       string `json:"repo"`
	DefaultRef string `json:"default_ref"`
}

// resolveGithub fetches the active github integration for the run's project
// and returns the owner/repo config plus a usable Bearer token.
//
// The token may be a PAT (returned verbatim) or a freshly-minted App
// installation token (cached for ~1h). Either way, the caller treats it as an
// opaque Bearer credential — see github_auth.go for the mechanics.
func resolveGithub(ctx context.Context, d *toolDeps) (*githubConfig, string, error) {
	selector := ""
	if d.Repo != "" {
		selector = "name:" + d.Repo
	}
	row, ext, secret, err := loadGithubIntegration(ctx, d.DB, d.WorkspaceID, selector)
	if err != nil {
		return nil, "", fmt.Errorf("no github integration configured for this workspace — ask the user to add one in Integrations")
	}
	if secret == "" {
		return nil, "", fmt.Errorf("github integration exists but no credential is stored — re-add it with a PAT or App private key")
	}
	token, _, err := resolveGithubToken(ctx, row.ID, ext, secret)
	if err != nil {
		return nil, "", err
	}
	return &githubConfig{
		Owner:      ext.Owner,
		Repo:       ext.Repo,
		DefaultRef: ext.DefaultRef,
	}, token, nil
}

var githubHTTP = &http.Client{Timeout: 15 * time.Second}

func githubGET(ctx context.Context, token, target string, headers map[string]string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "replay-triage-agent")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := githubHTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // cap at 4MB
	return body, resp.StatusCode, nil
}

// githubSend issues a POST/PATCH with a JSON body and returns the raw response.
// Shares the auth + version headers with githubGET so write tools (PR comments)
// authenticate identically to the read tools.
func githubSend(ctx context.Context, method, token, target string, payload any) ([]byte, int, error) {
	var bodyReader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bodyReader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "replay-triage-agent")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := githubHTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return body, resp.StatusCode, nil
}

// ── Tool handlers ────────────────────────────────────────────────────

func handleGithubSearchCode(ctx context.Context, d *toolDeps, input json.RawMessage) (any, error) {
	var args struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("bad input: %w", err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return nil, fmt.Errorf("query is required")
	}
	if args.Limit <= 0 || args.Limit > 20 {
		args.Limit = 10
	}
	cfg, token, err := resolveGithub(ctx, d)
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf("%s repo:%s/%s", args.Query, cfg.Owner, cfg.Repo)
	u := "https://api.github.com/search/code?q=" + url.QueryEscape(q) + fmt.Sprintf("&per_page=%d", args.Limit)
	body, status, err := githubGET(ctx, token, u, nil)
	if err != nil {
		return nil, fmt.Errorf("github request: %w", err)
	}
	if status >= 300 {
		return nil, fmt.Errorf("github %d: %s", status, string(body))
	}
	var resp struct {
		TotalCount int `json:"total_count"`
		Items      []struct {
			Name  string  `json:"name"`
			Path  string  `json:"path"`
			SHA   string  `json:"sha"`
			URL   string  `json:"html_url"`
			Score float64 `json:"score"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse github response: %w", err)
	}
	out := []map[string]any{}
	for _, it := range resp.Items {
		out = append(out, map[string]any{
			"path":  it.Path,
			"name":  it.Name,
			"sha":   it.SHA,
			"url":   it.URL,
			"score": it.Score,
		})
	}
	return map[string]any{
		"query":       q,
		"total_count": resp.TotalCount,
		"results":     out,
		"note":        "Use github_read_file to inspect any path returned here.",
	}, nil
}

func handleGithubReadFile(ctx context.Context, d *toolDeps, input json.RawMessage) (any, error) {
	var args struct {
		Path string `json:"path"`
		Ref  string `json:"ref"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("bad input: %w", err)
	}
	args.Path = strings.TrimPrefix(strings.TrimSpace(args.Path), "/")
	if args.Path == "" {
		return nil, fmt.Errorf("path is required")
	}
	cfg, token, err := resolveGithub(ctx, d)
	if err != nil {
		return nil, err
	}
	ref := args.Ref
	if ref == "" {
		ref = cfg.DefaultRef
	}
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s?ref=%s",
		cfg.Owner, cfg.Repo, url.PathEscape(args.Path), url.QueryEscape(ref))
	body, status, err := githubGET(ctx, token, u, nil)
	if err != nil {
		return nil, fmt.Errorf("github request: %w", err)
	}
	if status == 404 {
		return nil, fmt.Errorf("path %q not found on ref %q", args.Path, ref)
	}
	if status >= 300 {
		return nil, fmt.Errorf("github %d: %s", status, string(body))
	}
	var f struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Path     string `json:"path"`
		Encoding string `json:"encoding"`
		Size     int    `json:"size"`
		Content  string `json:"content"`
		SHA      string `json:"sha"`
		URL      string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("parse github response: %w", err)
	}
	if f.Type != "file" {
		return nil, fmt.Errorf("path %q is a %s, not a file — use github_list_directory", args.Path, f.Type)
	}
	if f.Size > 200_000 {
		return nil, fmt.Errorf("file is too large (%d bytes); cap is 200KB", f.Size)
	}
	var content string
	if f.Encoding == "base64" {
		// GitHub line-wraps base64; decoder tolerates that.
		decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
		if err != nil {
			return nil, fmt.Errorf("decode content: %w", err)
		}
		content = string(decoded)
	} else {
		content = f.Content
	}
	return map[string]any{
		"path":    f.Path,
		"ref":     ref,
		"sha":     f.SHA,
		"url":     f.URL,
		"size":    f.Size,
		"content": content,
	}, nil
}

func handleGithubListDirectory(ctx context.Context, d *toolDeps, input json.RawMessage) (any, error) {
	var args struct {
		Path string `json:"path"`
		Ref  string `json:"ref"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("bad input: %w", err)
	}
	args.Path = strings.TrimPrefix(strings.TrimSpace(args.Path), "/")
	cfg, token, err := resolveGithub(ctx, d)
	if err != nil {
		return nil, err
	}
	ref := args.Ref
	if ref == "" {
		ref = cfg.DefaultRef
	}
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s?ref=%s",
		cfg.Owner, cfg.Repo, url.PathEscape(args.Path), url.QueryEscape(ref))
	body, status, err := githubGET(ctx, token, u, nil)
	if err != nil {
		return nil, fmt.Errorf("github request: %w", err)
	}
	if status == 404 {
		return nil, fmt.Errorf("path %q not found on ref %q", args.Path, ref)
	}
	if status >= 300 {
		return nil, fmt.Errorf("github %d: %s", status, string(body))
	}
	// The endpoint returns an array for directories, an object for files.
	if len(body) > 0 && body[0] == '{' {
		return nil, fmt.Errorf("path %q is a file, not a directory — use github_read_file", args.Path)
	}
	var entries []struct {
		Type string `json:"type"`
		Name string `json:"name"`
		Path string `json:"path"`
		Size int    `json:"size"`
		SHA  string `json:"sha"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("parse github response: %w", err)
	}
	out := []map[string]any{}
	for _, e := range entries {
		out = append(out, map[string]any{
			"type": e.Type, "name": e.Name, "path": e.Path, "size": e.Size, "sha": e.SHA,
		})
	}
	return map[string]any{
		"path":    args.Path,
		"ref":     ref,
		"entries": out,
	}, nil
}
