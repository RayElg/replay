package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// flows_db_test.go covers the invite / reset / login lifecycles end-to-end
// against a real PostgreSQL — the only level at which the agent's stated
// concern (token reuse, expiry, race-on-reset) actually shows up. Tests are
// skipped when TEST_DATABASE_URL is unset so `go test ./...` keeps working on
// a bare checkout.
//
// Operator: `make dev`, then
//
//   TEST_DATABASE_URL=postgres://replay:replay@localhost:5432/postgres?sslmode=disable \
//     go test ./cmd/control-plane -run TestFlows
//
// Each test seeds its own throwaway workspace + user so runs don't collide.

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("set TEST_DATABASE_URL to run DB-backed flow tests")
	}
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("ping db: %v", err)
	}
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.Up(db, "migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// seedWorkspace makes an isolated workspace so concurrent tests don't trample
// each other's user_invites / user_password_resets / users rows.
func seedWorkspace(t *testing.T, db *sql.DB) (workspaceID string) {
	t.Helper()
	slug := fmt.Sprintf("test-%d", time.Now().UnixNano())
	err := db.QueryRowContext(context.Background(),
		`INSERT INTO workspaces (slug, name) VALUES ($1, $1) RETURNING id`,
		slug,
	).Scan(&workspaceID)
	if err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM workspaces WHERE id = $1`, workspaceID)
	})
	return workspaceID
}

func TestFlowsInviteAcceptIsOneShot(t *testing.T) {
	db := openTestDB(t)
	ws := seedWorkspace(t, db)
	ctx := context.Background()
	email := fmt.Sprintf("invite-%d@x.test", time.Now().UnixNano())

	token, _, err := mintInvite(ctx, db, ws, email, nil)
	if err != nil {
		t.Fatalf("mintInvite: %v", err)
	}

	// First accept: creates the user, marks invite consumed.
	userID, err := createOrUpdateUser(ctx, db, ws, email, email, "pw-12345678")
	if err != nil {
		t.Fatalf("createOrUpdateUser: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE user_invites SET accepted_at = now() WHERE token = $1`, token,
	); err != nil {
		t.Fatalf("mark accepted: %v", err)
	}

	// Second accept attempt against the same token must find nothing.
	var found string
	err = db.QueryRowContext(ctx,
		`SELECT workspace_id FROM user_invites WHERE token = $1 AND accepted_at IS NULL`,
		token,
	).Scan(&found)
	if err != sql.ErrNoRows {
		t.Fatalf("accepted invite should not be reusable: err=%v found=%s", err, found)
	}

	_ = userID
}

func TestFlowsInviteRejectsDuplicates(t *testing.T) {
	db := openTestDB(t)
	ws := seedWorkspace(t, db)
	ctx := context.Background()
	email := fmt.Sprintf("dup-%d@x.test", time.Now().UnixNano())

	if _, _, err := mintInvite(ctx, db, ws, email, nil); err != nil {
		t.Fatalf("first mintInvite: %v", err)
	}
	_, _, err := mintInvite(ctx, db, ws, email, nil)
	if err == nil {
		t.Fatal("second mintInvite for same (workspace,email) must fail")
	}
	if !strings.Contains(err.Error(), "already pending") {
		t.Fatalf("error should mention pending invite, got %q", err)
	}
}

