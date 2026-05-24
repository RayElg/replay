package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/go-chi/chi/v5"
)

// User invites + password resets.
//
// In self-hosted mode, minting an invite or reset is an *operator* concern —
// `control-plane invite` and `control-plane reset-link` produce a URL the
// operator delivers to the user out-of-band. There's no in-app "send invite"
// button and no email backend to configure. The HTTP surface here only
// exposes the *consume* side of those tokens (/api/invites/accept and
// /api/auth/password/reset/confirm), which the invitee's browser hits when
// they open the link.
//
// In SSO mode the IdP owns user lifecycle, so neither the CLI nor the HTTP
// routes apply — registerUserManagementRoutes exits early.

const (
	inviteLifetime = 7 * 24 * time.Hour
	resetLifetime  = 1 * time.Hour
)

var errInviteFlowDisabled = errors.New("invite flow is only available in password auth mode")

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// mintInvite inserts a pending invite and returns the raw token. The caller
// (CLI handler) builds the user-facing URL from REPLAY_EXTERNAL_URL.
//
// Returns an error if the address already has a pending invite for this
// workspace or already corresponds to an existing user — operators should see
// those collisions explicitly rather than silently producing a second token.
func mintInvite(ctx context.Context, db *sql.DB, workspaceID, email string, invitedBy *string) (token string, expiresAt time.Time, err error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") {
		return "", time.Time{}, errors.New("valid email required")
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM user_invites
		  WHERE workspace_id = $1 AND lower(email) = $2
		    AND accepted_at IS NULL AND expires_at > now()`,
		workspaceID, email).Scan(&n); err != nil {
		return "", time.Time{}, err
	}
	if n > 0 {
		return "", time.Time{}, errors.New("an invite for this email is already pending in this workspace")
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE workspace_id = $1 AND lower(email) = $2`,
		workspaceID, email).Scan(&n); err != nil {
		return "", time.Time{}, err
	}
	if n > 0 {
		return "", time.Time{}, errors.New("user already exists in this workspace")
	}
	token, err = randomToken(24)
	if err != nil {
		return "", time.Time{}, err
	}
	expiresAt = time.Now().Add(inviteLifetime)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO user_invites (token, workspace_id, email, invited_by, expires_at)
		VALUES ($1, $2, $3, $4, $5)`,
		token, workspaceID, email, invitedBy, expiresAt,
	); err != nil {
		return "", time.Time{}, err
	}
	return token, expiresAt, nil
}

// mintPasswordReset issues a reset token for a known password-mode user. The
// caller (CLI handler) builds the user-facing URL.
//
// Unlike the old HTTP /reset/request endpoint, this *does* return an error if
// the email is unknown — there's no presence-leak concern across a CLI that
// only an operator can run, and surfacing "no such user" is far more useful
// than a silent success.
func mintPasswordReset(ctx context.Context, db *sql.DB, email string) (token string, expiresAt time.Time, err error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return "", time.Time{}, errors.New("email required")
	}
	var userID string
	err = db.QueryRowContext(ctx,
		`SELECT id FROM users WHERE lower(email) = $1 AND password_hash IS NOT NULL LIMIT 1`,
		email,
	).Scan(&userID)
	if err == sql.ErrNoRows {
		return "", time.Time{}, errors.New("no password-mode user with that email")
	}
	if err != nil {
		return "", time.Time{}, err
	}
	token, err = randomToken(24)
	if err != nil {
		return "", time.Time{}, err
	}
	expiresAt = time.Now().Add(resetLifetime)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO user_password_resets (token, user_id, expires_at)
		VALUES ($1, $2, $3)`,
		token, userID, expiresAt,
	); err != nil {
		return "", time.Time{}, err
	}
	return token, expiresAt, nil
}

// externalURLBase resolves the operator-configured external URL. CLI commands
// that print user-facing links require this to be set — otherwise we'd print
// half-URLs the operator has to splice into a host they remember in their
// head, which is the kind of footgun that produces a typo'd link landing the
// invitee on a 404.
func externalURLBase() (string, error) {
	v := strings.TrimRight(strings.TrimSpace(os.Getenv("REPLAY_EXTERNAL_URL")), "/")
	if v == "" {
		return "", errors.New("REPLAY_EXTERNAL_URL is required to print invite/reset links")
	}
	return v, nil
}

