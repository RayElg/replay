package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

// autoTriageBasePrompt is the triggering user message for a background triage.
// buildAutoTriagePrompt appends the PR-comment instruction when enabled.
const autoTriageBasePrompt = "A new test failure was just detected. Please triage it: identify the most likely root cause, read the source code if you have a GitHub integration configured, and suggest a concrete fix. When you've reached a conclusion, call submit_triage_verdict with your classification, confidence, and a one-paragraph summary."

const autoTriagePRCommentInstruction = " Then, if your verdict is real_failure or test_bug at medium or high confidence and the run is tied to an open GitHub pull request, call post_pr_comment to share your findings on the PR: lead with the verdict, then the cause and the proposed fix. Skip the PR comment for flakes, environment issues, or low-confidence verdicts."

// autoPRCommentsEnabled gates whether background triage is allowed to post PR
// comments on its own. Default on; operators set REPLAY_AUTO_PR_COMMENTS=false
// to disable. The post_pr_comment tool stays available for interactive chat
// regardless — this only governs the autonomous path.
func autoPRCommentsEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("REPLAY_AUTO_PR_COMMENTS")))
	return v != "false" && v != "0" && v != "off"
}

func buildAutoTriagePrompt() string {
	if autoPRCommentsEnabled() {
		return autoTriageBasePrompt + autoTriagePRCommentInstruction
	}
	return autoTriageBasePrompt
}

// autoTriageDedupWindow is how recently another run for the same script must have
// been auto-triaged for us to skip a fresh agent session. The dedup payoff is
// largest for flaky tests that fail repeatedly in a short window — once we've
// already explained the failure, repeating costs tokens without adding signal.
const autoTriageDedupWindow = time.Hour

// autoTriageSem caps total concurrent triage goroutines (the global ceiling
// that protects the host). Per-workspace fairness is added on top so a single
// noisy tenant can't starve everyone else by monopolising the global pool.
var autoTriageSem = make(chan struct{}, 10)

// autoTriagePerWorkspaceCap is the max concurrent triages a single workspace
// can hold. Picked so a 10-slot global pool allocates at most 2/tenant before
// queueing — small enough that bursty tenants share, large enough that single-
// tenant deployments don't feel artificially throttled.
const autoTriagePerWorkspaceCap = 2

var (
	wsTriageMu    sync.Mutex
	wsTriageCount = map[string]int{}
)

// claimWorkspaceTriageSlot returns true if the caller may proceed with a
// triage for this workspace. On true, the caller must call releaseWorkspaceTriageSlot
// when done. False means the workspace is at its per-tenant cap — we leave the
// run un-claimed (auto_triaged stays FALSE) so the next tick picks it up.
func claimWorkspaceTriageSlot(workspaceID string) bool {
	wsTriageMu.Lock()
	defer wsTriageMu.Unlock()
	if wsTriageCount[workspaceID] >= autoTriagePerWorkspaceCap {
		return false
	}
	wsTriageCount[workspaceID]++
	return true
}

func releaseWorkspaceTriageSlot(workspaceID string) {
	wsTriageMu.Lock()
	defer wsTriageMu.Unlock()
	wsTriageCount[workspaceID]--
	if wsTriageCount[workspaceID] <= 0 {
		delete(wsTriageCount, workspaceID)
	}
}

// startAutoTriageLoop polls for newly failed runs and runs the agent against
// each one automatically. Runs until ctx is cancelled.
func startAutoTriageLoop(ctx context.Context, db *sql.DB, s3 *minio.Client, bucket string, anthClient *anthropic.Client, model string) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if anthClient == nil {
				continue
			}
			if err := processAutoTriageQueue(ctx, db, s3, bucket, anthClient, model); err != nil {
				slog.Warn("auto-triage: queue processing error", "error", err)
			}
		}
	}
}

func processAutoTriageQueue(ctx context.Context, db *sql.DB, s3 *minio.Client, bucket string, anthClient *anthropic.Client, model string) error {
	rows, err := db.QueryContext(ctx, `
		SELECT id, workspace_id FROM runs
		WHERE status = 'failed'
		  AND auto_triaged = FALSE
		  AND finished_at > NOW() - INTERVAL '30 minutes'
		ORDER BY finished_at ASC
		LIMIT 10`)
	if err != nil {
		return err
	}
	type pending struct{ id, workspaceID string }
	var queue []pending
	for rows.Next() {
		var p pending
		rows.Scan(&p.id, &p.workspaceID)
		queue = append(queue, p)
	}
	rows.Close()

	for _, p := range queue {
		// Per-workspace cap: if this tenant is already at its limit, skip the
		// run. auto_triaged stays FALSE so the next 15s tick will pick it up.
		if !claimWorkspaceTriageSlot(p.workspaceID) {
			continue
		}
		// Now claim the row so no other ticker / instance races us.
		if _, err := db.ExecContext(ctx,
			`UPDATE runs SET auto_triaged = TRUE WHERE id = $1 AND auto_triaged = FALSE`, p.id,
		); err != nil {
			releaseWorkspaceTriageSlot(p.workspaceID)
			slog.Warn("auto-triage: failed to claim run", "run_id", p.id, "error", err)
			continue
		}
		slog.Info("auto-triage: starting", "run_id", p.id, "workspace_id", p.workspaceID)
		go func(id, workspaceID string) {
			defer releaseWorkspaceTriageSlot(workspaceID)
			select {
			case autoTriageSem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-autoTriageSem }()
			tCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := autoTriageRun(tCtx, db, s3, bucket, anthClient, model, id); err != nil {
				slog.Warn("auto-triage: failed", "run_id", id, "error", err)
			}
		}(p.id, p.workspaceID)
	}
	return nil
}