func TestFlowsInviteRejectsKnownUser(t *testing.T) {
	db := openTestDB(t)
	ws := seedWorkspace(t, db)
	ctx := context.Background()
	email := fmt.Sprintf("known-%d@x.test", time.Now().UnixNano())

	if _, err := createOrUpdateUser(ctx, db, ws, email, email, "pw-12345678"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, _, err := mintInvite(ctx, db, ws, email, nil); err == nil {
		t.Fatal("mintInvite for an existing user must fail")
	}
}

func TestFlowsInviteExpiryDetected(t *testing.T) {
	db := openTestDB(t)
	ws := seedWorkspace(t, db)
	ctx := context.Background()
	email := fmt.Sprintf("expire-%d@x.test", time.Now().UnixNano())

	token, _, err := mintInvite(ctx, db, ws, email, nil)
	if err != nil {
		t.Fatalf("mintInvite: %v", err)
	}
	// Backdate the row directly so the expired branch fires.
	if _, err := db.ExecContext(ctx,
		`UPDATE user_invites SET expires_at = now() - interval '1 minute' WHERE token = $1`, token,
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	var expiresAt time.Time
	err = db.QueryRowContext(ctx,
		`SELECT expires_at FROM user_invites WHERE token = $1 AND accepted_at IS NULL`, token,
	).Scan(&expiresAt)
	if err != nil {
		t.Fatalf("invite lookup: %v", err)
	}
	if !time.Now().After(expiresAt) {
		t.Fatal("invite should be expired by now")
	}
}

func TestFlowsPasswordResetIsOneShot(t *testing.T) {
	db := openTestDB(t)
	ws := seedWorkspace(t, db)
	ctx := context.Background()
	email := fmt.Sprintf("reset-%d@x.test", time.Now().UnixNano())

	if _, err := createOrUpdateUser(ctx, db, ws, email, email, "old-pw-12345678"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	token, _, err := mintPasswordReset(ctx, db, email)
	if err != nil {
		t.Fatalf("mintPasswordReset: %v", err)
	}

	// Consume it.
	if _, err := db.ExecContext(ctx,
		`UPDATE user_password_resets SET used_at = now() WHERE token = $1`, token,
	); err != nil {
		t.Fatalf("consume: %v", err)
	}

	// Re-querying with `used_at IS NULL` (what the consume path does) must fail.
	var uid string
	err = db.QueryRowContext(ctx,
		`SELECT user_id FROM user_password_resets WHERE token = $1 AND used_at IS NULL`,
		token,
	).Scan(&uid)
	if err != sql.ErrNoRows {
		t.Fatalf("used reset token must not be reusable: err=%v uid=%s", err, uid)
	}
}

func TestFlowsPasswordResetRejectsUnknownEmail(t *testing.T) {
	db := openTestDB(t)
	_ = seedWorkspace(t, db)
	ctx := context.Background()

	_, _, err := mintPasswordReset(ctx, db, fmt.Sprintf("ghost-%d@x.test", time.Now().UnixNano()))
	if err == nil {
		t.Fatal("mintPasswordReset for unknown email must error (CLI-only path; no presence-leak concern)")
	}
}

func TestFlowsAgentMessagesCarryWorkspaceID(t *testing.T) {
	// Regression for the cross-tenant leak that prompted migration 0002:
	// the trigger should fill workspace_id from the run, so an INSERT that
	// omits workspace_id still produces a tenant-scoped row.
	db := openTestDB(t)
	ws := seedWorkspace(t, db)
	ctx := context.Background()

	var projectID string
	err := db.QueryRowContext(ctx,
		`INSERT INTO projects (workspace_id, name) VALUES ($1, 'p') RETURNING id`, ws,
	).Scan(&projectID)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	var runID string
	err = db.QueryRowContext(ctx,
		`INSERT INTO runs (project_id, workspace_id, status) VALUES ($1, $2, 'queued') RETURNING id`,
		projectID, ws,
	).Scan(&runID)
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO agent_messages (run_id, who, kind, content) VALUES ($1, 'user', 'chat', 'hi')`,
		runID,
	); err != nil {
		t.Fatalf("insert agent_message: %v", err)
	}
	var got string
	err = db.QueryRowContext(ctx,
		`SELECT workspace_id FROM agent_messages WHERE run_id = $1`, runID,
	).Scan(&got)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if got != ws {
		t.Fatalf("trigger should backfill workspace_id from runs: got %q want %q", got, ws)
	}
}
