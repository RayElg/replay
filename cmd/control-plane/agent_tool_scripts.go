package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Script-and-run tools — read a script, enumerate failure history, queue
// reruns, and propose edits. Every query filters by d.WorkspaceID so the
// agent cannot read or mutate state outside its tenant even if a model is
// tricked into supplying a foreign id.

func handleReadScript(ctx context.Context, d *toolDeps, input json.RawMessage) (any, error) {
	var args struct {
		ScriptID string `json:"script_id"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("bad input: %w", err)
	}
	if args.ScriptID == "" {
		return nil, fmt.Errorf("script_id is required")
	}
	var name, filename, content string
	var agentsMD sql.NullString
	err := d.DB.QueryRowContext(ctx,
		`SELECT name, filename, content, agents_md FROM scripts
		 WHERE id = $1 AND workspace_id = $2`,
		args.ScriptID, d.WorkspaceID,
	).Scan(&name, &filename, &content, &agentsMD)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("script %s not found", args.ScriptID)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id":        args.ScriptID,
		"name":      name,
		"filename":  filename,
		"content":   content,
		"agents_md": agentsMD.String,
	}, nil
}

func handleListSimilarFailures(ctx context.Context, d *toolDeps, input json.RawMessage) (any, error) {
	var args struct {
		Limit int `json:"limit"`
	}
	_ = json.Unmarshal(input, &args)
	if args.Limit <= 0 || args.Limit > 25 {
		args.Limit = 10
	}
	var scriptID sql.NullString
	if err := d.DB.QueryRowContext(ctx,
		`SELECT script_id FROM runs WHERE id = $1 AND workspace_id = $2`,
		d.RunID, d.WorkspaceID,
	).Scan(&scriptID); err != nil {
		return nil, err
	}
	if !scriptID.Valid {
		return map[string]any{"runs": []any{}, "note": "current run has no script_id"}, nil
	}
	rows, err := d.DB.QueryContext(ctx, `
		SELECT id, status, branch, commit_sha, created_at, finished_at
		FROM runs
		WHERE script_id = $1 AND workspace_id = $2 AND id <> $3 AND status = 'failed'
		ORDER BY created_at DESC LIMIT $4`,
		scriptID.String, d.WorkspaceID, d.RunID, args.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, status, branch, commit string
		var createdAt time.Time
		var finishedAt *time.Time
		rows.Scan(&id, &status, &branch, &commit, &createdAt, &finishedAt)
		out = append(out, map[string]any{
			"id": id, "status": status, "branch": branch,
			"commit_sha": commit, "created_at": createdAt, "finished_at": finishedAt,
		})
	}
	return map[string]any{"runs": out, "script_id": scriptID.String}, nil
}

func handleRerunRun(ctx context.Context, d *toolDeps, input json.RawMessage) (any, error) {
	var args struct {
		RunID  string `json:"run_id"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(input, &args)
	sourceID := args.RunID
	if sourceID == "" {
		sourceID = d.RunID
	}
	var projectID, branch, commitSHA, testFilter, rootRunID, workspaceID string
	var repo sql.NullString
	var scriptID, envID sql.NullString
	err := d.DB.QueryRowContext(ctx, `
		SELECT project_id, branch, commit_sha, test_filter, script_id, env_id, root_run_id, repo, workspace_id
		FROM runs WHERE id = $1 AND workspace_id = $2`,
		sourceID, d.WorkspaceID,
	).Scan(&projectID, &branch, &commitSHA, &testFilter, &scriptID, &envID, &rootRunID, &repo, &workspaceID)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("source run %s not found", sourceID)
	}
	if err != nil {
		return nil, err
	}
	// Rerun joins the same conversation as the source run — chat, history, and
	// follow-up triage all flow through the root.
	newID := uuid.New().String()
	_, err = d.DB.ExecContext(ctx, `
		INSERT INTO runs (id, project_id, workspace_id, branch, commit_sha, test_filter, script_id, env_id, status, root_run_id, repo)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'queued', $9, $10)`,
		newID, projectID, workspaceID, branch, commitSHA, testFilter, scriptID, envID, rootRunID, repo)
	if err != nil {
		return nil, fmt.Errorf("insert run: %w", err)
	}
	return map[string]any{
		"new_run_id":  newID,
		"source_run":  sourceID,
		"root_run_id": rootRunID,
		"status":      "queued",
		"reason":      args.Reason,
		"note":        "Rerun queued. It will execute asynchronously and appear in the conversation. Check back after a moment (or refresh) to see the new run.",
	}, nil
}

