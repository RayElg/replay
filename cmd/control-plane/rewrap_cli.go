package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/RayElg/replay/internal/replaycrypto"
)

// runRewrapSecretsCLI re-encrypts every at-rest secret under the current primary
// key, so an operator can finish a key rotation and then retire the old key
// (drop REPLAY_ENCRYPT_KEY_PREVIOUS). It walks the two durable encrypted stores —
// environments.env_vars and integrations.encrypted_token — decrypting each value
// through the keyring (which still holds the previous keys) and re-sealing it
// under the primary. Values already current are left untouched; plaintext rows
// (stored before a key was configured) are upgraded to ciphertext.
//
// runs.env_vars is intentionally skipped: those are short-lived per-run snapshots
// stored as plaintext, not part of the durable secret state.
func runRewrapSecretsCLI(args []string) {
	fs := flag.NewFlagSet("rewrap-secrets", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "report what would change without writing")
	_ = fs.Parse(args)

	kr, err := loadKeyring()
	if err != nil {
		fmt.Fprintln(os.Stderr, "rewrap-secrets: REPLAY_ENCRYPT_KEY is not set — nothing to rewrap under")
		os.Exit(1)
	}

	db := openDB()
	defer db.Close()
	runMigrations(db)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	r := &rewrapper{db: db, kr: kr, dryRun: *dryRun}
	if err := r.environments(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "rewrap-secrets: environments failed:", err)
		os.Exit(1)
	}
	if err := r.integrations(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "rewrap-secrets: integrations failed:", err)
		os.Exit(1)
	}

	verb := "rewrapped"
	if *dryRun {
		verb = "would rewrap"
	}
	fmt.Printf("%s %d environment value(s) across %d environment(s) and %d integration token(s) under key %s\n",
		verb, r.envValues, r.envRows, r.intRows, kr.PrimaryID())
	if r.failures > 0 {
		fmt.Fprintf(os.Stderr, "%d value(s) could not be decrypted — set REPLAY_ENCRYPT_KEY_PREVIOUS to the retired key(s) and re-run before dropping it\n", r.failures)
		os.Exit(1)
	}
	if r.envValues == 0 && r.intRows == 0 {
		fmt.Println("everything is already sealed under the primary key")
	}
}

type rewrapper struct {
	db     *sql.DB
	kr     *replaycrypto.Keyring
	dryRun bool

	envRows   int // environments touched
	envValues int // individual env-var values rewrapped
	intRows   int // integration tokens rewrapped
	failures  int // values that could not be decrypted
}

// rewrapValue decrypts a stored value through the keyring and re-seals it under
// the primary. Returns the new value and ok=true only when it actually changed.
func (r *rewrapper) rewrapValue(stored string) (string, bool) {
	if !r.kr.NeedsRewrap(stored) {
		return stored, false
	}
	plain, err := r.kr.Decrypt(stored)
	if err != nil {
		fmt.Fprintln(os.Stderr, "  decrypt failed:", err)
		r.failures++
		return stored, false
	}
	sealed, err := r.kr.Encrypt(plain)
	if err != nil {
		fmt.Fprintln(os.Stderr, "  encrypt failed:", err)
		r.failures++
		return stored, false
	}
	return sealed, true
}

func (r *rewrapper) environments(ctx context.Context) error {
	rows, err := r.db.QueryContext(ctx, `SELECT id, env_vars FROM environments`)
	if err != nil {
		return err
	}
	type envRow struct {
		id   string
		vars map[string]string
	}
	var pending []envRow
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		vars := map[string]string{}
		_ = json.Unmarshal(raw, &vars)
		changed := false
		for k, v := range vars {
			if nv, ok := r.rewrapValue(v); ok {
				vars[k] = nv
				changed = true
				r.envValues++
			}
		}
		if changed {
			pending = append(pending, envRow{id: id, vars: vars})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	r.envRows = len(pending)
	if r.dryRun {
		return nil
	}
	for _, e := range pending {
		blob, err := json.Marshal(e.vars)
		if err != nil {
			return err
		}
		if _, err := r.db.ExecContext(ctx,
			`UPDATE environments SET env_vars = $2 WHERE id = $1`, e.id, blob); err != nil {
			return err
		}
	}
	return nil
}

func (r *rewrapper) integrations(ctx context.Context) error {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, encrypted_token FROM integrations WHERE encrypted_token <> ''`)
	if err != nil {
		return err
	}
	type tokenRow struct{ id, token string }
	var pending []tokenRow
	for rows.Next() {
		var id, token string
		if err := rows.Scan(&id, &token); err != nil {
			rows.Close()
			return err
		}
		if nv, ok := r.rewrapValue(token); ok {
			pending = append(pending, tokenRow{id: id, token: nv})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	r.intRows = len(pending)
	if r.dryRun {
		return nil
	}
	for _, t := range pending {
		if _, err := r.db.ExecContext(ctx,
			`UPDATE integrations SET encrypted_token = $2 WHERE id = $1`, t.id, t.token); err != nil {
			return err
		}
	}
	return nil
}