// ─── HTTP (consume side only) ──────────────────────────────────────────

type inviteAcceptRequest struct {
	Password string `json:"password"`
	Name     string `json:"name"`
}

type resetConfirmBody struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

func registerUserManagementRoutes(r chi.Router, db *sql.DB) {
	// POST /api/invites/accept?token=... — public; the invitee isn't signed
	// in yet. Creates the user, marks invite consumed, returns a session
	// cookie in one shot so the UI can drop straight into /.
	r.Post("/api/invites/accept", func(w http.ResponseWriter, req *http.Request) {
		token := strings.TrimSpace(req.URL.Query().Get("token"))
		if token == "" {
			http.Error(w, "token required", http.StatusBadRequest)
			return
		}
		var body inviteAcceptRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if len(body.Password) < 8 {
			http.Error(w, "password must be at least 8 chars", http.StatusBadRequest)
			return
		}
		var workspaceID, email string
		var expiresAt time.Time
		err := db.QueryRowContext(req.Context(), `
			SELECT workspace_id, email, expires_at FROM user_invites
			 WHERE token = $1 AND accepted_at IS NULL`, token,
		).Scan(&workspaceID, &email, &expiresAt)
		if err == sql.ErrNoRows {
			http.Error(w, "invite not found or already used", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if time.Now().After(expiresAt) {
			http.Error(w, "invite expired", http.StatusGone)
			return
		}
		name := strings.TrimSpace(body.Name)
		if name == "" {
			name = email
		}
		userID, err := createOrUpdateUser(req.Context(), db, workspaceID, email, name, body.Password)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := db.ExecContext(req.Context(),
			`UPDATE user_invites SET accepted_at = now() WHERE token = $1`, token,
		); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		sid, err := createUserSession(req.Context(), db, userID, workspaceID, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		setSessionCookie(w, req, sid)
		w.WriteHeader(http.StatusNoContent)
	})

	// POST /api/auth/password/reset/confirm — public; consumes a reset token.
	r.Post("/api/auth/password/reset/confirm", func(w http.ResponseWriter, req *http.Request) {
		var body resetConfirmBody
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if len(body.NewPassword) < 8 {
			http.Error(w, "new password must be at least 8 chars", http.StatusBadRequest)
			return
		}
		tx, err := db.BeginTx(req.Context(), nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()
		var userID string
		var expiresAt time.Time
		err = tx.QueryRowContext(req.Context(), `
			SELECT user_id, expires_at FROM user_password_resets
			 WHERE token = $1 AND used_at IS NULL FOR UPDATE`,
			body.Token,
		).Scan(&userID, &expiresAt)
		if err == sql.ErrNoRows {
			http.Error(w, "invalid or used token", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if time.Now().After(expiresAt) {
			http.Error(w, "token expired", http.StatusGone)
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(body.NewPassword), bcryptCost)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := tx.ExecContext(req.Context(),
			`UPDATE users SET password_hash = $1 WHERE id = $2`, string(hash), userID,
		); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := tx.ExecContext(req.Context(),
			`UPDATE user_password_resets SET used_at = now() WHERE token = $1`, body.Token,
		); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Wipe every active session for the user — they should re-login with
		// the new password.
		if _, err := tx.ExecContext(req.Context(),
			`DELETE FROM user_sessions WHERE user_id = $1`, userID,
		); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// gcUserInvitesAndResets clears expired/used tokens. Cheap; runs hourly from main.
func gcUserInvitesAndResets(ctx context.Context, db *sql.DB) {
	t := time.NewTicker(1 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := db.ExecContext(ctx,
				`DELETE FROM user_invites WHERE expires_at < now() - interval '24 hours' OR accepted_at < now() - interval '7 days'`,
			); err != nil {
				slog.Warn("gc: user_invites delete failed", "error", err)
			}
			if _, err := db.ExecContext(ctx,
				`DELETE FROM user_password_resets WHERE expires_at < now() - interval '24 hours' OR used_at < now() - interval '24 hours'`,
			); err != nil {
				slog.Warn("gc: user_password_resets delete failed", "error", err)
			}
		}
	}
}
