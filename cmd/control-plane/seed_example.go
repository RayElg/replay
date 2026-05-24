package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"
)

// Deterministic IDs make `seed-example` idempotent — re-running upserts the same
// example environment and script instead of piling up duplicates.
const (
	exampleEnvID     = "00000000-0000-0000-0000-0000000000e0"
	exampleScriptID  = "00000000-0000-0000-0000-0000000000e1"
	exampleProjectID = "00000000-0000-0000-0000-000000000001" // seeded Default Project
)

// exampleSpec is a self-contained Playwright test that runs against
// https://example.com — no sibling repo, no local demo app. One test passes and
// one fails on purpose so a fresh install can watch a real failure get captured
// (video/trace/screenshot) and triaged by the agent.
const exampleSpec = `import { test, expect } from '@playwright/test';

// Seeded by 'make example' (control-plane seed-example). Runs against the
// "Example (example.com)" environment, which sets BASE_URL=https://example.com,
// so page.goto('/') resolves to https://example.com/.

test('homepage loads', async ({ page }) => {
  await page.goto('/');
  await expect(page).toHaveTitle(/Example Domain/);
});

test('intentional failure: missing sign-in link', async ({ page }) => {
  await page.goto('/');
  // example.com has no "Sign in" link, so this assertion fails on purpose. It's
  // here so you can watch Replay capture the artifacts and let the agent triage
  // a real failure. Delete this test once you've seen it work.
  await expect(page.getByRole('link', { name: 'Sign in' })).toBeVisible({ timeout: 5000 });
});
`

func runSeedExampleCLI(args []string) {
	fs := flag.NewFlagSet("seed-example", flag.ExitOnError)
	queue := fs.Bool("run", false, "also queue a run immediately (requires the runner to be up)")
	_ = fs.Parse(args)

	db := openDB()
	defer db.Close()
	runMigrations(db)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := seedExample(ctx, db, *queue); err != nil {
		fmt.Fprintln(os.Stderr, "seed-example failed:", err)
		os.Exit(1)
	}
}

// seedExample upserts the example environment + script into the Default Project,
// and (when queue is true) inserts a queued run. The runs BEFORE INSERT trigger
// sets root_run_id = id, and pgmqtt's CDC mapping publishes the row to the
// runner — so a raw INSERT is all it takes to kick off execution.
func seedExample(ctx context.Context, db *sql.DB, queue bool) error {
	// BASE_URL is stored as plaintext (not a secret) so the runner injects it
	// verbatim into the Playwright process.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO environments (id, project_id, workspace_id, name, slug, env_vars)
		VALUES ($1, $2, $3, 'Example (example.com)', 'example', '{"BASE_URL":"https://example.com"}'::jsonb)
		ON CONFLICT DO NOTHING`,
		exampleEnvID, exampleProjectID, defaultWorkspaceID,
	); err != nil {
		return fmt.Errorf("seed environment: %w", err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO scripts (id, project_id, workspace_id, name, filename, content, source_kind)
		VALUES ($1, $2, $3, 'Example: example.com', 'example.spec.ts', $4, 'inline')
		ON CONFLICT DO NOTHING`,
		exampleScriptID, exampleProjectID, defaultWorkspaceID, exampleSpec,
	); err != nil {
		return fmt.Errorf("seed script: %w", err)
	}

	fmt.Println("✔ seeded example environment + script in the Default Project")

	if !queue {
		fmt.Println("  open http://localhost:3000 → Scripts → 'Example: example.com' → Run")
		fmt.Println("  (or re-run with --run to queue it now)")
		return nil
	}

	var runID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO runs (project_id, workspace_id, script_id, env_id, status, branch)
		VALUES ($1, $2, $3, $4, 'queued', 'main')
		RETURNING id`,
		exampleProjectID, defaultWorkspaceID, exampleScriptID, exampleEnvID,
	).Scan(&runID); err != nil {
		return fmt.Errorf("queue run: %w", err)
	}
	fmt.Printf("✔ queued run %s — watch it live at http://localhost:3000\n", runID)
	return nil
}
