package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/minio/minio-go/v7"
)

// Artifact-reading tools — surface logs, trace summaries, screenshots, and
// video frames produced by Playwright runs. Workspace scoping is enforced in
// every query so an artifact id from another tenant cannot be fetched.

func handleReadArtifact(ctx context.Context, d *toolDeps, input json.RawMessage) (any, error) {
	var args struct {
		ArtifactID string `json:"artifact_id"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("bad input: %w", err)
	}
	if args.ArtifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	var kind, key string
	var size int64
	err := d.DB.QueryRowContext(ctx,
		`SELECT a.kind, a.storage_key, a.size_bytes
		 FROM artifacts a
		 JOIN run_results rr ON a.run_result_id = rr.id
		 JOIN runs r ON rr.run_id = r.id
		 WHERE a.id = $1 AND r.workspace_id = $2`,
		args.ArtifactID, d.WorkspaceID,
	).Scan(&kind, &key, &size)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("artifact %s not found", args.ArtifactID)
	}
	if err != nil {
		return nil, err
	}
	switch kind {
	case "screenshot", "video", "video_frame", "trace":
		return nil, fmt.Errorf("artifact kind %q is binary — cannot return as text. Use list_run_artifacts to see what's available", kind)
	}
	if size > 256*1024 {
		return nil, fmt.Errorf("artifact too large (%d bytes); read_artifact caps at 256KB", size)
	}
	obj, err := d.S3.GetObject(ctx, d.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("fetch artifact: %w", err)
	}
	defer obj.Close()
	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, fmt.Errorf("read artifact: %w", err)
	}
	return map[string]any{
		"kind":    kind,
		"content": string(data),
	}, nil
}

func handleReadImage(ctx context.Context, d *toolDeps, input json.RawMessage) (any, error) {
	var args struct {
		ArtifactID string `json:"artifact_id"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("bad input: %w", err)
	}
	if args.ArtifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	var kind, key string
	err := d.DB.QueryRowContext(ctx,
		`SELECT a.kind, a.storage_key
		 FROM artifacts a
		 JOIN run_results rr ON a.run_result_id = rr.id
		 JOIN runs r ON rr.run_id = r.id
		 WHERE a.id = $1 AND r.workspace_id = $2`,
		args.ArtifactID, d.WorkspaceID,
	).Scan(&kind, &key)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("artifact %s not found", args.ArtifactID)
	}
	if err != nil {
		return nil, err
	}
	switch kind {
	case "screenshot", "video_frame":
	default:
		return nil, fmt.Errorf("artifact kind %q is not an image. Use read_artifact for text artifacts", kind)
	}

	obj, err := d.S3.GetObject(ctx, d.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get object from storage: %w", err)
	}
	defer obj.Close()

	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to read image: %w", err)
	}

	mediaType := "image/jpeg"
	lk := strings.ToLower(key)
	switch {
	case strings.HasSuffix(lk, ".png"):
		mediaType = "image/png"
	case strings.HasSuffix(lk, ".webp"):
		mediaType = "image/webp"
	case strings.HasSuffix(lk, ".gif"):
		mediaType = "image/gif"
	}

	return map[string]any{
		"kind":       kind,
		"base64":     bytesToBase64(data),
		"media_type": mediaType,
	}, nil
}

func handleGetTraceSummary(ctx context.Context, d *toolDeps, _ json.RawMessage) (any, error) {
	var key string
	err := d.DB.QueryRowContext(ctx, `
		SELECT a.storage_key FROM artifacts a
		JOIN run_results rr ON a.run_result_id = rr.id
		JOIN runs r ON rr.run_id = r.id
		WHERE rr.run_id = $1 AND r.workspace_id = $2 AND a.kind = 'trace_summary'
		ORDER BY a.id LIMIT 1`, d.RunID, d.WorkspaceID).Scan(&key)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no trace_summary artifact for this run")
	}
	if err != nil {
		return nil, err
	}
	obj, err := d.S3.GetObject(ctx, d.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, err
	}
	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return string(data), nil
	}
	return parsed, nil
}

func handleListRunArtifacts(ctx context.Context, d *toolDeps, input json.RawMessage) (any, error) {
	var args struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(input, &args)
	runID := args.RunID
	if runID == "" {
		runID = d.RunID
	}
	rows, err := d.DB.QueryContext(ctx, `
		SELECT a.id, a.kind, a.storage_key, a.size_bytes
		FROM artifacts a
		JOIN run_results rr ON a.run_result_id = rr.id
		JOIN runs r ON rr.run_id = r.id
		WHERE rr.run_id = $1 AND r.workspace_id = $2
		ORDER BY a.id`, runID, d.WorkspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, kind, key string
		var size int64
		rows.Scan(&id, &kind, &key, &size)
		base := key
		if i := strings.LastIndex(key, "/"); i >= 0 {
			base = key[i+1:]
		}
		out = append(out, map[string]any{
			"id": id, "kind": kind, "filename": base, "size_bytes": size,
		})
	}
	return map[string]any{"run_id": runID, "artifacts": out}, nil
}

func bytesToBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}
