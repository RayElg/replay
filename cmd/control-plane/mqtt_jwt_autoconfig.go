package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// configurePgmqttJWT wires the broker's JWT authentication on boot when the
// operator has flipped to trusted-broker mode (REPLAY_MQTT_TRUSTED_BROKER=true).
// In that mode the browser presents an Ed25519-signed JWT (see broker.go) as
// its MQTT CONNECT password and pgmqtt enforces topic ACLs natively from the
// sub_claims array — no extra hop.
//
// Two prerequisites the operator owns:
//
//  1. A pgmqtt Enterprise license with the `jwt` feature active. We probe
//     pgmqtt_license_status() and refuse to boot if it isn't — silently falling
//     back to the unauthed Community broker would be a much worse outcome
//     than a noisy startup error.
//
//  2. An Ed25519 private key in REPLAY_BROKER_JWT_PRIVATE_KEY. The matching
//     public key gets ALTER SYSTEM SET into pgmqtt.jwt_public_key here, so the
//     key never has to be copy-pasted twice.
//
// Behavior of this routine is best-effort *after* the prerequisites pass:
// ALTER SYSTEM requires superuser; if the control-plane DB role isn't one
// we log and continue rather than crash, matching configurePgmqttAuth.
// REPLAY_PGMQTT_AUTOCONFIG=false short-circuits both for operators who manage
// pgmqtt GUCs out of band.
func configurePgmqttJWT(ctx context.Context, db *sql.DB) error {
	if !mqttTrustedBroker() {
		return nil
	}
	if os.Getenv("REPLAY_PGMQTT_AUTOCONFIG") == "false" {
		slog.Info("pgmqtt jwt: autoconfig disabled via REPLAY_PGMQTT_AUTOCONFIG=false")
		return nil
	}

	if err := applyPgmqttLicenseKey(ctx, db); err != nil {
		return err
	}
	if err := ensurePgmqttJWTLicensed(ctx, db); err != nil {
		return err
	}

	pub, err := brokerJWTPublicKey()
	if err != nil {
		return fmt.Errorf("derive broker JWT public key: %w", err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)

	// pgmqtt.jwt_public_key accepts base64url-encoded 32 raw bytes.
	// jwt_required_ws=on enforces JWT for WebSocket connections (browsers)
	// while leaving the TCP listener anonymous-capable for runners (which
	// stay on SCRAM via the replay_runner role).
	gucs := []string{
		fmt.Sprintf("ALTER SYSTEM SET pgmqtt.jwt_public_key = '%s'", pubB64),
		"ALTER SYSTEM SET pgmqtt.jwt_required_ws = 'on'",
	}
	if cert := strings.TrimSpace(os.Getenv("REPLAY_PGMQTT_TLS_CERT_FILE")); cert != "" {
		gucs = append(gucs,
			fmt.Sprintf("ALTER SYSTEM SET pgmqtt.tls_cert_file = '%s'", sqlEscape(cert)),
			fmt.Sprintf("ALTER SYSTEM SET pgmqtt.tls_key_file = '%s'",
				sqlEscape(strings.TrimSpace(os.Getenv("REPLAY_PGMQTT_TLS_KEY_FILE")))),
			"ALTER SYSTEM SET pgmqtt.wss_enabled = 'on'",
		)
	}
	for _, q := range gucs {
		if _, err := db.ExecContext(ctx, q); err != nil {
			slog.Warn("pgmqtt jwt: ALTER SYSTEM failed — operator must apply GUC manually",
				"query", q, "error", err)
			return nil
		}
	}
	if _, err := db.ExecContext(ctx, `SELECT pg_reload_conf()`); err != nil {
		slog.Warn("pgmqtt jwt: pg_reload_conf failed", "error", err)
		return nil
	}

	if hasTLS := os.Getenv("REPLAY_PGMQTT_TLS_CERT_FILE") != ""; hasTLS {
		slog.Warn("pgmqtt jwt: TLS listener GUCs require a pgmqtt restart to bind the WSS port",
			"hint", "docker compose restart pgmqtt")
	}
	slog.Info("pgmqtt jwt: trusted-broker mode enabled", "public_key", pubB64)
	return nil
}

// applyPgmqttLicenseKey lets the operator hand us their license token via
// REPLAY_PGMQTT_LICENSE_KEY and we plumb it into the broker via ALTER SYSTEM.
// Skipped if unset — the operator may have configured the GUC manually.
func applyPgmqttLicenseKey(ctx context.Context, db *sql.DB) error {
	key := strings.TrimSpace(os.Getenv("REPLAY_PGMQTT_LICENSE_KEY"))
	if key == "" {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf("ALTER SYSTEM SET pgmqtt.license_key = '%s'", sqlEscape(key))); err != nil {
		return fmt.Errorf("apply pgmqtt license key: %w", err)
	}
	if _, err := db.ExecContext(ctx, `SELECT pg_reload_conf()`); err != nil {
		return fmt.Errorf("reload after license key: %w", err)
	}
	return nil
}

// ensurePgmqttJWTLicensed bails out unless the broker has an active Enterprise
// license with the `jwt` feature. We check both `active` and `grace` so an
// expiring license doesn't take down the deployment mid-renewal.
func ensurePgmqttJWTLicensed(ctx context.Context, db *sql.DB) error {
	var status string
	var features []string
	row := db.QueryRowContext(ctx, `SELECT status, features FROM pgmqtt_license_status()`)
	if err := row.Scan(&status, &featureSlice{&features}); err != nil {
		return fmt.Errorf("pgmqtt_license_status: %w (Enterprise license required for trusted-broker mode)", err)
	}
	switch status {
	case "active", "grace":
	default:
		return fmt.Errorf("pgmqtt license status is %q, not active — set REPLAY_PGMQTT_LICENSE_KEY or configure pgmqtt.license_key manually", status)
	}
	for _, f := range features {
		if f == "jwt" {
			return nil
		}
	}
	return errors.New("pgmqtt Enterprise license does not include the 'jwt' feature — trusted-broker mode is unavailable")
}

// featureSlice scans pgmqtt_license_status().features (text[]) into a Go slice.
// We avoid pgx-specific scanners here because control-plane uses database/sql
// with the pgx stdlib driver, which returns text[] as a string we have to
// parse ourselves.
type featureSlice struct{ out *[]string }

func (f featureSlice) Scan(v any) error {
	switch s := v.(type) {
	case nil:
		*f.out = nil
		return nil
	case string:
		*f.out = parsePGTextArray(s)
		return nil
	case []byte:
		*f.out = parsePGTextArray(string(s))
		return nil
	default:
		return fmt.Errorf("unexpected type for text[]: %T", v)
	}
}

// parsePGTextArray turns `{"tls","jwt"}` into ["tls","jwt"]. Good enough for
// feature names which are short ASCII tokens; doesn't handle embedded quotes
// or commas (none appear in pgmqtt's feature vocabulary).
func parsePGTextArray(s string) []string {
	s = strings.TrimPrefix(strings.TrimSuffix(s, "}"), "{")
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.TrimPrefix(strings.TrimSuffix(p, `"`), `"`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// sqlEscape doubles single quotes for safe interpolation into ALTER SYSTEM
// strings, which don't accept bind parameters. Sufficient for the values we
// pass (base64 / file paths / JWT tokens — none contain newlines or backslashes
// that PostgreSQL would interpret).
func sqlEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