func handleProposeScriptEdit(ctx context.Context, d *toolDeps, input json.RawMessage) (any, error) {
	var args struct {
		ScriptID  string `json:"script_id"`
		Summary   string `json:"summary"`
		Rationale string `json:"rationale"`
		Find      string `json:"find"`
		Replace   string `json:"replace"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("bad input: %w", err)
	}
	if args.ScriptID == "" || args.Summary == "" || args.Find == "" {
		return nil, fmt.Errorf("script_id, summary, and find are required")
	}
	var oldContent string
	err := d.DB.QueryRowContext(ctx,
		`SELECT content FROM scripts WHERE id = $1 AND workspace_id = $2`,
		args.ScriptID, d.WorkspaceID,
	).Scan(&oldContent)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("script %s not found", args.ScriptID)
	}
	if err != nil {
		return nil, err
	}
	matches := strings.Count(oldContent, args.Find)
	if matches == 0 {
		return nil, fmt.Errorf("find text not found in script — verify it matches exactly (whitespace, indentation, line endings)")
	}
	if matches > 1 {
		return nil, fmt.Errorf("find text matches %d places in the script — extend the surrounding context so it is unique", matches)
	}
	newContent := strings.Replace(oldContent, args.Find, args.Replace, 1)
	if newContent == oldContent {
		return nil, fmt.Errorf("find and replace produced identical content")
	}
	patchID := uuid.New().String()
	_, err = d.DB.ExecContext(ctx, `
		INSERT INTO script_patches (id, script_id, proposed_by_run_id, proposed_by, summary, rationale, old_content, new_content, status)
		VALUES ($1, $2, $3, 'agent', $4, $5, $6, $7, 'pending')`,
		patchID, args.ScriptID, d.RunID, args.Summary, args.Rationale, oldContent, newContent)
	if err != nil {
		return nil, fmt.Errorf("insert patch: %w", err)
	}
	return map[string]any{
		"patch_id":  patchID,
		"script_id": args.ScriptID,
		"status":    "pending",
		"summary":   args.Summary,
		"note":      "Patch is queued for human review. Tell the user it's pending and what it changes.",
	}, nil
}

func handleProposeScriptPatch(ctx context.Context, d *toolDeps, input json.RawMessage) (any, error) {
	var args struct {
		ScriptID   string `json:"script_id"`
		Summary    string `json:"summary"`
		Rationale  string `json:"rationale"`
		NewContent string `json:"new_content"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("bad input: %w", err)
	}
	if args.ScriptID == "" || args.Summary == "" || args.NewContent == "" {
		return nil, fmt.Errorf("script_id, summary, and new_content are required")
	}
	var oldContent string
	err := d.DB.QueryRowContext(ctx,
		`SELECT content FROM scripts WHERE id = $1 AND workspace_id = $2`,
		args.ScriptID, d.WorkspaceID,
	).Scan(&oldContent)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("script %s not found", args.ScriptID)
	}
	if err != nil {
		return nil, err
	}
	if oldContent == args.NewContent {
		return nil, fmt.Errorf("new_content is identical to current script — nothing to patch")
	}
	patchID := uuid.New().String()
	_, err = d.DB.ExecContext(ctx, `
		INSERT INTO script_patches (id, script_id, proposed_by_run_id, proposed_by, summary, rationale, old_content, new_content, status)
		VALUES ($1, $2, $3, 'agent', $4, $5, $6, $7, 'pending')`,
		patchID, args.ScriptID, d.RunID, args.Summary, args.Rationale, oldContent, args.NewContent)
	if err != nil {
		return nil, fmt.Errorf("insert patch: %w", err)
	}
	return map[string]any{
		"patch_id":  patchID,
		"script_id": args.ScriptID,
		"status":    "pending",
		"summary":   args.Summary,
		"note":      "Patch is queued for human review. Tell the user it's pending and what it changes.",
	}, nil
}
