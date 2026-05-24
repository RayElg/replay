package main

import (
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// brokerJWT is an Ed25519-signed token the browser presents to pgmqtt as the
// MQTT CONNECT password when REPLAY_MQTT_TRUSTED_BROKER=true. pgmqtt verifies
// the signature against the configured public key (set via ALTER SYSTEM on
// boot — see configurePgmqttJWT in mqtt_auth.go) and enforces the sub_claims
// topic filters at SUBSCRIBE time. Format mirrors what pgmqtt Enterprise's
// `jwt` feature consumes — see docs/enterprise.md in the pgmqtt repo.
//
// We mint the token ourselves rather than using a JWT library to keep the
// trust surface tiny and the claim shape under our control. Standard 3-part
// JWT: base64url(header) . base64url(payload) . base64url(signature).

type brokerClaims struct {
	// Standard claims pgmqtt recognises. exp is required; everything else is
	// optional but we set them all so the broker has full context for logging
	// and the client_id binding catches imposters.
	Subject   string   `json:"sub"`
	IssuedAt  int64    `json:"iat"`
	ExpiresAt int64    `json:"exp"`
	ClientID  string   `json:"client_id"`
	SubClaims []string `json:"sub_claims"`
	PubClaims []string `json:"pub_claims"`
}

// Long enough that ordinary network blips don't outlive the token (mqtt.js
// reconnects in seconds); short enough that a leaked token has limited impact.
// The UI proactively re-mints a token before exp via a refresh timer.
const brokerJWTLifetime = 1 * time.Hour

// brokerJWTPrivateKey loads the Ed25519 signing key from REPLAY_BROKER_JWT_PRIVATE_KEY.
// The env value is the 32-byte seed base64-encoded (what `control-plane
// gen-broker-jwt-key` prints as "private"). We expand it to a full 64-byte
// ed25519.PrivateKey via NewKeyFromSeed.
func brokerJWTPrivateKey() (ed25519.PrivateKey, error) {
	raw := strings.TrimSpace(os.Getenv("REPLAY_BROKER_JWT_PRIVATE_KEY"))
	if raw == "" {
		return nil, errors.New("REPLAY_BROKER_JWT_PRIVATE_KEY is not set — run `control-plane gen-broker-jwt-key` to mint one")
	}
	seed, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		// Tolerate base64url variants from the operator's clipboard.
		seed, err = base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("REPLAY_BROKER_JWT_PRIVATE_KEY: not valid base64: %w", err)
		}
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("REPLAY_BROKER_JWT_PRIVATE_KEY: expected %d-byte seed, got %d", ed25519.SeedSize, len(seed))
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// brokerJWTPublicKey derives the public key from the configured private key.
// Returns the raw 32 bytes — pgmqtt accepts base64url(32 bytes) as the
// `pgmqtt.jwt_public_key` GUC value.
func brokerJWTPublicKey() ([]byte, error) {
	priv, err := brokerJWTPrivateKey()
	if err != nil {
		return nil, err
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("derived public key has wrong type")
	}
	return pub, nil
}

// issueBrokerJWT mints a token scoped to a single workspace + browser session.
// The sub_claims topic filter restricts SUBSCRIBE to topics under
// `runs/{workspace_id}/`, which covers both the per-run and per-result UI
// streams (see migration 0001 pgmqtt mappings). pub_claims is empty — browsers
// never publish; CDC events flow one-way DB → broker → browser.
//
// The client_id claim binds the token to a specific MQTT client identifier so
// even a leaked token can't be used to hijack another tab's connection.
func issueBrokerJWT(workspaceID, userID, clientID string) (string, error) {
	priv, err := brokerJWTPrivateKey()
	if err != nil {
		return "", err
	}
	now := time.Now()
	claims := brokerClaims{
		Subject:   userID,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(brokerJWTLifetime).Unix(),
		ClientID:  clientID,
		SubClaims: []string{"runs/" + workspaceID + "/#"},
		PubClaims: []string{},
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	body := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	sig := ed25519.Sign(priv, []byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func registerBrokerTokenRoute(r chi.Router, db *sql.DB) {
	r.Post("/api/auth/broker-token", func(w http.ResponseWriter, req *http.Request) {
		if !mqttTrustedBroker() {
			http.Error(w, "trusted broker not enabled", http.StatusNotFound)
			return
		}
		ar := authResultFromContext(req.Context())
		if ar == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		clientID, err := mqttClientID(ar.WorkspaceID, ar.ActorID)
		if err != nil {
			http.Error(w, "client_id generation failed", http.StatusInternalServerError)
			return
		}
		tok, err := issueBrokerJWT(ar.WorkspaceID, ar.ActorID, clientID)
		if err != nil {
			slog.Error("broker token issuance failed", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      tok,
			"client_id":  clientID,
			"expires_in": int(brokerJWTLifetime.Seconds()),
		})
	})
}

func mqttTrustedBroker() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("REPLAY_MQTT_TRUSTED_BROKER")))
	return v == "true" || v == "1"
}
