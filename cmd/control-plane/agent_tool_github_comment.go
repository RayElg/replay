package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// post_pr_comment tool. Delivers triage findings to the team by commenting on
// the GitHub pull request tied to the current run. Comments are upserted by a
// hidden marker keyed on the conversation's root run, so reruns edit the same
// comment instead of stacking new ones on the PR.

func prMarker(rootRunID string) string {
	return fmt.Sprintf("<!-- replay-triage:run=%s -->", rootRunID)
}

func handlePostPRComment(ctx context.Context, d *toolDeps, input json.RawMessage) (any, error) {
	var args struct {
		Body     string `json:"body"`
		PRNumber int    `json:"pr_number"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("bad input: %w", err)
	}
	if strings.TrimSpace(args.Body) == "" {
		return nil, fmt.Errorf("body is required")
	}

	cfg, token, err := resolveGithub(ctx, d)
	if err != nil {
		return nil, err
	}

	// Pull the run's coordinates: branch/commit to find the PR, root for the
	// upsert marker so reruns in the same conversation update one comment.
	var branch, commitSHA, rootRunID string
	if err := d.DB.QueryRowContext(ctx,
		`SELECT branch, commit_sha, root_run_id FROM runs WHERE id = $1 AND workspace_id = $2`,
		d.RunID, d.WorkspaceID,
	).Scan(&branch, &commitSHA, &rootRunID); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("run %s not found", d.RunID)
		}
		return nil, err
	}

	prNumber := args.PRNumber
	if prNumber == 0 {
		prNumber, err = resolvePRNumber(ctx, token, cfg, commitSHA, branch)
		if err != nil {
			return nil, err
		}
	}

	marker := prMarker(rootRunID)
	fullBody := buildPRCommentBody(args.Body, rootRunID, commitSHA, marker, cfg)

	// Upsert: if a prior triage comment exists for this run, edit it.
	existingID, err := findMarkedComment(ctx, token, cfg, prNumber, marker)
	if err != nil {
		return nil, err
	}

	var commentURL string
	if existingID != 0 {
		commentURL, err = patchIssueComment(ctx, token, cfg, existingID, fullBody)
	} else {
		commentURL, err = postIssueComment(ctx, token, cfg, prNumber, fullBody)
	}
	if err != nil {
		return nil, err
	}

	action := "created"
	if existingID != 0 {
		action = "updated"
	}
	return map[string]any{
		"pr_number":   prNumber,
		"action":      action,
		"comment_url": commentURL,
		"note":        fmt.Sprintf("Triage comment %s on PR #%d.", action, prNumber),
	}, nil
}

// buildPRCommentBody appends a Replay run footer and the hidden upsert marker
// to the agent-authored body. The footer deep-links to the run (when
// REPLAY_EXTERNAL_URL is set) and links the triaged commit to GitHub so
// reviewers can jump straight to the diff under test.
func buildPRCommentBody(body, rootRunID, commitSHA, marker string, cfg *githubConfig) string {
	var sb strings.Builder
	sb.WriteString(strings.TrimRight(body, "\n"))
	sb.WriteString("\n\n")

	parts := make([]string, 0, 3)
	if base := strings.TrimRight(strings.TrimSpace(os.Getenv("REPLAY_EXTERNAL_URL")), "/"); base != "" {
		parts = append(parts, fmt.Sprintf("🔁 Triaged by [Replay](%s/?run=%s)", base, url.QueryEscape(rootRunID)))
	} else {
		parts = append(parts, "🔁 Triaged by Replay")
	}
	if commitSHA != "" {
		if cfg != nil && cfg.Owner != "" && cfg.Repo != "" {
			parts = append(parts, fmt.Sprintf("commit [`%s`](https://github.com/%s/%s/commit/%s)",
				short(commitSHA), cfg.Owner, cfg.Repo, url.PathEscape(commitSHA)))
		} else {
			parts = append(parts, fmt.Sprintf("commit `%s`", short(commitSHA)))
		}
	}
	parts = append(parts, fmt.Sprintf("run `%s`", short(rootRunID)))

	sb.WriteString("<sub>")
	sb.WriteString(strings.Join(parts, " · "))
	sb.WriteString("</sub>\n")
	sb.WriteString(marker)
	return sb.String()
}

// resolvePRNumber finds the open PR for a run. Commit-based lookup is the most
// reliable (it survives branch renames and forks); branch head is the fallback.
func resolvePRNumber(ctx context.Context, token string, cfg *githubConfig, commitSHA, branch string) (int, error) {
	if commitSHA != "" {
		u := fmt.Sprintf("https://api.github.com/repos/%s/%s/commits/%s/pulls",
			cfg.Owner, cfg.Repo, url.PathEscape(commitSHA))
		body, status, err := githubGET(ctx, token, u, nil)
		if err != nil {
			return 0, fmt.Errorf("github request: %w", err)
		}
		if status < 300 {
			if n := firstOpenPR(body); n != 0 {
				return n, nil
			}
		}
	}
	if branch != "" {
		u := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls?state=open&head=%s",
			cfg.Owner, cfg.Repo, url.QueryEscape(cfg.Owner+":"+branch))
		body, status, err := githubGET(ctx, token, u, nil)
		if err != nil {
			return 0, fmt.Errorf("github request: %w", err)
		}
		if status < 300 {
			if n := firstOpenPR(body); n != 0 {
				return n, nil
			}
		}
	}
	return 0, fmt.Errorf("no open pull request found for this run (branch %q, commit %s) — pass pr_number explicitly if you know it", branch, short(commitSHA))
}

// firstOpenPR returns the number of the first open PR in a GitHub PR-list
// payload, or 0 if none. Both the commits/{sha}/pulls and the pulls?head=
// endpoints return the same array shape.
func firstOpenPR(body []byte) int {
	var prs []struct {
		Number int    `json:"number"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal(body, &prs); err != nil {
		return 0
	}
	for _, p := range prs {
		if p.State == "open" {
			return p.Number
		}
	}
	return 0
}

// findMarkedComment returns the id of an existing issue comment carrying our
// per-run marker, or 0 if none exists.
func findMarkedComment(ctx context.Context, token string, cfg *githubConfig, prNumber int, marker string) (int64, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d/comments?per_page=100",
		cfg.Owner, cfg.Repo, prNumber)
	body, status, err := githubGET(ctx, token, u, nil)
	if err != nil {
		return 0, fmt.Errorf("github request: %w", err)
	}
	if status >= 300 {
		return 0, fmt.Errorf("github %d listing comments: %s", status, string(body))
	}
	var comments []struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(body, &comments); err != nil {
		return 0, fmt.Errorf("parse comments: %w", err)
	}
	for _, c := range comments {
		if strings.Contains(c.Body, marker) {
			return c.ID, nil
		}
	}
	return 0, nil
}

func postIssueComment(ctx context.Context, token string, cfg *githubConfig, prNumber int, body string) (string, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d/comments", cfg.Owner, cfg.Repo, prNumber)
	resp, status, err := githubSend(ctx, "POST", token, u, map[string]string{"body": body})
	if err != nil {
		return "", fmt.Errorf("github request: %w", err)
	}
	if status >= 300 {
		return "", fmt.Errorf("github %d posting comment: %s", status, string(resp))
	}
	return commentHTMLURL(resp), nil
}

func patchIssueComment(ctx context.Context, token string, cfg *githubConfig, commentID int64, body string) (string, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/comments/%d", cfg.Owner, cfg.Repo, commentID)
	resp, status, err := githubSend(ctx, "PATCH", token, u, map[string]string{"body": body})
	if err != nil {
		return "", fmt.Errorf("github request: %w", err)
	}
	if status >= 300 {
		return "", fmt.Errorf("github %d editing comment: %s", status, string(resp))
	}
	return commentHTMLURL(resp), nil
}

func commentHTMLURL(resp []byte) string {
	var c struct {
		HTMLURL string `json:"html_url"`
	}
	_ = json.Unmarshal(resp, &c)
	return c.HTMLURL
}