func autoTriageRun(ctx context.Context, db *sql.DB, s3 *minio.Client, bucket string, anthClient *anthropic.Client, model, runID string) error {
	rootRunID, err := resolveRootRunID(ctx, db, runID)
	if err != nil {
		return err
	}

	// Dedup: if the same script has been auto-triaged in the dedup window, post a
	// pointer to the prior conversation instead of running the agent again.
	if priorRoots, scriptID, err := recentTriagedRuns(ctx, db, runID); err != nil {
		slog.Warn("auto-triage: dedup lookup failed; running anyway", "run_id", runID, "error", err)
	} else if len(priorRoots) > 0 {
		return postDedupNote(ctx, db, rootRunID, scriptID, priorRoots)
	}

	// Save the triggering user message so it appears in the chat history.
	prompt := buildAutoTriagePrompt()
	autoMsgID := uuid.New().String()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO agent_messages (id, run_id, who, kind, source, content) VALUES ($1, $2, 'user', 'chat', 'auto_triage', $3)`,
		autoMsgID, rootRunID, prompt,
	); err != nil {
		return err
	}

	cc, err := loadConversationContext(ctx, db, s3, bucket, rootRunID)
	if err != nil {
		return err
	}
	history, err := loadChatHistory(ctx, db, rootRunID, "")
	if err != nil {
		return err
	}
	// Strip the message we just inserted so buildAPIMessages adds it fresh.
	// Find by ID rather than position (history now contains interleaved tool rows).
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].ID == autoMsgID {
			history = append(history[:i], history[i+1:]...)
			break
		}
	}

	defs, toolByName, tools := getTools()

	var workspaceID string
	if err := db.QueryRowContext(ctx, `SELECT workspace_id FROM runs WHERE id = $1`, rootRunID).Scan(&workspaceID); err != nil {
		workspaceID = defaultWorkspaceID
	}
	latest := cc.LatestRun()
	if latest == nil {
		return fmt.Errorf("auto-triage: conversation %s has no runs", rootRunID)
	}
	deps := &toolDeps{
		DB:          db,
		S3:          s3,
		Bucket:      bucket,
		RunID:       latest.RunID,
		Repo:        latest.Repo,
		WorkspaceID: workspaceID,
	}
	apiMsgs := buildAPIMessages(cc, history, prompt)

	// Use a busEmitter so any UI client watching this run receives live events.
	// If nobody is subscribed, publish is a no-op.
	if _, err := runAgentLoop(ctx, db, &busEmitter{runID: rootRunID}, anthClient, model,
		apiMsgs, defs, tools, toolByName, deps, rootRunID, "auto_triage"); err != nil {
		return err
	}

	slog.Info("auto-triage: complete", "run_id", runID)
	return nil
}

// recentTriagedRuns returns up to 3 prior root_run_ids for the same (script,
// branch) that were auto-triaged inside the dedup window, paired with the
// script_id used to match. Excludes the current run and runs without a script.
//
// Why (script, branch) and not just script: a flake on `staging` shouldn't
// suppress a fresh failure on `prod` — same script, very different signal.
// Branch is the cheapest proxy for "different environment" without requiring
// the agent to disambiguate runs that share commit_sha but ran in different
// places.
func recentTriagedRuns(ctx context.Context, db *sql.DB, runID string) ([]string, string, error) {
	var scriptID sql.NullString
	var branch string
	if err := db.QueryRowContext(ctx,
		`SELECT script_id, branch FROM runs WHERE id = $1`, runID,
	).Scan(&scriptID, &branch); err != nil {
		return nil, "", err
	}
	if !scriptID.Valid {
		return nil, "", nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT root_run_id FROM runs
		WHERE script_id = $1
		  AND branch = $2
		  AND id <> $3
		  AND auto_triaged = TRUE
		  AND finished_at > NOW() - make_interval(secs => $4)
		ORDER BY root_run_id
		LIMIT 3`,
		scriptID.String, branch, runID, autoTriageDedupWindow.Seconds())
	if err != nil {
		return nil, scriptID.String, err
	}
	defer rows.Close()
	var roots []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err == nil {
			roots = append(roots, r)
		}
	}
	return roots, scriptID.String, nil
}

// postDedupNote inserts a synthetic agent message linking to prior auto-triages
// for the same script. The note is the only thing written for this run's
// conversation when dedup hits.
func postDedupNote(ctx context.Context, db *sql.DB, rootRunID, scriptID string, priorRoots []string) error {
	var sb strings.Builder
	sb.WriteString("Auto-triage skipped — this script has already been triaged in the last hour. ")
	if len(priorRoots) == 1 {
		sb.WriteString(fmt.Sprintf("See run `%s` for the most recent analysis.", short(priorRoots[0])))
	} else {
		sb.WriteString("Similar runs:")
		for _, r := range priorRoots {
			sb.WriteString(fmt.Sprintf(" `%s`", short(r)))
		}
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO agent_messages (id, run_id, who, kind, source, content) VALUES ($1, $2, 'agent', 'chat', 'auto_triage', $3)`,
		uuid.New().String(), rootRunID, sb.String(),
	); err != nil {
		return err
	}
	slog.Info("auto-triage: dedup hit — linked prior triages", "run_id", rootRunID, "script_id", scriptID, "prior_roots", priorRoots)
	return nil
}
